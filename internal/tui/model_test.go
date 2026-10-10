package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"airlock/internal/protocol"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

type fakeClient struct {
	subscribe func(context.Context, func(protocol.Snapshot) error, func(protocol.QueueEvent) error) error
	decide    func(context.Context, string, string) (protocol.Result, error)
}

func (c fakeClient) Subscribe(ctx context.Context, s func(protocol.Snapshot) error, e func(protocol.QueueEvent) error) error {
	return c.subscribe(ctx, s, e)
}
func (c fakeClient) Decide(ctx context.Context, id, p string) (protocol.Result, error) {
	if c.decide == nil {
		return protocol.Result{}, errors.New("unexpected decision")
	}
	return c.decide(ctx, id, p)
}

func fixture(id string, at time.Time) protocol.Pending {
	return protocol.Pending{Request: protocol.Request{ProtocolVersion: 1, RequestID: id, Agent: "fixture", ConversationID: "sanitized-" + id, Event: "before_shell_execution", Command: "fixture-proposal sanitized-argument"}, ReceivedAt: at, Deadline: at.Add(time.Hour)}
}
func ready(t *testing.T, c fakeClient, items ...protocol.Pending) *model {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	f := newFeed(ctx, c)
	f.generation = 1
	f.epoch = strings.Repeat("a", 32)
	f.live = true
	f.session = ctx
	f.cancel = cancel
	m := newModel(ctx, f)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 32})
	m.Update(snapshotMsg{1, protocol.Snapshot{Cursor: protocol.Cursor{Epoch: f.epoch, Sequence: uint64(len(items))}, Items: items}})
	return m
}
func key(m *model, k string) tea.Cmd {
	code := rune(0)
	text := k
	switch k {
	case "enter":
		code = tea.KeyEnter
		text = ""
	case "esc":
		code = tea.KeyEscape
		text = ""
	case "pgdown":
		code = tea.KeyPgDown
		text = ""
	default:
		if len(k) == 1 {
			code = rune(k[0])
		}
	}
	_, cmd := m.Update(tea.KeyPressMsg{Code: code, Text: text})
	return cmd
}
func event(m *model, p *protocol.Pending, r *protocol.Result) {
	m.Update(eventMsg{m.generation, protocol.QueueEvent{Cursor: protocol.Cursor{Epoch: m.cursor.Epoch, Sequence: m.cursor.Sequence + 1}, Pending: p, Result: r}})
}

func TestStableSelectionArrivalRemovalAndReconnect(t *testing.T) {
	at := time.Now()
	one, two, three := fixture("one", at), fixture("two", at.Add(time.Second)), fixture("three", at.Add(2*time.Second))
	m := ready(t, fakeClient{}, one, two, three)
	key(m, "j")
	if m.selected != "two" {
		t.Fatal("navigation failed")
	}
	earlier := fixture("earlier", at.Add(-time.Second))
	event(m, &earlier, nil)
	if m.selected != "two" {
		t.Fatal("new arrival shifted selection")
	}
	event(m, nil, &protocol.Result{RequestID: "one", State: "denied", Permission: "deny"})
	if m.selected != "two" {
		t.Fatal("removing another request shifted selection")
	}
	key(m, "a")
	event(m, nil, &protocol.Result{RequestID: "two", State: "allowed", Permission: "allow"})
	if m.selected != "three" || m.confirm != nil {
		t.Fatal("removed selection did not choose nearest survivor and clear confirmation")
	}
	m.Update(connectionMsg{generation: 1, retry: time.Second, err: errors.New("disconnected")})
	if m.live() || m.canDecide() {
		t.Fatal("offline decision enabled")
	}
	m.feed.mu.Lock()
	m.feed.generation = 2
	m.feed.epoch = strings.Repeat("b", 32)
	m.feed.mu.Unlock()
	m.Update(snapshotMsg{2, protocol.Snapshot{Cursor: protocol.Cursor{Epoch: strings.Repeat("b", 32)}, Items: []protocol.Pending{three}}})
	if m.selected != "three" || len(m.items) != 1 || !m.live() {
		t.Fatal("fresh snapshot failed to replace cached queue")
	}
}

func TestConfirmationTargetsFullIDAndSuppressesRepeatedKeys(t *testing.T) {
	at := time.Now()
	ids := []string{}
	permissions := []string{}
	m := ready(t, fakeClient{decide: func(_ context.Context, id, p string) (protocol.Result, error) {
		ids = append(ids, id)
		permissions = append(permissions, p)
		return protocol.Result{RequestID: id, State: "allowed", Permission: p}, nil
	}}, fixture("same-prefix-one", at), fixture("same-prefix-two", at.Add(time.Second)))
	key(m, "a")
	if len(ids) != 0 || m.confirm == nil {
		t.Fatal("allow did not require confirmation")
	}
	cmd := key(m, "enter")
	if cmd == nil || !m.busy {
		t.Fatal("missing decision command")
	}
	key(m, "j")
	key(m, "a")
	if key(m, "enter") != nil {
		t.Fatal("duplicate in-flight decision")
	}
	msg := cmd()
	m.Update(msg)
	if len(ids) != 1 || ids[0] != "same-prefix-one" || permissions[0] != "allow" || m.busy {
		t.Fatalf("decision misrouted: %v %v", ids, permissions)
	}
	key(m, "d")
	key(m, "esc")
	if key(m, "enter") != nil {
		t.Fatal("cancelled confirmation decided")
	}
}

func TestConfirmationExpiresAndStaleQueuedCommandNeverSends(t *testing.T) {
	calls := 0
	at := time.Now()
	p := fixture("expiry", at)
	m := ready(t, fakeClient{decide: func(context.Context, string, string) (protocol.Result, error) { calls++; return protocol.Result{}, nil }}, p)
	key(m, "a")
	m.clock = func() time.Time { return p.Deadline }
	m.Update(tickMsg(p.Deadline))
	if m.confirm != nil || key(m, "enter") != nil || calls != 0 {
		t.Fatal("expired confirmation allowed")
	}
	m.clock = time.Now
	key(m, "a")
	cmd := key(m, "enter")
	if cmd == nil {
		t.Fatal("missing queued command")
	}
	m.feed.refresh()
	m.Update(cmd())
	if calls != 0 || m.live() || !strings.Contains(m.notice, "no decision sent") {
		t.Fatal("stale queued command sent or retry implied")
	}
}

func TestDecisionFailureRefreshesWithoutRetry(t *testing.T) {
	calls := 0
	m := ready(t, fakeClient{decide: func(context.Context, string, string) (protocol.Result, error) {
		calls++
		return protocol.Result{}, errors.New("connection lost after write")
	}}, fixture("uncertain", time.Now()))
	key(m, "d")
	cmd := key(m, "enter")
	m.Update(cmd())
	if calls != 1 || m.live() || m.confirm != nil || !strings.Contains(ansi.Strip(m.View().Content), "not retried") {
		t.Fatal("uncertain acknowledgment was retried or hidden")
	}
}

func TestCountdownEscapingScrollingAndResize(t *testing.T) {
	at := time.Now()
	p := fixture("display", at)
	p.Request.Command = "printf '\x1b[2J\n\u202e' " + strings.Repeat("long-argument ", 100)
	p.Request.ConversationID = "chat\x1b]52;c;injected\a"
	m := ready(t, fakeClient{}, p)
	before := m.View().Content
	key(m, "pgdown")
	after := m.View().Content
	if before == after {
		t.Fatal("details did not scroll")
	}
	for _, size := range [][2]int{{120, 32}, {80, 24}, {52, 16}, {20, 8}} {
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		view := m.View()
		if !view.AltScreen {
			t.Fatal("terminal screen was not isolated")
		}
		lines := strings.Split(view.Content, "\n")
		if len(lines) > size[1] {
			t.Fatalf("height overflow: %v", size)
		}
		for _, line := range lines {
			if ansi.StringWidth(line) > size[0] {
				t.Fatalf("width overflow: %v", size)
			}
		}
		plain := ansi.Strip(view.Content)
		if strings.Contains(plain, "\u202e") || strings.Contains(plain, "\x1b") || strings.Contains(view.Content, "\x1b]52;c;injected") {
			t.Fatal("proposal injected terminal controls")
		}
	}
	if m.canDecide() {
		t.Fatal("tiny terminal enabled decisions")
	}
	if remaining(at, at) != "EXPIRED" || remaining(at.Add(time.Millisecond), at) != "1s" || remaining(at.Add(61*time.Second), at) != "1m 01s" {
		t.Fatal("incorrect countdown")
	}
}

func TestOldGenerationAndGapCannotEnableDecisions(t *testing.T) {
	m := ready(t, fakeClient{}, fixture("old", time.Now()))
	m.feed.mu.Lock()
	m.feed.live = false
	m.feed.mu.Unlock()
	m.Update(snapshotMsg{1, protocol.Snapshot{Cursor: m.cursor, Items: m.items}})
	if m.canDecide() {
		t.Fatal("queued old snapshot enabled decisions")
	}
	m.feed.mu.Lock()
	m.feed.live = true
	m.feed.mu.Unlock()
	m.Update(eventMsg{1, protocol.QueueEvent{Cursor: protocol.Cursor{Epoch: m.cursor.Epoch, Sequence: m.cursor.Sequence + 2}, Result: &protocol.Result{RequestID: "old", State: "denied", Permission: "deny"}}})
	if m.live() {
		t.Fatal("event gap left decision controls enabled")
	}
}

func TestLateDecisionErrorDoesNotInvalidateFreshSubscription(t *testing.T) {
	m := ready(t, fakeClient{}, fixture("old", time.Now()))
	m.feed.mu.Lock()
	m.feed.generation = 2
	m.feed.epoch = strings.Repeat("b", 32)
	m.feed.mu.Unlock()
	m.Update(snapshotMsg{2, protocol.Snapshot{Cursor: protocol.Cursor{Epoch: strings.Repeat("b", 32), Sequence: 1}, Items: []protocol.Pending{fixture("fresh", time.Now())}}})
	m.Update(decisionMsg{generation: 1, id: "old", permission: "deny", err: errors.New("old connection lost")})
	if !m.live() || m.selected != "fresh" || m.busy {
		t.Fatal("late result invalidated new subscription")
	}
}
