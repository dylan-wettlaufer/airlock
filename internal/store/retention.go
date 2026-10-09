package store

import (
	"database/sql"
	"errors"
	"time"
)

const DefaultMaxAge = 30 * 24 * time.Hour
const DefaultMaxRecords = 10000
const MaxRetentionRecords = 1000000

// Retention bounds completed history, independently of pending approvals and
// the permanent, minimal request-ID registry. Age is measured from completion.
type Retention struct {
	MaxAge     time.Duration
	MaxRecords int
}

func DefaultRetention() Retention {
	return Retention{MaxAge: DefaultMaxAge, MaxRecords: DefaultMaxRecords}
}

func (r Retention) Validate() error {
	if r.MaxAge <= 0 {
		return errors.New("history max age must be positive")
	}
	if r.MaxRecords < 1 || r.MaxRecords > MaxRetentionRecords {
		return errors.New("history max records must be 1..1000000")
	}
	return nil
}

// Prune applies both limits atomically. Read-only history never mutates data.
func (s *Store) Prune(at time.Time) error {
	if !s.writer {
		return errors.New("history pruning requires a writer")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.prune(tx, at); err != nil {
		return err
	}
	return tx.Commit()
}

// prune runs inside the admission/terminal transaction so a retention failure
// cannot acknowledge an operation that did not durably complete.
func (s *Store) prune(tx *sql.Tx, at time.Time) error {
	candidates := `SELECT request_id FROM actions WHERE state != 'pending' AND
 (finished_at < ? OR request_id IN (SELECT request_id FROM actions WHERE state != 'pending'
 ORDER BY finished_at DESC,request_id DESC LIMIT -1 OFFSET ?))`
	args := []interface{}{timestamp(at.Add(-s.retention.MaxAge)), s.retention.MaxRecords}
	// Remove dependent records before their action, then orphaned session metadata.
	for _, table := range []string{"decisions", "action_events", "actions"} {
		if _, err := tx.Exec("DELETE FROM "+table+" WHERE request_id IN ("+candidates+")", args...); err != nil {
			return err
		}
	}
	_, err := tx.Exec("DELETE FROM sessions WHERE NOT EXISTS (SELECT 1 FROM actions WHERE actions.session_id=sessions.id)")
	return err
}
