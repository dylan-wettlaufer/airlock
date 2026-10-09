package store

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"airlock/internal/protocol"
)

func retentionRequest(id string, at time.Time) protocol.Pending {
	return pending(id, "fixture", "chat-"+id, at)
}

func complete(t *testing.T, s *Store, id, state string, at time.Time) {
	t.Helper()
	if err := s.RecordPending(retentionRequest(id, at.Add(-time.Minute))); err != nil {
		t.Fatal(err)
	}
	permission := "deny"
	if state == "allowed" {
		permission = "allow"
	}
	if err := s.RecordResult(protocol.Result{RequestID: id, State: state, Permission: permission, Reason: "fixture terminal"}, at); err != nil {
		t.Fatal(err)
	}
}

func countTable(t *testing.T, s *Store, table string) int {
	t.Helper()
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func duplicateID(t *testing.T, s *Store, id string, at time.Time) {
	t.Helper()
	changed := retentionRequest(id, at)
	changed.Request.Command = "printf other"
	changed.Request.ConversationID = "changed"
	var wire *protocol.WireError
	for _, p := range []protocol.Pending{retentionRequest(id, at), changed} {
		if err := s.RecordPending(p); !errors.As(err, &wire) || wire.Code != "duplicate_request" {
			t.Fatalf("pruned ID %s accepted: %v", id, err)
		}
	}
}

func TestCountRetentionPrunesAllDetailsButProtectsPendingAndIDs(t *testing.T) {
	path := databasePath(t)
	s, err := OpenWithRetention(path, Retention{MaxAge: DefaultMaxAge, MaxRecords: 2})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	if err := s.RecordPending(retentionRequest("waiting", at.Add(-60*24*time.Hour))); err != nil {
		t.Fatal(err)
	}
	// Equal completion times exercise the deterministic ID tie break.
	complete(t, s, "a", "allowed", at)
	complete(t, s, "c", "denied", at)
	complete(t, s, "b", "cancelled", at)
	records := history(t, s, Query{Limit: 10})
	if len(records) != 3 {
		t.Fatalf("count includes pending: %+v", records)
	}
	if len(history(t, s, Query{Limit: 1, RequestID: "a"})) != 0 {
		t.Fatal("old completed record retained")
	}
	if history(t, s, Query{Limit: 1, RequestID: "waiting"})[0].State != "pending" {
		t.Fatal("pending request pruned")
	}
	if countTable(t, s, "decisions") != 1 || countTable(t, s, "action_events") != 5 || countTable(t, s, "sessions") != 3 || countTable(t, s, "used_request_ids") != 4 {
		t.Fatal("orphan details retained or ID registry pruned")
	}
	duplicateID(t, s, "a", at)
	s.Close()
	// Restart recovery makes waiting terminal, and count retention still applies.
	s, err = OpenWithRetention(path, Retention{MaxAge: DefaultMaxAge, MaxRecords: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if countTable(t, s, "actions") != 2 {
		t.Fatal("startup did not bound recovered records")
	}
	duplicateID(t, s, "a", at)
	violations, err := s.db.Query("PRAGMA foreign_key_check")
	if err != nil {
		t.Fatal(err)
	}
	defer violations.Close()
	if violations.Next() {
		t.Fatal("pruning broke foreign-key integrity")
	}
	if err := violations.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestAgeRetentionUsesCompletionAndKeepsExactBoundary(t *testing.T) {
	s, err := OpenWithRetention(databasePath(t), Retention{MaxAge: 10 * time.Hour, MaxRecords: 100})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	at := time.Now()
	complete(t, s, "expired-age", "allowed", at.Add(-11*time.Hour))
	complete(t, s, "boundary", "denied", at.Add(-10*time.Hour))
	// A long-running request must get its full retention window after completion.
	if err := s.RecordPending(retentionRequest("long-wait", at.Add(-100*time.Hour))); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordResult(protocol.Result{RequestID: "long-wait", State: "interrupted", Permission: "deny", Reason: "fixture terminal"}, at); err != nil {
		t.Fatal(err)
	}
	if len(history(t, s, Query{Limit: 1, RequestID: "expired-age"})) != 0 {
		t.Fatal("expired completed history retained")
	}
	if len(history(t, s, Query{Limit: 1, RequestID: "boundary"})) != 1 {
		t.Fatal("exact age boundary pruned")
	}
	duplicateID(t, s, "expired-age", at)
	if err := s.Prune(at.Add(time.Nanosecond)); err != nil {
		t.Fatal(err)
	}
	if len(history(t, s, Query{Limit: 10})) != 1 {
		t.Fatal("explicit age pruning failed")
	}
	if countTable(t, s, "sessions") != 1 || countTable(t, s, "action_events") != 2 || countTable(t, s, "decisions") != 0 {
		t.Fatal("age pruning retained dependent metadata")
	}
}

func TestVersionOneMigrationReservesIDsBeforeStartupPruning(t *testing.T) {
	path := databasePath(t)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-40 * 24 * time.Hour)
	complete(t, s, "legacy", "allowed", at)
	// Recreate the old schema version without a registry, as an existing v1 DB.
	if _, err := s.db.Exec("DROP TABLE used_request_ids; DROP INDEX actions_finished; DROP INDEX actions_session; PRAGMA user_version=1"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if countTable(t, s, "actions") != 0 || countTable(t, s, "used_request_ids") != 1 {
		t.Fatal("migration did not reserve ID before pruning old history")
	}
	duplicateID(t, s, "legacy", time.Now())
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 2 {
		t.Fatalf("schema version %d: %v", version, err)
	}
	if _, err := s.db.Exec("DELETE FROM sessions"); err != nil {
		t.Fatal(err)
	} // No orphan sessions survived.
}

func TestPruningFailureRollsBackDetailsAndTerminalTransaction(t *testing.T) {
	s, err := OpenWithRetention(databasePath(t), Retention{MaxAge: DefaultMaxAge, MaxRecords: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	at := time.Now()
	complete(t, s, "old", "allowed", at)
	if err := s.RecordPending(retentionRequest("new", at.Add(time.Minute))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER prevent_prune BEFORE DELETE ON actions BEGIN SELECT RAISE(ABORT,'synthetic prune failure'); END`); err != nil {
		t.Fatal(err)
	}
	err = s.RecordResult(protocol.Result{RequestID: "new", State: "denied", Permission: "deny", Reason: "fixture terminal"}, at.Add(time.Minute))
	if err == nil {
		t.Fatal("terminal acknowledged failed pruning")
	}
	if history(t, s, Query{Limit: 1, RequestID: "new"})[0].State != "pending" || countTable(t, s, "decisions") != 1 || countTable(t, s, "action_events") != 3 || countTable(t, s, "used_request_ids") != 2 {
		t.Fatal("partial terminal or pruning transaction")
	}
	// Idle pruning must also roll back dependent deletes before failing on actions.
	if err := s.Prune(at.Add(31 * 24 * time.Hour)); err == nil {
		t.Fatal("idle prune succeeded despite storage error")
	}
	if countTable(t, s, "actions") != 2 || countTable(t, s, "decisions") != 1 || countTable(t, s, "action_events") != 3 {
		t.Fatal("idle prune partially deleted history")
	}
}

func TestAdmissionPruningFailureDoesNotReserveUnacceptedID(t *testing.T) {
	s, err := Open(databasePath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	at := time.Now()
	complete(t, s, "old", "allowed", at)
	if _, err := s.db.Exec(`CREATE TRIGGER prevent_prune BEFORE DELETE ON actions BEGIN SELECT RAISE(ABORT,'synthetic prune failure'); END`); err != nil {
		t.Fatal(err)
	}
	p := retentionRequest("unaccepted", at.Add(31*24*time.Hour))
	if err := s.RecordPending(p); err == nil {
		t.Fatal("admission committed failed pruning")
	}
	if countTable(t, s, "actions") != 1 || countTable(t, s, "used_request_ids") != 1 || countTable(t, s, "sessions") != 1 {
		t.Fatal("partial admission reserved an ID")
	}
	if _, err := s.db.Exec("DROP TRIGGER prevent_prune"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordPending(p); err != nil {
		t.Fatalf("unaccepted ID cannot be retried: %v", err)
	}
	duplicateID(t, s, "old", p.ReceivedAt)
}

func TestReadOnlyHistoryDoesNotPruneAndPolicyValidation(t *testing.T) {
	path := databasePath(t)
	s, err := OpenWithRetention(path, Retention{MaxAge: 365 * 24 * time.Hour, MaxRecords: 100})
	if err != nil {
		t.Fatal(err)
	}
	complete(t, s, "old", "allowed", time.Now().Add(-40*24*time.Hour))
	s.Close()
	reader, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if countTable(t, reader, "actions") != 1 {
		t.Fatal("reader applied default retention")
	}
	if err := reader.Prune(time.Now()); err == nil {
		t.Fatal("reader pruned history")
	}
	reader.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if countTable(t, s, "actions") != 0 {
		t.Fatal("startup did not apply tighter policy")
	}
	duplicateID(t, s, "old", time.Now())
	for _, r := range []Retention{{MaxAge: 0, MaxRecords: 1}, {MaxAge: -1, MaxRecords: 1}, {MaxAge: time.Hour, MaxRecords: 0}, {MaxAge: time.Hour, MaxRecords: MaxRetentionRecords + 1}} {
		newPath := databasePath(t)
		if db, err := OpenWithRetention(newPath, r); err == nil {
			db.Close()
			t.Fatalf("invalid retention accepted: %+v", r)
		}
		if _, err := os.Stat(newPath); !os.IsNotExist(err) {
			t.Fatal("invalid policy created database")
		}
	}
	// The permanent registry holds IDs only, never commands or session metadata.
	var schema string
	if err := s.db.QueryRow("SELECT sql FROM sqlite_master WHERE name='used_request_ids'").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(schema, "command") || strings.Contains(schema, "conversation") {
		t.Fatal("ID registry retained unnecessary metadata")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.History(ctx, Query{Limit: 1}); err == nil {
		t.Fatal("cancelled reader ignored context")
	}
}
