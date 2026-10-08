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

// migrations takes a store from each schema version to the next: the first
// creates a new store's schema 1. The version a store is at is kept in the
// database's user_version.
var migrations = []string{
	`
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
`,
	`
CREATE TABLE providers (
	name         TEXT NOT NULL,
	version      TEXT NOT NULL,
	path         TEXT NOT NULL,
	sha256       TEXT NOT NULL,
	installed_at TEXT NOT NULL,
	PRIMARY KEY (name, version)
);
`,
	`
CREATE TABLE agents (
	agent      TEXT PRIMARY KEY,
	role       TEXT NOT NULL,
	state      TEXT NOT NULL,
	generation INTEGER NOT NULL,
	provider   TEXT NOT NULL,
	version    TEXT NOT NULL,
	model      TEXT NOT NULL,
	pid        INTEGER NOT NULL,
	started_at TEXT NOT NULL,
	ended_at   TEXT,
	exit       TEXT NOT NULL DEFAULT ''
);
`,
	`
CREATE TABLE repositories (
	name           TEXT PRIMARY KEY,
	location       TEXT NOT NULL,
	origin         TEXT NOT NULL,
	default_branch TEXT NOT NULL,
	clone          TEXT NOT NULL,
	added_at       TEXT NOT NULL
);
CREATE TABLE jobs (
	number     INTEGER PRIMARY KEY AUTOINCREMENT,
	repository TEXT,
	state      TEXT NOT NULL
);
`,
	`
ALTER TABLE jobs ADD COLUMN role TEXT NOT NULL DEFAULT 'coordination';
ALTER TABLE jobs ADD COLUMN witness INTEGER NOT NULL DEFAULT 0;
ALTER TABLE jobs ADD COLUMN words TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN reading TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN mandate TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN criteria TEXT NOT NULL DEFAULT '[]';
ALTER TABLE jobs ADD COLUMN opened_at TEXT NOT NULL DEFAULT '';
`,
}

// schemaVersion is the version of the schema this asmai writes.
var schemaVersion = len(migrations)

// The kinds of journal entry.
const (
	KindDaemonStarted     = "daemon.started"
	KindDaemonStopped     = "daemon.stopped"
	KindProviderInstalled = "provider.installed"
	// KindSessionStarted records an agent session's start: the provider
	// version, the settings passed on its command line and its generation.
	KindSessionStarted = "session.started"
	KindSessionEnded   = "session.ended"
	// KindObservation records a lifecycle event a session's hooks reported.
	KindObservation = "observation"
	// KindMessageWitnessed records a witnessed message: what the user
	// submitted from their own keyboard in an agent's terminal, word for
	// word, citing the prompt-submission observation that confirmed it.
	KindMessageWitnessed = "message.witnessed"
	// KindConversationEntered and KindConversationLeft record the user
	// entering and leaving the conversation with Coordination.
	KindConversationEntered = "conversation.entered"
	KindConversationLeft    = "conversation.left"
	// KindRepositoryAdded and KindRepositoryRemoved record the user
	// registering and removing a repository.
	KindRepositoryAdded   = "repository.added"
	KindRepositoryRemoved = "repository.removed"
	KindJobOpened         = "job.opened"
)

// The states an agent can be in.
const (
	AgentRunning = "running"
	AgentStopped = "stopped"
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

// ProviderInstall is one pinned provider version AsmAI installed, and where
// it keeps it.
type ProviderInstall struct {
	Name        string    `json:"name"`
	Version     string    `json:"version"`
	Path        string    `json:"path"`
	SHA256      string    `json:"sha256"`
	InstalledAt time.Time `json:"installed_at"`
}

// SessionStart is an agent session the daemon started: the provider it runs
// and everything passed on its command line. It never holds the session's
// environment.
type SessionStart struct {
	Agent string `json:"agent"`
	Role  string `json:"role"`
	// Provider and Version are the provider install the session runs.
	Provider string `json:"provider"`
	Version  string `json:"version"`
	Model    string `json:"model"`
	// Executable and Args are the session's command line, with the settings
	// passed on it.
	Executable string    `json:"executable"`
	Args       []string  `json:"args"`
	Dir        string    `json:"dir"`
	PID        int       `json:"pid"`
	At         time.Time `json:"-"`
}

// Agent is an agent's current state, as the store holds it.
type Agent struct {
	Agent string `json:"agent"`
	Role  string `json:"role"`
	State string `json:"state"`
	// Generation counts the agent's sessions: each start is the next.
	Generation int       `json:"generation"`
	Provider   string    `json:"provider"`
	Version    string    `json:"version"`
	Model      string    `json:"model"`
	PID        int       `json:"pid"`
	StartedAt  time.Time `json:"started_at"`
	EndedAt    time.Time `json:"ended_at,omitzero"`
	// Exit is how the last session ended.
	Exit string `json:"exit,omitempty"`
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
	if version == 0 {
		var objects int
		if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_schema`).Scan(&objects); err != nil {
			return err
		}
		if objects != 0 {
			return errors.New("it is a database, but not an AsmAI store")
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, migration := range migrations[version:] {
		if _, err := tx.Exec(migration); err != nil {
			return err
		}
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

// ProviderInstalled records that AsmAI installed p, and journals it. Recording
// an install the store already holds, at the same path with the same SHA-256,
// changes nothing and journals nothing, and returns false.
func (s *Store) ProviderInstalled(p ProviderInstall) (recorded bool, err error) {
	err = s.change(p.InstalledAt, func(tx *sql.Tx) (string, any, error) {
		var path, digest string
		err := tx.QueryRow(`SELECT path, sha256 FROM providers WHERE name = ? AND version = ?`, p.Name, p.Version).Scan(&path, &digest)
		if err == nil && path == p.Path && digest == p.SHA256 {
			return "", nil, nil
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", nil, err
		}
		_, err = tx.Exec(`INSERT INTO providers (name, version, path, sha256, installed_at) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (name, version) DO UPDATE SET path = excluded.path, sha256 = excluded.sha256, installed_at = excluded.installed_at`,
			p.Name, p.Version, p.Path, p.SHA256, timestamp(p.InstalledAt))
		recorded = err == nil
		data := map[string]any{"name": p.Name, "version": p.Version, "path": p.Path, "sha256": p.SHA256}
		return KindProviderInstalled, data, err
	})
	return recorded && err == nil, err
}

// Providers returns the provider installs the store records, by name and
// version.
func (s *Store) Providers() ([]ProviderInstall, error) {
	rows, err := s.db.Query(`SELECT name, version, path, sha256, installed_at FROM providers ORDER BY name, version`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	installs := []ProviderInstall{}
	for rows.Next() {
		var p ProviderInstall
		var at string
		if err := rows.Scan(&p.Name, &p.Version, &p.Path, &p.SHA256, &at); err != nil {
			return nil, err
		}
		if p.InstalledAt, err = time.Parse(time.RFC3339Nano, at); err != nil {
			return nil, err
		}
		installs = append(installs, p)
	}
	return installs, rows.Err()
}

// SessionStarted records that the daemon started the agent session s, as
// the agent's next generation, and journals it with the generation.
func (s *Store) SessionStarted(start SessionStart) (generation int, err error) {
	err = s.change(start.At, func(tx *sql.Tx) (string, any, error) {
		err := tx.QueryRow(`SELECT generation FROM agents WHERE agent = ?`, start.Agent).Scan(&generation)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", nil, err
		}
		generation++
		_, err = tx.Exec(`INSERT INTO agents (agent, role, state, generation, provider, version, model, pid, started_at, ended_at, exit)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, '')
			ON CONFLICT (agent) DO UPDATE SET role = excluded.role, state = excluded.state, generation = excluded.generation,
				provider = excluded.provider, version = excluded.version, model = excluded.model, pid = excluded.pid,
				started_at = excluded.started_at, ended_at = NULL, exit = ''`,
			start.Agent, start.Role, AgentRunning, generation, start.Provider, start.Version, start.Model, start.PID, timestamp(start.At))
		data := struct {
			SessionStart
			Generation int `json:"generation"`
		}{start, generation}
		return KindSessionStarted, data, err
	})
	return generation, err
}

// SessionEnded records that generation of agent's session ended at at: exit
// is how its process ended, and by what ended it.
func (s *Store) SessionEnded(agent string, generation int, exit, by string, at time.Time) error {
	return s.change(at, func(tx *sql.Tx) (string, any, error) {
		var role string
		var current int
		if err := tx.QueryRow(`SELECT role, generation FROM agents WHERE agent = ?`, agent).Scan(&role, &current); err != nil {
			return "", nil, fmt.Errorf("no session of %s is recorded: %w", agent, err)
		}
		var err error
		if current == generation {
			_, err = tx.Exec(`UPDATE agents SET state = ?, ended_at = ?, exit = ? WHERE agent = ?`, AgentStopped, timestamp(at), exit, agent)
		}
		data := map[string]any{"agent": agent, "role": role, "generation": generation, "exit": exit, "by": by}
		return KindSessionEnded, data, err
	})
}

// EndAbandonedSessions records every agent session still recorded as running
// as ended: at a start, they are the sessions of a daemon that did not stop
// cleanly, which ended with it.
func (s *Store) EndAbandonedSessions(at time.Time) error {
	agents, err := s.Agents()
	if err != nil {
		return err
	}
	for _, a := range agents {
		if a.State != AgentRunning {
			continue
		}
		if err := s.SessionEnded(a.Agent, a.Generation, "unknown", "the previous daemon, which did not stop cleanly", at); err != nil {
			return err
		}
	}
	return nil
}

// Observed journals an observation of generation of agent's session: event
// is the lifecycle event its hooks reported, and payload what the provider
// passed the hook, kept as it is.
func (s *Store) Observed(agent, role string, generation int, event string, payload json.RawMessage, at time.Time) error {
	return s.change(at, func(tx *sql.Tx) (string, any, error) {
		data := map[string]any{"agent": agent, "role": role, "generation": generation, "event": event, "payload": payload}
		return KindObservation, data, nil
	})
}

// Witnessed is a message the user submitted from their own keyboard in an
// agent's terminal: Text is their words, as the provider reported them
// submitted, and Terminal the user's terminal they typed them in.
type Witnessed struct {
	Agent      string `json:"agent"`
	Role       string `json:"role"`
	Generation int    `json:"generation"`
	Terminal   string `json:"terminal"`
	Text       string `json:"text"`
}

// ObservedWitnessed journals a prompt-submission observation of
// w.Generation of w.Agent's session that was the user's own submission, and
// in the same transaction the witnessed message w, citing it. payload is what
// the provider passed the hook, kept as it is. It returns the two entries'
// IDs; the witnessed message's is the ID it is cited by.
func (s *Store) ObservedWitnessed(w Witnessed, event string, payload json.RawMessage, at time.Time) (observation, message int64, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	observed := map[string]any{"agent": w.Agent, "role": w.Role, "generation": w.Generation, "event": event, "payload": payload}
	if observation, err = appendEntry(tx, at, KindObservation, observed); err != nil {
		return 0, 0, err
	}
	data := struct {
		Witnessed
		Observation int64 `json:"observation"`
	}{w, observation}
	if message, err = appendEntry(tx, at, KindMessageWitnessed, data); err != nil {
		return 0, 0, err
	}
	return observation, message, tx.Commit()
}

// ConversationEntered journals that the user entered the conversation with
// generation of agent's session, from their terminal.
func (s *Store) ConversationEntered(agent, role string, generation int, terminal string, at time.Time) error {
	return s.change(at, func(tx *sql.Tx) (string, any, error) {
		data := map[string]any{"agent": agent, "role": role, "generation": generation, "terminal": terminal}
		return KindConversationEntered, data, nil
	})
}

// ConversationLeft journals that the user left the conversation with
// generation of agent's session, from their terminal: how is how they left,
// and input who owns the session's input now.
func (s *Store) ConversationLeft(agent, role string, generation int, terminal, how, input string, at time.Time) error {
	return s.change(at, func(tx *sql.Tx) (string, any, error) {
		data := map[string]any{"agent": agent, "role": role, "generation": generation, "terminal": terminal, "how": how, "input": input}
		return KindConversationLeft, data, nil
	})
}

// Agents returns every agent the store records, by address.
func (s *Store) Agents() ([]Agent, error) {
	rows, err := s.db.Query(`SELECT agent, role, state, generation, provider, version, model, pid, started_at, ended_at, exit FROM agents ORDER BY agent`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	agents := []Agent{}
	for rows.Next() {
		var a Agent
		var started string
		var ended sql.NullString
		if err := rows.Scan(&a.Agent, &a.Role, &a.State, &a.Generation, &a.Provider, &a.Version, &a.Model, &a.PID, &started, &ended, &a.Exit); err != nil {
			return nil, err
		}
		if a.StartedAt, err = time.Parse(time.RFC3339Nano, started); err != nil {
			return nil, err
		}
		if ended.Valid {
			if a.EndedAt, err = time.Parse(time.RFC3339Nano, ended.String); err != nil {
				return nil, err
			}
		}
		agents = append(agents, a)
	}
	return agents, rows.Err()
}

// change makes a change to the current state at at and journals it in one
// transaction. apply makes the change and returns the journal entry's kind
// and data; an empty kind means there was nothing to change, and nothing is
// written.
func (s *Store) change(at time.Time, apply func(tx *sql.Tx) (kind string, data any, err error)) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	kind, data, err := apply(tx)
	if err != nil || kind == "" {
		return err
	}
	if _, err := appendEntry(tx, at, kind, data); err != nil {
		return err
	}
	return tx.Commit()
}

// appendEntry appends a journal entry in tx and returns its ID.
func appendEntry(tx *sql.Tx, at time.Time, kind string, data any) (int64, error) {
	encoded, err := json.Marshal(data)
	if err != nil {
		return 0, err
	}
	result, err := tx.Exec(`INSERT INTO journal (at, kind, data) VALUES (?, ?, ?)`, timestamp(at), kind, string(encoded))
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
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
