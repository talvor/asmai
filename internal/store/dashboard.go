// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
	Writing      bool
	NeedsYou     int
	OpenBlocking int
	// Validated is whether Quality's validation was accepted and sent to the
	// delivery owner for the current head. Pushed and PRCurrent are also
	// evidence for that head.
	Validated bool
	Pushed    bool
	PRCurrent bool
}

// JobWorks returns what the records say of each job, in job order. It only
// reads.
func (s *Store) JobWorks() ([]JobWork, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	works, err := jobWorks(tx)
	if err != nil {
		return nil, err
	}
	return works, tx.Commit()
}

func jobWorks(tx *sql.Tx) ([]JobWork, error) {
	rows, err := tx.Query(`WITH latest_pr AS (
			SELECT p.job, p.number, p.title, p.head FROM pull_requests p
			JOIN (SELECT job, MAX(id) id FROM pull_requests GROUP BY job) last ON last.id = p.id
		), assignment_state AS (
			SELECT job,
				MAX(CASE WHEN read_only AND state NOT IN (?, ?) THEN 1 ELSE 0 END) validating,
				MAX(CASE WHEN NOT read_only AND state NOT IN (?, ?) THEN 1 ELSE 0 END) writing
			FROM assignments GROUP BY job
		), finding_state AS (
			SELECT job,
				SUM(CASE WHEN kind = ? THEN 1 ELSE 0 END) needs_you,
				SUM(CASE WHEN kind = ? AND cleared_by IS NULL THEN 1 ELSE 0 END) open_blocking
			FROM findings GROUP BY job
		), validations AS (
			SELECT m.job, r.commit_id, 1 validated FROM messages m
			JOIN reports r ON r.id = m.report JOIN assignments va ON va.id = r.assignment
			WHERE m.kind = ? AND va.review != ? GROUP BY m.job, r.commit_id
		), pushed_heads AS (
			SELECT job, commit_id, 1 pushed FROM pushes WHERE result = ? GROUP BY job, commit_id
		)
		SELECT j.number, j.state = ?, j.words, COALESCE(p.number, 0), COALESCE(p.title, ''),
			COALESCE(a.validating, 0), COALESCE(a.writing, 0),
			COALESCE(f.needs_you, 0), COALESCE(f.open_blocking, 0),
			COALESCE(v.validated, 0), COALESCE(x.pushed, 0), COALESCE(p.head = b.tip, 0)
		FROM jobs j
		LEFT JOIN job_branches b ON b.job = j.number
		LEFT JOIN latest_pr p ON p.job = j.number
		LEFT JOIN assignment_state a ON a.job = j.number
		LEFT JOIN finding_state f ON f.job = j.number
		LEFT JOIN validations v ON v.job = j.number AND v.commit_id = b.tip
		LEFT JOIN pushed_heads x ON x.job = j.number AND x.commit_id = b.tip
		ORDER BY j.number`, AssignmentAccepted, AssignmentCancelled, AssignmentAccepted, AssignmentCancelled, FindingNeedsYou, FindingBlocking, MessageValidation, ReviewEarly, PushPushed, JobEnded)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	works := []JobWork{}
	for rows.Next() {
		w := JobWork{Assignments: map[string]int{}}
		if err := rows.Scan(&w.Number, &w.Ended, &w.Words, &w.PullRequest, &w.Title, &w.Validating, &w.Writing, &w.NeedsYou, &w.OpenBlocking, &w.Validated, &w.Pushed, &w.PRCurrent); err != nil {
			return nil, err
		}
		works = append(works, w)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	counts, err := tx.Query(`SELECT job, state, count(*) FROM assignments GROUP BY job, state`)
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

// NeedsYouFinding is a needs-you finding of a job that has not ended, for the Dashboard to say the factory waits on the user for it.
type NeedsYouFinding struct {
	ID   int64
	Job  int64
	Text string
	At   time.Time
}

// NeedsYouFindings returns the needs-you findings of the jobs that have not
// ended, oldest first. A finding has no state of its own, so the end of its
// job ends its wait. It only reads.
func (s *Store) NeedsYouFindings() ([]NeedsYouFinding, error) {
	rows, err := s.db.Query(`SELECT f.id, f.job, f.text, f.created_at FROM findings f
		JOIN jobs j ON j.number = f.job
		WHERE f.kind = ? AND j.state <> ? ORDER BY f.id`, FindingNeedsYou, JobEnded)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []NeedsYouFinding{}
	for rows.Next() {
		var f NeedsYouFinding
		var at string
		if err := rows.Scan(&f.ID, &f.Job, &f.Text, &at); err != nil {
			return nil, err
		}
		if f.At, err = time.Parse(time.RFC3339Nano, at); err != nil {
			return nil, err
		}
		list = append(list, f)
	}
	return list, rows.Err()
}

// JobDetail is what a job's records say of its branch, pull request and work,
// for the daemon to put in the Dashboard's job detail. Every item carries the
// time it last changed.
type JobDetail struct {
	// Branch and Head are the job branch's name and tip, empty for a job with
	// no repository.
	Branch, Head string
	// PullRequest is the latest pull request recorded for the job, nil when it
	// has none; Checks are the latest observation of the checks on a pull
	// request's head, nil when there is none.
	PullRequest *PullRequest
	Checks      *ChecksObservation
	// Findings, Assignments and Handoffs are the job's, oldest first.
	Findings    []FindingWork
	Assignments []AssignmentWork
	Handoffs    []HandoffWork
}

// FindingWork is a finding with when it was recorded or, for a blocking
// finding, cleared.
type FindingWork struct {
	ID, Assignment, Report, ClearedBy int64
	Kind, Text, Commit                string
	Cleared                           bool
	CreatedAt, ClearedAt, At          time.Time
}

// AssignmentWork is an assignment with when it last changed, and the state of
// the latest dispatch that carried it to its worker, empty when it has none.
type AssignmentWork struct {
	ID        int64
	Owner     string
	Worker    string
	Outcome   string
	Criteria  []string
	State     string
	ReadOnly  bool
	Commit    string
	Skill     string
	Skills    []string
	Dispatch  string
	CreatedAt time.Time
	At        time.Time
}

// HandoffWork is a handoff with when it was sent or answered.
type HandoffWork struct {
	ID          int64
	Sender      string
	Receiver    string
	Outcome     string
	Decisions   string
	Evidence    string
	Constraints string
	Permissions string
	Criteria    []string
	State       string
	Answer      string
	Head        string
	CreatedAt   time.Time
	At          time.Time
}

// assignmentChanges are the journal kinds that change an assignment: they
// name it in their data's assignment field. Its own state is not stamped, so
// when it last changed is the latest of these.
var assignmentChanges = []string{
	KindResultSubmitted, KindBlockedReported, KindResultAccepted, KindResultRejected, KindResultReturned,
	KindCancellationRequested, KindAssignmentCancelled, KindAssignmentResumed, KindAssignmentNeedsReconciliation,
}

func jobChangeTimes(tx *sql.Tx, job int64) (map[int64]string, map[int64]string, error) {
	changes := "'" + strings.Join(assignmentChanges, "','") + "'"
	rows, err := tx.Query(`SELECT kind, json_extract(data, '$.assignment'), json_extract(data, '$.id'), at
		FROM journal WHERE json_extract(data, '$.job') = ? AND kind IN (`+changes+`, ?, ?) ORDER BY id`, job, KindHandoffSent, KindHandoffAnswered)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	assignments, handoffs := map[int64]string{}, map[int64]string{}
	for rows.Next() {
		var kind, at string
		var assignment, handoff sql.NullInt64
		if err := rows.Scan(&kind, &assignment, &handoff, &at); err != nil {
			return nil, nil, err
		}
		if kind == KindHandoffSent || kind == KindHandoffAnswered {
			if handoff.Valid {
				handoffs[handoff.Int64] = at
			}
		} else if assignment.Valid {
			assignments[assignment.Int64] = at
		}
	}
	return assignments, handoffs, rows.Err()
}

// JobWorkDetail reads the job's stage inputs and detail in one snapshot.
func (s *Store) JobWorkDetail(job int64) (JobWork, JobDetail, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return JobWork{}, JobDetail{}, err
	}
	defer tx.Rollback()
	works, err := jobWorks(tx)
	if err != nil {
		return JobWork{}, JobDetail{}, err
	}
	for _, work := range works {
		if work.Number == job {
			detail, err := jobDetail(tx, job)
			if err != nil {
				return JobWork{}, JobDetail{}, err
			}
			return work, detail, tx.Commit()
		}
	}
	return JobWork{}, JobDetail{}, fmt.Errorf("job %d does not exist", job)
}

func jobDetail(tx *sql.Tx, job int64) (JobDetail, error) {
	var d JobDetail
	if _, err := jobFor(tx, job); err != nil {
		return JobDetail{}, err
	}
	if b, err := jobBranch(tx, job); err == nil {
		d.Branch, d.Head = b.Name, b.Tip
	} else if !errors.Is(err, sql.ErrNoRows) {
		return JobDetail{}, err
	}
	var pr PullRequest
	var at string
	switch err := tx.QueryRow(`SELECT id, job, number, url, title, draft, not_ready, branch, head, state, leader, dispatch, effect, created_at FROM pull_requests WHERE job = ? ORDER BY id DESC LIMIT 1`, job).
		Scan(&pr.ID, &pr.Job, &pr.Number, &pr.URL, &pr.Title, &pr.Draft, &pr.NotReady, &pr.Branch, &pr.Head, &pr.State, &pr.Leader, &pr.Dispatch, &pr.Effect, &at); {
	case err == nil:
		if pr.At, err = time.Parse(time.RFC3339Nano, at); err != nil {
			return JobDetail{}, err
		}
		d.PullRequest = &pr
	case !errors.Is(err, sql.ErrNoRows):
		return JobDetail{}, err
	}
	if o, err := latestChecks(tx, job); err == nil {
		if d.PullRequest != nil && o.Number == d.PullRequest.Number && o.Head == d.PullRequest.Head && o.Head == d.Head {
			d.Checks = &o
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return JobDetail{}, err
	}

	rows, err := tx.Query(`SELECT id, assignment, report, kind, text, commit_id, COALESCE(cleared_by, 0), cleared_at, created_at FROM findings WHERE job = ? ORDER BY id`, job)
	if err != nil {
		return JobDetail{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var f FindingWork
		var cleared sql.NullString
		var created string
		if err := rows.Scan(&f.ID, &f.Assignment, &f.Report, &f.Kind, &f.Text, &f.Commit, &f.ClearedBy, &cleared, &created); err != nil {
			return JobDetail{}, err
		}
		f.Cleared = f.ClearedBy != 0
		if f.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
			return JobDetail{}, err
		}
		f.At = f.CreatedAt
		if cleared.Valid {
			if f.ClearedAt, err = time.Parse(time.RFC3339Nano, cleared.String); err != nil {
				return JobDetail{}, err
			}
			f.At = f.ClearedAt
		}
		d.Findings = append(d.Findings, f)
	}
	if err := rows.Err(); err != nil {
		return JobDetail{}, err
	}
	rows.Close()

	assignmentTimes, handoffTimes, err := jobChangeTimes(tx, job)
	if err != nil {
		return JobDetail{}, err
	}
	arows, err := tx.Query(`SELECT a.id, a.owner, a.worker, a.outcome, a.criteria, a.state, a.read_only, a.skill, a.skills, COALESCE(w.base, ''),
			COALESCE(x.state, ''), a.created_at,
			COALESCE(x.updated_at, a.created_at)
		FROM assignments a
		LEFT JOIN workspaces w ON w.assignment = a.id
		LEFT JOIN dispatches x ON x.id = (SELECT d.id FROM dispatches d JOIN messages m ON m.id = d.message
			WHERE m.assignment = a.id AND m.recipient = a.worker AND d.agent = a.worker ORDER BY d.id DESC LIMIT 1)
		WHERE a.job = ? ORDER BY a.id`, job)
	if err != nil {
		return JobDetail{}, err
	}
	defer arows.Close()
	for arows.Next() {
		var a AssignmentWork
		var criteria, skills, base, created, dispatched string
		if err := arows.Scan(&a.ID, &a.Owner, &a.Worker, &a.Outcome, &criteria, &a.State, &a.ReadOnly, &a.Skill, &skills, &base, &a.Dispatch, &created, &dispatched); err != nil {
			return JobDetail{}, err
		}
		if err := json.Unmarshal([]byte(criteria), &a.Criteria); err != nil {
			return JobDetail{}, err
		}
		if err := json.Unmarshal([]byte(skills), &a.Skills); err != nil {
			return JobDetail{}, err
		}
		if a.ReadOnly {
			a.Commit = base
		}
		if a.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
			return JobDetail{}, err
		}
		stamps := []string{created, dispatched}
		if changed, ok := assignmentTimes[a.ID]; ok {
			stamps = append(stamps, changed)
		}
		for _, stamp := range stamps {
			at, err := time.Parse(time.RFC3339Nano, stamp)
			if err != nil {
				return JobDetail{}, err
			}
			if at.After(a.At) {
				a.At = at
			}
		}
		d.Assignments = append(d.Assignments, a)
	}
	if err := arows.Err(); err != nil {
		return JobDetail{}, err
	}
	arows.Close()

	hrows, err := tx.Query(`SELECT h.id, h.sender, h.receiver, h.outcome, h.decisions, h.evidence, h.constraints, h.permissions, h.criteria, h.state, h.answer, h.head, h.created_at
		FROM handoffs h WHERE h.job = ? ORDER BY h.id`, job)
	if err != nil {
		return JobDetail{}, err
	}
	defer hrows.Close()
	for hrows.Next() {
		var h HandoffWork
		var criteria, at string
		if err := hrows.Scan(&h.ID, &h.Sender, &h.Receiver, &h.Outcome, &h.Decisions, &h.Evidence, &h.Constraints, &h.Permissions, &criteria, &h.State, &h.Answer, &h.Head, &at); err != nil {
			return JobDetail{}, err
		}
		if h.CreatedAt, err = time.Parse(time.RFC3339Nano, at); err != nil {
			return JobDetail{}, err
		}
		if err := json.Unmarshal([]byte(criteria), &h.Criteria); err != nil {
			return JobDetail{}, err
		}
		if changed, ok := handoffTimes[h.ID]; ok {
			at = changed
		}
		if h.At, err = time.Parse(time.RFC3339Nano, at); err != nil {
			return JobDetail{}, err
		}
		d.Handoffs = append(d.Handoffs, h)
	}
	if err := hrows.Err(); err != nil {
		return JobDetail{}, err
	}
	return d, nil
}

// jobFor returns job, or an error saying it does not exist.
func jobFor(q queryer, number int64) (Job, error) {
	j, err := job(q, number)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, fmt.Errorf("job %d does not exist", number)
	}
	return j, err
}
