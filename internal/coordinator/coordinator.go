// Package coordinator serializes manual decisions and cancellation with optional durable history.
package coordinator

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sort"
	"sync"
	"time"

	"airlock/internal/protocol"
)

const MaxPending = 128

type entry struct {
	pending protocol.Pending
	result  chan protocol.Result
	timer   *time.Timer
}

type Coordinator struct {
	mu          sync.Mutex
	pending     map[string]*entry
	closed      bool
	history     Recorder
	failed      bool
	now         func() time.Time
	epoch       string
	sequence    uint64
	subscribers map[*subscriber]struct{}
}

// Recorder commits audit records before a proposal or terminal result is visible.
type Recorder interface {
	RecordPending(protocol.Pending) error
	RecordResult(protocol.Result, time.Time) error
	Prune(time.Time) error
}

// PruneHistory serializes idle maintenance with decisions. Failure denies all
// waiters just like a failed admission or terminal commit.
func (c *Coordinator) PruneHistory() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failed {
		return protocol.Error("history_unavailable", "history persistence failed; authorization denied")
	}
	if c.closed {
		return protocol.Error("unavailable", "daemon is shutting down")
	}
	if c.history != nil && c.history.Prune(c.now()) != nil {
		c.failHistory()
		return protocol.Error("history_unavailable", "could not prune history; authorization denied")
	}
	return nil
}

func NewWithHistory(history Recorder) *Coordinator {
	c := New()
	c.history = history
	return c
}

func New() *Coordinator {
	var epoch [16]byte
	if _, err := rand.Read(epoch[:]); err != nil {
		panic("could not generate coordinator stream epoch")
	}
	return &Coordinator{pending: make(map[string]*entry), now: time.Now, epoch: hex.EncodeToString(epoch[:]), subscribers: make(map[*subscriber]struct{})}
}

// Submit copies mutable fields so callers cannot change an outstanding proposal.
func (c *Coordinator) Submit(r protocol.Request, wait time.Duration) (<-chan protocol.Result, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	if wait <= 0 || wait > protocol.MaxWait {
		return nil, protocol.Error("invalid_wait", "invalid wait duration")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failed {
		return nil, protocol.Error("history_unavailable", "history persistence failed; restart the daemon after resolving the storage error")
	}
	if c.closed {
		return nil, protocol.Error("unavailable", "daemon is shutting down")
	}
	if _, exists := c.pending[r.RequestID]; exists {
		return nil, protocol.Error("duplicate_request", "request ID is already pending")
	}
	if len(c.pending) >= MaxPending {
		return nil, protocol.Error("queue_full", "pending queue is full")
	}
	r.WorkspaceRoots = append([]string(nil), r.WorkspaceRoots...)
	if r.SourceToolCallID != nil {
		id := *r.SourceToolCallID
		r.SourceToolCallID = &id
	}
	now := c.now()
	e := &entry{pending: protocol.Pending{Request: r, ReceivedAt: now, Deadline: now.Add(wait)}, result: make(chan protocol.Result, 1)}
	if c.history != nil {
		if err := c.history.RecordPending(e.pending); err != nil {
			var wire *protocol.WireError
			if errors.As(err, &wire) && wire.Code == "duplicate_request" {
				return nil, err
			}
			c.failHistory()
			return nil, protocol.Error("history_unavailable", "could not persist request history")
		}
	}
	c.pending[r.RequestID] = e
	c.emit(protocol.QueueEvent{Pending: &e.pending})
	e.timer = time.AfterFunc(max(e.pending.Deadline.Sub(c.now()), 0), func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.pending[r.RequestID] == e {
			c.finish(e, "expired", "decision deadline expired")
		}
	})
	return e.result, nil
}

// finish is called only with the mutex held. Commit the terminal state and its
// event before removing the request or publishing a result to either client.
func (c *Coordinator) finish(e *entry, state, reason string) (protocol.Result, error) {
	permission := "deny"
	if state == "allowed" {
		permission = "allow"
	}
	r := protocol.Result{RequestID: e.pending.Request.RequestID, State: state, Permission: permission, Reason: reason}
	if c.history != nil {
		if c.history.RecordResult(r, c.now()) != nil {
			// The failed write may have an ambiguous commit outcome. Do not
			// retry this transition or send an approval; restart recovers any
			// record still pending. Deny every other waiter immediately too.
			r = historyDenial(e)
			c.failHistory()
			return r, protocol.Error("history_unavailable", "could not persist terminal history; authorization denied")
		}
	}
	c.publish(e, r)
	return r, nil
}

// publish never blocks because each entry has one buffered terminal result.
func (c *Coordinator) publish(e *entry, r protocol.Result) {
	delete(c.pending, e.pending.Request.RequestID)
	if e.timer != nil {
		e.timer.Stop()
	}
	c.emit(protocol.QueueEvent{Result: &r})
	e.result <- r
}

func historyDenial(e *entry) protocol.Result {
	return protocol.Result{RequestID: e.pending.Request.RequestID, State: "interrupted", Permission: "deny", Reason: "history persistence failed"}
}

// failHistory is called with the mutex held. Safety denials are the only results
// permitted without a durable commit. Do not perform further storage operations
// after failure: they could delay denial of the remaining waiters. Startup
// recovery interrupts any records still pending.
func (c *Coordinator) failHistory() {
	c.failed = true
	c.stopSubscribers(protocol.Error("history_unavailable", "history persistence failed; authorization denied"))
	for _, e := range c.pending {
		r := historyDenial(e)
		c.publish(e, r)
	}
}

func (c *Coordinator) Decide(id, permission string) (protocol.Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if permission != "allow" && permission != "deny" {
		return protocol.Result{}, protocol.Error("invalid_decision", "decision must be allow or deny")
	}
	if c.failed {
		return protocol.Result{}, protocol.Error("history_unavailable", "history persistence failed; authorization denied")
	}
	e := c.pending[id]
	if e == nil {
		return protocol.Result{}, protocol.Error("not_pending", "request is unknown or no longer pending")
	}
	if !c.now().Before(e.pending.Deadline) {
		if _, err := c.finish(e, "expired", "decision deadline expired"); err != nil {
			return protocol.Result{}, err
		}
		return protocol.Result{}, protocol.Error("not_pending", "request deadline expired")
	}
	state := "denied"
	if permission == "allow" {
		state = "allowed"
	}
	return c.finish(e, state, "manual decision")
}

// Cancel uses the original result channel as a generation token, so late
// disconnect handling cannot cancel a subsequent request that reused an ID.
func (c *Coordinator) Cancel(id string, result <-chan protocol.Result) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.pending[id]; e != nil && e.result == result {
		c.finish(e, "cancelled", "submitter disconnected")
	}
}

func (c *Coordinator) List() []protocol.Pending {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.list()
}

// list is called with the coordinator mutex held.
func (c *Coordinator) list() []protocol.Pending {
	items := make([]protocol.Pending, 0, len(c.pending))
	if c.failed {
		return items
	}
	for _, e := range c.pending {
		if !c.now().Before(e.pending.Deadline) {
			c.finish(e, "expired", "decision deadline expired")
			if c.failed {
				return []protocol.Pending{}
			}
			continue
		}
		items = append(items, e.pending.Clone())
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].ReceivedAt.Equal(items[j].ReceivedAt) {
			return items[i].Request.RequestID < items[j].Request.RequestID
		}
		return items[i].ReceivedAt.Before(items[j].ReceivedAt)
	})
	return items
}

func (c *Coordinator) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	for _, e := range c.pending {
		c.finish(e, "interrupted", "daemon shutting down")
	}
	c.stopSubscribers(protocol.Error("unavailable", "daemon is shutting down"))
}
