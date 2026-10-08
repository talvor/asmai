// SPDX-License-Identifier: Apache-2.0

// Package store is the factory's durable record: the current state the daemon
// keeps, and the append-only journal of everything that happened, in one
// SQLite database. Only the daemon opens it. Every change to the current
// state is written in the same transaction as the journal entry that records
// it, and the journal is never trimmed: the database itself refuses to update
// or delete an entry.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	// ncruces/go-sqlite3 runs SQLite without CGo. It stays at v0.32, the
	// last release that carries its SQLite build in its own MIT-licensed
	// module: later releases move it to a module under MIT-0, which is not
	// on the license allow-list.
	_ "github.com/ncruces/go-sqlite3/driver"
	_ "github.com/ncruces/go-sqlite3/embed"
)

// schemaVersion is the version of the schema this asmai writes, kept in the
// database's user_version.
const schemaVersion = 1

const schema = `
CREATE TABLE factory (
	id         INTEGER PRIMARY KEY CHECK (id = 1),
	state      TEXT NOT NULL,
	version    TEXT NOT NULL,
	pid        INTEGER NOT NULL,
	started_at TEXT NOT NULL,
	stopped_at TEXT
);
CREATE TABLE journal (
	id   INTEGER PRIMARY KEY AUTOINCREMENT,
	at   TEXT NOT NULL,
	kind TEXT NOT NULL,
	data TEXT NOT NULL
);
CREATE TRIGGER journal_is_append_only_update BEFORE UPDATE ON journal
BEGIN SELECT RAISE(ABORT, 'the journal is append-only'); END;
CREATE TRIGGER journal_is_append_only_delete BEFORE DELETE ON journal
BEGIN SELECT RAISE(ABORT, 'the journal is append-only'); END;
`

// The kinds of journal entry.
const (
	KindDaemonStarted = "daemon.started"
	KindDaemonStopped = "daemon.stopped"
)

// The states the factory can be left in.
const (
	// StateNew is a store no daemon has started on.
	StateNew = "new"
	// StateRunning is a store a daemon has started on and not stopped. Found
	// at a start, it means the last daemon did not stop cleanly.
	StateRunning = "running"
	// StateStopped is a store whose last daemon stopped cleanly.
	StateStopped = "stopped"
)

// Store is an open store.
type Store struct {
	db *sql.DB
}

// Factory is the factory's current state, as the store holds it.
type Factory struct {
	State     string
	Version   string
	PID       int
	StartedAt time.Time
	StoppedAt time.Time
}

// Entry is one journal entry.
type Entry struct {
	ID   int64           `json:"id"`
	At   time.Time       `json:"at"`
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
}

// Open opens the store at path, creating it readable only by the user if it
// is missing. It refuses a file that is not an AsmAI store, and a store a
// later asmai wrote.
func Open(path string) (*Store, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("the store %s cannot be used: %w", path, err)
	}
	f.Close()
	// One connection makes the daemon's writes one at a time; FULL
	// synchronous mode makes each committed transaction durable.
	db, err := sql.Open("sqlite3", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)")
	if err != nil {
		return nil, fmt.Errorf("the store %s cannot be used: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("the store %s cannot be used: %w", path, err)
	}
	return s, nil
}

func (s *Store) migrate() error {
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	switch {
	case version == schemaVersion:
		return nil
	case version > schemaVersion:
		return fmt.Errorf("it was written by a later asmai (store schema %d; this asmai knows %d)", version, schemaVersion)
	}
	var objects int
	if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_schema`).Scan(&objects); err != nil {
		return err
	}
	if objects != 0 {
		return errors.New("it is a database, but not an AsmAI store")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(schema); err != nil {
		return err
	}
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

// Close closes the store. Everything committed is already durable; closing
// also folds SQLite's write-ahead log back into the store file.
func (s *Store) Close() error {
	return s.db.Close()
}

// Factory returns the factory's current state.
func (s *Store) Factory() (Factory, error) {
	return factory(s.db)
}

type queryer interface {
	QueryRow(query string, args ...any) *sql.Row
}

func factory(q queryer) (Factory, error) {
	var f Factory
	var startedAt string
	var stoppedAt sql.NullString
	err := q.QueryRow(`SELECT state, version, pid, started_at, stopped_at FROM factory WHERE id = 1`).
		Scan(&f.State, &f.Version, &f.PID, &startedAt, &stoppedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Factory{State: StateNew}, nil
	}
	if err != nil {
		return Factory{}, err
	}
	if f.StartedAt, err = time.Parse(time.RFC3339Nano, startedAt); err != nil {
		return Factory{}, err
	}
	if stoppedAt.Valid {
		if f.StoppedAt, err = time.Parse(time.RFC3339Nano, stoppedAt.String); err != nil {
			return Factory{}, err
		}
	}
	return f, nil
}

// Started records that a daemon of version, with process ID pid, started at
// at, and returns the state the store was left in. A previous state of
// StateRunning means the last daemon did not stop cleanly.
func (s *Store) Started(version string, pid int, at time.Time) (previous Factory, err error) {
	err = s.change(at, func(tx *sql.Tx) (string, any, error) {
		if previous, err = factory(tx); err != nil {
			return "", nil, err
		}
		_, err := tx.Exec(`INSERT INTO factory (id, state, version, pid, started_at, stopped_at) VALUES (1, ?, ?, ?, ?, NULL)
			ON CONFLICT (id) DO UPDATE SET state = excluded.state, version = excluded.version, pid = excluded.pid,
				started_at = excluded.started_at, stopped_at = NULL`,
			StateRunning, version, pid, timestamp(at))
		data := map[string]any{"version": version, "pid": pid, "previous": previous.State}
		return KindDaemonStarted, data, err
	})
	return previous, err
}

// Stopped records that the running daemon stopped at at, asked to by by.
func (s *Store) Stopped(by string, at time.Time) error {
	return s.change(at, func(tx *sql.Tx) (string, any, error) {
		f, err := factory(tx)
		if err != nil {
			return "", nil, err
		}
		if f.State != StateRunning {
			return "", nil, fmt.Errorf("no daemon is recorded as running (the factory is %s)", f.State)
		}
		_, err = tx.Exec(`UPDATE factory SET state = ?, stopped_at = ? WHERE id = 1`, StateStopped, timestamp(at))
		data := map[string]any{"version": f.Version, "pid": f.PID, "by": by}
		return KindDaemonStopped, data, err
	})
}

// change makes a change to the current state at at and journals it in one
// transaction. apply makes the change and returns the journal entry's kind
// and data.
func (s *Store) change(at time.Time, apply func(tx *sql.Tx) (kind string, data any, err error)) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	kind, data, err := apply(tx)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO journal (at, kind, data) VALUES (?, ?, ?)`, timestamp(at), kind, string(encoded)); err != nil {
		return err
	}
	return tx.Commit()
}

// Journal returns every journal entry, oldest first.
func (s *Store) Journal() ([]Entry, error) {
	rows, err := s.db.Query(`SELECT id, at, kind, data FROM journal ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := []Entry{}
	for rows.Next() {
		var e Entry
		var at, data string
		if err := rows.Scan(&e.ID, &at, &e.Kind, &data); err != nil {
			return nil, err
		}
		if e.At, err = time.Parse(time.RFC3339Nano, at); err != nil {
			return nil, err
		}
		e.Data = json.RawMessage(data)
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

func timestamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}
