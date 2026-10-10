package tui

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"airlock/internal/protocol"
)

func await[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not converge")
	}
	var zero T
	return zero
}
func TestFeedReconnectReplacesEpochAndCancelsDecisionContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var attempts atomic.Int32
	firstEnded := make(chan struct{})
	var decisionContext context.Context
	c := fakeClient{subscribe: func(ctx context.Context, snapshot func(protocol.Snapshot) error, _ func(protocol.QueueEvent) error) error {
		n := attempts.Add(1)
		epoch := strings.Repeat("a", 32)
		id := "old"
		if n > 1 {
			epoch = strings.Repeat("b", 32)
			id = "fresh"
		}
		if err := snapshot(protocol.Snapshot{Cursor: protocol.Cursor{Epoch: epoch, Sequence: 1}, Items: []protocol.Pending{fixture(id, time.Now())}}); err != nil {
			return err
		}
		<-ctx.Done()
		if n == 1 {
			close(firstEnded)
		}
		return ctx.Err()
	}, decide: func(ctx context.Context, id, p string) (protocol.Result, error) {
		decisionContext = ctx
		return protocol.Result{RequestID: id, State: "denied", Permission: "deny"}, nil
	}}
	f := newFeed(ctx, c)
	go f.run()
	defer func() { cancel(); await(t, f.done) }()
	m := newModel(ctx, f)
	m.width = 120
	m.height = 32
	for !m.live() {
		m.Update(await(t, f.messages))
	}
	if m.selected != "old" {
		t.Fatal("first snapshot not installed")
	}
	if _, err := f.decide(m.generation, m.cursor.Epoch, "old", "deny", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	f.refresh()
	await(t, firstEnded)
	if decisionContext.Err() == nil || m.canDecide() {
		t.Fatal("broken lease retained a live decision context")
	}
	for m.selected != "fresh" || !m.live() {
		m.Update(await(t, f.messages))
	}
	if len(m.items) != 1 || attempts.Load() != 2 || m.cursor.Epoch != strings.Repeat("b", 32) {
		t.Fatal("reconnect retained stale queue or epoch")
	}
}

func TestFeedOverflowInvalidatesBeforeUIConsumesBacklog(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	returned := make(chan error, 1)
	f := newFeed(ctx, fakeClient{subscribe: func(_ context.Context, s func(protocol.Snapshot) error, e func(protocol.QueueEvent) error) error {
		if err := s(protocol.Snapshot{Cursor: protocol.Cursor{Epoch: strings.Repeat("a", 32)}}); err != nil {
			return err
		}
		for i := 0; i < 30; i++ {
			p := fixture("overflow", time.Now())
			if err := e(protocol.QueueEvent{Cursor: protocol.Cursor{Epoch: strings.Repeat("a", 32), Sequence: uint64(i + 1)}, Pending: &p}); err != nil {
				returned <- err
				return err
			}
		}
		return errors.New("expected overflow")
	}})
	go f.run()
	defer func() { cancel(); await(t, f.done) }()
	if err := await(t, returned); !errors.Is(err, errSlowUI) {
		t.Fatalf("overflow: %v", err)
	}
	// Wait for run to invalidate before it attempts the blocking status handoff.
	deadline := time.Now().Add(time.Second)
	for f.valid(1, strings.Repeat("a", 32)) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if f.valid(1, strings.Repeat("a", 32)) || len(f.messages) > 16 {
		t.Fatal("overflow left a usable cached view")
	}
}

func TestDecisionLeaseRejectsChangedGenerationAndExpiredDeadline(t *testing.T) {
	calls := 0
	m := ready(t, fakeClient{decide: func(context.Context, string, string) (protocol.Result, error) { calls++; return protocol.Result{}, nil }}, fixture("one", time.Now()))
	if _, err := m.feed.decide(1, m.cursor.Epoch, "one", "allow", time.Now().Add(-time.Second)); err == nil {
		t.Fatal("expired decision sent")
	}
	m.feed.mu.Lock()
	m.feed.generation = 2
	m.feed.mu.Unlock()
	if _, err := m.feed.decide(1, m.cursor.Epoch, "one", "allow", time.Now().Add(time.Hour)); !errors.Is(err, errStaleView) {
		t.Fatal("old generation sent")
	}
	if calls != 0 {
		t.Fatal("invalid decisions reached network")
	}
}
