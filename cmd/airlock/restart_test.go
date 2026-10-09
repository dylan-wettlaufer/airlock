package main

import (
	"context"
	"database/sql"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"airlock/internal/protocol"
	"airlock/internal/store"
	"airlock/internal/transport"
)

func failedCLI(t *testing.T, p *cliProcess, diagnostic string) {
	t.Helper()
	select {
	case err := <-p.done:
		if err == nil {
			t.Fatal("invalid startup succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("invalid startup did not stop")
	}
	if !strings.Contains(p.stderr.String(), diagnostic) {
		t.Fatalf("startup diagnostic: %s, want %s", p.stderr.String(), diagnostic)
	}
}

func seedPendingHistory(t *testing.T, path string) {
	t.Helper()
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	at := time.Now()
	p := protocol.Pending{Request: protocol.Request{ProtocolVersion: 1, RequestID: "leftover", Agent: "fixture", ConversationID: "crash-chat", Event: "before_shell_execution", Command: "printf fixture"}, ReceivedAt: at, Deadline: at.Add(time.Hour)}
	if err := s.RecordPending(p); err != nil {
		t.Fatal(err)
	}
}

func assertStillPending(t *testing.T, path string) {
	t.Helper()
	s, err := store.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	records, err := s.History(context.Background(), store.Query{Limit: 10})
	if err != nil || len(records) != 1 || records[0].State != "pending" || len(records[0].Events) != 1 {
		t.Fatalf("failed startup changed history: %+v %v", records, err)
	}
}

func TestDaemonSocketOwnershipCheckedBeforeRecovery(t *testing.T) {
	for _, mode := range []string{"held-lock", "live-legacy-listener", "active-daemon", "unsafe-socket"} {
		t.Run(mode, func(t *testing.T) {
			path := shortSocket(t)
			if mode == "active-daemon" {
				var daemon *cliProcess
				path, daemon = daemonCLI(t, "5s")
				defer func() { daemon.cmd.Process.Signal(syscall.SIGTERM); finishCLI(t, daemon) }()
			}
			dbPath := filepath.Join(t.TempDir(), "private", "history.sqlite3")
			seedPendingHistory(t, dbPath)
			diagnostic := "another daemon owns this socket"
			switch mode {
			case "held-lock":
				lock, err := transport.AcquireDaemon(path)
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Close()
			case "live-legacy-listener":
				listener, err := net.Listen("unix", path)
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
				if err := os.Chmod(path, 0600); err != nil {
					t.Fatal(err)
				}
				diagnostic = "active listener"
			case "unsafe-socket":
				if err := os.WriteFile(path, []byte("keep"), 0600); err != nil {
					t.Fatal(err)
				}
				diagnostic = "unsafe ownership or permissions"
			}
			p := startCLI(t, "", "daemon", "--socket", path, "--database", dbPath)
			failedCLI(t, p, diagnostic)
			assertStillPending(t, dbPath)
		})
	}
}

func TestDatabaseLockPreventsRecoveryAtAnotherSocket(t *testing.T) {
	path, daemon := daemonCLI(t, "5s")
	dbPath := filepath.Join(filepath.Dir(path), "history.sqlite3")
	hook := startCLI(t, nativePayload(t, "active-chat"), "hook", "--socket", path)
	id := pendingCLI(t, path, 1)[0].Request.RequestID
	other := shortSocket(t)
	failedCLI(t, startCLI(t, "", "daemon", "--socket", other, "--database", dbPath), "history database is already owned")
	if _, err := os.Lstat(other); !os.IsNotExist(err) {
		t.Fatal("second socket bound before database lock")
	}
	assertStillPending(t, dbPath)
	finishCLI(t, startCLI(t, "", "decide", "--socket", path, id, "allow"))
	nativePermission(t, hook, "allow")
	daemon.cmd.Process.Signal(syscall.SIGTERM)
	finishCLI(t, daemon)
}

func TestDatabaseFailureLeavesStaleSocketAndHistoryUntouched(t *testing.T) {
	for _, mode := range []string{"malformed", "newer-schema", "recovery-failure"} {
		t.Run(mode, func(t *testing.T) {
			path := shortSocket(t)
			dbPath := filepath.Join(filepath.Dir(path), "history.sqlite3")
			listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			listener.SetUnlinkOnClose(false)
			if err := os.Chmod(path, 0600); err != nil {
				t.Fatal(err)
			}
			listener.Close()
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			diagnostic := "could not read history database"
			if mode == "malformed" {
				if err := os.WriteFile(dbPath, []byte("invalid database"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				seedPendingHistory(t, dbPath)
				db, err := sql.Open("sqlite", dbPath)
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				statement := "PRAGMA user_version=999"
				if mode == "recovery-failure" {
					statement = `CREATE TRIGGER recovery_failure BEFORE UPDATE ON actions WHEN OLD.state='pending' BEGIN SELECT RAISE(ABORT,'synthetic recovery failure'); END`
					diagnostic = "synthetic recovery failure"
				} else {
					diagnostic = "schema is newer"
				}
				if _, err := db.Exec(statement); err != nil {
					t.Fatal(err)
				}
			}
			failedCLI(t, startCLI(t, "", "daemon", "--socket", path, "--database", dbPath), diagnostic)
			after, err := os.Lstat(path)
			if err != nil || !os.SameFile(before, after) {
				t.Fatal("database failure removed stale endpoint")
			}
			if err := (transport.Client{Socket: path}).Health(context.Background()); err == nil {
				t.Fatal("accepted clients after failed database initialization")
			}
			if mode == "recovery-failure" {
				assertStillPending(t, dbPath)
				db, err := sql.Open("sqlite", dbPath)
				if err != nil {
					t.Fatal(err)
				}
				_, err = db.Exec("DROP TRIGGER recovery_failure")
				db.Close()
				if err != nil {
					t.Fatal(err)
				}
				// Failed setup released both locks. Retrying recovers the same endpoint.
				daemon := startCLI(t, "", "daemon", "--socket", path, "--database", dbPath)
				waitForHealth(t, path)
				pendingCLI(t, path, 0)
				daemon.cmd.Process.Signal(syscall.SIGTERM)
				finishCLI(t, daemon)
			}
		})
	}
}
