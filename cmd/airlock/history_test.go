package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"airlock/internal/protocol"
	"airlock/internal/store"
	"airlock/internal/transport"
)

func TestHistoryAfterDaemonStops(t *testing.T) {
	path, daemon := daemonCLI(t, "5s")
	dbPath := filepath.Join(filepath.Dir(path), "history.sqlite3")
	r := protocol.Request{ProtocolVersion: 1, RequestID: "durable-id", Agent: "fixture", ConversationID: "chat", Event: "before_shell_execution", Command: "curl --header 'Authorization: synthetic-secret'"}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	submit := startCLI(t, string(data), "submit", "--socket", path)
	pendingCLI(t, path, 1)
	finishCLI(t, startCLI(t, "", "decide", "--socket", path, r.RequestID, "allow"))
	finishCLI(t, submit)
	if err := daemon.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	finishCLI(t, daemon)
	output := finishCLI(t, startCLI(t, "", "history", "--database", dbPath, "--json", "--limit", "1"))
	var records []store.Record
	if err := json.Unmarshal([]byte(output), &records); err != nil || len(records) != 1 || records[0].State != "allowed" || records[0].Decision == nil || len(records[0].Events) != 2 {
		t.Fatalf("durable history: %s %v", output, err)
	}
	if strings.Contains(output, "synthetic-secret") {
		t.Fatal("history exposed raw command")
	}
	// A daemon restart uses a fresh socket but the same persistent database.
	nextPath := shortSocket(t)
	next := startCLI(t, "", "daemon", "--socket", nextPath, "--database", dbPath, "--wait", "5s")
	waitForHealth(t, nextPath)
	clientResult, err := (transport.Client{Socket: nextPath}).Submit(context.Background(), r, time.Second)
	if err == nil {
		t.Fatalf("reused ID accepted: %+v", clientResult)
	}
	fresh := r
	fresh.RequestID = "fresh-id"
	freshData, _ := json.Marshal(fresh)
	freshSubmit := startCLI(t, string(freshData), "submit", "--socket", nextPath)
	pendingCLI(t, nextPath, 1)
	finishCLI(t, startCLI(t, "", "decide", "--socket", nextPath, fresh.RequestID, "deny"))
	finishCLI(t, freshSubmit)
	if err := next.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	finishCLI(t, next)
}

func waitForHealth(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if (transport.Client{Socket: path}).Health(context.Background()) == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("daemon did not start")
}

func TestHistoryCrashRecoveryProcess(t *testing.T) {
	path, daemon := daemonCLI(t, "5s")
	dbPath := filepath.Join(filepath.Dir(path), "history.sqlite3")
	hook := startCLI(t, nativePayload(t, "crash-chat"), "hook", "--socket", path)
	items := pendingCLI(t, path, 1)
	if err := daemon.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-daemon.done
	nativePermission(t, hook, "deny")
	before := finishCLI(t, startCLI(t, "", "history", "--database", dbPath, "--json"))
	var records []store.Record
	if err := json.Unmarshal([]byte(before), &records); err != nil || len(records) != 1 || records[0].State != "pending" {
		t.Fatalf("reader changed crash state: %s %v", before, err)
	}
	nextPath := shortSocket(t)
	next := startCLI(t, "", "daemon", "--socket", nextPath, "--database", dbPath)
	waitForHealth(t, nextPath)
	after := finishCLI(t, startCLI(t, "", "history", "--database", dbPath, "--json", "--request", items[0].Request.RequestID))
	if err := json.Unmarshal([]byte(after), &records); err != nil || len(records) != 1 || records[0].State != "interrupted" || len(records[0].Events) != 2 {
		t.Fatalf("recovery history: %s %v", after, err)
	}
	if err := next.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	finishCLI(t, next)
}

func TestHistoryOptionsAndSafeText(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "history.sqlite3")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	p := protocol.Pending{Request: protocol.Request{ProtocolVersion: 1, RequestID: "safe", Agent: "fixture", ConversationID: "chat\x1b[2J", Event: "before_shell_execution", Command: "printf 'synthetic-secret'"}, ReceivedAt: time.Now(), Deadline: time.Now().Add(time.Hour)}
	if err := s.RecordPending(p); err != nil {
		t.Fatal(err)
	}
	s.Close()
	var out, diagnostics bytes.Buffer
	if code := run(context.Background(), []string{"history", "--database", path}, nil, &out, &diagnostics); code != 0 {
		t.Fatalf("history: %s", diagnostics.String())
	}
	if strings.ContainsRune(out.String(), '\x1b') || strings.Contains(out.String(), "synthetic-secret") || !strings.Contains(out.String(), "\\x1b") {
		t.Fatalf("unsafe history: %q", out.String())
	}
	for _, args := range [][]string{{"--limit", "201"}, {"--limit", "0"}, {"--offset", "10001"}, {"--state", "executed"}, {"extra"}} {
		out.Reset()
		diagnostics.Reset()
		if code := run(context.Background(), append([]string{"history", "--database", path}, args...), nil, &out, &diagnostics); code == 0 {
			t.Fatalf("invalid options accepted: %v", args)
		}
	}
}
