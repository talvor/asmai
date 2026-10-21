// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// MessageChecksRed tells the delivery owner, Engineering's leader, that the
// checks on the head the daemon pushed are red, naming the failing ones. It
// is about a job, not an assignment or a handoff.
const MessageChecksRed = "checks-red"

// The sources of a decision record: a leader's own judgment, delegated, or
// the user's, human, which cites the witnessed message it came from.
const (
	SourceDelegated = "delegated"
	SourceHuman     = "human"
)

// Sources are the sources a decision record may name.
var Sources = []string{SourceDelegated, SourceHuman}

// MandateRelied is what a judged re-run relies on when its leader names
// nothing else: the authority of the job's mandate.
const MandateRelied = "the job's mandate"

// ChoiceRerunFailed is Engineering's leader's judgment that a head's red
// checks failed for a reason unrelated to the change, so their failed jobs
// are re-run once.
const ChoiceRerunFailed = "rerun-failed"

// DecisionRecord is a decision a leader recorded with asmai decision record,
// with the fields of rule 25: the accountable leader, the job, the mandate or
// grant relied on, the choice, its rationale and evidence, its material
// assumptions, the conditions that would reopen it, and its source. It also
// holds the dispatch the leader was working on. A judged re-run is one kind
// of decision: its choice is ChoiceRerunFailed.
type DecisionRecord struct {
	ID     int64  `json:"id"`
	Job    int64  `json:"job"`
	Leader string `json:"leader"`
	// Source is SourceDelegated, or SourceHuman with Cites the witnessed
	// message or, when Lavish is set, the Lavish answer the user's decision
	// came from.
	Source string `json:"source"`
	Cites  int64  `json:"cites,omitempty"`
	Lavish bool   `json:"lavish,omitempty"`
	// ReliedOn is the mandate or grant the decision relied on.
	ReliedOn  string `json:"relied_on"`
	Choice    string `json:"choice"`
	Rationale string `json:"rationale"`
	Evidence  string `json:"evidence"`
	// Assumptions are the material assumptions the choice rests on, and
	// ReopenWhen the conditions that would reopen it.
	Assumptions []string `json:"assumptions"`
	ReopenWhen  []string `json:"reopen_when"`
	Dispatch    int64    `json:"dispatch"`
	// Request and Version name the version of the decision request that a
	// human decision answers in the conversation, with Cites the witnessed
	// message that holds the user's words and Reading Coordination's reading
	// of them, stored beside them.
	Request int64     `json:"request,omitempty"`
	Version int       `json:"version,omitempty"`
	Reading string    `json:"reading,omitempty"`
	At      time.Time `json:"at"`
}

// Rerun is the daemon re-running the failed jobs of the red checks on Head,
// the head of pull request Number, once, on the delegated decision Decision:
// the workflow runs it re-ran and the checks that had failed.
type Rerun struct {
	ID       int64        `json:"id"`
	Job      int64        `json:"job"`
	Number   int          `json:"number"`
	Head     string       `json:"head"`
	Runs     []int64      `json:"runs"`
	Checks   []CheckState `json:"checks"`
	Decision int64        `json:"decision"`
	At       time.Time    `json:"at"`
}

// FailedChecks returns the checks that failed among checks.
func FailedChecks(checks []CheckState) []CheckState {
	var failed []CheckState
	for _, c := range checks {
		if c.State == "failed" {
			failed = append(failed, c)
		}
	}
	return failed
}

// ChecksRed tells recipient, with a message of kind MessageChecksRed with
// body, that o, the latest observation of o.Job's checks, is red, and returns
// its dispatch. The delivery owner is told once each time the head's checks
// turn red, and again on the first red observation of the head after its
// failed jobs were re-run: when it was already told since, as while a failed
// check stays failed, nothing is sent and it returns false.
func (s *Store) ChecksRed(o ChecksObservation, recipient, body string, at time.Time) (Dispatch, bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Dispatch{}, false, err
	}
	defer tx.Rollback()
	last, err := latestChecks(tx, o.Job)
	if err != nil {
		return Dispatch{}, false, err
	}
	if last.ID != o.ID || last.Verdict != "red" {
		return Dispatch{}, false, fmt.Errorf("observation %d is not the latest red one of job %d's checks", o.ID, o.Job)
	}
	var since, rerun int64
	if err := tx.QueryRow(`SELECT
 (SELECT COALESCE(MAX(id), 0) FROM check_observations WHERE job = ? AND (verdict != 'red' OR head != ?)),
 (SELECT COALESCE(MAX(observation), 0) FROM reruns WHERE job = ? AND head = ?)`, o.Job, o.Head, o.Job, o.Head).Scan(&since, &rerun); err != nil {
		return Dispatch{}, false, err
	}
	if o.ID <= rerun {
		return Dispatch{}, false, nil
	}
	since = max(since, rerun)
	var told int
	if err := tx.QueryRow(`SELECT count(*) FROM red_checks WHERE job = ? AND head = ? AND observation > ?`, o.Job, o.Head, since).Scan(&told); err != nil {
		return Dispatch{}, false, err
	}
	if told > 0 {
		return Dispatch{}, false, nil
	}
	dispatch, err := createMessage(tx, messageRef{}, o.Job, Daemon, recipient, MessageChecksRed, body, at)
	if err != nil {
		return Dispatch{}, false, err
	}
	if _, err := tx.Exec(`INSERT INTO red_checks (observation, job, head, dispatch) VALUES (?, ?, ?, ?)`, o.ID, o.Job, o.Head, dispatch.ID); err != nil {
		return Dispatch{}, false, err
	}
	return dispatch, true, tx.Commit()
}

// RerunDecided records d, the delegated decision of d.Leader's session,
// generation, working on d.Dispatch, to re-run the failed jobs of the red
// checks on r.Head, and r, the daemon's re-run of them on it, and journals
// both. The failed jobs of a head are re-run once: a second re-run of the
// same head is refused.
func (s *Store) RerunDecided(d DecisionRecord, r Rerun, generation int, at time.Time) (DecisionRecord, Rerun, error) {
	if d.Choice != ChoiceRerunFailed || d.Source != "" && d.Source != SourceDelegated {
		return DecisionRecord{}, Rerun{}, errors.New("a judged re-run is a delegated decision to re-run the failed jobs")
	}
	if r.Head == "" || len(r.Runs) == 0 {
		return DecisionRecord{}, Rerun{}, errors.New("a re-run names its head and the workflow runs it re-ran")
	}
	if d.ReliedOn == "" {
		d.ReliedOn = MandateRelied
	}
	tx, err := s.db.Begin()
	if err != nil {
		return DecisionRecord{}, Rerun{}, err
	}
	defer tx.Rollback()
	job, _, err := currentDispatch(tx, d.Dispatch, d.Leader, generation)
	if err != nil {
		return DecisionRecord{}, Rerun{}, err
	}
	if job != d.Job || r.Job != d.Job {
		return DecisionRecord{}, Rerun{}, fmt.Errorf("dispatch %d is of job %d, not job %d", d.Dispatch, job, d.Job)
	}
	if earlier, err := rerunOf(tx, r.Job, r.Head); err == nil {
		return DecisionRecord{}, Rerun{}, RerunRefused(earlier)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return DecisionRecord{}, Rerun{}, err
	}
	if d, err = insertDecisionRecord(tx, d, generation, at); err != nil {
		return DecisionRecord{}, Rerun{}, err
	}
	runs, err := json.Marshal(r.Runs)
	if err != nil {
		return DecisionRecord{}, Rerun{}, err
	}
	if r.Checks == nil {
		r.Checks = []CheckState{}
	}
	checks, err := json.Marshal(r.Checks)
	if err != nil {
		return DecisionRecord{}, Rerun{}, err
	}
	r.Decision, r.At = d.ID, at.UTC()
	result, err := tx.Exec(`INSERT INTO reruns (job, number, head, runs, checks, decision, observation, created_at) VALUES (?, ?, ?, ?, ?, ?, (SELECT COALESCE(MAX(id), 0) FROM check_observations WHERE job = ?), ?)`, r.Job, r.Number, r.Head, string(runs), string(checks), r.Decision, r.Job, timestamp(at))
	if err != nil {
		return DecisionRecord{}, Rerun{}, err
	}
	if r.ID, err = result.LastInsertId(); err != nil {
		return DecisionRecord{}, Rerun{}, err
	}
	if _, err := appendEntry(tx, at, KindChecksRerun, r); err != nil {
		return DecisionRecord{}, Rerun{}, err
	}
	return d, r, tx.Commit()
}

// WorkingOn returns the job of dispatch, which generation of agent's session
// must be working on.
func (s *Store) WorkingOn(dispatch int64, agent string, generation int) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	job, _, err := currentDispatch(tx, dispatch, agent, generation)
	return job, err
}

// RerunRefused is the refusal of a second re-run of the head earlier re-ran.
func RerunRefused(earlier Rerun) error {
	return fmt.Errorf("the failed jobs of head %s were re-run once already, on decision %d; a second re-run of the same head is refused. Assign a fix", earlier.Head, earlier.Decision)
}

// RerunOf returns the re-run of the failed jobs on job's head, or
// sql.ErrNoRows.
func (s *Store) RerunOf(job int64, head string) (Rerun, error) {
	return rerunOf(s.db, job, head)
}

func rerunOf(q querier, job int64, head string) (Rerun, error) {
	reruns, err := scanReruns(q, `job = ? AND head = ?`, job, head)
	if err != nil {
		return Rerun{}, err
	}
	if len(reruns) == 0 {
		return Rerun{}, sql.ErrNoRows
	}
	return reruns[0], nil
}

// Reruns returns job's re-runs, oldest first.
func (s *Store) Reruns(job int64) ([]Rerun, error) {
	return scanReruns(s.db, `job = ?`, job)
}

func scanReruns(q querier, where string, args ...any) ([]Rerun, error) {
	rows, err := q.Query(`SELECT id, job, number, head, runs, checks, decision, created_at FROM reruns WHERE `+where+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	reruns := []Rerun{}
	for rows.Next() {
		var r Rerun
		var runs, checks, at string
		if err := rows.Scan(&r.ID, &r.Job, &r.Number, &r.Head, &runs, &checks, &r.Decision, &at); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(runs), &r.Runs); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(checks), &r.Checks); err != nil {
			return nil, err
		}
		if r.At, err = time.Parse(time.RFC3339Nano, at); err != nil {
			return nil, err
		}
		reruns = append(reruns, r)
	}
	return reruns, rows.Err()
}

// DecisionRecords returns the decisions recorded for job, oldest first.
func (s *Store) DecisionRecords(job int64) ([]DecisionRecord, error) {
	return scanDecisionRecords(s.db, `job = ?`, job)
}

// CIWaitReplaced journals that the daemon dropped its wait on the checks of
// from, an older head of job's pull request, for those of to, the head it
// pushed since, once for each head it waits on.
func (s *Store) CIWaitReplaced(job int64, from, to string, at time.Time) (bool, error) {
	replaced := false
	err := s.change(at, func(tx *sql.Tx) (string, any, error) {
		var n int
		if err := tx.QueryRow(`SELECT count(*) FROM journal WHERE kind = ? AND json_extract(data, '$.job') = ? AND json_extract(data, '$.to') = ?`, KindCIWaitReplaced, job, to).Scan(&n); err != nil || n > 0 {
			return "", nil, err
		}
		replaced = true
		return KindCIWaitReplaced, map[string]any{"job": job, "from": from, "to": to}, nil
	})
	return replaced, err
}
