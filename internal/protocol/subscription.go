package protocol

import (
	"encoding/hex"
	"errors"
)

// Cursor identifies a position in one coordinator lifetime. It is not an audit
// offset and is never used to resume or replay authorizations.
type Cursor struct {
	Epoch    string `json:"epoch"`
	Sequence uint64 `json:"sequence"`
}

func (c Cursor) Validate() error {
	data, err := hex.DecodeString(c.Epoch)
	if err != nil || len(data) != 16 {
		return errors.New("invalid stream epoch")
	}
	return nil
}

// Snapshot is delivered only after every bounded snapshot frame is received.
type Snapshot struct {
	Cursor Cursor    `json:"cursor"`
	Items  []Pending `json:"items"`
}

// QueueEvent contains exactly one admitted proposal or terminal result.
type QueueEvent struct {
	Cursor  Cursor   `json:"cursor"`
	Pending *Pending `json:"pending,omitempty"`
	Result  *Result  `json:"result,omitempty"`
}

func (e QueueEvent) Validate() error {
	if err := e.Cursor.Validate(); err != nil {
		return err
	}
	if e.Cursor.Sequence == 0 || (e.Pending == nil) == (e.Result == nil) {
		return errors.New("invalid queue event")
	}
	if e.Pending != nil {
		return e.Pending.Validate()
	}
	return e.Result.Validate(e.Result.RequestID)
}

func (p Pending) Validate() error {
	if err := p.Request.Validate(); err != nil {
		return err
	}
	if p.ReceivedAt.IsZero() || !p.Deadline.After(p.ReceivedAt) || p.Deadline.Sub(p.ReceivedAt) > MaxWait {
		return errors.New("invalid pending timestamps")
	}
	return nil
}

// Clone keeps snapshots and subscribers from mutating the coordinator or peers.
func (p Pending) Clone() Pending {
	p.Request.WorkspaceRoots = append([]string(nil), p.Request.WorkspaceRoots...)
	if p.Request.SourceToolCallID != nil {
		id := *p.Request.SourceToolCallID
		p.Request.SourceToolCallID = &id
	}
	return p
}
