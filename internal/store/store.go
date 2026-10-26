// SPDX-License-Identifier: Apache-2.0

// Package store is the factory's durable record: the current state the daemon
// keeps, and the append-only journal of everything that happened, in one
// SQLite database. Only the daemon opens it. Every change to the current
// state is written in the same transaction as the journal entry that records
// it, and the journal is never trimmed: the database itself refuses to update
// or delete an entry.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
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
	`
CREATE TABLE handoffs (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 job INTEGER NOT NULL REFERENCES jobs(number), sender TEXT NOT NULL, receiver TEXT NOT NULL,
 outcome TEXT NOT NULL, decisions TEXT NOT NULL, evidence TEXT NOT NULL,
 constraints TEXT NOT NULL, permissions TEXT NOT NULL, criteria TEXT NOT NULL,
 state TEXT NOT NULL, answer TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL
);
CREATE TABLE messages (
 id INTEGER PRIMARY KEY AUTOINCREMENT, handoff INTEGER NOT NULL REFERENCES handoffs(id),
 job INTEGER NOT NULL, sender TEXT NOT NULL, recipient TEXT NOT NULL,
 kind TEXT NOT NULL, body TEXT NOT NULL, fetched_at TEXT
);
CREATE TABLE dispatches (
 id INTEGER PRIMARY KEY AUTOINCREMENT, message INTEGER NOT NULL UNIQUE REFERENCES messages(id),
 agent TEXT NOT NULL, generation INTEGER NOT NULL DEFAULT 0, state TEXT NOT NULL,
 transcript TEXT NOT NULL DEFAULT '', updated_at TEXT NOT NULL
);
`,
	`
CREATE TABLE job_branches (
 job INTEGER PRIMARY KEY REFERENCES jobs(number), repository TEXT NOT NULL, name TEXT NOT NULL,
 started_from TEXT NOT NULL, tip TEXT NOT NULL, view TEXT NOT NULL, created_at TEXT NOT NULL
);
CREATE TABLE assignments (
 id INTEGER PRIMARY KEY AUTOINCREMENT, job INTEGER NOT NULL REFERENCES jobs(number),
 owner TEXT NOT NULL, worker TEXT NOT NULL, outcome TEXT NOT NULL, criteria TEXT NOT NULL,
 state TEXT NOT NULL, created_at TEXT NOT NULL
);
CREATE TABLE workspaces (
 assignment INTEGER PRIMARY KEY REFERENCES assignments(id), slot INTEGER NOT NULL,
 path TEXT NOT NULL, tmp TEXT NOT NULL, branch TEXT NOT NULL, base TEXT NOT NULL
);
CREATE TABLE effects (
 id INTEGER PRIMARY KEY AUTOINCREMENT, job INTEGER NOT NULL REFERENCES jobs(number),
 assignment INTEGER REFERENCES assignments(id), dispatch INTEGER NOT NULL REFERENCES dispatches(id),
 agent TEXT NOT NULL, generation INTEGER NOT NULL, kind TEXT NOT NULL, ref TEXT NOT NULL, at TEXT NOT NULL
);
CREATE TABLE reports (
 id INTEGER PRIMARY KEY AUTOINCREMENT, assignment INTEGER NOT NULL REFERENCES assignments(id),
 dispatch INTEGER NOT NULL REFERENCES dispatches(id), kind TEXT NOT NULL, agent TEXT NOT NULL,
 commit_id TEXT NOT NULL, data TEXT NOT NULL, created_at TEXT NOT NULL
);
CREATE TABLE messages_next (
 id INTEGER PRIMARY KEY AUTOINCREMENT, handoff INTEGER REFERENCES handoffs(id),
 assignment INTEGER REFERENCES assignments(id), report INTEGER REFERENCES reports(id),
 job INTEGER NOT NULL, sender TEXT NOT NULL, recipient TEXT NOT NULL,
 kind TEXT NOT NULL, body TEXT NOT NULL, fetched_at TEXT
);
INSERT INTO messages_next (id, handoff, job, sender, recipient, kind, body, fetched_at)
 SELECT id, handoff, job, sender, recipient, kind, body, fetched_at FROM messages;
DROP TABLE messages;
ALTER TABLE messages_next RENAME TO messages;
`,
	`
CREATE TABLE decisions (
 id INTEGER PRIMARY KEY AUTOINCREMENT, assignment INTEGER NOT NULL REFERENCES assignments(id),
 report INTEGER REFERENCES reports(id), kind TEXT NOT NULL, leader TEXT NOT NULL,
 reasons TEXT NOT NULL, commit_id TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL
);
`,
	`
ALTER TABLE assignments ADD COLUMN read_only INTEGER NOT NULL DEFAULT 0;
ALTER TABLE handoffs ADD COLUMN head TEXT NOT NULL DEFAULT '';
CREATE TABLE findings (
 id INTEGER PRIMARY KEY AUTOINCREMENT, job INTEGER NOT NULL REFERENCES jobs(number),
 assignment INTEGER NOT NULL REFERENCES assignments(id), report INTEGER NOT NULL REFERENCES reports(id),
 kind TEXT NOT NULL, text TEXT NOT NULL, commit_id TEXT NOT NULL,
 cleared_by INTEGER REFERENCES reports(id), cleared_at TEXT, created_at TEXT NOT NULL
);
ALTER TABLE dispatches ADD COLUMN resumes INTEGER REFERENCES dispatches(id);
ALTER TABLE dispatches ADD COLUMN native_session TEXT NOT NULL DEFAULT '';
CREATE TABLE native_sessions (
 agent TEXT NOT NULL, generation INTEGER NOT NULL, provider TEXT NOT NULL,
 assignment INTEGER REFERENCES assignments(id),
 session_id TEXT NOT NULL DEFAULT '', transcript TEXT NOT NULL DEFAULT '',
 PRIMARY KEY (agent, generation)
);
`,
	`
CREATE TABLE pushes (
 id INTEGER PRIMARY KEY AUTOINCREMENT, job INTEGER NOT NULL REFERENCES jobs(number),
 branch TEXT NOT NULL, commit_id TEXT NOT NULL, result TEXT NOT NULL,
 outside TEXT NOT NULL DEFAULT '[]', found TEXT NOT NULL DEFAULT '',
 leader TEXT NOT NULL, dispatch INTEGER NOT NULL REFERENCES dispatches(id),
 effect INTEGER REFERENCES effects(id), merged_in TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL
);
`,
	`
CREATE TABLE pull_requests (
 id INTEGER PRIMARY KEY AUTOINCREMENT, job INTEGER NOT NULL REFERENCES jobs(number),
 number INTEGER NOT NULL, url TEXT NOT NULL, title TEXT NOT NULL, draft INTEGER NOT NULL,
 not_ready INTEGER NOT NULL, branch TEXT NOT NULL, head TEXT NOT NULL, state TEXT NOT NULL,
 leader TEXT NOT NULL, dispatch INTEGER NOT NULL REFERENCES dispatches(id),
 effect INTEGER NOT NULL REFERENCES effects(id), created_at TEXT NOT NULL
);
`,
	`
CREATE TABLE check_observations (
 id INTEGER PRIMARY KEY AUTOINCREMENT, job INTEGER NOT NULL REFERENCES jobs(number),
 number INTEGER NOT NULL, head TEXT NOT NULL, checks TEXT NOT NULL, verdict TEXT NOT NULL,
 created_at TEXT NOT NULL
);
CREATE TABLE deliveries (
 job INTEGER PRIMARY KEY REFERENCES jobs(number), number INTEGER NOT NULL, url TEXT NOT NULL,
 head TEXT NOT NULL, checks TEXT NOT NULL, created_at TEXT NOT NULL
);
`,
	`
CREATE TABLE red_checks (
 observation INTEGER PRIMARY KEY REFERENCES check_observations(id),
 job INTEGER NOT NULL REFERENCES jobs(number), head TEXT NOT NULL,
 dispatch INTEGER NOT NULL REFERENCES dispatches(id)
);
CREATE TABLE decision_records (
 id INTEGER PRIMARY KEY AUTOINCREMENT, job INTEGER NOT NULL REFERENCES jobs(number),
 leader TEXT NOT NULL, source TEXT NOT NULL, choice TEXT NOT NULL, rationale TEXT NOT NULL,
 evidence TEXT NOT NULL, dispatch INTEGER NOT NULL REFERENCES dispatches(id), created_at TEXT NOT NULL
);
CREATE TABLE reruns (
 id INTEGER PRIMARY KEY AUTOINCREMENT, job INTEGER NOT NULL REFERENCES jobs(number),
 number INTEGER NOT NULL, head TEXT NOT NULL, runs TEXT NOT NULL, checks TEXT NOT NULL,
 decision INTEGER NOT NULL REFERENCES decision_records(id), observation INTEGER NOT NULL,
 created_at TEXT NOT NULL, UNIQUE (job, head)
);
`,
	`
ALTER TABLE assignments ADD COLUMN skill TEXT NOT NULL DEFAULT '';
ALTER TABLE assignments ADD COLUMN skills TEXT NOT NULL DEFAULT '[]';
ALTER TABLE native_sessions ADD COLUMN bundle TEXT NOT NULL DEFAULT '';
ALTER TABLE native_sessions ADD COLUMN skills TEXT NOT NULL DEFAULT '{}';
ALTER TABLE dispatches ADD COLUMN bundle TEXT NOT NULL DEFAULT '';
ALTER TABLE dispatches ADD COLUMN skills TEXT NOT NULL DEFAULT '{}';
`,
	`
ALTER TABLE native_sessions ADD COLUMN unqualified TEXT NOT NULL DEFAULT '{}';
ALTER TABLE dispatches ADD COLUMN unqualified TEXT NOT NULL DEFAULT '{}';
`,
	`
CREATE TABLE decision_requests (
 id INTEGER PRIMARY KEY AUTOINCREMENT, request INTEGER NOT NULL, version INTEGER NOT NULL,
 job INTEGER NOT NULL REFERENCES jobs(number), requester TEXT NOT NULL, recipient TEXT NOT NULL,
 assignment INTEGER REFERENCES assignments(id), kind TEXT NOT NULL, surface TEXT NOT NULL,
 content TEXT NOT NULL, questions TEXT NOT NULL, blocks TEXT NOT NULL DEFAULT '[]',
 state TEXT NOT NULL, answer TEXT NOT NULL DEFAULT '', answered_by TEXT NOT NULL DEFAULT '',
 relied_on TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, settled_at TEXT,
 UNIQUE (request, version)
);
ALTER TABLE messages ADD COLUMN request INTEGER REFERENCES decision_requests(id);
`,
	`
CREATE TABLE decision_records_next (
 id INTEGER PRIMARY KEY AUTOINCREMENT, job INTEGER NOT NULL REFERENCES jobs(number),
 leader TEXT NOT NULL, source TEXT NOT NULL, cites INTEGER NOT NULL DEFAULT 0, relied_on TEXT NOT NULL,
 choice TEXT NOT NULL, rationale TEXT NOT NULL, evidence TEXT NOT NULL,
 assumptions TEXT NOT NULL DEFAULT '[]', reopen_when TEXT NOT NULL DEFAULT '[]',
 dispatch INTEGER REFERENCES dispatches(id), created_at TEXT NOT NULL
);
INSERT INTO decision_records_next (id, job, leader, source, relied_on, choice, rationale, evidence, dispatch, created_at)
 SELECT id, job, leader, source, 'the job''s mandate', choice, rationale, evidence, dispatch, created_at FROM decision_records;
DROP TABLE decision_records;
ALTER TABLE decision_records_next RENAME TO decision_records;
ALTER TABLE assignments ADD COLUMN decision_records TEXT NOT NULL DEFAULT '[]';
ALTER TABLE handoffs ADD COLUMN decision_records TEXT NOT NULL DEFAULT '[]';
`,
	`
ALTER TABLE decision_requests ADD COLUMN shown INTEGER NOT NULL DEFAULT 0;
ALTER TABLE decision_requests ADD COLUMN cites INTEGER NOT NULL DEFAULT 0;
ALTER TABLE decision_requests ADD COLUMN reading TEXT NOT NULL DEFAULT '';
ALTER TABLE decision_requests ADD COLUMN record INTEGER NOT NULL DEFAULT 0;
ALTER TABLE decision_records ADD COLUMN request INTEGER NOT NULL DEFAULT 0;
ALTER TABLE decision_records ADD COLUMN version INTEGER NOT NULL DEFAULT 0;
ALTER TABLE decision_records ADD COLUMN reading TEXT NOT NULL DEFAULT '';
`,
	`
CREATE TABLE grants (
 id INTEGER PRIMARY KEY AUTOINCREMENT, scope TEXT NOT NULL, job INTEGER NOT NULL DEFAULT 0,
 repository TEXT NOT NULL DEFAULT '', action TEXT NOT NULL, approved TEXT NOT NULL,
 amount INTEGER NOT NULL DEFAULT 0, currency TEXT NOT NULL DEFAULT '', conditions TEXT NOT NULL DEFAULT '[]',
 leader TEXT NOT NULL, cites INTEGER NOT NULL, answer INTEGER NOT NULL DEFAULT 0, words TEXT NOT NULL,
 state TEXT NOT NULL, ended_by TEXT NOT NULL DEFAULT '', ended_reason TEXT NOT NULL DEFAULT '',
 ended_at TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL
);
CREATE TABLE authorizations (
 id INTEGER PRIMARY KEY AUTOINCREMENT, grant_id INTEGER NOT NULL REFERENCES grants(id),
 job INTEGER NOT NULL REFERENCES jobs(number), assignment INTEGER NOT NULL DEFAULT 0,
 dispatch INTEGER NOT NULL DEFAULT 0, agent TEXT NOT NULL, action TEXT NOT NULL, what TEXT NOT NULL,
 amount INTEGER NOT NULL DEFAULT 0, currency TEXT NOT NULL DEFAULT '', effect INTEGER NOT NULL DEFAULT 0,
 created_at TEXT NOT NULL
);
`,
	`
ALTER TABLE decision_requests ADD COLUMN answered TEXT NOT NULL DEFAULT '[]';
ALTER TABLE decision_requests ADD COLUMN page_file TEXT NOT NULL DEFAULT '';
ALTER TABLE decision_requests ADD COLUMN page_url TEXT NOT NULL DEFAULT '';
ALTER TABLE decision_requests ADD COLUMN page_closed INTEGER NOT NULL DEFAULT 0;
ALTER TABLE decision_records ADD COLUMN lavish INTEGER NOT NULL DEFAULT 0;
CREATE TABLE lavish_answers (
 id INTEGER PRIMARY KEY REFERENCES journal(id), decision INTEGER NOT NULL REFERENCES decision_requests(id),
 answers TEXT NOT NULL, created_at TEXT NOT NULL
);
`,
	`
ALTER TABLE decision_requests ADD COLUMN carries INTEGER NOT NULL DEFAULT 0;
ALTER TABLE decision_requests ADD COLUMN carries_version INTEGER NOT NULL DEFAULT 0;
`,
	`
ALTER TABLE decision_requests ADD COLUMN skill TEXT NOT NULL DEFAULT '';
ALTER TABLE decision_requests ADD COLUMN step TEXT NOT NULL DEFAULT '';
ALTER TABLE decision_requests ADD COLUMN relied_records TEXT NOT NULL DEFAULT '[]';
ALTER TABLE decision_requests ADD COLUMN relied_grants TEXT NOT NULL DEFAULT '[]';
`,
	`
CREATE TABLE setup_answers (
 id INTEGER PRIMARY KEY AUTOINCREMENT, repository TEXT NOT NULL, job INTEGER NOT NULL REFERENCES jobs(number),
 request INTEGER NOT NULL, version INTEGER NOT NULL, question TEXT NOT NULL, title TEXT NOT NULL,
 choice TEXT NOT NULL, notes TEXT NOT NULL, cites INTEGER NOT NULL, created_at TEXT NOT NULL
);
CREATE INDEX setup_answers_repository ON setup_answers(repository, id);
`,
	`
ALTER TABLE jobs ADD COLUMN follows INTEGER NOT NULL DEFAULT 0;
ALTER TABLE job_branches ADD COLUMN continues INTEGER NOT NULL DEFAULT 0;
`,
	`
ALTER TABLE handoffs ADD COLUMN review TEXT NOT NULL DEFAULT '';
ALTER TABLE handoffs ADD COLUMN since TEXT NOT NULL DEFAULT '';
ALTER TABLE handoffs ADD COLUMN assignment INTEGER NOT NULL DEFAULT 0;
ALTER TABLE assignments ADD COLUMN review TEXT NOT NULL DEFAULT '';
ALTER TABLE assignments ADD COLUMN since TEXT NOT NULL DEFAULT '';
ALTER TABLE assignments ADD COLUMN reviews INTEGER NOT NULL DEFAULT 0;
ALTER TABLE assignments ADD COLUMN requester TEXT NOT NULL DEFAULT '';
`,
	`
ALTER TABLE decision_requests ADD COLUMN quoted TEXT NOT NULL DEFAULT '[]';
ALTER TABLE findings ADD COLUMN cleared_decision INTEGER REFERENCES decision_records(id);
`,
	`
ALTER TABLE jobs ADD COLUMN cancel_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN cancel_witness INTEGER NOT NULL DEFAULT 0;
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
	KindHandoffSent       = "handoff.sent"
	KindHandoffAnswered   = "handoff.answered"
	KindMessageCreated    = "message.created"
	KindMessageFetched    = "message.fetched"
	KindDispatchChanged   = "dispatch.changed"

	// KindConfigurationApplied records the user applying the configuration
	// file with `asmai config apply`: the file, what changed and which
	// agents pick the change up.
	KindConfigurationApplied = "configuration.applied"

	// KindJobBranchMade records the daemon making a job's branch from the
	// default branch as fetched from origin, with the commit it started from.
	KindJobBranchMade = "job.branch"
	// KindAssignmentCreated and KindWorkspaceCreated record a writing
	// assignment, and the workspace of AsmAI's clone the daemon made for it
	// on its own branch.
	KindAssignmentCreated = "assignment.created"
	KindWorkspaceCreated  = "workspace.created"
	// KindEffectRecorded records an effect a worker or leader reported,
	// tagged with its dispatch.
	KindEffectRecorded = "effect.recorded"
	// KindResultSubmitted and KindBlockedReported record a worker's result,
	// which submits its assignment, and its blocked report.
	KindResultSubmitted = "result.submitted"
	KindBlockedReported = "blocked.reported"
	// KindResultAccepted and KindResultRejected record the owning leader's
	// decision on a result with its reasons. Accepting also moves the job
	// branch, which KindJobBranchMoved records in the same step.
	KindResultAccepted = "result.accepted"
	KindResultRejected = "result.rejected"
	KindJobBranchMoved = "job.branch.moved"
	// KindResultReturned records a submitted result going back to its worker
	// because the job branch moved after it was submitted: not a rejection,
	// and no decision of the owning leader.
	KindResultReturned = "result.returned"
	// KindJobLinked records Coordination linking a follow-up job to the job
	// it follows, and KindJobBranchDropped the daemon dropping the branch it
	// made for a follow-up before any work was on it, so that the follow-up
	// can continue its predecessor's.
	KindJobLinked        = "job.linked"
	KindJobBranchDropped = "job.branch.dropped"
	// KindWorkspaceRemoved records the daemon removing a writing assignment's
	// workspace once its work is on the job branch.
	KindWorkspaceRemoved = "workspace.removed"
	// KindCancellationRequested records the owning leader cancelling an
	// assignment, which its worker is told to push its branch for, and
	// KindAssignmentCancelled the assignment ending once the worker stopped.
	KindCancellationRequested = "assignment.cancelling"
	KindAssignmentCancelled   = "assignment.cancelled"
	// KindValidationAccepted records Quality's leader accepting a validation
	// report: the findings it recorded, with their IDs, and the earlier
	// blocking findings it cleared. KindReadOnlyWorkspaceRemoved records the
	// daemon removing a read-only workspace when its assignment ended.
	KindValidationAccepted       = "validation.accepted"
	KindReadOnlyWorkspaceRemoved = "workspace.read-only.removed"
	// KindSessionIdentified records the provider's own identifier of an agent
	// session, as its hooks reported it: what a later session resumes.
	KindSessionIdentified = "session.identified"
	// KindAssignmentResumed records a stopped worker's assignment continuing
	// after a restart, in a new dispatch that resumes the one that stopped,
	// and KindAssignmentNeedsReconciliation an assignment held for its
	// owning leader to reconcile. KindCancellationRestarted records a
	// cancelled job's cancelling assignment, whose worker's session cannot be
	// resumed, given a new cancellation dispatch in a new session instead.
	KindAssignmentResumed             = "assignment.resumed"
	KindAssignmentNeedsReconciliation = "assignment.needs_reconciliation"
	KindCancellationRestarted         = "assignment.cancellation_restarted"
	// KindJobBranchPushed records the daemon pushing a job branch for its
	// delivery owner, with the effect it records and the outside changes the
	// pushed commit took in. KindOutsideChange records a push refused because
	// origin's job branch has commits AsmAI did not make: the commits and
	// where they were found.
	KindJobBranchPushed = "job.branch.pushed"
	KindOutsideChange   = "outside_change.found"
	// KindPullRequestRecorded records the pull request the delivery owner
	// opened or updated, as GitHub had it when the daemon looked it up for
	// the effect that names it.
	KindPullRequestRecorded = "pull_request.recorded"
	// KindChecksObserved records the checks on the head of a job's pull
	// request, as the daemon read them from GitHub through gh, each time they
	// change: each check's state, and the verdict on them.
	KindChecksObserved = "checks.observed"
	// KindDecisionRecorded records a leader's decision with asmai decision
	// record: the job, the choice, its rationale and evidence, and its
	// source. KindChecksRerun records the daemon re-running the failed jobs
	// of a head's red checks once, on the delegated decision it cites.
	KindDecisionRecorded = "decision.recorded"
	// KindDecisionRequested, KindDecisionReplaced, KindDecisionAnswered and
	// KindDecisionWithdrawn record a decision request's versions and how each
	// ended: replaced by a newer version, answered, or withdrawn.
	KindDecisionRequested = "decision.requested"
	KindDecisionReplaced  = "decision.replaced"
	KindDecisionAnswered  = "decision.answered"
	KindDecisionWithdrawn = "decision.withdrawn"
	// KindPromptRelayed records the daemon typing the user's witnessed
	// answer to a native prompt request into the terminal of the agent that
	// waits at the prompt.
	KindPromptRelayed = "prompt.relayed"
	// KindDecisionPresented records Coordination showing the user a version
	// of a decision request in the conversation, and KindDecisionRefused the
	// daemon refusing a record that cited a nudge, or a message typed
	// against an older version, as the user's answer to it.
	KindDecisionPresented = "decision.presented"
	KindDecisionRefused   = "decision.refused"
	// KindLavishPageOpened records the daemon opening the Lavish page of a
	// version of a decision request, which is when the version is shown to
	// the user. KindLavishAnswered records what the user sent from the page,
	// word for word, which a record attributed to the user cites, and
	// KindLavishRefused what the daemon refused to apply because it came
	// from a page that is no longer current. KindLavishPageClosed records
	// the daemon ending the page's session once its version is settled.
	KindLavishPageOpened = "lavish.page.opened"
	KindLavishAnswered   = "lavish.answered"
	KindLavishRefused    = "lavish.refused"
	KindLavishPageClosed = "lavish.page.closed"
	KindChecksRerun      = "checks.rerun"
	// KindGrantRecorded records a leader recording a grant the user gave in
	// words, citing them. KindActionAuthorized records the daemon finding a
	// grant that covers a consequential action an agent asked to take, and
	// KindGrantRevoked and KindGrantInvalidated a grant ending: the user's
	// revocation, and a leader reporting that a condition no longer holds.
	KindGrantRecorded    = "grant.recorded"
	KindActionAuthorized = "action.authorized"
	KindGrantRevoked     = "grant.revoked"
	KindGrantInvalidated = "grant.invalidated"
	// KindCIWaitReplaced records the daemon dropping its wait on the checks
	// of an older head of a job's pull request for those of the head it
	// pushed since.
	KindCIWaitReplaced = "ci.wait.replaced"
	// KindJobDelivered records the daemon marking a job's pull request ready
	// for review once every check on the pushed head is green: the delivery,
	// with the pull request, the head commit and the time. KindJobEnded
	// records the job ending in the same step, and KindJobLocalRemoved the
	// daemon removing everything local to it afterwards: its view and its
	// branches in AsmAI's clone, which stay on origin.
	KindJobDelivered    = "job.delivered"
	KindJobEnded        = "job.ended"
	KindJobLocalRemoved = "job.local.removed"
	// KindPrototypeKept records the owning leader's acceptance of a result
	// as a prototype that stays on its own assignment branch: the job
	// branch does not move.
	KindPrototypeKept = "prototype.kept"
	// KindScratchCleaned records the daemon removing the scratch workspaces
	// of a job with no repository that ended, on the user's command.
	KindScratchCleaned = "job.scratch.cleaned"
	// KindSetupAnswered records the user's answer to a question of a
	// repository's setup, recorded for that repository at once.
	KindSetupAnswered = "setup.answered"
	// KindFindingsOverruled records the user's decision, answering a leader's
	// dispute of blocking findings, that they stand cleared.
	KindFindingsOverruled = "findings.overruled"
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
	// Assignment is the assignment a worker's session carries, and zero for a
	// leader's. Resumes is the provider's own session this one resumes, when
	// it does.
	Assignment int64  `json:"assignment,omitempty"`
	Resumes    string `json:"resumes,omitempty"`
	// Bundle is the skill bundle's version and Skills the fingerprint of
	// each skill of the list the session was given, by name: what every
	// dispatch delivered in the session records. Unqualified says where
	// each skill that is not qualified comes from, "user" for an added skill
	// and "repository" for a repository's own.
	Bundle      string            `json:"bundle,omitempty"`
	Skills      map[string]string `json:"skills,omitempty"`
	Unqualified map[string]string `json:"unqualified,omitempty"`
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
	db, err := sql.Open("sqlite3", "file:"+(&url.URL{Path: path}).EscapedPath()+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)")
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

func OpenReadOnly(path string) (*Store, error) {
	db, err := sql.Open("sqlite3", "file:"+(&url.URL{Path: path}).EscapedPath()+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return &Store{db: db}, nil
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
	// A migration may rebuild a table other tables refer to, which SQLite
	// allows only with foreign keys off. They stay off on this one connection
	// for the migration, which then checks them itself before it commits.
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return err
	}
	defer conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`)
	tx, err := conn.BeginTx(ctx, nil)
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
	violations, err := tx.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		return err
	}
	broken := violations.Next()
	violations.Close()
	if broken {
		return errors.New("a migration left a foreign key broken")
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
		if err != nil {
			return "", nil, err
		}
		skills, err := json.Marshal(nonNilSkills(start.Skills))
		if err != nil {
			return "", nil, err
		}
		unqualified, err := json.Marshal(nonNilSkills(start.Unqualified))
		if err != nil {
			return "", nil, err
		}
		_, err = tx.Exec(`INSERT INTO native_sessions (agent, generation, provider, assignment, bundle, skills, unqualified) VALUES (?, ?, ?, NULLIF(?, 0), ?, ?, ?)`,
			start.Agent, generation, start.Provider, start.Assignment, start.Bundle, string(skills), string(unqualified))
		if err != nil {
			return "", nil, err
		}
		_, err = tx.Exec(`UPDATE dispatches SET generation=?,
			bundle=CASE WHEN state=? THEN ? ELSE bundle END,
			skills=CASE WHEN state=? THEN ? ELSE skills END,
			unqualified=CASE WHEN state=? THEN ? ELSE unqualified END WHERE agent=? AND (
			(state IN (?,?,?) AND EXISTS (
				SELECT 1 FROM messages WHERE messages.id=dispatches.message AND messages.fetched_at IS NULL
			)) OR (state IN (?,?) AND EXISTS (
				SELECT 1 FROM messages JOIN handoffs ON handoffs.id=messages.handoff
				WHERE messages.id=dispatches.message AND messages.kind='handoff' AND messages.fetched_at IS NOT NULL AND handoffs.state=?
			))
		)`, generation, DispatchCreated, start.Bundle, DispatchCreated, string(skills), DispatchCreated, string(unqualified), start.Agent, DispatchCreated, DispatchNudged, DispatchUnknown, DispatchDelivered, DispatchNudged, HandoffPending)
		if err != nil {
			return "", nil, err
		}
		data := struct {
			SessionStart
			Generation int `json:"generation"`
		}{start, generation}
		return KindSessionStarted, data, err
	})
	return generation, err
}

func nonNilSkills(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
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

// JournalHead is the ID of the latest journal entry, zero when there is none.
func (s *Store) JournalHead() (int64, error) {
	var id int64
	err := s.db.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM journal`).Scan(&id)
	return id, err
}

func timestamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}
