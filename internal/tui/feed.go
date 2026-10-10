package tui

import (
	"context"
	"errors"
	"sync"
	"time"

	"airlock/internal/protocol"
)

type client interface {
	Subscribe(context.Context, func(protocol.Snapshot) error, func(protocol.QueueEvent) error) error
	Decide(context.Context, string, string) (protocol.Result, error)
}

type snapshotMsg struct {
	generation uint64
	snapshot   protocol.Snapshot
}
type eventMsg struct {
	generation uint64
	event      protocol.QueueEvent
}
type connectionMsg struct {
	generation uint64
	retry      time.Duration
	err        error
}

// feed bounds UI handoff and invalidates its lease before announcing a broken
// stream. Commands queued by the UI must recheck that lease before socket I/O.
type feed struct {
	ctx        context.Context
	client     client
	messages   chan interfaceMessage
	done       chan struct{}
	wake       chan struct{}
	mu         sync.Mutex
	generation uint64
	epoch      string
	live       bool
	session    context.Context
	cancel     context.CancelFunc
}

// Only these typed stream messages cross the worker/UI boundary.
type interfaceMessage interface{ streamMessage() }

func (snapshotMsg) streamMessage()   {}
func (eventMsg) streamMessage()      {}
func (connectionMsg) streamMessage() {}

var errStaleView = errors.New("queue is disconnected; wait for a fresh snapshot")
var errExpired = errors.New("request deadline expired")

var errSlowUI = errors.New("screen fell behind; refreshing queue")

func newFeed(ctx context.Context, c client) *feed {
	return &feed{ctx: ctx, client: c, messages: make(chan interfaceMessage, 16), done: make(chan struct{}), wake: make(chan struct{}, 1)}
}
func (f *feed) valid(generation uint64, epoch string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.live && f.generation == generation && f.epoch == epoch && f.session.Err() == nil
}
func (f *feed) refresh() {
	f.mu.Lock()
	f.live = false
	if f.cancel != nil {
		f.cancel()
	}
	f.mu.Unlock()
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

// A late acknowledgment from an older connection must not cancel a freshly
// synchronized subscription that has already replaced it.
func (f *feed) refreshAt(generation uint64) {
	f.mu.Lock()
	if f.generation != generation {
		f.mu.Unlock()
		return
	}
	f.live = false
	if f.cancel != nil {
		f.cancel()
	}
	f.mu.Unlock()
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

func (f *feed) offer(message interfaceMessage) error {
	select {
	case <-f.ctx.Done():
		return f.ctx.Err()
	case f.messages <- message:
		return nil
	default:
		return errSlowUI
	}
}
func (f *feed) status(message connectionMsg) bool {
	select {
	case <-f.ctx.Done():
		return false
	case f.messages <- message:
		return true
	}
}

func (f *feed) run() {
	defer close(f.done)
	backoff := 500 * time.Millisecond
	for f.ctx.Err() == nil {
		ctx, cancel := context.WithCancel(f.ctx)
		f.mu.Lock()
		f.generation++
		generation := f.generation
		f.live = false
		f.session = ctx
		f.cancel = cancel
		f.mu.Unlock()
		if !f.status(connectionMsg{generation: generation}) {
			cancel()
			return
		}
		synchronized := false
		err := f.client.Subscribe(ctx, func(s protocol.Snapshot) error {
			f.mu.Lock()
			if ctx.Err() != nil {
				f.mu.Unlock()
				return ctx.Err()
			}
			f.epoch = s.Cursor.Epoch
			f.live = true
			f.mu.Unlock()
			synchronized = true
			return f.offer(snapshotMsg{generation, s})
		}, func(e protocol.QueueEvent) error { return f.offer(eventMsg{generation, e}) })
		// Cancel in-flight decisions too. An uncertain acknowledgment is never retried.
		f.mu.Lock()
		f.live = false
		cancel()
		f.mu.Unlock()
		if f.ctx.Err() != nil {
			return
		}
		if synchronized {
			backoff = 500 * time.Millisecond
		}
		if err == nil {
			err = errors.New("queue stream ended")
		}
		if !f.status(connectionMsg{generation: generation, retry: backoff, err: err}) {
			return
		}
		timer := time.NewTimer(backoff)
		select {
		case <-f.ctx.Done():
			timer.Stop()
			return
		case <-f.wake:
			timer.Stop()
		case <-timer.C:
		}
		backoff = min(5*time.Second, backoff*2)
	}
}

func (f *feed) decide(generation uint64, epoch, id, permission string, deadline time.Time) (protocol.Result, error) {
	f.mu.Lock()
	if !f.live || f.generation != generation || f.epoch != epoch || f.session.Err() != nil {
		f.mu.Unlock()
		return protocol.Result{}, errStaleView
	}
	ctx := f.session
	f.mu.Unlock()
	if !time.Now().Before(deadline) {
		return protocol.Result{}, errExpired
	}
	return f.client.Decide(ctx, id, permission)
}
