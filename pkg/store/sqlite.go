package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	_ "modernc.org/sqlite" // pure Go driver, registered as "sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS resources (
  kind      TEXT    NOT NULL,
  namespace TEXT    NOT NULL,
  name      TEXT    NOT NULL,
  version   INTEGER NOT NULL,
  data      BLOB    NOT NULL,
  PRIMARY KEY (kind, namespace, name)
);
CREATE TABLE IF NOT EXISTS meta (
  key   TEXT PRIMARY KEY,
  value BLOB NOT NULL
);`

const (
	upsertResource = `
INSERT INTO resources (kind, namespace, name, version, data) VALUES (?, ?, ?, ?, ?)
ON CONFLICT (kind, namespace, name) DO UPDATE SET version = excluded.version, data = excluded.data`
	deleteResource = `DELETE FROM resources WHERE kind = ? AND namespace = ? AND name = ?`
	upsertRevision = `
INSERT INTO meta (key, value) VALUES ('revision', ?)
ON CONFLICT (key) DO UPDATE SET value = excluded.value`
)

// file is the SQLite file, with the three statements that write to it
// compiled once: they run for every change.
type file struct {
	db                         *sql.DB
	upsert, remove, toRevision *sql.Stmt
}

// openFile opens the file in WAL mode and syncs on every commit. Skipping
// the sync could lose an allocation on power loss, and hand a server out
// twice.
func openFile(path string) (file, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return file{}, err
	}
	db.SetMaxOpenConns(1) // one writer at a time is all SQLite allows anyway
	f := file{db: db}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return file{}, fmt.Errorf("create schema: %w", err)
	}
	for text, compiled := range map[string]**sql.Stmt{upsertResource: &f.upsert, deleteResource: &f.remove, upsertRevision: &f.toRevision} {
		if *compiled, err = db.Prepare(text); err != nil {
			_ = db.Close()
			return file{}, err
		}
	}
	return f, nil
}

// load reads the whole file into memory. It runs once, from Open.
func (s *Store) load() error {
	err := s.file.db.QueryRow(`SELECT value FROM meta WHERE key = 'revision'`).Scan(&s.revision)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read revision: %w", err)
	}
	rows, err := s.file.db.Query(`SELECT kind, namespace, name, data FROM resources`)
	if err != nil {
		return fmt.Errorf("read resources: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if err := s.loadRow(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (s *Store) loadRow(rows *sql.Rows) error {
	var key Key
	var data []byte
	if err := rows.Scan(&key.Kind, &key.Namespace, &key.Name, &data); err != nil {
		return err
	}
	factory, ok := s.factories[key.Kind]
	if !ok {
		return fmt.Errorf("%s: unknown kind in database", key)
	}
	obj := factory()
	if err := json.Unmarshal(data, obj); err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	s.hold(key, obj)
	return nil
}

// asStored returns obj the way the file holds it, and the way a later Open
// would read that back.
func (s *Store) asStored(key Key, obj Object) (stored Object, data []byte, err error) {
	if data, err = json.Marshal(obj); err != nil {
		return nil, nil, fmt.Errorf("%s: %w", key, err)
	}
	stored = s.factories[key.Kind]()
	if err := json.Unmarshal(data, stored); err != nil {
		return nil, nil, fmt.Errorf("%s: %w", key, err)
	}
	return stored, data, nil
}

// write puts changes into the file in one transaction, with the revision
// they lead to: all of them, or none.
func (f file) write(changes []change) error {
	tx, err := f.db.Begin()
	if err != nil {
		return err
	}
	if err := f.writeIn(tx, changes); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (f file) writeIn(tx *sql.Tx, changes []change) error {
	for _, c := range lastOfEachKey(changes) {
		key, err := c.event.Key, error(nil)
		if c.data == nil {
			_, err = tx.Stmt(f.remove).Exec(key.Kind, key.Namespace, key.Name)
		} else {
			_, err = tx.Stmt(f.upsert).Exec(key.Kind, key.Namespace, key.Name, c.revision, c.data)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
	}
	_, err := tx.Stmt(f.toRevision).Exec(changes[len(changes)-1].revision)
	return err
}

// lastOfEachKey leaves out the changes that a later one overwrites. Nobody
// can open the file between two changes of one transaction, so only the
// last one to a key is ever read.
func lastOfEachKey(changes []change) []change {
	last := make(map[Key]int, len(changes))
	for i, c := range changes {
		last[c.event.Key] = i
	}
	if len(last) == len(changes) {
		return changes
	}
	kept := make([]change, 0, len(last))
	for i, c := range changes {
		if last[c.event.Key] == i {
			kept = append(kept, c)
		}
	}
	return kept
}
