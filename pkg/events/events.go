// Package events keeps a short history of what happened to each resource:
// why a game server is waiting for a node, why it became Unhealthy, what a
// Fleet's rollout did. It is the first place to look when something is off.
//
// Events are written to the same SQLite file as the resources, in their own
// table, and are thrown away after a while. They are a log for people; no
// controller ever reads them.
//
// Upstream counterpart: Kubernetes Events, written through an EventRecorder.
package events

import (
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	_ "modernc.org/sqlite" // pure Go driver, registered as "sqlite"
)

// Type sorts events into the expected and the worrying.
type Type string

// The two types, named as in Kubernetes.
const (
	Normal  Type = "Normal"
	Warning Type = "Warning"
)

// Event is one thing that happened to one resource.
type Event struct {
	Time      time.Time `json:"time"`
	Kind      string    `json:"kind"`
	Namespace string    `json:"namespace"`
	Name      string    `json:"name"`
	Type      Type      `json:"type"`
	Reason    string    `json:"reason"`
	Message   string    `json:"message"`
}

const schema = `
CREATE TABLE IF NOT EXISTS events (
  id        INTEGER PRIMARY KEY AUTOINCREMENT,
  time      INTEGER NOT NULL,
  kind      TEXT NOT NULL,
  namespace TEXT NOT NULL,
  name      TEXT NOT NULL,
  type      TEXT NOT NULL,
  reason    TEXT NOT NULL,
  message   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS events_by_resource ON events (kind, namespace, name, id);`

// Recorder writes and reads events. A nil *Recorder is valid and drops
// everything, which is convenient in tests.
type Recorder struct {
	db  *sql.DB
	now func() time.Time
}

// Open returns a Recorder on the SQLite file at path, creating its table
// when missing. The resources live in the same file, in other tables.
func Open(path string) (*Recorder, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create events table: %w", err)
	}
	return &Recorder{db: db, now: time.Now}, nil
}

// Close releases the SQLite file.
func (r *Recorder) Close() error {
	if r == nil {
		return nil
	}
	return r.db.Close()
}

// Record notes that something happened to a resource. A failure to write is
// logged and otherwise ignored: losing a history line must not stop a
// controller.
func (r *Recorder) Record(kind, namespace, name string, t Type, reason, format string, args ...any) {
	if r == nil {
		return
	}
	message := fmt.Sprintf(format, args...)
	_, err := r.db.Exec(
		`INSERT INTO events (time, kind, namespace, name, type, reason, message) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		r.now().UnixMilli(), kind, namespace, name, string(t), reason, message)
	if err != nil {
		slog.Warn("cannot record event", "reason", reason, "message", message, "error", err)
	}
}

// For returns the events of one resource, newest first.
func (r *Recorder) For(kind, namespace, name string) ([]Event, error) {
	if r == nil {
		return nil, nil
	}
	rows, err := r.db.Query(
		`SELECT time, kind, namespace, name, type, reason, message FROM events
		 WHERE kind = ? AND namespace = ? AND name = ? ORDER BY id DESC`, kind, namespace, name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Event
	for rows.Next() {
		var e Event
		var millis int64
		if err := rows.Scan(&millis, &e.Kind, &e.Namespace, &e.Name, &e.Type, &e.Reason, &e.Message); err != nil {
			return nil, err
		}
		e.Time = time.UnixMilli(millis).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

// Prune deletes events older than keep.
func (r *Recorder) Prune(keep time.Duration) error {
	if r == nil {
		return nil
	}
	_, err := r.db.Exec(`DELETE FROM events WHERE time < ?`, r.now().Add(-keep).UnixMilli())
	return err
}
