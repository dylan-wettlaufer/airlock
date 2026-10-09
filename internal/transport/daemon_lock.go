package transport

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"airlock/internal/coordinator"
	"airlock/internal/protocol"
)

// DaemonLock reserves a socket before database migration/recovery. The lock file
// is persistent: unlinking it would let a new process lock a different inode.
// Close releases ownership after the server, coordinator, and database stop.
type DaemonLock struct {
	mu        sync.Mutex
	path      string
	file      *os.File
	identity  os.FileInfo
	stale     os.FileInfo
	listening bool
}

func privateLock(info os.FileInfo) bool {
	return info.Mode().IsRegular() && owned(info) && info.Mode().Perm() == 0600
}

// AcquireDaemon validates the endpoint before callers touch persistent history.
// Only a private socket with an explicit connection refusal is considered stale.
func AcquireDaemon(path string) (*DaemonLock, error) {
	if err := privateDirectory(path, true); err != nil {
		return nil, err
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	path = filepath.Join(dir, filepath.Base(path))
	lockPath := path + ".lock"
	if info, err := os.Lstat(lockPath); err == nil {
		if !privateLock(info) {
			return nil, errors.New("daemon lock must be a private user-owned regular file, not a symlink")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("could not inspect daemon lock")
	}
	fd, err := syscall.Open(lockPath, syscall.O_RDWR|syscall.O_CREAT|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, errors.New("could not open daemon lock")
	}
	f := os.NewFile(uintptr(fd), lockPath)
	success := false
	defer func() {
		if !success {
			f.Close()
		}
	}()
	info, err := f.Stat()
	if err != nil || !privateLock(info) {
		return nil, errors.New("daemon lock has unsafe ownership, type, or permissions")
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, errors.New("another daemon owns this socket")
	}
	current, err := os.Lstat(lockPath)
	if err != nil || !os.SameFile(info, current) {
		return nil, errors.New("daemon lock changed during startup")
	}
	stale, err := staleSocket(path)
	if err != nil {
		return nil, err
	}
	success = true
	return &DaemonLock{path: path, file: f, identity: info, stale: stale}, nil
}

func staleSocket(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("could not inspect existing socket")
	}
	if err := validateSocket(path); err != nil {
		return nil, err
	}
	conn, err := net.DialTimeout("unix", path, 200*time.Millisecond)
	if err == nil {
		conn.Close()
		return nil, errors.New("socket already has an active listener; refusing to replace it")
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		return nil, errors.New("could not prove existing socket is stale; refusing to replace it")
	}
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, current) {
		return nil, errors.New("socket changed while checking for a stale listener")
	}
	return info, nil
}

// Listen binds only after callers have completed database setup and recovery.
// The caller retains the lock until after Serve and database shutdown.
func (d *DaemonLock) Listen(wait time.Duration, queue *coordinator.Coordinator) (*Server, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.file == nil || d.listening {
		return nil, errors.New("daemon lock is closed or already serving")
	}
	if queue == nil {
		return nil, errors.New("coordinator is required")
	}
	if wait <= 0 || wait > protocol.MaxWait {
		return nil, errors.New("wait must be positive and at most 24h")
	}
	if err := privateDirectory(d.path, false); err != nil {
		return nil, err
	}
	lockInfo, err := os.Lstat(d.path + ".lock")
	if err != nil || !privateLock(lockInfo) || !os.SameFile(d.identity, lockInfo) {
		return nil, errors.New("daemon lock changed during startup")
	}
	current, err := staleSocket(d.path)
	if err != nil {
		return nil, err
	}
	if d.stale == nil && current != nil || d.stale != nil && current != nil && !os.SameFile(d.stale, current) {
		return nil, errors.New("socket changed during startup; refusing to replace it")
	}
	if current != nil {
		// Recheck identity immediately before unlinking. The private directory
		// prevents other users from swapping the endpoint or lock file.
		latest, err := os.Lstat(d.path)
		if err != nil || !os.SameFile(current, latest) {
			return nil, errors.New("stale socket changed before removal")
		}
		if err := os.Remove(d.path); err != nil {
			return nil, errors.New("could not remove stale socket")
		}
	}
	server, err := listenSocket(d.path, wait, queue)
	if err != nil {
		return nil, err
	}
	d.listening = true
	return server, nil
}

func (d *DaemonLock) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.file == nil {
		return nil
	}
	err := d.file.Close() // Closing also releases flock, including after a crash.
	d.file = nil
	return err
}
