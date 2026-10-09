package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"airlock/internal/coordinator"
	"airlock/internal/protocol"
	"airlock/internal/store"
	"airlock/internal/transport"
)

func TestDaemonRetentionFlagsAndPrunedIDAfterRestart(t *testing.T) {
	path := shortSocket(t)
	dbPath := filepath.Join(filepath.Dir(path), "history.sqlite3")
	daemon := startCLI(t, "", "daemon", "--socket", path, "--database", dbPath, "--history-max-records", "1", "--history-max-age", "24h")
	waitForHealth(t, path)
	for _, id := range []string{"first", "second"} {
		r := protocol.Request{ProtocolVersion: 1, RequestID: id, Agent: "fixture", ConversationID: "chat", Event: "before_shell_execution", Command: "printf fixture"}
		data, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		waiter := startCLI(t, string(data), "submit", "--socket", path)
		pendingCLI(t, path, 1)
		finishCLI(t, startCLI(t, "", "decide", "--socket", path, id, "allow"))
		finishCLI(t, waiter)
	}
	output := finishCLI(t, startCLI(t, "", "history", "--database", dbPath, "--json"))
	var records []store.Record
	if err := json.Unmarshal([]byte(output), &records); err != nil || len(records) != 1 || records[0].RequestID != "second" {
		t.Fatalf("daemon did not apply count retention: %s %v", output, err)
	}
	if err := daemon.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	finishCLI(t, daemon)
	next := startCLI(t, "", "daemon", "--socket", path, "--database", dbPath, "--history-max-records", "1")
	waitForHealth(t, path)
	r := protocol.Request{ProtocolVersion: 1, RequestID: "first", Agent: "fixture", ConversationID: "chat", Event: "before_shell_execution", Command: "printf changed"}
	_, err := (transport.Client{Socket: path}).Submit(context.Background(), r, time.Second)
	var wire *protocol.WireError
	if !errors.As(err, &wire) || wire.Code != "duplicate_request" {
		t.Fatalf("pruned ID reused after restart: %v", err)
	}
	if err := next.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	finishCLI(t, next)
}

func TestInvalidRetentionOptionsDoNotCreateDaemonFiles(t *testing.T) {
	for _, options := range [][]string{{"--history-max-age", "0s"}, {"--history-max-age", "-1h"}, {"--history-max-records", "0"}, {"--history-max-records", "1000001"}} {
		path := shortSocket(t)
		dbPath := filepath.Join(filepath.Dir(path), "history.sqlite3")
		args := append([]string{"daemon", "--socket", path, "--database", dbPath}, options...)
		var out, diagnostics bytes.Buffer
		if code := run(context.Background(), args, nil, &out, &diagnostics); code == 0 {
			t.Fatalf("invalid options accepted: %v", options)
		}
		for _, file := range []string{path, path + ".lock", dbPath} {
			if _, err := os.Lstat(file); !os.IsNotExist(err) {
				t.Fatalf("invalid policy created %s", file)
			}
		}
	}
}

func TestIdleRetentionPrunesOrDeniesOnFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "prune"
		if fail {
			name = "failure"
		}
		t.Run(name, func(t *testing.T) {
			path := shortSocket(t)
			dbPath := filepath.Join(filepath.Dir(path), "history.sqlite3")
			history, err := store.Open(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { history.Close() })
			queue := coordinator.NewWithHistory(history)
			t.Cleanup(queue.Close)
			now := time.Now()
			proposal := protocol.Request{ProtocolVersion: 1, RequestID: "waiting", Agent: "fixture", ConversationID: "chat", Event: "before_shell_execution", Command: "printf fixture"}
			waiter, err := queue.Submit(proposal, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			// Seed old history after the last admission, to exercise idle maintenance.
			old := proposal
			old.RequestID = "old"
			at := now.Add(-40 * 24 * time.Hour)
			if err := history.RecordPending(protocol.Pending{Request: old, ReceivedAt: at, Deadline: at.Add(time.Hour)}); err != nil {
				t.Fatal(err)
			}
			if err := history.RecordResult(protocol.Result{RequestID: "old", State: "allowed", Permission: "allow", Reason: "fixture decision"}, at); err != nil {
				t.Fatal(err)
			}
			if fail {
				db, err := sql.Open("sqlite", dbPath)
				if err != nil {
					t.Fatal(err)
				}
				_, err = db.Exec(`CREATE TRIGGER fail_retention BEFORE DELETE ON actions BEGIN SELECT RAISE(ABORT,'synthetic prune failure'); END`)
				db.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			server, err := transport.ListenWithCoordinator(path, time.Hour, queue)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { defer close(done); done <- serveWithRetention(ctx, server, queue, 10*time.Millisecond) }()
			t.Cleanup(func() { cancel(); <-done })
			if fail {
				select {
				case result := <-waiter:
					if result.Permission != "deny" || result.State != "interrupted" {
						t.Fatalf("maintenance failure: %+v", result)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("maintenance stranded waiter")
				}
				select {
				case err := <-done:
					var wire *protocol.WireError
					if !errors.As(err, &wire) || wire.Code != "history_unavailable" {
						t.Fatalf("maintenance error: %v", err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("maintenance failure did not stop daemon")
				}
				records, err := history.History(context.Background(), store.Query{Limit: 10})
				if err != nil || len(records) != 2 {
					t.Fatalf("failed prune changed history: %+v %v", records, err)
				}
			} else {
				deadline := time.Now().Add(3 * time.Second)
				removed := false
				for time.Now().Before(deadline) {
					records, err := history.History(context.Background(), store.Query{Limit: 10})
					if err != nil {
						t.Fatal(err)
					}
					if len(records) == 1 && records[0].RequestID == "waiting" {
						removed = true
						break
					}
					time.Sleep(5 * time.Millisecond)
				}
				if !removed {
					t.Fatal("idle maintenance did not prune age-expired history")
				}
				cancel()
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("maintenance worker did not stop")
				}
			}
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				t.Fatal("maintenance shutdown left socket")
			}
		})
	}
}
