package transport

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"airlock/internal/protocol"
)

func socketPath(t *testing.T) string {
	t.Helper()
	// Keep paths short: macOS Unix socket names have a small length limit.
	dir, err := os.MkdirTemp("/tmp", "airlock-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "airlock.sock")
}

func startServer(t *testing.T, wait time.Duration) (*Server, Client) {
	t.Helper()
	path := socketPath(t)
	server, err := Listen(path, wait)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("daemon shutdown hung")
		}
	})
	return server, Client{Socket: path}
}

func request(id string) protocol.Request {
	return protocol.Request{ProtocolVersion: protocol.Version, RequestID: id, Agent: "cursor", ConversationID: "chat-" + id, Event: "before_shell_execution", Command: "printf test"}
}

func waitCount(t *testing.T, client Client, count int) []protocol.Pending {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		items, err := client.List(context.Background())
		if err == nil && len(items) == count {
			return items
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("queue never reached %d requests", count)
	return nil
}

func rawSubmit(t *testing.T, client Client, r protocol.Request) net.Conn {
	t.Helper()
	conn, err := net.Dial("unix", client.Socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := protocol.WriteFrame(conn, protocol.Message{ProtocolVersion: protocol.Version, Type: "submit", Request: &r}); err != nil {
		t.Fatal(err)
	}
	return conn
}

func TestRoutingDuplicateDisconnectAndHealth(t *testing.T) {
	_, client := startServer(t, time.Hour)
	if err := client.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	one := rawSubmit(t, client, request("one"))
	two := rawSubmit(t, client, request("two"))
	waitCount(t, client, 2)
	_, err := client.Submit(context.Background(), request("one"), time.Second)
	var wire *protocol.WireError
	if !errors.As(err, &wire) || wire.Code != "duplicate_request" {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := client.Decide(context.Background(), "two", "deny"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Decide(context.Background(), "one", "allow"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		conn           net.Conn
		id, permission string
	}{{one, "one", "allow"}, {two, "two", "deny"}} {
		_ = tc.conn.SetReadDeadline(time.Now().Add(time.Second))
		var response protocol.Response
		if err := protocol.ReadFrame(bufio.NewReader(tc.conn), &response); err != nil {
			t.Fatal(err)
		}
		if response.Result == nil || response.Result.RequestID != tc.id || response.Result.Permission != tc.permission {
			t.Fatalf("misrouted: %+v", response)
		}
	}
	third := rawSubmit(t, client, request("three"))
	waitCount(t, client, 1)
	third.Close()
	waitCount(t, client, 0)
	if _, err := client.Decide(context.Background(), "three", "allow"); err == nil {
		t.Fatal("allowed disconnected submitter")
	}
}

func TestBoundedFramesAndFragmentedInput(t *testing.T) {
	_, client := startServer(t, time.Second)
	for _, tc := range []struct{ name, data, want string }{
		{"malformed", "{\n", "invalid_frame"},
		{"oversized", strings.Repeat(" ", protocol.MaxFrameBytes) + "\n", "frame_too_large"},
		{"version", "{\"protocol_version\":2,\"type\":\"health\"}\n", "unsupported_version"},
		{"type", "{\"protocol_version\":1,\"type\":\"unknown\"}\n", "invalid_message"},
		{"fields", "{\"protocol_version\":1,\"type\":\"submit\"}\n", "invalid_message"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, err := net.Dial("unix", client.Socket)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			if _, err := conn.Write([]byte(tc.data)); err != nil {
				t.Fatal(err)
			}
			var response protocol.Response
			if err := protocol.ReadFrame(bufio.NewReader(conn), &response); err != nil {
				t.Fatal(err)
			}
			if response.Type != "error" || response.Error == nil || response.Error.Code != tc.want {
				t.Fatalf("got %+v, want %s", response, tc.want)
			}
		})
	}
	conn, err := net.Dial("unix", client.Socket)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	for _, piece := range []string{"{\"protocol_version\":", "1,\"type\":", "\"health\"}", "\n"} {
		if _, err := conn.Write([]byte(piece)); err != nil {
			t.Fatal(err)
		}
	}
	var response protocol.Response
	if err := protocol.ReadFrame(bufio.NewReader(conn), &response); err != nil || response.Type != "health" {
		t.Fatalf("fragmented frame: %+v %v", response, err)
	}
}

func TestLargeQueueListUsesMultipleFrames(t *testing.T) {
	_, client := startServer(t, time.Hour)
	for _, id := range []string{"one", "two", "three", "four"} {
		r := request(id)
		r.Command = strings.Repeat("x", 700000)
		rawSubmit(t, client, r)
	}
	items := waitCount(t, client, 4)
	for _, p := range items {
		if len(p.Request.Command) != 700000 {
			t.Fatal("large proposal truncated")
		}
	}
}

func TestWaitCeilingAndCancellation(t *testing.T) {
	_, client := startServer(t, 20*time.Millisecond)
	r, err := client.Submit(context.Background(), request("expires"), time.Hour)
	if err != nil || r.Permission != "deny" || r.State != "expired" {
		t.Fatalf("wait ceiling: %+v %v", r, err)
	}
	_, client = startServer(t, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := client.Submit(ctx, request("cancelled"), time.Hour); done <- err }()
	waitCount(t, client, 1)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled client returned success")
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not close client")
	}
	waitCount(t, client, 0)
}

func TestShutdownAndSocketIdentity(t *testing.T) {
	path := socketPath(t)
	s, err := Listen(path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	client := Client{Socket: path}
	conn := rawSubmit(t, client, request("waiting"))
	waitCount(t, client, 1)
	cancel()
	var response protocol.Response
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	if err := protocol.ReadFrame(bufio.NewReader(conn), &response); err != nil {
		t.Fatal(err)
	}
	if response.Result == nil || response.Result.State != "interrupted" || response.Result.Permission != "deny" {
		t.Fatalf("shutdown: %+v", response)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("socket not cleaned up")
	}

	s, err = Listen(path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	if err := s.Serve(ctx); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "replacement" {
		t.Fatal("removed a replacement socket path")
	}
}

func TestUnsafeAndOccupiedPaths(t *testing.T) {
	path := socketPath(t)
	if err := os.Chmod(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(path, time.Second); err == nil {
		t.Fatal("accepted public directory")
	}
	if err := os.Chmod(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(path, time.Second); err == nil {
		t.Fatal("replaced an occupied path")
	}
	if data, _ := os.ReadFile(path); string(data) != "keep" {
		t.Fatal("occupied path changed")
	}
	if err := (Client{Socket: path}).Health(context.Background()); err == nil {
		t.Fatal("accepted non-socket")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := (Client{Socket: path}).Health(context.Background()); err == nil {
		t.Fatal("accepted unavailable daemon")
	}
	symlink := filepath.Join(filepath.Dir(path), "link")
	if err := os.Symlink(filepath.Dir(path), symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(filepath.Join(symlink, "other.sock"), time.Second); err == nil {
		t.Fatal("accepted symlink directory")
	}
	_, client := startServer(t, time.Second)
	if _, err := Listen(client.Socket, time.Second); err == nil {
		t.Fatal("second daemon replaced socket")
	}
	if err := os.Chmod(client.Socket, 0666); err != nil {
		t.Fatal(err)
	}
	if err := client.Health(context.Background()); err == nil {
		t.Fatal("accepted public socket")
	}
}

func TestClientRejectsInvalidResponses(t *testing.T) {
	for _, response := range []protocol.Response{
		{ProtocolVersion: 2, Type: "submit", Result: &protocol.Result{RequestID: "one", State: "allowed", Permission: "allow"}},
		{ProtocolVersion: 1, Type: "submit", Result: &protocol.Result{RequestID: "other", State: "allowed", Permission: "allow"}},
		{ProtocolVersion: 1, Type: "submit", Result: &protocol.Result{RequestID: "one", State: "expired", Permission: "allow"}},
		{ProtocolVersion: 1, Type: "health"},
	} {
		path := socketPath(t)
		listener, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0600); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
			var m protocol.Message
			_ = json.NewDecoder(conn).Decode(&m)
			_ = protocol.WriteFrame(conn, response)
		}()
		if _, err := (Client{Socket: path}).Submit(context.Background(), request("one"), time.Second); err == nil {
			t.Fatalf("accepted invalid response: %+v", response)
		}
		listener.Close()
		<-done
	}
}
