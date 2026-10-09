package coordinator

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"airlock/internal/protocol"
	"airlock/internal/store"
)

func durableStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "private", "history.sqlite3")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func durableRecords(t *testing.T, s *store.Store) []store.Record {
	t.Helper()
	records, err := s.History(context.Background(), store.Query{Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	return records
}

type commitGate struct {
	Recorder
	pendingEntered, pendingRelease chan struct{}
	resultEntered, resultRelease   chan struct{}
}

func (g *commitGate) RecordPending(p protocol.Pending) error {
	if g.pendingEntered != nil {
		close(g.pendingEntered)
		<-g.pendingRelease
	}
	return g.Recorder.RecordPending(p)
}
func (g *commitGate) RecordResult(r protocol.Result, at time.Time) error {
	if g.resultEntered != nil {
		close(g.resultEntered)
		<-g.resultRelease
	}
	return g.Recorder.RecordResult(r, at)
}

func signal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("operation did not reach commit gate")
	}
}

func TestAdmissionAndDecisionWaitForDurableCommit(t *testing.T) {
	s, _ := durableStore(t)
	g := &commitGate{Recorder: s, pendingEntered: make(chan struct{}), pendingRelease: make(chan struct{}), resultEntered: make(chan struct{}), resultRelease: make(chan struct{})}
	var releasePending, releaseResult sync.Once
	t.Cleanup(func() {
		releasePending.Do(func() { close(g.pendingRelease) })
		releaseResult.Do(func() { close(g.resultRelease) })
	})
	c := NewWithHistory(g)
	// Defer closing until both gates are released, including on test failure.
	defer func() {
		releasePending.Do(func() { close(g.pendingRelease) })
		releaseResult.Do(func() { close(g.resultRelease) })
		c.Close()
	}()
	type submitted struct {
		result <-chan protocol.Result
		err    error
	}
	accepted := make(chan submitted, 1)
	go func() { result, err := c.Submit(proposal("gated"), time.Hour); accepted <- submitted{result, err} }()
	signal(t, g.pendingEntered)
	if len(durableRecords(t, s)) != 0 {
		t.Fatal("request recorded before gated admission")
	}
	select {
	case <-accepted:
		t.Fatal("uncommitted admission returned")
	default:
	}
	reviewed := make(chan []protocol.Pending, 1)
	go func() { reviewed <- c.List() }()
	select {
	case <-reviewed:
		t.Fatal("review passed an uncommitted admission")
	case <-time.After(20 * time.Millisecond):
	}
	releasePending.Do(func() { close(g.pendingRelease) })
	var admission submitted
	select {
	case admission = <-accepted:
	case <-time.After(time.Second):
		t.Fatal("committed admission did not return")
	}
	if admission.err != nil {
		t.Fatal(admission.err)
	}
	select {
	case items := <-reviewed:
		if len(items) != 1 {
			t.Fatalf("missing committed review: %+v", items)
		}
	case <-time.After(time.Second):
		t.Fatal("review blocked after commit")
	}
	records := durableRecords(t, s)
	if len(records) != 1 || records[0].State != "pending" || len(records[0].Events) != 1 {
		t.Fatalf("admission not durable: %+v", records)
	}
	type decided struct {
		result protocol.Result
		err    error
	}
	ack := make(chan decided, 1)
	go func() { r, err := c.Decide("gated", "allow"); ack <- decided{r, err} }()
	signal(t, g.resultEntered)
	select {
	case <-ack:
		t.Fatal("acknowledged uncommitted decision")
	default:
	}
	select {
	case <-admission.result:
		t.Fatal("hook allowed before commit")
	default:
	}
	records = durableRecords(t, s)
	if records[0].State != "pending" || records[0].Decision != nil || len(records[0].Events) != 1 {
		t.Fatalf("uncommitted terminal record: %+v", records[0])
	}
	releaseResult.Do(func() { close(g.resultRelease) })
	select {
	case a := <-ack:
		if a.err != nil || a.result.Permission != "allow" {
			t.Fatalf("decision: %+v", a)
		}
	case <-time.After(time.Second):
		t.Fatal("decision did not return")
	}
	select {
	case r := <-admission.result:
		if r.Permission != "allow" {
			t.Fatalf("hook: %+v", r)
		}
	case <-time.After(time.Second):
		t.Fatal("hook did not return")
	}
	records = durableRecords(t, s)
	if records[0].State != "allowed" || records[0].Decision == nil || len(records[0].Events) != 2 || records[0].Events[1].State != "allowed" {
		t.Fatalf("ack without atomic audit: %+v", records[0])
	}
}

// A deferred foreign-key violation fails COMMIT itself, after the action,
// decision, and event statements have all succeeded. This exercises real SQL
// rollback and the coordinator's failure handling together.
func injectCommitFailure(t *testing.T, path, condition string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	_, err = db.Exec(`CREATE TABLE commit_fault(reference TEXT REFERENCES actions(request_id) DEFERRABLE INITIALLY DEFERRED);
 CREATE TRIGGER fail_commit AFTER INSERT ON action_events WHEN ` + condition + ` BEGIN
 INSERT INTO commit_fault(reference) VALUES('nonexistent-request'); END;`)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func TestAdmissionCommitFailureDeniesExistingWaiters(t *testing.T) {
	s, path := durableStore(t)
	c := NewWithHistory(s)
	defer c.Close()
	result, err := c.Submit(proposal("existing"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	injectCommitFailure(t, path, "NEW.state='pending'")
	_, err = c.Submit(proposal("failed"), time.Hour)
	requireCode(t, err, "history_unavailable")
	select {
	case r := <-result:
		if r.Permission != "deny" || r.State != "interrupted" {
			t.Fatalf("existing waiter: %+v", r)
		}
	case <-time.After(time.Second):
		t.Fatal("existing waiter not denied on persistence failure")
	}
	if len(c.List()) != 0 {
		t.Fatal("review exposed requests after persistence failure")
	}
	_, err = c.Decide("existing", "allow")
	requireCode(t, err, "history_unavailable")
	records := durableRecords(t, s)
	if len(records) != 1 || records[0].RequestID != "existing" || records[0].State != "pending" || len(records[0].Events) != 1 {
		t.Fatalf("partial admission or further writes after failure: %+v", records)
	}
}

func TestTerminalCommitFailuresDenyAllWaiters(t *testing.T) {
	for _, mode := range []string{"allow", "deny", "deadline", "cancel", "shutdown", "list-expiry"} {
		t.Run(mode, func(t *testing.T) {
			s, path := durableStore(t)
			c := NewWithHistory(s)
			defer c.Close()
			now := time.Now()
			c.now = func() time.Time { return now }
			one, err := c.Submit(proposal("one"), time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			two, err := c.Submit(proposal("two"), 2*time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			injectCommitFailure(t, path, "NEW.state!='pending'")
			switch mode {
			case "allow", "deny":
				_, err = c.Decide("one", mode)
				requireCode(t, err, "history_unavailable")
			case "deadline":
				now = now.Add(time.Hour)
				_, err = c.Decide("one", "allow")
				requireCode(t, err, "history_unavailable")
			case "cancel":
				c.Cancel("one", one)
			case "shutdown":
				c.Close()
			case "list-expiry":
				now = now.Add(time.Hour)
				if len(c.List()) != 0 {
					t.Fatal("review returned a partial snapshot after failure")
				}
			}
			for _, ch := range []<-chan protocol.Result{one, two} {
				select {
				case r := <-ch:
					if r.Permission != "deny" || r.State != "interrupted" {
						t.Fatalf("persistence failure returned %+v", r)
					}
				case <-time.After(time.Second):
					t.Fatal("pending waiter stranded after persistence failure")
				}
				select {
				case r := <-ch:
					t.Fatalf("second result: %+v", r)
				default:
				}
			}
			if len(c.List()) != 0 {
				t.Fatal("requests still reviewable after failure")
			}
			_, err = c.Submit(proposal("new"), time.Hour)
			requireCode(t, err, "history_unavailable")
			_, err = c.Decide("two", "deny")
			requireCode(t, err, "history_unavailable")
			for _, r := range durableRecords(t, s) {
				if r.State != "pending" || r.Decision != nil || len(r.Events) != 1 || r.FinishedAt != nil {
					t.Fatalf("partial terminal transaction: %+v", r)
				}
			}
		})
	}
}

func TestDurableDuplicatesNeverChangeOriginal(t *testing.T) {
	s, path := durableStore(t)
	c := NewWithHistory(s)
	result, err := c.Submit(proposal("unique"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	changed := proposal("unique")
	changed.Command = "printf changed"
	changed.ConversationID = "other-chat"
	for _, r := range []protocol.Request{proposal("unique"), changed} {
		_, err = c.Submit(r, time.Hour)
		requireCode(t, err, "duplicate_request")
	}
	items := c.List()
	if len(items) != 1 || items[0].Request.Command != proposal("unique").Command {
		t.Fatal("duplicate mutated original review")
	}
	if _, err = c.Decide("unique", "allow"); err != nil {
		t.Fatal(err)
	}
	<-result
	for _, permission := range []string{"allow", "deny"} {
		_, err = c.Decide("unique", permission)
		requireCode(t, err, "not_pending")
	}
	for _, r := range []protocol.Request{proposal("unique"), changed} {
		_, err = c.Submit(r, time.Hour)
		requireCode(t, err, "duplicate_request")
	}
	c.Close()
	s.Close()
	reopened, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	c = NewWithHistory(reopened)
	defer c.Close()
	for _, permission := range []string{"allow", "deny"} {
		_, err = c.Decide("unique", permission)
		requireCode(t, err, "not_pending")
	}
	_, err = c.Submit(changed, time.Hour)
	requireCode(t, err, "duplicate_request")
	records := durableRecords(t, reopened)
	if len(records) != 1 || records[0].State != "allowed" || len(records[0].Events) != 2 || records[0].ConversationID != "conversation" {
		t.Fatalf("duplicates changed durable original: %+v", records)
	}
}

func TestDurableCompetingTransitionsHaveOneWinner(t *testing.T) {
	s, _ := durableStore(t)
	for i := range 20 {
		c := NewWithHistory(s)
		id := fmt.Sprintf("race-%d", i)
		result, err := c.Submit(proposal(id), time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		for _, mode := range []string{"allow", "deny", "cancel", "shutdown"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				switch mode {
				case "cancel":
					c.Cancel(id, result)
				case "shutdown":
					c.Close()
				default:
					_, _ = c.Decide(id, mode)
				}
			}()
		}
		wg.Wait()
		r := <-result
		records, err := s.History(context.Background(), store.Query{Limit: 1, RequestID: id})
		if err != nil {
			t.Fatal(err)
		}
		if len(records) != 1 || records[0].State != r.State || len(records[0].Events) != 2 || records[0].Events[1].State != r.State {
			t.Fatalf("winner differs from durable result: %+v %+v", r, records)
		}
		select {
		case extra := <-result:
			t.Fatalf("second winner: %+v", extra)
		default:
		}
		c.Close()
	}
}
