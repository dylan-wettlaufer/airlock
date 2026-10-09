package transport

import (
	"bufio"
	"context"
	"errors"
	"time"

	"airlock/internal/coordinator"
	"airlock/internal/protocol"
)

// Snapshot takes a consistent queue view with a coordinator-lifetime cursor.
func (c Client) Snapshot(ctx context.Context) (protocol.Snapshot, error) {
	var snapshot protocol.Snapshot
	err := c.stream(ctx, false, func(s protocol.Snapshot) error { snapshot = s; return nil }, nil)
	return snapshot, err
}

// Subscribe calls onSnapshot once, then onEvent in sequence on the calling
// goroutine. Callbacks must return promptly and honor cancellation when handing off to a UI.
// Any return invalidates the view. Reconnect by calling Subscribe again; every
// connection starts from a complete fresh snapshot, with no history replay.
func (c Client) Subscribe(ctx context.Context, onSnapshot func(protocol.Snapshot) error, onEvent func(protocol.QueueEvent) error) error {
	if onSnapshot == nil || onEvent == nil {
		return errors.New("subscription callbacks are required")
	}
	return c.stream(ctx, true, onSnapshot, onEvent)
}

func (c Client) stream(ctx context.Context, subscribe bool, onSnapshot func(protocol.Snapshot) error, onEvent func(protocol.QueueEvent) error) error {
	conn, err := c.connect(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-finished:
		}
	}()
	// Initial synchronization is bounded; healthy idle subscriptions have no
	// read timeout. Cancellation still closes the connection immediately.
	initialDeadline := time.Now().Add(ioTimeout)
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(initialDeadline) {
		initialDeadline = deadline
	}
	if err := conn.SetDeadline(initialDeadline); err != nil {
		return err
	}
	operation := "snapshot"
	if subscribe {
		operation = "subscribe"
	}
	if err := protocol.WriteFrame(conn, protocol.Message{ProtocolVersion: protocol.Version, Type: operation}); err != nil {
		return errors.New("could not send daemon request")
	}
	reader := bufio.NewReader(conn)
	read := func() (protocol.Response, error) {
		var response protocol.Response
		if err := protocol.ReadFrame(reader, &response); err != nil {
			if ctx.Err() != nil {
				return response, ctx.Err()
			}
			return response, errors.New("queue stream disconnected or returned an invalid frame; resynchronize")
		}
		return response, responseError(response)
	}
	begin, err := read()
	if err != nil {
		return err
	}
	if begin.Type != "snapshot_begin" || begin.Cursor == nil || begin.Pending != nil || begin.Result != nil || begin.Event != nil {
		return errors.New("invalid snapshot header")
	}
	if err := begin.Cursor.Validate(); err != nil {
		return err
	}
	snapshot := protocol.Snapshot{Cursor: *begin.Cursor, Items: []protocol.Pending{}}
	pending := map[string]bool{}
	for {
		response, err := read()
		if err != nil {
			return err
		}
		if response.Type == "snapshot" {
			if response.Cursor == nil || *response.Cursor != snapshot.Cursor || response.Pending != nil || response.Result != nil || response.Event != nil {
				return errors.New("invalid snapshot boundary")
			}
			break
		}
		if response.Type != "pending" || response.Pending == nil || response.Cursor != nil || response.Result != nil || response.Event != nil || len(snapshot.Items) >= coordinator.MaxPending {
			return errors.New("invalid snapshot item")
		}
		p := *response.Pending
		if err := p.Validate(); err != nil {
			return err
		}
		if pending[p.Request.RequestID] {
			return errors.New("duplicate snapshot request")
		}
		if len(snapshot.Items) > 0 {
			previous := snapshot.Items[len(snapshot.Items)-1]
			if p.ReceivedAt.Before(previous.ReceivedAt) || (p.ReceivedAt.Equal(previous.ReceivedAt) && p.Request.RequestID <= previous.Request.RequestID) {
				return errors.New("unordered snapshot")
			}
		}
		pending[p.Request.RequestID] = true
		snapshot.Items = append(snapshot.Items, p)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return err
	}
	cursor := snapshot.Cursor // callbacks may edit their snapshot without editing validation state
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err := onSnapshot(snapshot); err != nil {
		return err
	}
	if !subscribe {
		return nil
	}
	for {
		response, err := read()
		if err != nil {
			return err
		}
		if response.Type != "event" || response.Event == nil || response.Cursor != nil || response.Pending != nil || response.Result != nil {
			return errors.New("invalid event envelope")
		}
		event := *response.Event
		if err := event.Validate(); err != nil {
			return err
		}
		if event.Cursor.Epoch != cursor.Epoch || cursor.Sequence == ^uint64(0) || event.Cursor.Sequence != cursor.Sequence+1 {
			return protocol.Error("resync_required", "queue stream gap or changed epoch; resynchronize")
		}
		if event.Pending != nil {
			id := event.Pending.Request.RequestID
			if pending[id] || len(pending) >= coordinator.MaxPending {
				return errors.New("invalid queue admission")
			}
			pending[id] = true
		} else {
			id := event.Result.RequestID
			if !pending[id] {
				return errors.New("terminal event for unknown request")
			}
			delete(pending, id)
		}
		cursor = event.Cursor
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := onEvent(event); err != nil {
			return err
		}
	}
}
