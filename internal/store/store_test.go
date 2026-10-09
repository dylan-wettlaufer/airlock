package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"airlock/internal/protocol"
)

func databasePath(t *testing.T) string {
	t.Helper()
	// Go's test directory itself can be mode 0755; let Open create its private child.
	return filepath.Join(t.TempDir(), "private", "history.sqlite3")
}

func pending(id, agent, conversation string, at time.Time) protocol.Pending {
	return protocol.Pending{Request: protocol.Request{ProtocolVersion: 1, RequestID: id, Agent: agent, ConversationID: conversation, AgentVersion: "fixture", Event: "before_shell_execution", Command: "curl --header 'Authorization: Bearer synthetic-secret' https://example.test", CWD: "/project", WorkspaceRoots: []string{"/project"}}, ReceivedAt: at, Deadline: at.Add(time.Hour)}
}

func history(t *testing.T, s *Store, q Query) []Record {
	t.Helper()
	records, err := s.History(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	return records
}

func TestMigrationDurabilityRedactionAndSessions(t *testing.T) {
	path := databasePath(t)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	for _, p := range []protocol.Pending{pending("one", "cursor", "chat", at), pending("two", "cursor", "chat", at), pending("three", "other", "chat", at)} {
		if err := s.RecordPending(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RecordResult(protocol.Result{RequestID: "one", State: "allowed", Permission: "allow", Reason: "manual decision"}, at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordResult(protocol.Result{RequestID: "one", State: "denied", Permission: "deny", Reason: "manual decision"}, at.Add(time.Second)); err == nil {
		t.Fatal("second terminal transition accepted")
	}
	var count, version int
	if err := s.db.QueryRow("SELECT count(*) FROM sessions").Scan(&count); err != nil || count != 2 {
		t.Fatalf("sessions: %d %v", count, err)
	}
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("schema: %d %v", version, err)
	}
	r := history(t, s, Query{Limit: 50, RequestID: "one"})[0]
	if r.CommandDisplay != "curl [arguments redacted]" || r.Decision == nil || r.Decision.Source != "manual" || len(r.Events) != 2 || r.FinishedAt == nil || r.ExecutionState != "unobserved" {
		t.Fatalf("incomplete record: %+v", r)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "synthetic-secret") {
		t.Fatal("raw command persisted")
	}
	reader, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if history(t, reader, Query{Limit: 50, RequestID: "two"})[0].State != "pending" {
		t.Fatal("read-only history recovered pending records")
	}
	reader.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r = history(t, s, Query{Limit: 50, RequestID: "two"})[0]
	if r.State != "interrupted" || len(r.Events) != 2 || r.Decision != nil || !strings.Contains(r.Reason, "restarted") {
		t.Fatalf("bad recovery: %+v", r)
	}
	if history(t, s, Query{Limit: 50, RequestID: "one"})[0].State != "allowed" {
		t.Fatal("recovery changed terminal state")
	}
	var wire *protocol.WireError
	if err := s.RecordPending(pending("one", "cursor", "chat", at)); !errors.As(err, &wire) || wire.Code != "duplicate_request" {
		t.Fatalf("durable uniqueness: %v", err)
	}
}

func TestHistoryBoundsOrderingAndFilters(t *testing.T) {
	s, err := Open(databasePath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	at := time.Now()
	for _, id := range []string{"a", "b", "c"} {
		if err := s.RecordPending(pending(id, "cursor", "chat", at)); err != nil {
			t.Fatal(err)
		}
	}
	records := history(t, s, Query{Limit: 1, Offset: 1, Agent: "cursor", ConversationID: "chat", State: "pending"})
	if len(records) != 1 || records[0].RequestID != "b" {
		t.Fatalf("bad page: %+v", records)
	}
	if len(history(t, s, Query{Limit: 50, Agent: "' OR 1=1 --"})) != 0 {
		t.Fatal("filter was not parameterized")
	}
	for _, q := range []Query{{Limit: 0}, {Limit: 201}, {Limit: 1, Offset: -1}, {Limit: 1, Offset: 10001}, {Limit: 1, State: "executed"}} {
		if _, err := s.History(context.Background(), q); err == nil {
			t.Fatalf("accepted unbounded query %+v", q)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.History(ctx, Query{Limit: 1}); err == nil {
		t.Fatal("ignored cancelled history query")
	}
}

func TestPrivateFilesAndWriterLock(t *testing.T) {
	path := databasePath(t)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, entry := range []struct {
		path string
		mode os.FileMode
	}{{filepath.Dir(path), 0700}, {path, 0600}, {path + ".lock", 0600}} {
		info, err := os.Lstat(entry.path)
		if err != nil || info.Mode().Perm() != entry.mode {
			t.Fatalf("permissions %s: %v", entry.path, err)
		}
	}
	if second, err := Open(path); err == nil {
		second.Close()
		t.Fatal("second writer acquired lock")
	}
	reader, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	reader.Close()
	// Rollback journals must inherit private database permissions while present.
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("INSERT INTO sessions(agent,conversation_id,first_seen,last_seen) VALUES('fixture','journal','now','now')"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path + "-journal")
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("journal permissions: %v", err)
	}
	tx.Rollback()
}

func TestUnsafePathsAndReadDoesNotCreate(t *testing.T) {
	path := databasePath(t)
	if _, err := Read(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing history: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatal("history reader created directory")
	}
	for _, mode := range []string{"directory-mode", "database-mode", "database-link", "directory-link", "sidecar-link", "non-file"} {
		t.Run(mode, func(t *testing.T) {
			path := databasePath(t)
			dir := filepath.Dir(path)
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "directory-mode":
				os.Chmod(dir, 0755)
			case "database-mode":
				os.WriteFile(path, nil, 0644)
			case "database-link":
				os.Symlink(filepath.Join(t.TempDir(), "target"), path)
			case "directory-link":
				os.Remove(dir)
				os.Symlink(t.TempDir(), dir)
			case "sidecar-link":
				os.Symlink(filepath.Join(t.TempDir(), "target"), path+"-journal")
			case "non-file":
				os.Mkdir(path, 0700)
			}
			if s, err := Open(path); err == nil {
				s.Close()
				t.Fatal("unsafe path accepted")
			}
		})
	}
	if s, err := Open("relative.sqlite3"); err == nil {
		s.Close()
		t.Fatal("relative path accepted")
	}
}

func TestNewerSchemaAndMigrationRollback(t *testing.T) {
	path := databasePath(t)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("PRAGMA user_version=999"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if s, err := Open(path); err == nil {
		s.Close()
		t.Fatal("newer schema accepted")
	}
	if s, err := Read(path); err == nil {
		s.Close()
		t.Fatal("newer schema reader accepted")
	}
	// A conflicting legacy table must leave user_version and migration tables untouched.
	path = databasePath(t)
	os.Mkdir(filepath.Dir(path), 0700)
	os.WriteFile(path, nil, 0600)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE actions(existing TEXT)"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if s, err := Open(path); err == nil {
		s.Close()
		t.Fatal("invalid schema migrated")
	}
	db, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version, count int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 0 {
		t.Fatalf("failed migration version: %d %v", version, err)
	}
	if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='sessions'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial migration: %d %v", count, err)
	}
}

func TestDisplayCommand(t *testing.T) {
	for input, want := range map[string]string{
		"curl -H 'Authorization: synthetic-secret'":        "curl [arguments redacted]",
		"TOKEN=synthetic-secret curl https://example.test": "[command redacted]",
		"/private/path/git push":                           "git [arguments redacted]",
		"echo;synthetic-secret":                            "[command redacted]",
		"printf\nsynthetic-secret":                         "printf [arguments redacted]",
		"$(cat secret)":                                    "[command redacted]",
		"git":                                              "git",
	} {
		if got := DisplayCommand(input); got != want {
			t.Errorf("%q: %q want %q", input, got, want)
		}
	}
}

func TestTerminalTransactionRollsBackOnEventFailure(t *testing.T) {
	s, err := Open(databasePath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	at := time.Now()
	if err := s.RecordPending(pending("one", "cursor", "chat", at)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER reject_terminal BEFORE INSERT ON action_events WHEN NEW.state != 'pending' BEGIN SELECT RAISE(ABORT,'synthetic event failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordResult(protocol.Result{RequestID: "one", State: "allowed", Permission: "allow", Reason: "manual decision"}, at); err == nil {
		t.Fatal("failed event committed")
	}
	r := history(t, s, Query{Limit: 1})[0]
	if r.State != "pending" || r.Decision != nil || len(r.Events) != 1 || r.FinishedAt != nil {
		t.Fatalf("partial transaction: %+v", r)
	}
}

func TestDefaultPersistentPath(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/persistent-state")
	path, err := DefaultPath()
	if err != nil || path != "/persistent-state/airlock/history.sqlite3" {
		t.Fatalf("state path %q: %v", path, err)
	}
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "/synthetic-home")
	path, err = DefaultPath()
	if err != nil || path != "/synthetic-home/.local/state/airlock/history.sqlite3" {
		t.Fatalf("home path %q: %v", path, err)
	}
	t.Setenv("XDG_STATE_HOME", "relative")
	if _, err := DefaultPath(); err == nil {
		t.Fatal("relative state directory accepted")
	}
}

func TestCompetingDurableResults(t *testing.T) {
	s, err := Open(databasePath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.RecordPending(pending("race", "cursor", "chat", time.Now())); err != nil {
		t.Fatal(err)
	}
	outcomes := make(chan error, 2)
	for _, result := range []protocol.Result{{RequestID: "race", State: "allowed", Permission: "allow", Reason: "manual decision"}, {RequestID: "race", State: "denied", Permission: "deny", Reason: "manual decision"}} {
		go func() { outcomes <- s.RecordResult(result, time.Now()) }()
	}
	successes := 0
	for range 2 {
		if <-outcomes == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("%d transitions won", successes)
	}
	r := history(t, s, Query{Limit: 1})[0]
	if len(r.Events) != 2 || r.Decision == nil || r.Events[1].State != r.State || (r.State == "allowed") != (r.Decision.Permission == "allow") {
		t.Fatalf("inconsistent winner: %+v", r)
	}
}
