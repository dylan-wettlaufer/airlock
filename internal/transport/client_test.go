package transport

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestDaemonNotRunningErrorClassification(t *testing.T) {
	path := socketPath(t)
	for _, absent := range []string{path, filepath.Join(filepath.Dir(path), "missing", "airlock.sock")} {
		if err := (Client{Socket: absent}).Health(context.Background()); !errors.Is(err, ErrDaemonNotRunning) {
			t.Fatalf("absent daemon: %v", err)
		}
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	if err := os.Chmod(path, 0600); err != nil {
		listener.Close()
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := (Client{Socket: path}).Health(context.Background()); !errors.Is(err, ErrDaemonNotRunning) {
		t.Fatalf("stale socket: %v", err)
	}
	if err := os.Chmod(path, 0666); err != nil {
		t.Fatal(err)
	}
	if err := (Client{Socket: path}).Health(context.Background()); err == nil || errors.Is(err, ErrDaemonNotRunning) {
		t.Fatalf("unsafe socket treated as offline: %v", err)
	}
}
