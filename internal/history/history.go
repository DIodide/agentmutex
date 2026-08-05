// Package history is the append-only audit log for agentmutex: every lease
// state change (acquired, renewed, released, force-released, expired,
// reclaimed) is recorded in a SQLite database at <root>/history.db.
//
// Recording is strictly BEST-EFFORT: the lock protocol's correctness never
// depends on the history database. Callers treat any error here as a warning
// at most — a full disk or a corrupt history.db must never block a deploy
// from acquiring or releasing its lease.
//
// Concurrency: many CLI processes append concurrently. WAL mode plus a busy
// timeout makes multi-process appends safe; writes happen outside the per-key
// flock guard so they never extend a lock critical section.
package history

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: keeps CGO_ENABLED=0 cross-builds working
)

// Event types.
const (
	EventAcquired      = "acquired"
	EventRenewed       = "renewed"
	EventReleased      = "released"
	EventForceReleased = "force-released"
	EventExpired       = "expired" // lease ended by TTL (displaced or pruned)
	EventReclaimed     = "reclaimed"
)

// Event is one row of the lock changelog. LeaseID correlates all events of a
// single lease (it is public — never the lease token).
type Event struct {
	ID          int64     `json:"id"`
	TS          time.Time `json:"ts"`
	Key         string    `json:"key"`
	Event       string    `json:"event"`
	LeaseID     string    `json:"lease_id,omitempty"`
	Agent       string    `json:"agent,omitempty"`
	PID         int       `json:"pid,omitempty"`
	Host        string    `json:"host,omitempty"`
	Reason      string    `json:"reason,omitempty"`
	TTLSeconds  float64   `json:"ttl_seconds,omitempty"`
	HeldSeconds float64   `json:"held_seconds,omitempty"`
	Detail      string    `json:"detail,omitempty"`
}

// Log is a handle on the history database.
type Log struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS events (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	ts           TEXT    NOT NULL,
	key          TEXT    NOT NULL,
	event        TEXT    NOT NULL,
	lease_id     TEXT    NOT NULL DEFAULT '',
	agent        TEXT    NOT NULL DEFAULT '',
	pid          INTEGER NOT NULL DEFAULT 0,
	host         TEXT    NOT NULL DEFAULT '',
	reason       TEXT    NOT NULL DEFAULT '',
	ttl_seconds  REAL    NOT NULL DEFAULT 0,
	held_seconds REAL    NOT NULL DEFAULT 0,
	detail       TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_events_key_id ON events(key, id);
`

// Open opens (creating if needed) the history database under root. The file
// is created 0600-equivalent via SQLite defaults inside the 0700 state root.
func Open(root string) (*Log, error) {
	path := filepath.Join(root, "history.db")
	// WAL: concurrent multi-process appends without writer starvation.
	// busy_timeout: contending writers wait instead of failing with BUSY.
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// A CLI process is short-lived and single-purpose: one connection.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Log{db: db}, nil
}

func (l *Log) Close() error { return l.db.Close() }

// Record appends one event. TS defaults to now.
func (l *Log) Record(e Event) error {
	if e.TS.IsZero() {
		e.TS = time.Now()
	}
	_, err := l.db.Exec(
		`INSERT INTO events (ts, key, event, lease_id, agent, pid, host, reason, ttl_seconds, held_seconds, detail)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.TS.UTC().Format(time.RFC3339Nano), e.Key, e.Event, e.LeaseID, e.Agent,
		e.PID, e.Host, e.Reason, e.TTLSeconds, e.HeldSeconds, e.Detail,
	)
	return err
}

// QueryOpts filters Query.
type QueryOpts struct {
	Key           string    // "" = all keys
	Since         time.Time // zero = no lower bound
	IncludeRenews bool
	Limit         int // <= 0 means a sane default
}

// Query returns matching events, newest first.
func (l *Log) Query(o QueryOpts) ([]Event, error) {
	if o.Limit <= 0 {
		o.Limit = 50
	}
	var conds []string
	var args []any
	if o.Key != "" {
		conds = append(conds, "key = ?")
		args = append(args, o.Key)
	}
	if !o.Since.IsZero() {
		conds = append(conds, "ts >= ?")
		args = append(args, o.Since.UTC().Format(time.RFC3339Nano))
	}
	if !o.IncludeRenews {
		conds = append(conds, "event != ?")
		args = append(args, EventRenewed)
	}
	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}
	args = append(args, o.Limit)
	rows, err := l.db.Query(
		`SELECT id, ts, key, event, lease_id, agent, pid, host, reason, ttl_seconds, held_seconds, detail
		 FROM events `+where+` ORDER BY id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var ts string
		if err := rows.Scan(&e.ID, &ts, &e.Key, &e.Event, &e.LeaseID, &e.Agent,
			&e.PID, &e.Host, &e.Reason, &e.TTLSeconds, &e.HeldSeconds, &e.Detail); err != nil {
			return nil, err
		}
		e.TS, _ = time.Parse(time.RFC3339Nano, ts)
		out = append(out, e)
	}
	return out, rows.Err()
}
