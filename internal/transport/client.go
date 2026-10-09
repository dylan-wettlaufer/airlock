package transport

import (
	"bufio"
	"context"
	"errors"
	"net"
	"time"

	"airlock/internal/coordinator"
	"airlock/internal/protocol"
)

type Client struct{ Socket string }

func (c Client) exchange(ctx context.Context, message protocol.Message, timeout time.Duration, consume func(protocol.Response) (bool, error)) error {
	if err := message.Validate(); err != nil {
		return err
	}
	if err := validateSocket(c.Socket); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", c.Socket)
	if err != nil {
		return errors.New("daemon connection failed")
	}
	defer conn.Close()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	if err := protocol.WriteFrame(conn, message); err != nil {
		return errors.New("could not send daemon request")
	}
	reader := bufio.NewReader(conn)
	for {
		var response protocol.Response
		if err := protocol.ReadFrame(reader, &response); err != nil {
			return errors.New("daemon disconnected or returned an invalid response")
		}
		if response.ProtocolVersion != protocol.Version {
			return errors.New("unsupported daemon response version")
		}
		if response.Type == "error" {
			if response.Error == nil || response.Error.Code == "" || response.Result != nil || response.Pending != nil {
				return errors.New("invalid daemon error response")
			}
			// Do not trust diagnostic strings returned by an arbitrary socket peer.
			messages := map[string]string{
				"duplicate_request":   "request ID is already pending",
				"queue_full":          "pending queue is full",
				"not_pending":         "request is unknown, expired, or no longer pending",
				"unavailable":         "daemon is shutting down",
				"unsupported_version": "unsupported protocol version",
				"invalid_request":     "invalid proposal",
				"invalid_message":     "invalid daemon operation",
				"frame_too_large":     "frame exceeds 2 MiB",
				"invalid_frame":       "invalid JSON frame",
			}
			message, ok := messages[response.Error.Code]
			if !ok {
				message = "daemon rejected request"
			}
			return protocol.Error(response.Error.Code, message)
		}
		if response.Error != nil {
			return errors.New("invalid daemon response")
		}
		finished, err := consume(response)
		if err != nil {
			return err
		}
		if finished {
			return nil
		}
	}
}

func (c Client) Submit(ctx context.Context, request protocol.Request, wait time.Duration) (protocol.Result, error) {
	var result protocol.Result
	if wait < time.Millisecond || wait > protocol.MaxWait {
		return result, errors.New("wait must be at least 1ms and at most 24h")
	}
	err := c.exchange(ctx, protocol.Message{ProtocolVersion: protocol.Version, Type: "submit", Request: &request, WaitMS: wait.Milliseconds()}, wait+ioTimeout, func(r protocol.Response) (bool, error) {
		if r.Type != "submit" || r.Result == nil || r.Pending != nil {
			return true, errors.New("invalid submit response")
		}
		if err := r.Result.Validate(request.RequestID); err != nil {
			return true, err
		}
		result = *r.Result
		return true, nil
	})
	return result, err
}

func (c Client) Decide(ctx context.Context, id, permission string) (protocol.Result, error) {
	var result protocol.Result
	err := c.exchange(ctx, protocol.Message{ProtocolVersion: protocol.Version, Type: "decide", RequestID: id, Permission: permission}, ioTimeout, func(r protocol.Response) (bool, error) {
		if r.Type != "decide" || r.Result == nil || r.Pending != nil {
			return true, errors.New("invalid decision response")
		}
		if err := r.Result.Validate(id); err != nil {
			return true, err
		}
		if r.Result.Permission != permission {
			return true, errors.New("decision response does not match permission")
		}
		result = *r.Result
		return true, nil
	})
	return result, err
}

func (c Client) List(ctx context.Context) ([]protocol.Pending, error) {
	items := make([]protocol.Pending, 0)
	err := c.exchange(ctx, protocol.Message{ProtocolVersion: protocol.Version, Type: "list"}, ioTimeout, func(r protocol.Response) (bool, error) {
		if r.Type == "list" && r.Pending == nil && r.Result == nil {
			return true, nil
		}
		if r.Type != "pending" || r.Pending == nil || r.Result != nil || len(items) >= coordinator.MaxPending {
			return true, errors.New("invalid list response")
		}
		if err := r.Pending.Request.Validate(); err != nil {
			return true, errors.New("invalid pending proposal")
		}
		items = append(items, *r.Pending)
		return false, nil
	})
	return items, err
}

func (c Client) Health(ctx context.Context) error {
	return c.exchange(ctx, protocol.Message{ProtocolVersion: protocol.Version, Type: "health"}, ioTimeout, func(r protocol.Response) (bool, error) {
		if r.Type != "health" || r.Result != nil || r.Pending != nil {
			return true, errors.New("invalid health response")
		}
		return true, nil
	})
}
