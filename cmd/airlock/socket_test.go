package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"airlock/internal/adapters/cursor"
	"airlock/internal/protocol"
	"airlock/internal/transport"
)

type cliProcess struct {
	cmd            *exec.Cmd
	stdout, stderr bytes.Buffer
	done           chan error
}

func startCLI(t *testing.T, input string, args ...string) *cliProcess {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	p := &cliProcess{cmd: exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=TestHelperProcess", "--"}, args...)...), done: make(chan error, 1)}
	p.cmd.Env = append(os.Environ(), "AIRLOCK_TEST_HELPER=1")
	p.cmd.Stdin = strings.NewReader(input)
	p.cmd.Stdout, p.cmd.Stderr = &p.stdout, &p.stderr
	if err := p.cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	go func() { p.done <- p.cmd.Wait(); close(p.done) }()
	t.Cleanup(func() { cancel(); <-p.done })
	return p
}

func finishCLI(t *testing.T, p *cliProcess) string {
	t.Helper()
	select {
	case err := <-p.done:
		if err != nil {
			t.Fatalf("CLI failed: %v: %s", err, p.stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CLI did not finish")
	}
	return p.stdout.String()
}

func shortSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "airlock-cli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "airlock.sock")
}

func daemonCLI(t *testing.T, wait string) (string, *cliProcess) {
	t.Helper()
	path := shortSocket(t)
	p := startCLI(t, "", "daemon", "--socket", path, "--wait", wait)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := (transport.Client{Socket: path}).Health(context.Background()); err == nil {
			return path, p
		}
		select {
		case err := <-p.done:
			t.Fatalf("daemon exited early: %v: %s", err, p.stderr.String())
		default:
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("daemon did not start")
	return "", nil
}

func pendingCLI(t *testing.T, path string, count int) []protocol.Pending {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		items, err := (transport.Client{Socket: path}).List(context.Background())
		if err == nil && len(items) == count {
			return items
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("queue did not reach %d requests", count)
	return nil
}

func nativePayload(t *testing.T, conversation string) string {
	t.Helper()
	data, err := json.Marshal(cursor.Input{ConversationID: conversation, HookEventName: "beforeShellExecution", CursorVersion: "fixture", Command: "printf 'airlock-test\\n'"})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func nativePermission(t *testing.T, p *cliProcess, want string) {
	t.Helper()
	output := finishCLI(t, p)
	var response cursor.Response
	if err := json.Unmarshal([]byte(output), &response); err != nil || response.Permission != want {
		t.Fatalf("native response %q: %v, want %s", output, err, want)
	}
	if strings.Contains(p.stderr.String(), "printf") {
		t.Fatal("command leaked to diagnostics")
	}
}

func TestTwoConcurrentHookProcesses(t *testing.T) {
	path, daemon := daemonCLI(t, "5s")
	one := startCLI(t, nativePayload(t, "chat-one"), "hook", "--socket", path, "--wait", "5s")
	two := startCLI(t, nativePayload(t, "chat-two"), "hook", "--socket", path, "--wait", "5s")
	pendingCLI(t, path, 2)
	for _, p := range []*cliProcess{one, two} {
		select {
		case err := <-p.done:
			t.Fatalf("hook returned before review: %v", err)
		default:
		}
	}
	listed := finishCLI(t, startCLI(t, "", "list", "--socket", path, "--json"))
	var items []protocol.Pending
	if err := json.Unmarshal([]byte(listed), &items); err != nil || len(items) != 2 {
		t.Fatalf("list: %q %v", listed, err)
	}
	for _, item := range items {
		permission := "deny"
		if item.Request.ConversationID == "chat-one" {
			permission = "allow"
		}
		output := finishCLI(t, startCLI(t, "", "decide", "--socket", path, item.Request.RequestID, permission))
		var result protocol.Result
		if err := json.Unmarshal([]byte(output), &result); err != nil || result.Permission != permission {
			t.Fatalf("decide: %q %v", output, err)
		}
	}
	nativePermission(t, one, "allow")
	nativePermission(t, two, "deny")
	pendingCLI(t, path, 0)
	if err := daemon.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	finishCLI(t, daemon)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("daemon did not remove its socket")
	}
}

func TestHookExpiryCancellationAndShutdownProcesses(t *testing.T) {
	for _, mode := range []string{"expiry", "cancel", "shutdown", "crash"} {
		t.Run(mode, func(t *testing.T) {
			wait := "5s"
			if mode == "expiry" {
				wait = "150ms"
			}
			path, daemon := daemonCLI(t, wait)
			hook := startCLI(t, nativePayload(t, "chat"), "hook", "--socket", path, "--wait", "5s")
			pendingCLI(t, path, 1)
			switch mode {
			case "cancel":
				if err := hook.cmd.Process.Signal(syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
			case "shutdown":
				if err := daemon.cmd.Process.Signal(syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
				finishCLI(t, daemon)
			case "crash":
				if err := daemon.cmd.Process.Kill(); err != nil {
					t.Fatal(err)
				}
				if err := <-daemon.done; err == nil {
					t.Fatal("killed daemon exited successfully")
				}
			}
			nativePermission(t, hook, "deny")
			if mode == "expiry" || mode == "cancel" {
				pendingCLI(t, path, 0)
			}
		})
	}
}

func TestSubmitAndEscapedList(t *testing.T) {
	path, _ := daemonCLI(t, "5s")
	r := protocol.Request{ProtocolVersion: 1, RequestID: "manual-id", Agent: "fixture", ConversationID: "chat\x1b[2J", Event: "before_shell_execution", Command: "printf '\x1b[2J'\nsecond line"}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	submit := startCLI(t, string(data), "submit", "--socket", path, "--wait", "5s")
	pendingCLI(t, path, 1)
	select {
	case <-submit.done:
		t.Fatal("submit returned before decision")
	default:
	}
	output := finishCLI(t, startCLI(t, "", "list", "--socket", path))
	if strings.ContainsRune(output, '\x1b') || !strings.Contains(output, "manual-id") || !strings.Contains(output, "cwd=unknown") || !strings.Contains(output, "\\x1b") {
		t.Fatalf("unsafe or incomplete display: %q", output)
	}
	finishCLI(t, startCLI(t, "", "decide", "--socket", path, "manual-id", "deny"))
	var result protocol.Result
	if err := json.Unmarshal([]byte(finishCLI(t, submit)), &result); err != nil || result.State != "denied" {
		t.Fatalf("submit result: %+v %v", result, err)
	}
}

func TestHookRejectsProbeOptionsOutsideSpike(t *testing.T) {
	path, _ := daemonCLI(t, "5s")
	for _, options := range [][]string{{"--decision", "deny"}, {"--delay", "0s"}, {"--failure", "empty"}} {
		args := append([]string{"hook", "--socket", path}, options...)
		p := startCLI(t, nativePayload(t, "chat"), args...)
		nativePermission(t, p, "deny")
		if !strings.Contains(p.stderr.String(), "require --spike") {
			t.Fatal("probe option was not rejected")
		}
	}
	pendingCLI(t, path, 0)
}

func TestHookDeniesInvalidDaemonResponse(t *testing.T) {
	path := shortSocket(t)
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
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
		if err := protocol.ReadFrame(bufio.NewReader(conn), &m); err != nil {
			return
		}
		_ = protocol.WriteFrame(conn, protocol.Response{ProtocolVersion: 1, Type: "submit", Result: &protocol.Result{RequestID: m.Request.RequestID, State: "expired", Permission: "allow"}})
	}()
	p := startCLI(t, nativePayload(t, "chat"), "hook", "--socket", path)
	nativePermission(t, p, "deny")
	<-done
}
