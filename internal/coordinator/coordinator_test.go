package coordinator

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"airlock/internal/protocol"
)

func proposal(id string) protocol.Request {
	return protocol.Request{ProtocolVersion: protocol.Version, RequestID: id, Agent: "cursor", ConversationID: "conversation", Event: "before_shell_execution", Command: "printf test", WorkspaceRoots: []string{"/tmp"}}
}

func requireCode(t *testing.T, err error, code string) {
	t.Helper()
	var wire *protocol.WireError
	if !errors.As(err, &wire) || wire.Code != code {
		t.Fatalf("got %v, want %s", err, code)
	}
}

func TestIndependentWaitersAndImmutableProposals(t *testing.T) {
	c := New()
	defer c.Close()
	r := proposal("one")
	id := "native-id"
	r.SourceToolCallID = &id
	one, err := c.Submit(r, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	two, err := c.Submit(proposal("two"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	r.WorkspaceRoots[0], id = "/changed", "changed"
	items := c.List()
	if items[0].Request.WorkspaceRoots[0] != "/tmp" || *items[0].Request.SourceToolCallID != "native-id" {
		t.Fatal("proposal mutated through submit arguments")
	}
	items[0].Request.WorkspaceRoots[0] = "/changed"
	*items[0].Request.SourceToolCallID = "changed"
	if c.List()[0].Request.WorkspaceRoots[0] != "/tmp" || *c.List()[0].Request.SourceToolCallID != "native-id" {
		t.Fatal("proposal mutated through snapshot")
	}
	select {
	case <-one:
		t.Fatal("returned before decision")
	default:
	}
	select {
	case <-two:
		t.Fatal("returned before decision")
	default:
	}
	if _, err := c.Decide("two", "deny"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Decide("one", "allow"); err != nil {
		t.Fatal(err)
	}
	if r := <-one; r.RequestID != "one" || r.Permission != "allow" {
		t.Fatalf("misrouted: %+v", r)
	}
	if r := <-two; r.RequestID != "two" || r.Permission != "deny" {
		t.Fatalf("misrouted: %+v", r)
	}
	if len(c.List()) != 0 {
		t.Fatal("terminal requests remained pending")
	}
	_, err = c.Decide("one", "allow")
	requireCode(t, err, "not_pending")
}

func TestDeadlineCheckedBeforeTimerRuns(t *testing.T) {
	c := New()
	defer c.Close()
	now := time.Now()
	c.now = func() time.Time { return now }
	result, err := c.Submit(proposal("one"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	_, err = c.Decide("one", "allow")
	requireCode(t, err, "not_pending")
	if r := <-result; r.State != "expired" || r.Permission != "deny" {
		t.Fatalf("expired request allowed: %+v", r)
	}
}

func TestDuplicateCancellationAndShutdown(t *testing.T) {
	c := New()
	one, err := c.Submit(proposal("one"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	r := proposal("one")
	r.Command = "different command"
	_, err = c.Submit(r, time.Hour)
	requireCode(t, err, "duplicate_request")
	c.Cancel("one", one)
	if r := <-one; r.State != "cancelled" || r.Permission != "deny" {
		t.Fatalf("bad cancellation: %+v", r)
	}
	two, err := c.Submit(proposal("one"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	c.Cancel("one", one)
	if len(c.List()) != 1 {
		t.Fatal("old disconnect cancelled a new request")
	}
	c.Close()
	if r := <-two; r.State != "interrupted" || r.Permission != "deny" {
		t.Fatalf("bad shutdown: %+v", r)
	}
	_, err = c.Submit(proposal("three"), time.Hour)
	requireCode(t, err, "unavailable")
}

func TestCapacityAndExpiry(t *testing.T) {
	c := New()
	defer c.Close()
	for i := 0; i < MaxPending; i++ {
		if _, err := c.Submit(proposal(fmt.Sprintf("id-%d", i)), time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	_, err := c.Submit(proposal("overflow"), time.Hour)
	requireCode(t, err, "queue_full")
	if _, err := c.Decide("id-0", "deny"); err != nil {
		t.Fatal(err)
	}
	result, err := c.Submit(proposal("expiring"), 5*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-result:
		if r.State != "expired" || r.Permission != "deny" {
			t.Fatalf("bad expiry: %+v", r)
		}
	case <-time.After(time.Second):
		t.Fatal("expiry did not release waiter")
	}
}

func TestCompetingDecisionsAndCancellation(t *testing.T) {
	for i := 0; i < 100; i++ {
		c := New()
		result, err := c.Submit(proposal("race"), time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		for _, decision := range []string{"allow", "deny", "cancel"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if decision == "cancel" {
					c.Cancel("race", result)
				} else {
					_, _ = c.Decide("race", decision)
				}
			}()
		}
		wg.Wait()
		r := <-result
		if err := r.Validate("race"); err != nil {
			t.Fatal(err)
		}
		select {
		case extra := <-result:
			t.Fatalf("second terminal result: %+v", extra)
		default:
		}
		if len(c.List()) != 0 {
			t.Fatal("winning transition did not remove request")
		}
		c.Close()
	}
}

func TestInvalidProposalAndDecision(t *testing.T) {
	c := New()
	defer c.Close()
	r := proposal("one")
	r.CWD = "relative"
	_, err := c.Submit(r, time.Hour)
	requireCode(t, err, "invalid_request")
	_, err = c.Decide("one", "yes")
	requireCode(t, err, "invalid_decision")
}

type failingRecorder struct{ pendingErr, resultErr error }

func (f *failingRecorder) RecordPending(protocol.Pending) error          { return f.pendingErr }
func (f *failingRecorder) RecordResult(protocol.Result, time.Time) error { return f.resultErr }
func (f *failingRecorder) Prune(time.Time) error                         { return nil }

func TestHistoryFailureNeverAllows(t *testing.T) {
	recorder := &failingRecorder{pendingErr: errors.New("synthetic storage failure")}
	c := NewWithHistory(recorder)
	_, err := c.Submit(proposal("one"), time.Hour)
	requireCode(t, err, "history_unavailable")
	if len(c.List()) != 0 {
		t.Fatal("unpersisted request visible")
	}
	c.Close()
	recorder = &failingRecorder{}
	c = NewWithHistory(recorder)
	defer c.Close()
	result, err := c.Submit(proposal("one"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	recorder.resultErr = errors.New("synthetic commit failure")
	_, err = c.Decide("one", "allow")
	requireCode(t, err, "history_unavailable")
	if r := <-result; r.Permission != "deny" || r.State != "interrupted" {
		t.Fatalf("unpersisted approval: %+v", r)
	}
	_, err = c.Submit(proposal("two"), time.Hour)
	requireCode(t, err, "history_unavailable")
}
