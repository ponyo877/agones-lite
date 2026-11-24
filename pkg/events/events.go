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
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sync"
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
	db        *sql.DB
	insertOne *sql.Stmt // compiled once: it runs for every event
	now       func() time.Time

	mu      sync.Mutex
	unsaved []Event // recorded while Run is running, and not written yet
	behind  bool
	wake    chan struct{}
}

// Open returns a Recorder on the SQLite file at path, creating its table
// when missing. The resources live in the same file, in other tables.
//
// An event is not worth a sync of the disk: after a crash the last few may
// be missing, and nothing depends on them.
func Open(path string) (*Recorder, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create events table: %w", err)
	}
	insertOne, err := db.Prepare(`INSERT INTO events (time, kind, namespace, name, type, reason, message) VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Recorder{db: db, insertOne: insertOne, now: time.Now, wake: make(chan struct{}, 1)}, nil
}

// Close writes what is not written yet and releases the SQLite file.
func (r *Recorder) Close() error {
	if r == nil {
		return nil
	}
	r.flush()
	return r.db.Close()
}

// Record notes that something happened to a resource. A failure to write is
// logged and otherwise ignored: losing a history line must not stop a
// controller.
func (r *Recorder) Record(kind, namespace, name string, t Type, reason, format string, args ...any) {
	if r == nil {
		return
	}
	e := Event{Time: r.now(), Kind: kind, Namespace: namespace, Name: name, Type: t, Reason: reason, Message: fmt.Sprintf(format, args...)}
	if !r.leaveToRun(e) {
		r.write([]Event{e})
	}
}

// Run writes events in the background until ctx is done. While it runs,
// Record only puts the event aside, and a controller does not wait for the
// file every time something happens: Run writes whatever has gathered in
// one go. The store writes to the same file the same way (store.Run).
func (r *Recorder) Run(ctx context.Context) {
	if r == nil {
		return
	}
	r.setBehind(true)
	defer r.flush()
	defer r.setBehind(false)
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.wake:
			r.flush()
		}
	}
}

func (r *Recorder) setBehind(behind bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.behind = behind
}

// leaveToRun puts an event aside for Run, and reports false when Run is
// not there to take it.
func (r *Recorder) leaveToRun(e Event) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.behind {
		return false
	}
	r.unsaved = append(r.unsaved, e)
	select {
	case r.wake <- struct{}{}:
	default: // Run has been told already
	}
	return true
}

// flush writes the events that were put aside.
func (r *Recorder) flush() {
	r.mu.Lock()
	gathered := r.unsaved
	r.unsaved = nil
	r.mu.Unlock()
	r.write(gathered)
}

// write puts events into the file, all in one transaction.
func (r *Recorder) write(batch []Event) {
	if len(batch) == 0 {
		return
	}
	if err := r.insert(batch); err != nil {
		slog.Warn("cannot record events", "events", len(batch), "first", batch[0].Message, "error", err)
	}
}

func (r *Recorder) insert(batch []Event) error {
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	for _, e := range batch {
		_, err := tx.Stmt(r.insertOne).Exec(e.Time.UnixMilli(), e.Kind, e.Namespace, e.Name, string(e.Type), e.Reason, e.Message)
		if err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// For returns the events of one resource, newest first.
func (r *Recorder) For(kind, namespace, name string) ([]Event, error) {
	if r == nil {
		return nil, nil
	}
	r.flush() // what was put aside belongs to the answer
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
