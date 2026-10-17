// SPDX-License-Identifier: Apache-2.0

package store

import (
	"time"
)

// AgentWork is what an agent's latest dispatch and assignments say it is
// doing, for the daemon to derive the agent's status from.
type AgentWork struct {
	Agent string
	// Dispatch is the state of the agent's latest dispatch, and empty when it
	// has had none; At is when that dispatch last changed.
	Dispatch string
	At       time.Time
	// Job, Kind, Assignment and Outcome say what that dispatch delivered: the
	// job it is about, the kind of message, and the assignment or handoff
	// outcome it carried, empty for a message that carries none.
	Job        int64
	Kind       string
	Assignment int64
	Outcome    string
	// Reconciling is the assignment of the agent, as its worker, that needs
	// reconciliation, and zero when it has none.
	Reconciling    int64
	ReconcilingJob int64
	ReconcilingAt  time.Time
}

// AgentWorks returns, by agent, the work of each agent that has had a
// dispatch or has an assignment needing reconciliation. It only reads.
func (s *Store) AgentWorks() (map[string]AgentWork, error) {
	works := map[string]AgentWork{}
	rows, err := s.db.Query(`SELECT d.agent, d.state, d.updated_at, m.job, m.kind, COALESCE(m.assignment, 0),
			COALESCE(a.outcome, h.outcome, '')
		FROM dispatches d JOIN messages m ON m.id = d.message
		LEFT JOIN assignments a ON a.id = m.assignment
		LEFT JOIN handoffs h ON h.id = m.handoff
		WHERE d.id = (SELECT x.id FROM dispatches x WHERE x.agent = d.agent ORDER BY x.updated_at DESC, x.id DESC LIMIT 1)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var w AgentWork
		var at string
		if err := rows.Scan(&w.Agent, &w.Dispatch, &at, &w.Job, &w.Kind, &w.Assignment, &w.Outcome); err != nil {
			return nil, err
		}
		if w.At, err = time.Parse(time.RFC3339Nano, at); err != nil {
			return nil, err
		}
		works[w.Agent] = w
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	// The assignment's own change is not stamped, so the time is its
	// reconciliation's entry in the journal.
	held, err := s.db.Query(`SELECT a.worker, a.id, a.job, COALESCE((SELECT j.at FROM journal j WHERE j.kind = ? AND json_extract(j.data, '$.assignment') = a.id ORDER BY j.id DESC LIMIT 1), a.created_at)
		FROM assignments a WHERE a.state = ? ORDER BY a.id`, KindAssignmentNeedsReconciliation, AssignmentNeedsReconciliation)
	if err != nil {
		return nil, err
	}
	defer held.Close()
	for held.Next() {
		var worker, at string
		var id, job int64
		if err := held.Scan(&worker, &id, &job, &at); err != nil {
			return nil, err
		}
		w := works[worker]
		w.Agent, w.Reconciling, w.ReconcilingJob = worker, id, job
		if w.ReconcilingAt, err = time.Parse(time.RFC3339Nano, at); err != nil {
			return nil, err
		}
		works[worker] = w
	}
	return works, held.Err()
}

// JobWork is what a job's records say about where it is, for the daemon to
// derive its stage from.
type JobWork struct {
	Number int64
	// Ended is whether the job is ended, which a delivery does.
	Ended bool
	// Words is the user's request, for a job with no pull request to name it.
	Words string
	// PullRequest and Title are the latest pull request recorded for the
	// job, zero and empty when it has none.
	PullRequest int
	Title       string
	// Assignments counts the job's assignments by state.
	Assignments map[string]int
	// Validating is whether a read-only assignment, which is Quality's
	// validation, has not ended. NeedsYou counts the needs-you findings, and
	// OpenBlocking the blocking findings no validation has cleared.
	Validating   bool
	NeedsYou     int
	OpenBlocking int
	// Validated is whether Quality's validation was accepted and sent to the
	// delivery owner, and Pushed whether the job branch was pushed.
	Validated bool
	Pushed    bool
}

// JobWorks returns what the records say of each job, in job order. It only
// reads.
func (s *Store) JobWorks() ([]JobWork, error) {
	rows, err := s.db.Query(`SELECT j.number, j.state = ?, j.words,
			COALESCE((SELECT p.number FROM pull_requests p WHERE p.job = j.number ORDER BY p.id DESC LIMIT 1), 0),
			COALESCE((SELECT p.title FROM pull_requests p WHERE p.job = j.number ORDER BY p.id DESC LIMIT 1), ''),
			EXISTS (SELECT 1 FROM assignments a WHERE a.job = j.number AND a.read_only AND a.state NOT IN (?, ?)),
			(SELECT count(*) FROM findings f WHERE f.job = j.number AND f.kind = ?),
			(SELECT count(*) FROM findings f WHERE f.job = j.number AND f.kind = ? AND f.cleared_by IS NULL),
			EXISTS (SELECT 1 FROM messages m WHERE m.job = j.number AND m.kind = ?),
			EXISTS (SELECT 1 FROM pushes x WHERE x.job = j.number)
		FROM jobs j ORDER BY j.number`, JobEnded, AssignmentAccepted, AssignmentCancelled, FindingNeedsYou, FindingBlocking, MessageValidation)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	works := []JobWork{}
	for rows.Next() {
		w := JobWork{Assignments: map[string]int{}}
		if err := rows.Scan(&w.Number, &w.Ended, &w.Words, &w.PullRequest, &w.Title, &w.Validating, &w.NeedsYou, &w.OpenBlocking, &w.Validated, &w.Pushed); err != nil {
			return nil, err
		}
		works = append(works, w)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	counts, err := s.db.Query(`SELECT job, state, count(*) FROM assignments GROUP BY job, state`)
	if err != nil {
		return nil, err
	}
	defer counts.Close()
	byJob := map[int64]map[string]int{}
	for counts.Next() {
		var job int64
		var state string
		var n int
		if err := counts.Scan(&job, &state, &n); err != nil {
			return nil, err
		}
		if byJob[job] == nil {
			byJob[job] = map[string]int{}
		}
		byJob[job][state] = n
	}
	if err := counts.Err(); err != nil {
		return nil, err
	}
	for i := range works {
		if c := byJob[works[i].Number]; c != nil {
			works[i].Assignments = c
		}
	}
	return works, nil
}
