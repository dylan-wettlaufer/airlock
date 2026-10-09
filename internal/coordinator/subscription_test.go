package coordinator

import (
	"sync"
	"testing"
	"time"

	"airlock/internal/protocol"
)

func nextEvent(t *testing.T, s *Subscription) protocol.QueueEvent {
	t.Helper()
	select {
	case <-s.Done:
		t.Fatalf("subscription invalidated: %v", s.Err())
	case event := <-s.Events:
		return event
	case <-time.After(time.Second):
		t.Fatal("missing event")
	}
	return protocol.QueueEvent{}
}
func noEvent(t *testing.T, s *Subscription) {
	t.Helper()
	select {
	case e := <-s.Events:
		t.Fatalf("unexpected event: %+v", e)
	default:
	}
}

func TestSubscriptionSnapshotAndImmutableOrderedEvents(t *testing.T) {
	c := New()
	defer c.Close()
	id := "source"
	r := proposal("one")
	r.SourceToolCallID = &id
	r.WorkspaceRoots = []string{"/sanitized"}
	one, err := c.Submit(r, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	s, err := c.Subscribe()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	peer, err := c.Subscribe()
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	if s.Snapshot.Cursor.Sequence != 1 || len(s.Snapshot.Items) != 1 {
		t.Fatal("incorrect snapshot")
	}
	s.Snapshot.Items[0].Request.WorkspaceRoots[0] = "/changed"
	*s.Snapshot.Items[0].Request.SourceToolCallID = "changed"
	if c.List()[0].Request.WorkspaceRoots[0] != "/sanitized" || *peer.Snapshot.Items[0].Request.SourceToolCallID != "source" {
		t.Fatal("mutable snapshot leaked")
	}
	two, err := c.Submit(rWithID(r, "two"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	e := nextEvent(t, s)
	if e.Pending == nil || e.Cursor.Sequence != 2 || e.Pending.Request.RequestID != "two" {
		t.Fatal("incorrect admission event")
	}
	e.Pending.Request.WorkspaceRoots[0] = "/mutated-event"
	if nextEvent(t, peer).Pending.Request.WorkspaceRoots[0] != "/sanitized" || c.List()[1].Request.WorkspaceRoots[0] != "/sanitized" {
		t.Fatal("mutable event leaked")
	}
	winner, err := c.Decide("one", "allow")
	if err != nil {
		t.Fatal(err)
	}
	e = nextEvent(t, s)
	if e.Result == nil || *e.Result != winner || e.Cursor.Sequence != 3 || e.Cursor.Epoch != s.Snapshot.Cursor.Epoch {
		t.Fatal("incorrect result event")
	}
	<-one
	_, err = c.Decide("one", "deny")
	requireCode(t, err, "not_pending")
	noEvent(t, s)
	c.Cancel("two", two)
	e = nextEvent(t, s)
	if e.Cursor.Sequence != 4 || e.Result.State != "cancelled" {
		t.Fatal("cancel event missing")
	}
	<-two
	snapshot, err := c.Snapshot()
	if err != nil || len(snapshot.Items) != 0 || snapshot.Cursor.Sequence != 4 {
		t.Fatalf("final snapshot: %+v %v", snapshot, err)
	}
}
func rWithID(r protocol.Request, id string) protocol.Request { r.RequestID = id; return r }

func TestSubscribeDecisionRaceHasNoGap(t *testing.T) {
	for range 50 {
		c := New()
		result, err := c.Submit(proposal("racing"), time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		var s *Subscription
		var subErr, decisionErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); s, subErr = c.Subscribe() }()
		go func() { defer wg.Done(); _, decisionErr = c.Decide("racing", "deny") }()
		wg.Wait()
		if subErr != nil || decisionErr != nil {
			t.Fatalf("race: %v %v", subErr, decisionErr)
		}
		if len(s.Snapshot.Items) == 1 {
			e := nextEvent(t, s)
			if e.Result == nil || e.Cursor.Sequence != s.Snapshot.Cursor.Sequence+1 {
				t.Fatal("lost transition at snapshot boundary")
			}
		} else if s.Snapshot.Cursor.Sequence != 2 {
			t.Fatal("snapshot missed transition")
		}
		noEvent(t, s)
		<-result
		s.Close()
		c.Close()
	}
}

func TestSlowSubscriberAndLimitDoNotBlockDecisions(t *testing.T) {
	c := New()
	defer c.Close()
	subscribers := []*Subscription{}
	for range MaxSubscribers {
		s, err := c.Subscribe()
		if err != nil {
			t.Fatal(err)
		}
		subscribers = append(subscribers, s)
	}
	_, err := c.Subscribe()
	requireCode(t, err, "subscriber_limit")
	for i := 0; i < SubscriptionBuffer+1; i++ {
		// Reuse is permitted by the in-memory fixture; production SQLite rejects it.
		result, err := c.Submit(proposal("overflow"), time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = c.Decide("overflow", "deny"); err != nil {
			t.Fatal(err)
		}
		<-result
	}
	for _, s := range subscribers {
		select {
		case <-s.Done:
		default:
			t.Fatal("slow subscriber remained registered")
		}
		requireCode(t, s.Err(), "resync_required")
		s.Close()
	}
	fresh, err := c.Subscribe()
	if err != nil || len(fresh.Snapshot.Items) != 0 {
		t.Fatalf("fresh subscription: %v", err)
	}
	fresh.Close()
}

func TestSnapshotExpiresStaleAndRejectsShutdown(t *testing.T) {
	c := New()
	now := time.Now()
	c.now = func() time.Time { return now }
	result, err := c.Submit(proposal("expired"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	s, err := c.Subscribe()
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Snapshot.Items) != 0 || s.Snapshot.Cursor.Sequence != 2 || (<-result).State != "expired" {
		t.Fatal("stale pending snapshot")
	}
	c.Close()
	requireCode(t, s.Err(), "unavailable")
	_, err = c.Snapshot()
	requireCode(t, err, "unavailable")
	_, err = c.Subscribe()
	requireCode(t, err, "unavailable")
	if New().epoch == c.epoch {
		t.Fatal("coordinator epochs repeated")
	}
}

func TestSubscriptionWaitsForCommitAndInvalidatesOnStorageFailure(t *testing.T) {
	db, path := durableStore(t)
	gate := &commitGate{Recorder: db, pendingEntered: make(chan struct{}), pendingRelease: make(chan struct{}), resultEntered: make(chan struct{}), resultRelease: make(chan struct{})}
	c := NewWithHistory(gate)
	s, err := c.Subscribe()
	if err != nil {
		t.Fatal(err)
	}
	var pendingOnce, resultOnce sync.Once
	defer func() {
		pendingOnce.Do(func() { close(gate.pendingRelease) })
		resultOnce.Do(func() { close(gate.resultRelease) })
		c.Close()
	}()
	admitted := make(chan error, 1)
	go func() { _, err := c.Submit(proposal("committed"), time.Hour); admitted <- err }()
	signal(t, gate.pendingEntered)
	noEvent(t, s)
	pendingOnce.Do(func() { close(gate.pendingRelease) })
	if err := <-admitted; err != nil {
		t.Fatal(err)
	}
	if nextEvent(t, s).Pending == nil {
		t.Fatal("missing committed admission")
	}
	ack := make(chan error, 1)
	go func() { _, err := c.Decide("committed", "allow"); ack <- err }()
	signal(t, gate.resultEntered)
	noEvent(t, s)
	resultOnce.Do(func() { close(gate.resultRelease) })
	if err := <-ack; err != nil {
		t.Fatal(err)
	}
	if nextEvent(t, s).Result.Permission != "allow" {
		t.Fatal("missing committed decision")
	}
	// Disable the one-use gates before the next operations.
	c.history = db
	_, err = c.Submit(proposal("failed"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	nextEvent(t, s)
	injectCommitFailure(t, path, "NEW.state!='pending'")
	_, err = c.Decide("failed", "allow")
	requireCode(t, err, "history_unavailable")
	requireCode(t, s.Err(), "history_unavailable")
	noEvent(t, s)
	_, err = c.Snapshot()
	requireCode(t, err, "history_unavailable")
	_, err = c.Subscribe()
	requireCode(t, err, "history_unavailable")
}
