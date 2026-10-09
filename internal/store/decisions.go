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

// The decisions an owning leader makes about an assignment.
const (
	DecisionAccepted  = "accepted"
	DecisionRejected  = "rejected"
	DecisionCancelled = "cancelled"
)

// Decision is what the owning leader decided about an assignment: to accept
// or reject its result, or to cancel it, with the leader and its reasons.
type Decision struct {
	ID         int64  `json:"id"`
	Assignment int64  `json:"assignment"`
	Job        int64  `json:"job"`
	Kind       string `json:"kind"`
	Leader     string `json:"leader"`
	// Report is the result a result decision is about, and Commit its
	// commit; a cancellation has neither.
	Report    int64     `json:"report,omitempty"`
	Commit    string    `json:"commit,omitempty"`
	Reasons   []string  `json:"reasons"`
	CreatedAt time.Time `json:"created_at"`
}

// Acceptance is the owning leader's acceptance of the result its assignment
// is submitted with.
type Acceptance struct {
	Assignment int64
	Leader     string
	Reasons    []string
	// FromTip is the job branch's tip that the daemon fast-forwarded it
	// from: the acceptance is recorded only while the store still holds it.
	FromTip string
}

// decided checks that leader owns the assignment and gave reasons, and
// returns the assignment.
func decided(tx *sql.Tx, id int64, leader string, reasons []string) (Assignment, error) {
	if len(reasons) == 0 {
		return Assignment{}, errors.New("a decision gives its reasons")
	}
	for _, reason := range reasons {
		if strings.TrimSpace(reason) == "" {
			return Assignment{}, errors.New("each reason must have text")
		}
	}
	a, err := assignment(tx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return Assignment{}, fmt.Errorf("assignment %d does not exist", id)
	}
	if err != nil {
		return Assignment{}, err
	}
	if a.Owner != leader {
		return Assignment{}, fmt.Errorf("assignment %d is owned by %s, not %s", a.ID, a.Owner, leader)
	}
	return a, nil
}

// latestResult returns the latest report of assignment, which must be a
// result.
func latestResult(q queryer, assignment int64) (Report, error) {
	var id int64
	if err := q.QueryRow(`SELECT id FROM reports WHERE assignment = ? ORDER BY id DESC LIMIT 1`, assignment).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Report{}, fmt.Errorf("assignment %d has no result", assignment)
		}
		return Report{}, err
	}
	r, err := report(q, id)
	if err != nil {
		return Report{}, err
	}
	if r.Kind != ReportResult {
		return Report{}, fmt.Errorf("assignment %d has no result: its latest report is %s", assignment, r.Kind)
	}
	return r, nil
}

// LatestResult returns the result assignment is submitted with.
func (s *Store) LatestResult(assignment int64) (Report, error) {
	return latestResult(s.db, assignment)
}

// decide records d as the decision it is, and journals it as kind.
func decide(tx *sql.Tx, d Decision, kind string, at time.Time) (Decision, error) {
	reasons, err := json.Marshal(d.Reasons)
	if err != nil {
		return Decision{}, err
	}
	d.CreatedAt = at.UTC()
	result, err := tx.Exec(`INSERT INTO decisions (assignment, report, kind, leader, reasons, commit_id, created_at) VALUES (?, NULLIF(?, 0), ?, ?, ?, ?, ?)`,
		d.Assignment, d.Report, d.Kind, d.Leader, string(reasons), d.Commit, timestamp(at))
	if err != nil {
		return Decision{}, err
	}
	if d.ID, err = result.LastInsertId(); err != nil {
		return Decision{}, err
	}
	if _, err := appendEntry(tx, at, kind, d); err != nil {
		return Decision{}, err
	}
	return d, nil
}

// ResultAccepted records the owning leader's acceptance of the result its
// assignment is submitted with, and the job branch moving to the result's
// commit, and journals them together: the assignment ends and the job
// branch's tip is the accepted commit. The daemon fast-forwarded the branch
// from in.FromTip, which the store must still hold as its tip.
func (s *Store) ResultAccepted(in Acceptance, at time.Time) (Decision, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Decision{}, err
	}
	defer tx.Rollback()
	a, err := decided(tx, in.Assignment, in.Leader, in.Reasons)
	if err != nil {
		return Decision{}, err
	}
	if a.State != AssignmentSubmitted {
		return Decision{}, fmt.Errorf("assignment %d is %s, so it has no result to accept", a.ID, a.State)
	}
	r, err := latestResult(tx, a.ID)
	if err != nil {
		return Decision{}, err
	}
	b, err := jobBranch(tx, a.Job)
	if err != nil {
		return Decision{}, fmt.Errorf("job %d has no job branch: %w", a.Job, err)
	}
	if b.Tip != in.FromTip {
		return Decision{}, fmt.Errorf("the job branch's tip is %s, not %s", b.Tip, in.FromTip)
	}
	if _, err := tx.Exec(`UPDATE job_branches SET tip = ? WHERE job = ?`, r.Commit, a.Job); err != nil {
		return Decision{}, err
	}
	if _, err := tx.Exec(`UPDATE assignments SET state = ? WHERE id = ?`, AssignmentAccepted, a.ID); err != nil {
		return Decision{}, err
	}
	if _, err := appendEntry(tx, at, KindJobBranchMoved, map[string]any{
		"job": a.Job, "branch": b.Name, "from": b.Tip, "to": r.Commit, "assignment": a.ID, "report": r.ID,
	}); err != nil {
		return Decision{}, err
	}
	d, err := decide(tx, Decision{Assignment: a.ID, Job: a.Job, Kind: DecisionAccepted, Leader: in.Leader, Report: r.ID, Commit: r.Commit, Reasons: in.Reasons}, KindResultAccepted, at)
	if err != nil {
		return Decision{}, err
	}
	return d, tx.Commit()
}

// ResultRejected records the owning leader's rejection of the result its
// assignment is submitted with, returns the assignment to active, and sends
// the reasons to the same worker as a message with a new dispatch, which is
// returned.
func (s *Store) ResultRejected(id int64, leader string, reasons []string, at time.Time) (Decision, Dispatch, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Decision{}, Dispatch{}, err
	}
	defer tx.Rollback()
	a, err := decided(tx, id, leader, reasons)
	if err != nil {
		return Decision{}, Dispatch{}, err
	}
	if a.State != AssignmentSubmitted {
		return Decision{}, Dispatch{}, fmt.Errorf("assignment %d is %s, so it has no result to reject", a.ID, a.State)
	}
	r, err := latestResult(tx, a.ID)
	if err != nil {
		return Decision{}, Dispatch{}, err
	}
	if _, err := tx.Exec(`UPDATE assignments SET state = ? WHERE id = ?`, AssignmentActive, a.ID); err != nil {
		return Decision{}, Dispatch{}, err
	}
	d, err := decide(tx, Decision{Assignment: a.ID, Job: a.Job, Kind: DecisionRejected, Leader: leader, Report: r.ID, Commit: r.Commit, Reasons: reasons}, KindResultRejected, at)
	if err != nil {
		return Decision{}, Dispatch{}, err
	}
	dispatch, err := createMessage(tx, messageRef{assignment: a.ID, report: r.ID}, a.Job, leader, a.Worker, MessageRejection, strings.Join(reasons, "\n"), at)
	if err != nil {
		return Decision{}, Dispatch{}, err
	}
	return d, dispatch, tx.Commit()
}

// CancellationRequested records the owning leader's cancellation of an
// assignment that is active or submitted, with its reasons. The assignment
// is cancelling, and still holds its worker and workspace, until the worker
// has pushed its assignment branch and stopped. The worker is told with a
// message and a new dispatch, which is returned.
func (s *Store) CancellationRequested(id int64, leader string, reasons []string, at time.Time) (Decision, Dispatch, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Decision{}, Dispatch{}, err
	}
	defer tx.Rollback()
	a, err := decided(tx, id, leader, reasons)
	if err != nil {
		return Decision{}, Dispatch{}, err
	}
	if a.State != AssignmentActive && a.State != AssignmentSubmitted {
		return Decision{}, Dispatch{}, fmt.Errorf("assignment %d is %s, so it cannot be cancelled", a.ID, a.State)
	}
	if _, err := tx.Exec(`UPDATE assignments SET state = ? WHERE id = ?`, AssignmentCancelling, a.ID); err != nil {
		return Decision{}, Dispatch{}, err
	}
	d, err := decide(tx, Decision{Assignment: a.ID, Job: a.Job, Kind: DecisionCancelled, Leader: leader, Reasons: reasons}, KindCancellationRequested, at)
	if err != nil {
		return Decision{}, Dispatch{}, err
	}
	dispatch, err := createMessage(tx, messageRef{assignment: a.ID}, a.Job, leader, a.Worker, MessageCancellation, strings.Join(reasons, "\n"), at)
	if err != nil {
		return Decision{}, Dispatch{}, err
	}
	return d, dispatch, tx.Commit()
}

// Cancellation is how a cancelled assignment's worker left its workspace
// when it stopped.
type Cancellation struct {
	// Commit is the commit the workspace was at, and Pushed whether origin's
	// assignment branch holds it.
	Commit string `json:"commit"`
	Pushed bool   `json:"pushed"`
}

// AssignmentCancelled ends a cancelling assignment, now its worker has
// stopped, and journals how the worker left its workspace.
func (s *Store) AssignmentCancelled(id int64, left Cancellation, at time.Time) (Assignment, error) {
	var a Assignment
	err := s.change(at, func(tx *sql.Tx) (string, any, error) {
		var err error
		if a, err = assignment(tx, id); err != nil {
			return "", nil, fmt.Errorf("assignment %d: %w", id, err)
		}
		if a.State != AssignmentCancelling {
			return "", nil, fmt.Errorf("assignment %d is %s, not cancelling", a.ID, a.State)
		}
		if !left.Pushed {
			return "", nil, fmt.Errorf("assignment %d cannot be cancelled before its workspace commit is pushed", a.ID)
		}
		if _, err := tx.Exec(`UPDATE assignments SET state = ? WHERE id = ?`, AssignmentCancelled, a.ID); err != nil {
			return "", nil, err
		}
		a.State = AssignmentCancelled
		return KindAssignmentCancelled, struct {
			Assignment int64  `json:"assignment"`
			Job        int64  `json:"job"`
			Worker     string `json:"worker"`
			Cancellation
		}{a.ID, a.Job, a.Worker, left}, nil
	})
	return a, err
}

// WorkspaceRemoved journals that the daemon removed the workspace of
// assignment, whose work is on the job branch.
func (s *Store) WorkspaceRemoved(a Assignment, at time.Time) error {
	return s.change(at, func(tx *sql.Tx) (string, any, error) {
		return KindWorkspaceRemoved, struct {
			Workspace
			Job    int64  `json:"job"`
			Worker string `json:"worker"`
		}{a.Workspace, a.Job, a.Worker}, nil
	})
}

// Decisions returns the decisions about assignment, oldest first.
func (s *Store) Decisions(assignment int64) ([]Decision, error) {
	rows, err := s.db.Query(`SELECT d.id, d.assignment, a.job, COALESCE(d.report, 0), d.kind, d.leader, d.reasons, d.commit_id, d.created_at FROM decisions d JOIN assignments a ON a.id = d.assignment WHERE d.assignment = ? ORDER BY d.id`, assignment)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	decisions := []Decision{}
	for rows.Next() {
		var d Decision
		var reasons, created string
		if err := rows.Scan(&d.ID, &d.Assignment, &d.Job, &d.Report, &d.Kind, &d.Leader, &reasons, &d.Commit, &created); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(reasons), &d.Reasons); err != nil {
			return nil, err
		}
		if d.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
			return nil, err
		}
		decisions = append(decisions, d)
	}
	return decisions, rows.Err()
}

// DispatchKind returns the kind of message the dispatch carries and the
// assignment it is about, which is zero when it is about none.
func (s *Store) DispatchKind(dispatch int64) (kind string, assignment int64, err error) {
	var about sql.NullInt64
	err = s.db.QueryRow(`SELECT m.kind, m.assignment FROM dispatches d JOIN messages m ON m.id = d.message WHERE d.id = ?`, dispatch).Scan(&kind, &about)
	return kind, about.Int64, err
}
