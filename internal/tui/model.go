package tui

import (
	"airlock/internal/transport"
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"airlock/internal/protocol"
	tea "charm.land/bubbletea/v2"
)

type tickMsg time.Time
type confirmation struct{ id, permission string }
type decisionMsg struct {
	generation     uint64
	id, permission string
	result         protocol.Result
	err            error
}

type model struct {
	ctx              context.Context
	feed             *feed
	items            []protocol.Pending
	selected         string
	cursor           protocol.Cursor
	generation       uint64
	connected        bool
	retry            time.Duration
	clock            func() time.Time
	now              time.Time
	width, height    int
	detailOffset     int
	confirm          *confirmation
	busy             bool
	notice           string
	connectionReason string
	socket           string
}

func newModel(ctx context.Context, f *feed) *model {
	return &model{ctx: ctx, feed: f, items: []protocol.Pending{}, clock: time.Now, now: time.Now()}
}
func (m *model) Init() tea.Cmd { return tea.Batch(m.listen(), tick()) }
func tick() tea.Cmd {
	return tea.Tick(250*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}
func (m *model) listen() tea.Cmd {
	return func() tea.Msg {
		select {
		case <-m.ctx.Done():
			return tea.Quit()
		case message := <-m.feed.messages:
			return message
		}
	}
}
func (m *model) live() bool { return m.connected && m.feed.valid(m.generation, m.cursor.Epoch) }
func (m *model) selectedIndex() int {
	for i, p := range m.items {
		if p.Request.RequestID == m.selected {
			return i
		}
	}
	return -1
}
func (m *model) selection() *protocol.Pending {
	if i := m.selectedIndex(); i >= 0 {
		return &m.items[i]
	}
	return nil
}
func (m *model) choose(previous int) {
	if m.selectedIndex() >= 0 {
		return
	}
	m.confirm = nil
	m.detailOffset = 0
	if len(m.items) == 0 {
		m.selected = ""
		return
	}
	m.selected = m.items[max(0, min(previous, len(m.items)-1))].Request.RequestID
}
func (m *model) sort() {
	sort.Slice(m.items, func(i, j int) bool {
		a, b := m.items[i], m.items[j]
		if a.ReceivedAt.Equal(b.ReceivedAt) {
			return a.Request.RequestID < b.Request.RequestID
		}
		return a.ReceivedAt.Before(b.ReceivedAt)
	})
}
func (m *model) canDecide() bool {
	p := m.selection()
	return m.live() && !m.busy && m.width >= 52 && m.height >= 16 && p != nil && m.clock().Before(p.Deadline)
}
func (m *model) offline() { m.connected = false; m.confirm = nil }

func (m *model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	case tickMsg:
		m.now = m.clock()
		if !m.live() || !m.canDecide() {
			m.confirm = nil
		}
		return m, tick()
	case connectionMsg:
		if msg.generation >= m.generation {
			m.generation = msg.generation
			m.offline()
			m.retry = msg.retry
			m.connectionReason = connectionReason(msg.err)
		}
		return m, m.listen()
	case snapshotMsg:
		if msg.generation >= m.generation && m.feed.valid(msg.generation, msg.snapshot.Cursor.Epoch) {
			previous := m.selectedIndex()
			m.items = msg.snapshot.Items
			m.cursor = msg.snapshot.Cursor
			m.generation = msg.generation
			m.connected = true
			m.retry = 0
			m.confirm = nil
			m.detailOffset = 0
			m.sort()
			m.choose(previous)
		}
		return m, m.listen()
	case eventMsg:
		if msg.generation != m.generation || !m.live() {
			return m, m.listen()
		}
		event := msg.event
		if event.Validate() != nil || event.Cursor.Epoch != m.cursor.Epoch || event.Cursor.Sequence != m.cursor.Sequence+1 {
			m.offline()
			m.feed.refresh()
			return m, m.listen()
		}
		previous := m.selectedIndex()
		if event.Pending != nil {
			m.items = append(m.items, event.Pending.Clone())
			m.sort()
		} else {
			for i, p := range m.items {
				if p.Request.RequestID == event.Result.RequestID {
					m.items = append(m.items[:i], m.items[i+1:]...)
					break
				}
			}
		}
		m.cursor = event.Cursor
		m.choose(previous)
		if m.confirm != nil && (m.selection() == nil || !m.clock().Before(m.selection().Deadline)) {
			m.confirm = nil
		}
		return m, m.listen()
	case decisionMsg:
		m.busy = false
		if msg.err != nil {
			m.notice = "Decision response unavailable; not retried."
			if errors.Is(msg.err, errStaleView) || errors.Is(msg.err, errExpired) {
				m.notice = "Queue changed or deadline expired; no decision sent."
			}
			var wire *protocol.WireError
			if errors.As(msg.err, &wire) {
				if wire.Code == "not_pending" {
					m.notice = "Request is no longer pending."
				}
				if wire.Code == "history_unavailable" {
					m.notice = "Storage unavailable; authorization denied."
				}
			}
			if msg.generation == m.generation {
				m.offline()
			}
			m.feed.refreshAt(msg.generation)
		} else {
			m.notice = fmt.Sprintf("%s: %s · execution unobserved", msg.id, msg.result.State)
		}
	case tea.KeyPressMsg:
		m.now = m.clock()
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "r":
			m.offline()
			m.retry = 0
			m.feed.refresh()
			m.notice = "Refreshing queue…"
		case "esc":
			m.confirm = nil
		case "up", "k":
			m.move(-1)
		case "down", "j":
			m.move(1)
		case "home", "g":
			m.move(-len(m.items))
		case "end", "G":
			m.move(len(m.items))
		case "pgdown":
			m.detailOffset += max(1, m.detailHeight()-1)
		case "pgup":
			m.detailOffset = max(0, m.detailOffset-max(1, m.detailHeight()-1))
		case "a", "d":
			if m.canDecide() {
				permission := "allow"
				if msg.String() == "d" {
					permission = "deny"
				}
				m.confirm = &confirmation{m.selected, permission}
				m.notice = ""
			}
		case "enter":
			if m.confirm != nil && m.canDecide() && m.confirm.id == m.selected {
				target := *m.confirm
				deadline := m.selection().Deadline
				generation, epoch := m.generation, m.cursor.Epoch
				m.confirm = nil
				m.busy = true
				m.notice = "Sending decision for " + target.id + "…"
				return m, func() tea.Msg {
					r, err := m.feed.decide(generation, epoch, target.id, target.permission, deadline)
					return decisionMsg{generation: generation, id: target.id, permission: target.permission, result: r, err: err}
				}
			}
		}
	}
	return m, nil
}
func (m *model) move(delta int) {
	if len(m.items) == 0 {
		return
	}
	index := max(0, min(m.selectedIndex()+delta, len(m.items)-1))
	if m.selected != m.items[index].Request.RequestID {
		m.selected = m.items[index].Request.RequestID
		m.confirm = nil
		m.detailOffset = 0
	}
}

func connectionReason(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, transport.ErrDaemonNotRunning) {
		return "No daemon reachable at the configured socket"
	}
	if errors.Is(err, errSlowUI) {
		return "Refreshing after a screen backlog"
	}
	if errors.Is(err, context.Canceled) {
		return "Refresh requested"
	}
	var wire *protocol.WireError
	if errors.As(err, &wire) {
		switch wire.Code {
		case "history_unavailable":
			return "History storage unavailable"
		case "invalid_message", "unsupported_version":
			return "Daemon does not support live queues"
		case "subscriber_limit":
			return "All subscription slots are busy"
		}
	}
	return "Connection lost"
}
