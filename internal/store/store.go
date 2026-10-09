// Package store persists sanitized authorization history. It never records execution.
package store

import (
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"airlock/internal/protocol"
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrations embed.FS

const SchemaVersion = 2
const MaxLimit = 200
const MaxOffset = 10000

// Store owns one serialized SQL connection and, for writers, a process lock.
type Store struct {
	db        *sql.DB
	lock      *os.File
	retention Retention
	writer    bool
}

func DefaultPath() (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", errors.New("could not determine history directory")
		}
		base = filepath.Join(home, ".local", "state")
	}
	if !filepath.IsAbs(base) {
		return "", errors.New("XDG_STATE_HOME must be absolute")
	}
	return filepath.Join(base, "airlock", "history.sqlite3"), nil
}

func private(path string, directory bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	mode := os.FileMode(0600)
	if directory {
		mode = 0700
	}
	if !ok || stat.Uid != uint32(os.Getuid()) || info.Mode().Perm() != mode ||
		(directory && !info.IsDir()) || (!directory && !info.Mode().IsRegular()) {
		return errors.New("history paths must be owned by this user, not symlinks, with directory mode 0700 and file mode 0600")
	}
	return nil
}

func createFile(path string) (*os.File, error) {
	if err := private(path, false); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_CREAT|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	if err := private(path, false); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// Open creates/migrates a writer and recovers pending records. Only one daemon
// may own a history database, even if daemons use different sockets.
func Open(path string) (*Store, error) { return OpenWithRetention(path, DefaultRetention()) }

// OpenWithRetention uses a validated policy for automatic pruning.
func OpenWithRetention(path string, retention Retention) (*Store, error) {
	if err := retention.Validate(); err != nil {
		return nil, err
	}
	return open(path, true, retention)
}

// Read opens existing history without creating files, migrating, or recovery.
func Read(path string) (*Store, error) { return open(path, false, Retention{}) }

func open(path string, writer bool, retention Retention) (*Store, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("history database path must be absolute")
	}
	path = filepath.Clean(path)
	dir := filepath.Dir(path)
	if writer {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, errors.New("could not create history directory")
		}
	}
	if err := private(dir, true); err != nil {
		return nil, err
	}
	// Resolve ancestor aliases (including macOS /tmp and /var) before SQLite opens.
	canonical, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, err
	}
	path = filepath.Join(canonical, filepath.Base(path))
	s := &Store{writer: writer, retention: retention}
	success := false
	defer func() {
		if !success {
			s.Close()
		}
	}()
	if writer {
		f, err := createFile(path + ".lock")
		if err != nil {
			return nil, err
		}
		s.lock = f
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			return nil, errors.New("history database is already owned by another daemon")
		}
		f, err = createFile(path)
		if err != nil {
			return nil, err
		}
		f.Close()
	}
	if err := private(path, false); err != nil {
		return nil, err
	}
	for _, suffix := range []string{"-journal", "-wal", "-shm"} {
		if err := private(path+suffix, false); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	uri := &url.URL{Scheme: "file", Path: path}
	q := url.Values{}
	if !writer {
		q.Set("mode", "ro")
	}
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "busy_timeout(2000)")
	uri.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, errors.New("could not open history database")
	}
	s.db = db
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		return nil, errors.New("could not read history database")
	}
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return nil, err
	}
	if version > SchemaVersion {
		return nil, errors.New("history schema is newer than this Airlock version")
	}
	if writer {
		// DELETE journaling keeps readers independent and avoids persistent WAL files.
		if _, err := db.Exec("PRAGMA journal_mode=DELETE; PRAGMA synchronous=FULL"); err != nil {
			return nil, err
		}
		for n := version + 1; n <= SchemaVersion; n++ {
			data, err := migrations.ReadFile(fmt.Sprintf("migrations/%03d_history.sql", n))
			if err != nil {
				return nil, err
			}
			tx, err := db.Begin()
			if err != nil {
				return nil, err
			}
			if _, err = tx.Exec(string(data)); err == nil {
				_, err = tx.Exec(fmt.Sprintf("PRAGMA user_version=%d", n))
			}
			if err != nil {
				tx.Rollback()
				return nil, err
			}
			if err := tx.Commit(); err != nil {
				return nil, err
			}
		}
		if err := s.recover(); err != nil {
			return nil, err
		}
		if err := s.Prune(time.Now()); err != nil {
			return nil, err
		}
	} else if version != SchemaVersion {
		return nil, errors.New("history schema requires daemon migration")
	}
	success = true
	return s, nil
}

func (s *Store) Close() error {
	var err error
	if s.db != nil {
		err = s.db.Close()
	}
	if s.lock != nil {
		_ = syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
		if e := s.lock.Close(); err == nil {
			err = e
		}
	}
	return err
}

// Fixed-width UTC strings sort lexicographically, including subsecond times.
func timestamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000000Z") }

var executable = regexp.MustCompile(`^[a-zA-Z0-9_./-]+$`)

// DisplayCommand deliberately drops all arguments and shell syntax instead of
// attempting to enumerate secret names. Assignment-leading commands are hidden.
// Executable names and other metadata may themselves be sensitive.
func DisplayCommand(command string) string {
	fields := strings.Fields(command)
	if len(fields) == 0 || !executable.MatchString(fields[0]) {
		return "[command redacted]"
	}
	name := filepath.Base(fields[0])
	if len(name) > 80 {
		return "[command redacted]"
	}
	if len(fields) == 1 {
		return name
	}
	return name + " [arguments redacted]"
}

func (s *Store) RecordPending(p protocol.Pending) error {
	if err := p.Request.Validate(); err != nil {
		return err
	}
	if !p.Deadline.After(p.ReceivedAt) {
		return errors.New("invalid history deadline")
	}
	r := p.Request
	roots, err := jsonRoots(r.WorkspaceRoots)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Check uniqueness before touching session timestamps. The constraint also
	// protects this invariant if another writer ever bypasses the process lock.
	var existing string
	err = tx.QueryRow("SELECT request_id FROM used_request_ids WHERE request_id=?", r.RequestID).Scan(&existing)
	if err == nil {
		return protocol.Error("duplicate_request", "request ID has already been used")
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := tx.Exec("INSERT INTO used_request_ids(request_id) VALUES(?)", r.RequestID); err != nil {
		return err
	}
	at := timestamp(p.ReceivedAt)
	_, err = tx.Exec(`INSERT INTO sessions(agent,conversation_id,first_seen,last_seen) VALUES(?,?,?,?)
 ON CONFLICT(agent,conversation_id) DO UPDATE SET last_seen=excluded.last_seen`, r.Agent, r.ConversationID, at, at)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO actions(request_id,session_id,protocol_version,agent_version,source_tool_call_id,event,command_display,cwd,workspace_roots,received_at,deadline,state,reason)
 VALUES(?,(SELECT id FROM sessions WHERE agent=? AND conversation_id=?),?,?,?,?,?,?,?,?,?,'pending','request received')`,
		r.RequestID, r.Agent, r.ConversationID, r.ProtocolVersion, r.AgentVersion, r.SourceToolCallID, r.Event, DisplayCommand(r.Command), r.CWD, roots, at, timestamp(p.Deadline))
	if err != nil {
		return err
	}
	if _, err := tx.Exec("INSERT INTO action_events(request_id,state,reason,occurred_at) VALUES(?,'pending','request received',?)", r.RequestID, at); err != nil {
		return err
	}
	if err := s.prune(tx, p.ReceivedAt); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RecordResult(r protocol.Result, at time.Time) error {
	if err := r.Validate(r.RequestID); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec("UPDATE actions SET state=?,reason=?,finished_at=? WHERE request_id=? AND state='pending'", r.State, r.Reason, timestamp(at), r.RequestID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return errors.New("history request is no longer pending")
	}
	if r.State == "allowed" || r.State == "denied" {
		if _, err := tx.Exec("INSERT INTO decisions(request_id,permission,source,reason,decided_at) VALUES(?,?,'manual',?,?)", r.RequestID, r.Permission, r.Reason, timestamp(at)); err != nil {
			return err
		}
	}
	if _, err := tx.Exec("INSERT INTO action_events(request_id,state,reason,occurred_at) VALUES(?,?,?,?)", r.RequestID, r.State, r.Reason, timestamp(at)); err != nil {
		return err
	}
	if err := s.prune(tx, at); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) recover() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	at := timestamp(time.Now())
	reason := "daemon restarted before a terminal result was recorded"
	if _, err := tx.Exec("INSERT INTO action_events(request_id,state,reason,occurred_at) SELECT request_id,'interrupted',?,? FROM actions WHERE state='pending'", reason, at); err != nil {
		return err
	}
	if _, err := tx.Exec("UPDATE actions SET state='interrupted',reason=?,finished_at=? WHERE state='pending'", reason, at); err != nil {
		return err
	}
	return tx.Commit()
}
