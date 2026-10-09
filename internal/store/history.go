package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

func jsonRoots(roots []string) (string, error) {
	if roots == nil {
		roots = []string{}
	}
	data, err := json.Marshal(roots)
	return string(data), err
}

type Query struct {
	Limit          int
	Offset         int
	RequestID      string
	Agent          string
	ConversationID string
	State          string
}

func (q Query) Validate() error {
	if q.Limit < 1 || q.Limit > MaxLimit || q.Offset < 0 || q.Offset > MaxOffset {
		return errors.New("history limit must be 1..200 and offset 0..10000")
	}
	switch q.State {
	case "", "pending", "allowed", "denied", "expired", "cancelled", "interrupted":
	default:
		return errors.New("invalid history state")
	}
	if len(q.RequestID) > 128 || len(q.Agent) > 4096 || len(q.ConversationID) > 4096 {
		return errors.New("history filter exceeds size limit")
	}
	return nil
}

type Event struct {
	State      string `json:"state"`
	Reason     string `json:"reason"`
	OccurredAt string `json:"occurred_at"`
}

type Decision struct {
	Permission string `json:"permission"`
	Source     string `json:"source"`
	Reason     string `json:"reason"`
	DecidedAt  string `json:"decided_at"`
}

type Record struct {
	ProtocolVersion  int       `json:"protocol_version"`
	Event            string    `json:"event"`
	RequestID        string    `json:"request_id"`
	Agent            string    `json:"agent"`
	ConversationID   string    `json:"conversation_id"`
	AgentVersion     string    `json:"agent_version"`
	SourceToolCallID *string   `json:"source_tool_call_id"`
	CommandDisplay   string    `json:"command_display"`
	CWD              string    `json:"cwd"`
	WorkspaceRoots   []string  `json:"workspace_roots"`
	ReceivedAt       string    `json:"received_at"`
	Deadline         string    `json:"deadline"`
	FinishedAt       *string   `json:"finished_at"`
	State            string    `json:"state"`
	Reason           string    `json:"reason"`
	ExecutionState   string    `json:"execution_state"`
	Decision         *Decision `json:"decision,omitempty"`
	Events           []Event   `json:"events"`
}

// History is newest first with a stable request-ID tie break. It never returns
// raw command text. Both records and their lifecycle event lists are bounded.
func (s *Store) History(ctx context.Context, q Query) ([]Record, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	predicates := []string{"1=1"}
	args := []interface{}{}
	for _, filter := range []struct{ column, value string }{{"a.request_id", q.RequestID}, {"s.agent", q.Agent}, {"s.conversation_id", q.ConversationID}, {"a.state", q.State}} {
		if filter.value != "" {
			predicates = append(predicates, filter.column+"=?")
			args = append(args, filter.value)
		}
	}
	args = append(args, q.Limit, q.Offset)
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT a.protocol_version,a.event,a.request_id,s.agent,s.conversation_id,a.agent_version,a.source_tool_call_id,a.command_display,a.cwd,a.workspace_roots,a.received_at,a.deadline,a.finished_at,a.state,a.reason,a.execution_state
 FROM actions a JOIN sessions s ON a.session_id=s.id WHERE `+strings.Join(predicates, " AND ")+` ORDER BY a.received_at DESC,a.request_id DESC LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, err
	}
	records := []Record{}
	for rows.Next() {
		var r Record
		var roots string
		if err := rows.Scan(&r.ProtocolVersion, &r.Event, &r.RequestID, &r.Agent, &r.ConversationID, &r.AgentVersion, &r.SourceToolCallID, &r.CommandDisplay, &r.CWD, &roots, &r.ReceivedAt, &r.Deadline, &r.FinishedAt, &r.State, &r.Reason, &r.ExecutionState); err != nil {
			rows.Close()
			return nil, err
		}
		if err := json.Unmarshal([]byte(roots), &r.WorkspaceRoots); err != nil {
			rows.Close()
			return nil, errors.New("invalid stored workspace roots")
		}
		records = append(records, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	// Close the first cursor before making further queries on the one connection.
	for i := range records {
		r := &records[i]
		var d Decision
		err := tx.QueryRowContext(ctx, "SELECT permission,source,reason,decided_at FROM decisions WHERE request_id=?", r.RequestID).Scan(&d.Permission, &d.Source, &d.Reason, &d.DecidedAt)
		if err == nil {
			r.Decision = &d
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		events, err := tx.QueryContext(ctx, "SELECT state,reason,occurred_at FROM action_events WHERE request_id=? ORDER BY id LIMIT 10", r.RequestID)
		if err != nil {
			return nil, err
		}
		r.Events = []Event{}
		for events.Next() {
			var e Event
			if err := events.Scan(&e.State, &e.Reason, &e.OccurredAt); err != nil {
				events.Close()
				return nil, err
			}
			r.Events = append(r.Events, e)
		}
		err = events.Err()
		events.Close()
		if err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return records, nil
}
