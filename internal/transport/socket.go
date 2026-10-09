// Package transport carries bounded protocol frames over private Unix sockets.
package transport

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"airlock/internal/coordinator"
	"airlock/internal/protocol"
)

const ioTimeout = 2 * time.Second
const maxConnections = 256

// ErrDaemonNotRunning means no daemon was reachable before exchanging any
// request. It excludes unsafe paths and failures after a successful connection.
var ErrDaemonNotRunning = errors.New("daemon is not running")

func DefaultSocket() string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("airlock-%d", os.Getuid()), "airlock.sock")
}

func owned(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Getuid())
}

func privateDirectory(path string, create bool) error {
	if !filepath.IsAbs(path) {
		return errors.New("socket path must be absolute")
	}
	dir := filepath.Dir(path)
	if create {
		if err := os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return errors.New("could not create socket directory")
		}
	}
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return ErrDaemonNotRunning
	}
	if err != nil || !info.IsDir() || !owned(info) || info.Mode().Perm() != 0700 {
		return errors.New("socket directory must be owned by this user, mode 0700, and not a symlink")
	}
	return nil
}

func validateSocket(path string) error {
	if err := privateDirectory(path, false); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return ErrDaemonNotRunning
	}
	if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0600 || !owned(info) {
		return errors.New("socket is unavailable or has unsafe ownership or permissions")
	}
	return nil
}

type Server struct {
	listener    *net.UnixListener
	path        string
	identity    os.FileInfo
	wait        time.Duration
	queue       *coordinator.Coordinator
	mu          sync.Mutex
	connections map[net.Conn]struct{}
	wg          sync.WaitGroup
	once        sync.Once
	daemonLock  *DaemonLock
}

func Listen(path string, wait time.Duration) (*Server, error) {
	return ListenWithCoordinator(path, wait, coordinator.New())
}

// ListenWithCoordinator lets the daemon supply a durable audit recorder.
func ListenWithCoordinator(path string, wait time.Duration, queue *coordinator.Coordinator) (*Server, error) {
	lock, err := AcquireDaemon(path)
	if err != nil {
		return nil, err
	}
	server, err := lock.Listen(wait, queue)
	if err != nil {
		lock.Close()
		return nil, err
	}
	server.daemonLock = lock
	return server, nil
}

func listenSocket(path string, wait time.Duration, queue *coordinator.Coordinator) (*Server, error) {
	if queue == nil {
		return nil, errors.New("coordinator is required")
	}
	if wait <= 0 || wait > protocol.MaxWait {
		return nil, errors.New("wait must be positive and at most 24h")
	}
	if err := privateDirectory(path, true); err != nil {
		return nil, err
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("socket path already exists; refusing to replace it")
	}
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, errors.New("could not bind Unix socket (check path length and availability)")
	}
	l.SetUnlinkOnClose(false)
	identity, err := os.Lstat(path)
	if err != nil {
		l.Close()
		return nil, errors.New("could not inspect socket")
	}
	s := &Server{listener: l, path: path, identity: identity, wait: wait, queue: queue, connections: make(map[net.Conn]struct{})}
	if err := os.Chmod(path, 0600); err != nil {
		l.Close()
		s.cleanup()
		return nil, errors.New("could not restrict socket permissions")
	}
	return s, nil
}

func (s *Server) cleanup() {
	if current, err := os.Lstat(s.path); err == nil && os.SameFile(s.identity, current) {
		_ = os.Remove(s.path)
	}
}

func (s *Server) stop() {
	s.once.Do(func() {
		s.queue.Close()
		_ = s.listener.Close()
		s.mu.Lock()
		defer s.mu.Unlock()
		for conn := range s.connections {
			_ = conn.SetReadDeadline(time.Now())
		}
	})
}

// Serve blocks until cancellation or a listener failure, then releases waiters.
func (s *Server) Serve(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			s.stop()
		case <-done:
		}
	}()
	defer func() {
		s.stop()
		s.wg.Wait()
		s.cleanup()
		if s.daemonLock != nil {
			_ = s.daemonLock.Close()
		}
		close(done)
	}()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return errors.New("daemon listener stopped")
		}
		s.mu.Lock()
		if len(s.connections) >= maxConnections {
			s.mu.Unlock()
			conn.Close()
			continue
		}
		s.connections[conn] = struct{}{}
		s.wg.Add(1)
		s.mu.Unlock()
		go func() {
			defer s.wg.Done()
			defer func() { conn.Close(); s.mu.Lock(); delete(s.connections, conn); s.mu.Unlock() }()
			s.handle(conn)
		}()
	}
}

func send(conn net.Conn, response protocol.Response) error {
	response.ProtocolVersion = protocol.Version
	_ = conn.SetWriteDeadline(time.Now().Add(ioTimeout))
	return protocol.WriteFrame(conn, response)
}

func sendError(conn net.Conn, err error) {
	var wire *protocol.WireError
	if !errors.As(err, &wire) {
		wire = protocol.Error("invalid_frame", "could not read request frame")
	}
	_ = send(conn, protocol.Response{Type: "error", Error: wire})
}

func (s *Server) handle(conn net.Conn) {
	_ = conn.SetReadDeadline(time.Now().Add(ioTimeout))
	reader := bufio.NewReader(conn)
	var m protocol.Message
	if err := protocol.ReadFrame(reader, &m); err != nil {
		sendError(conn, err)
		return
	}
	if err := m.Validate(); err != nil {
		sendError(conn, err)
		return
	}
	switch m.Type {
	case "health":
		_ = send(conn, protocol.Response{Type: "health"})
	case "list":
		for _, p := range s.queue.List() {
			if err := send(conn, protocol.Response{Type: "pending", Pending: &p}); err != nil {
				return
			}
		}
		_ = send(conn, protocol.Response{Type: "list"})
	case "decide":
		r, err := s.queue.Decide(m.RequestID, m.Permission)
		if err != nil {
			sendError(conn, err)
			return
		}
		_ = send(conn, protocol.Response{Type: "decide", Result: &r})
	case "submit":
		wait := s.wait
		if m.WaitMS > 0 && time.Duration(m.WaitMS)*time.Millisecond < wait {
			wait = time.Duration(m.WaitMS) * time.Millisecond
		}
		result, err := s.queue.Submit(*m.Request, wait)
		if err != nil {
			sendError(conn, err)
			return
		}
		_ = conn.SetReadDeadline(time.Time{})
		monitored := make(chan struct{})
		go func() {
			defer close(monitored)
			// Any further input or EOF invalidates this one-operation connection.
			_, _ = reader.ReadByte()
			s.queue.Cancel(m.Request.RequestID, result)
		}()
		// Cancellation also publishes a terminal result. Always drain that result,
		// including shutdown, rather than racing it against a disconnect signal.
		defer func() { conn.Close(); <-monitored }()
		r := <-result
		_ = send(conn, protocol.Response{Type: "submit", Result: &r})
	}
}
