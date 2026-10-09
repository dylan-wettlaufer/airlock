package transport

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"airlock/internal/coordinator"
)

func leaveSocket(t *testing.T, path string) os.FileInfo {
	t.Helper()
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	l.SetUnlinkOnClose(false)
	if err := os.Chmod(path, 0600); err != nil {
		l.Close()
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func TestDaemonLockReservesEndpointBeforeBinding(t *testing.T) {
	path := socketPath(t)
	lock, err := AcquireDaemon(path)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("lock acquisition bound a socket before recovery")
	}
	info, err := os.Lstat(path + ".lock")
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("daemon lock permissions: %v", err)
	}
	if other, err := AcquireDaemon(path); err == nil {
		other.Close()
		t.Fatal("second daemon acquired reserved endpoint")
	}
	// A path through /tmp and its canonical alias must share the same lock.
	canonical, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if other, err := AcquireDaemon(filepath.Join(canonical, filepath.Base(path))); err == nil {
		other.Close()
		t.Fatal("alias bypassed daemon lock")
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	other, err := AcquireDaemon(path)
	if err != nil {
		t.Fatal(err)
	}
	other.Close()
	after, err := os.Lstat(path + ".lock")
	if err != nil || !os.SameFile(info, after) {
		t.Fatal("lock inode replaced between acquisitions")
	}
	if _, err := lock.Listen(time.Second, coordinator.New()); err == nil {
		t.Fatal("closed lease allowed binding")
	}
}

func TestVerifiedStaleSocketReplacedOnlyAtListen(t *testing.T) {
	path := socketPath(t)
	stale := leaveSocket(t, path)
	lock, err := AcquireDaemon(path)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	before, err := os.Lstat(path)
	if err != nil || !os.SameFile(stale, before) {
		t.Fatal("stale socket removed before database setup")
	}
	server, err := lock.Listen(time.Second, coordinator.New())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lock.Listen(time.Second, coordinator.New()); err == nil {
		t.Fatal("lease bound twice")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := server.Serve(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("replacement socket was not cleaned up")
	}
	// Lock remains held through caller-managed database shutdown.
	if other, err := AcquireDaemon(path); err == nil {
		other.Close()
		t.Fatal("server released caller-owned startup lock")
	}
}

func TestDaemonLockRefusesLiveUnmanagedListener(t *testing.T) {
	path := socketPath(t)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if lock, err := AcquireDaemon(path); err == nil {
		lock.Close()
		t.Fatal("replaced a live listener without a lock file")
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("live socket changed")
	}
}

func TestDaemonLockRejectsUnsafeFiles(t *testing.T) {
	for _, mode := range []string{"public-lock", "symlink-lock", "directory-lock", "public-socket", "socket-symlink", "regular-socket"} {
		t.Run(mode, func(t *testing.T) {
			path := socketPath(t)
			switch mode {
			case "public-lock":
				if err := os.WriteFile(path+".lock", []byte("keep"), 0644); err != nil {
					t.Fatal(err)
				}
			case "symlink-lock":
				if err := os.Symlink("missing", path+".lock"); err != nil {
					t.Fatal(err)
				}
			case "directory-lock":
				if err := os.Mkdir(path+".lock", 0700); err != nil {
					t.Fatal(err)
				}
			case "public-socket":
				leaveSocket(t, path)
				if err := os.Chmod(path, 0666); err != nil {
					t.Fatal(err)
				}
			case "socket-symlink":
				if err := os.Symlink("missing", path); err != nil {
					t.Fatal(err)
				}
			case "regular-socket":
				if err := os.WriteFile(path, []byte("keep"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			check := path
			if mode == "public-lock" || mode == "symlink-lock" || mode == "directory-lock" {
				check = path + ".lock"
			}
			before, err := os.Lstat(check)
			if err != nil {
				t.Fatal(err)
			}
			if lock, err := AcquireDaemon(path); err == nil {
				lock.Close()
				t.Fatal("unsafe endpoint accepted")
			}
			after, err := os.Lstat(check)
			if err != nil || !os.SameFile(before, after) {
				t.Fatal("unsafe file removed or replaced")
			}
		})
	}
}

func TestStartupRefusesSocketAndLockReplacement(t *testing.T) {
	for _, mode := range []string{"socket-file", "socket-new-stale", "lock-file"} {
		t.Run(mode, func(t *testing.T) {
			path := socketPath(t)
			leaveSocket(t, path)
			lock, err := AcquireDaemon(path)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			target := path
			if mode == "lock-file" {
				target = path + ".lock"
			}
			// Rename keeps the old inode alive, avoiding inode reuse in this test.
			if err := os.Rename(target, target+".old"); err != nil {
				t.Fatal(err)
			}
			if mode == "socket-new-stale" {
				leaveSocket(t, path)
			} else if err := os.WriteFile(target, []byte("replacement"), 0600); err != nil {
				t.Fatal(err)
			}
			before, err := os.Lstat(target)
			if err != nil {
				t.Fatal(err)
			}
			if server, err := lock.Listen(time.Second, coordinator.New()); err == nil {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				server.Serve(ctx)
				t.Fatal("startup replaced an endpoint changed during recovery")
			}
			after, err := os.Lstat(target)
			if err != nil || !os.SameFile(before, after) {
				t.Fatal("replacement file was removed")
			}
		})
	}
}
