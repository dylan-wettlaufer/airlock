// Package coordinator serializes manual decisions and cancellation with optional durable history.
package coordinator

import (
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
	mu      sync.Mutex
	pending map[string]*entry
	closed  bool
	history Recorder
	failed  bool
	now     func() time.Time
}

// Recorder commits audit records before a proposal or terminal result is visible.
type Recorder interface {
	RecordPending(protocol.Pending) error
	RecordResult(protocol.Result, time.Time) error
}

func NewWithHistory(history Recorder) *Coordinator {
	c := New()
	c.history = history
	return c
}

func New() *Coordinator { return &Coordinator{pending: make(map[string]*entry), now: time.Now} }

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
			c.failed = true
			return nil, protocol.Error("history_unavailable", "could not persist request history")
		}
	}
	c.pending[r.RequestID] = e
	e.timer = time.AfterFunc(max(e.pending.Deadline.Sub(c.now()), 0), func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.pending[r.RequestID] == e {
			c.finish(e, "expired", "decision deadline expired")
		}
	})
	return e.result, nil
}

// finish is called only with the mutex held; the buffered result never blocks.
func (c *Coordinator) finish(e *entry, state, reason string) (protocol.Result, error) {
	delete(c.pending, e.pending.Request.RequestID)
	if e.timer != nil {
		e.timer.Stop()
	}
	permission := "deny"
	if state == "allowed" {
		permission = "allow"
	}
	r := protocol.Result{RequestID: e.pending.Request.RequestID, State: state, Permission: permission, Reason: reason}
	var err error
	if c.history != nil {
		if c.history.RecordResult(r, c.now()) != nil {
			c.failed = true
			err = protocol.Error("history_unavailable", "could not persist terminal history; authorization denied")
			r.State, r.Permission, r.Reason = "interrupted", "deny", "history persistence failed"
		}
	}
	e.result <- r
	return r, err
}

func (c *Coordinator) Decide(id, permission string) (protocol.Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if permission != "allow" && permission != "deny" {
		return protocol.Result{}, protocol.Error("invalid_decision", "decision must be allow or deny")
	}
	e := c.pending[id]
	if e == nil {
		return protocol.Result{}, protocol.Error("not_pending", "request is unknown or no longer pending")
	}
	if c.failed {
		return c.finish(e, "interrupted", "history persistence failed")
	}
	if !c.now().Before(e.pending.Deadline) {
		c.finish(e, "expired", "decision deadline expired")
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
	items := make([]protocol.Pending, 0, len(c.pending))
	for _, e := range c.pending {
		if !c.now().Before(e.pending.Deadline) {
			c.finish(e, "expired", "decision deadline expired")
			continue
		}
		p := e.pending
		p.Request.WorkspaceRoots = append([]string(nil), p.Request.WorkspaceRoots...)
		if p.Request.SourceToolCallID != nil {
			id := *p.Request.SourceToolCallID
			p.Request.SourceToolCallID = &id
		}
		items = append(items, p)
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
}
