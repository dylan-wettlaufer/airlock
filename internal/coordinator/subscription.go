package coordinator

import "airlock/internal/protocol"

const MaxSubscribers = 8
const SubscriptionBuffer = 16

type subscriber struct {
	events chan protocol.QueueEvent
	done   chan struct{}
	err    error // guarded by Coordinator.mu
}

// Subscription starts at Snapshot.Cursor. Consumers must discard their view on
// Done, even when Events still contains buffered items, and take a fresh snapshot.
type Subscription struct {
	Snapshot    protocol.Snapshot
	Events      <-chan protocol.QueueEvent
	Done        <-chan struct{}
	coordinator *Coordinator
	subscriber  *subscriber
}

func (s *Subscription) Close() {
	c := s.coordinator
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropSubscriber(s.subscriber, protocol.Error("unavailable", "subscription closed"))
}
func (s *Subscription) Err() error {
	c := s.coordinator
	c.mu.Lock()
	defer c.mu.Unlock()
	return s.subscriber.err
}

func (c *Coordinator) available() error {
	if c.failed {
		return protocol.Error("history_unavailable", "history persistence failed; authorization denied")
	}
	if c.closed {
		return protocol.Error("unavailable", "daemon is shutting down")
	}
	return nil
}

func (c *Coordinator) Snapshot() (protocol.Snapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snapshot()
}
func (c *Coordinator) snapshot() (protocol.Snapshot, error) {
	items := c.list()
	if err := c.available(); err != nil {
		return protocol.Snapshot{}, err
	}
	return protocol.Snapshot{Cursor: c.cursor(), Items: items}, nil
}

// Subscribe expires stale proposals, captures state, and registers the observer
// under one lock. No transition can fall between the snapshot and event stream.
func (c *Coordinator) Subscribe() (*Subscription, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.available(); err != nil {
		return nil, err
	}
	if len(c.subscribers) >= MaxSubscribers {
		return nil, protocol.Error("subscriber_limit", "too many subscribers")
	}
	snapshot, err := c.snapshot()
	if err != nil {
		return nil, err
	}
	s := &subscriber{events: make(chan protocol.QueueEvent, SubscriptionBuffer), done: make(chan struct{})}
	c.subscribers[s] = struct{}{}
	return &Subscription{Snapshot: snapshot, Events: s.events, Done: s.done, coordinator: c, subscriber: s}, nil
}

func (c *Coordinator) cursor() protocol.Cursor {
	return protocol.Cursor{Epoch: c.epoch, Sequence: c.sequence}
}

// emit never waits for an observer. Overflow invalidates that stream; it cannot
// delay a hook result or silently skip an event for a connected observer.
func (c *Coordinator) emit(event protocol.QueueEvent) {
	c.sequence++
	event.Cursor = c.cursor()
	for s := range c.subscribers {
		copy := event
		if event.Pending != nil {
			p := event.Pending.Clone()
			copy.Pending = &p
		}
		if event.Result != nil {
			r := *event.Result
			copy.Result = &r
		}
		select {
		case s.events <- copy:
		default:
			c.dropSubscriber(s, protocol.Error("resync_required", "subscriber fell behind; reconnect for a fresh snapshot"))
		}
	}
}
func (c *Coordinator) dropSubscriber(s *subscriber, err error) {
	if _, ok := c.subscribers[s]; !ok {
		return
	}
	delete(c.subscribers, s)
	s.err = err
	close(s.done)
	// Do not close Events: Done is the authoritative invalidation signal.
}
func (c *Coordinator) stopSubscribers(err error) {
	for s := range c.subscribers {
		c.dropSubscriber(s, err)
	}
}
