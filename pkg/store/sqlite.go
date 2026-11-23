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

// openDB opens the file in WAL mode and syncs on every commit. Skipping the
// sync could lose an allocation on power loss, and hand a server out twice.
func openDB(path string) (*sql.DB, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // the Store serialises access anyway
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	return db, nil
}

// load reads the whole file into memory. It runs once, from Open.
func (s *Store) load() error {
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key = 'revision'`).Scan(&s.revision)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read revision: %w", err)
	}
	rows, err := s.db.Query(`SELECT kind, namespace, name, data FROM resources`)
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
	s.put(key, obj)
	return nil
}

// persist writes one object and the new revision in a single transaction.
func (s *Store) persist(key Key, revision int64, obj Object) error {
	data, err := json.Marshal(obj)
	if err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	return s.inTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(upsertResource, key.Kind, key.Namespace, key.Name, revision, data); err != nil {
			return err
		}
		_, err := tx.Exec(upsertRevision, revision)
		return err
	})
}

// remove deletes one object and records the new revision.
func (s *Store) remove(key Key, revision int64) error {
	return s.inTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(deleteResource, key.Kind, key.Namespace, key.Name); err != nil {
			return err
		}
		_, err := tx.Exec(upsertRevision, revision)
		return err
	})
}

func (s *Store) inTx(fn func(*sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}
