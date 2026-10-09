package protocol

import (
	"strings"
	"testing"
	"time"
)

func TestSubscriptionOperationsRejectExtraFields(t *testing.T) {
	for _, kind := range []string{"snapshot", "subscribe"} {
		if err := (Message{ProtocolVersion: Version, Type: kind}).Validate(); err != nil {
			t.Fatal(err)
		}
		for _, m := range []Message{
			{ProtocolVersion: Version, Type: kind, WaitMS: 1},
			{ProtocolVersion: Version, Type: kind, RequestID: "one"},
			{ProtocolVersion: Version, Type: kind, Permission: "allow"},
			{ProtocolVersion: Version, Type: kind, Request: &Request{}},
		} {
			if err := m.Validate(); err == nil {
				t.Fatalf("extra fields accepted: %+v", m)
			}
		}
	}
}
func TestQueueEventValidation(t *testing.T) {
	cursor := Cursor{Epoch: strings.Repeat("a", 32), Sequence: 1}
	p := Pending{Request: Request{ProtocolVersion: Version, RequestID: "one", Agent: "fixture", ConversationID: "sanitized", Event: "before_shell_execution", Command: "fixture-proposal"}, ReceivedAt: time.Now(), Deadline: time.Now().Add(time.Hour)}
	r := Result{RequestID: "one", State: "denied", Permission: "deny"}
	for _, event := range []QueueEvent{{Cursor: cursor, Pending: &p}, {Cursor: cursor, Result: &r}} {
		if err := event.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, event := range []QueueEvent{
		{Cursor: cursor},
		{Cursor: cursor, Pending: &p, Result: &r},
		{Cursor: Cursor{Epoch: "not-an-epoch", Sequence: 1}, Pending: &p},
		{Cursor: Cursor{Epoch: cursor.Epoch}, Pending: &p},
		{Cursor: cursor, Result: &Result{RequestID: "one", State: "expired", Permission: "allow"}},
	} {
		if err := event.Validate(); err == nil {
			t.Fatalf("invalid event accepted: %+v", event)
		}
	}
	p.Deadline = p.ReceivedAt
	if err := p.Validate(); err == nil {
		t.Fatal("invalid deadline accepted")
	}
	p.Deadline = p.ReceivedAt.Add(MaxWait + time.Second)
	if err := p.Validate(); err == nil {
		t.Fatal("excess wait accepted")
	}
}
