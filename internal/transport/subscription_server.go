package transport

import (
	"bufio"
	"net"
	"time"

	"airlock/internal/protocol"
)

func sendSnapshot(conn net.Conn, snapshot protocol.Snapshot, valid func() bool) error {
	frames := []protocol.Response{{Type: "snapshot_begin", Cursor: &snapshot.Cursor}}
	// Build only envelopes: proposal strings remain shared immutable data.
	for i := range snapshot.Items {
		frames = append(frames, protocol.Response{Type: "pending", Pending: &snapshot.Items[i]})
	}
	frames = append(frames, protocol.Response{Type: "snapshot", Cursor: &snapshot.Cursor})
	for _, frame := range frames {
		if valid != nil && !valid() {
			return protocol.Error("resync_required", "subscription invalidated")
		}
		if err := send(conn, frame); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) subscribe(conn net.Conn, reader *bufio.Reader) {
	subscription, err := s.queue.Subscribe()
	if err != nil {
		sendError(conn, err)
		return
	}
	defer subscription.Close()
	_ = conn.SetReadDeadline(time.Time{})
	disconnected := make(chan struct{})
	go func() { defer close(disconnected); _, _ = reader.ReadByte() }()
	defer func() { conn.Close(); <-disconnected }()
	valid := func() bool {
		select {
		case <-subscription.Done:
			return false
		case <-disconnected:
			return false
		default:
			return true
		}
	}
	if err := sendSnapshot(conn, subscription.Snapshot, valid); err != nil {
		return
	}
	for {
		// Invalidation wins over buffered events: never imply a complete stream
		// after overflow, history failure, shutdown or submitter input/EOF.
		if !valid() {
			return
		}
		select {
		case <-disconnected:
			return
		case <-subscription.Done:
			return
		case event := <-subscription.Events:
			if !valid() {
				return
			}
			if err := send(conn, protocol.Response{Type: "event", Event: &event}); err != nil {
				return
			}
		}
	}
}
