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
	// EndsJob is the owning leader's acceptance of the job's outcome, which
	// ends a job with no repository.
	EndsJob bool
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
func latestResult(q querier, assignment int64) (Report, error) {
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
	if a.Scratch {
		return Decision{}, fmt.Errorf("assignment %d works in a scratch workspace, so its acceptance moves no job branch: it is a scratch acceptance", a.ID)
	}
	if in.EndsJob {
		return Decision{}, fmt.Errorf("job %d has a repository, so it does not end on its leader's acceptance: it ends in its delivery", a.Job)
	}
	pending, err := blockingDecisions(tx, a.ID)
	if err != nil {
		return Decision{}, err
	}
	if len(pending) > 0 {
		return Decision{}, waitingOnDecision(a.ID, pending)
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

// ResultReturned returns the result its assignment is submitted with to the
// same worker, because the job branch moved after the result was submitted:
// the result's commit no longer has the branch's tip in its history, which is
// now tip. The assignment is active again, and the worker is told with a
// message and a new dispatch, which is returned, to take in the new tip and
// submit again. It is not a rejection: it records no decision, and leader
// only runs the acceptance that found the branch moved (02 rule 31, 05 rule
// 19).
func (s *Store) ResultReturned(id int64, leader, tip string, at time.Time) (Dispatch, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Dispatch{}, err
	}
	defer tx.Rollback()
	a, err := assignment(tx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return Dispatch{}, fmt.Errorf("assignment %d does not exist", id)
	}
	if err != nil {
		return Dispatch{}, err
	}
	if a.Owner != leader {
		return Dispatch{}, fmt.Errorf("assignment %d is owned by %s, not %s", a.ID, a.Owner, leader)
	}
	if a.State != AssignmentSubmitted {
		return Dispatch{}, fmt.Errorf("assignment %d is %s, so it has no result to return", a.ID, a.State)
	}
	r, err := latestResult(tx, a.ID)
	if err != nil {
		return Dispatch{}, err
	}
	b, err := jobBranch(tx, a.Job)
	if err != nil {
		return Dispatch{}, fmt.Errorf("job %d has no job branch: %w", a.Job, err)
	}
	if b.Tip != tip {
		return Dispatch{}, fmt.Errorf("the job branch's tip is %s, not %s", b.Tip, tip)
	}
	if _, err := tx.Exec(`UPDATE assignments SET state = ? WHERE id = ?`, AssignmentActive, a.ID); err != nil {
		return Dispatch{}, err
	}
	if _, err := appendEntry(tx, at, KindResultReturned, map[string]any{
		"job": a.Job, "assignment": a.ID, "report": r.ID, "commit": r.Commit, "branch": b.Name, "reported_tip": r.JobTip, "tip": tip, "leader": leader,
	}); err != nil {
		return Dispatch{}, err
	}
	body := fmt.Sprintf("Job branch %s moved to %s after you submitted your result at %s, so your result does not have its tip in its history. This is not a rejection and nothing is wrong with your work: take the new tip in (merge it into your assignment branch, resolving any conflicts), run the checks again, commit, push your assignment branch, record the push, and submit again with `asmai result`.", b.Name, tip, r.Commit)
	dispatch, err := createMessage(tx, messageRef{assignment: a.ID, report: r.ID}, a.Job, leader, a.Worker, MessageMovedTip, body, at)
	if err != nil {
		return Dispatch{}, err
	}
	return dispatch, tx.Commit()
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
	d, dispatch, err := requestCancellation(tx, a, leader, reasons, at)
	if err != nil {
		return Decision{}, Dispatch{}, err
	}
	return d, dispatch, tx.Commit()
}

// requestCancellation makes the active or submitted assignment a cancelling,
// records leader's decision with its reasons, and tells the worker with a
// message and a new dispatch, which is returned.
func requestCancellation(tx *sql.Tx, a Assignment, leader string, reasons []string, at time.Time) (Decision, Dispatch, error) {
	if a.State != AssignmentActive && a.State != AssignmentSubmitted {
		return Decision{}, Dispatch{}, fmt.Errorf("assignment %d is %s, so it cannot be cancelled", a.ID, a.State)
	}
	return cancelAssignment(tx, a, leader, reasons, at)
}

// cancelAssignment makes a cancelling, whatever its state, records leader's
// decision with its reasons, and tells the worker with a message and a new
// dispatch, which is returned.
func cancelAssignment(tx *sql.Tx, a Assignment, leader string, reasons []string, at time.Time) (Decision, Dispatch, error) {
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
	return d, dispatch, nil
}

// AssignmentRedispatched sends the assignment id, which leader owns and which
// is active or needs reconciliation, to its worker again, with the owning
// leader's reasons, in a new dispatch that is returned: the worker's earlier
// dispatches are never reused. An assignment that needed reconciliation is
// active again. It is journaled, but it is no decision on a result.
func (s *Store) AssignmentRedispatched(id int64, leader string, reasons []string, at time.Time) (Dispatch, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Dispatch{}, err
	}
	defer tx.Rollback()
	a, err := decided(tx, id, leader, reasons)
	if err != nil {
		return Dispatch{}, err
	}
	if a.State != AssignmentActive && a.State != AssignmentNeedsReconciliation {
		return Dispatch{}, fmt.Errorf("assignment %d is %s, so it is not sent to its worker again: only an active assignment, or one that needs reconciliation, is", a.ID, a.State)
	}
	w, err := workerDispatch(tx, a.ID)
	if err != nil {
		return Dispatch{}, err
	}
	rows, err := tx.Query(`SELECT d.id, d.generation, d.state, m.kind, m.fetched_at IS NULL
		FROM messages m JOIN dispatches d ON d.message = m.id
		WHERE m.assignment = ? AND m.recipient = ?
		AND m.kind IN (?, ?, ?, ?, ?, ?) AND d.state IN (?, ?, ?) ORDER BY d.id`,
		a.ID, a.Worker, MessageAssignment, MessageRedispatch, MessageRejection, MessageMovedTip, MessageResumption, MessageDecisionAnswer,
		DispatchCreated, DispatchNudged, DispatchUnknown)
	if err != nil {
		return Dispatch{}, err
	}
	var unknown []Dispatch
	for rows.Next() {
		var d Dispatch
		var kind string
		var unfetched bool
		if err := rows.Scan(&d.ID, &d.Generation, &d.State, &kind, &unfetched); err != nil {
			rows.Close()
			return Dispatch{}, err
		}
		if d.State == DispatchUnknown && a.State == AssignmentNeedsReconciliation && kind != MessageDecisionAnswer {
			unknown = append(unknown, d)
		} else if d.State == DispatchUnknown && a.State == AssignmentNeedsReconciliation && unfetched {
			// The answer is the user's or the owner's word: it is never
			// dropped with the unknown work, only by the owner's choice.
			rows.Close()
			return Dispatch{}, fmt.Errorf("assignment %[1]d holds decision answer dispatch %[2]d, which %[3]s may not have seen; deliver it again with `asmai answer redeliver --dispatch %[2]d --reason <why>`, or retire it with `asmai answer retire --dispatch %[2]d --reason <why>` and then redispatch", a.ID, d.ID, a.Worker)
		} else if unfetched {
			rows.Close()
			return Dispatch{}, fmt.Errorf("assignment %d has queued %s dispatch %d for %s; wait for the worker to receive it or cancel the assignment", a.ID, kind, d.ID, a.Worker)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return Dispatch{}, err
	}
	if a.State == AssignmentActive && w.Dispatch.State != DispatchStopped {
		return Dispatch{}, fmt.Errorf("assignment %d's latest worker dispatch %d is %s; wait for it to stop before redispatching", a.ID, w.Dispatch.ID, w.Dispatch.State)
	}
	for _, d := range unknown {
		if _, err := tx.Exec(`UPDATE dispatches SET state = ?, closed = ?, updated_at = ? WHERE id = ?`, DispatchStopped, ClosedRedispatched, timestamp(at), d.ID); err != nil {
			return Dispatch{}, err
		}
		if _, err := appendEntry(tx, at, KindDispatchChanged, map[string]any{
			"id": d.ID, "agent": a.Worker, "generation": d.Generation, "from": DispatchUnknown, "state": DispatchStopped, "closed": ClosedRedispatched, "assignment": a.ID,
		}); err != nil {
			return Dispatch{}, err
		}
	}
	if _, err := tx.Exec(`UPDATE assignments SET state = ? WHERE id = ?`, AssignmentActive, a.ID); err != nil {
		return Dispatch{}, err
	}
	body := fmt.Sprintf("%[1]s sends assignment %[2]d of job %[3]d to you again, in a new dispatch; your earlier dispatch, %[4]d, is not reused.\nReasons from %[1]s:\n%[5]s\n\n"+
		"Load `asmai brief %[3]d`, look at your workspace and at the effects you have recorded, and continue the assignment from where you stopped. Never repeat an effect you already made. If your assignment has a branch and a push of it failed, push it again and record the push. Submit a result with `asmai result`, or if you cannot go on, `asmai blocked --reason <why> --needs <what would unblock you>`, which needs no push.",
		leader, a.ID, a.Job, w.Dispatch.ID, strings.Join(reasons, "\n"))
	dispatch, err := createMessage(tx, messageRef{assignment: a.ID}, a.Job, leader, a.Worker, MessageRedispatch, body, at)
	if err != nil {
		return Dispatch{}, err
	}
	if _, err := appendEntry(tx, at, KindAssignmentRedispatched, map[string]any{
		"assignment": a.ID, "job": a.Job, "worker": a.Worker, "owner": a.Owner, "from": a.State,
		"previous": w.Dispatch.ID, "dispatch": dispatch.ID, "reasons": reasons,
	}); err != nil {
		return Dispatch{}, err
	}
	return dispatch, tx.Commit()
}

// WorkerStalled tells the owning leader of the active assignment id, with a
// message with body and a dispatch of its own, which is returned, that its
// worker's turn for dispatch ended with no result and no blocked report. The
// assignment is not changed.
func (s *Store) WorkerStalled(id, dispatch int64, body string, at time.Time) (Dispatch, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Dispatch{}, err
	}
	defer tx.Rollback()
	w, err := workerDispatch(tx, id)
	if err != nil {
		return Dispatch{}, fmt.Errorf("assignment %d: %w", id, err)
	}
	a := w.Assignment
	if a.State != AssignmentActive {
		return Dispatch{}, fmt.Errorf("assignment %d is %s, not active", a.ID, a.State)
	}
	if w.Dispatch.ID != dispatch {
		return Dispatch{}, fmt.Errorf("dispatch %d is no longer the worker's latest dispatch for assignment %d", dispatch, a.ID)
	}
	if w.Report != "" {
		return Dispatch{}, fmt.Errorf("dispatch %d ended in a report", dispatch)
	}
	told, err := createMessage(tx, messageRef{assignment: a.ID}, a.Job, Daemon, a.Owner, MessageStalled, body, at)
	if err != nil {
		return Dispatch{}, err
	}
	return told, tx.Commit()
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
		// A read-only assignment has no branch to push, and nor does a
		// scratch one.
		if !left.Pushed && !a.ReadOnly && !a.Scratch {
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
// assignment: a writing assignment's, whose work is on the job branch, or a
// read-only assignment's, which ended.
func (s *Store) WorkspaceRemoved(a Assignment, at time.Time) error {
	kind := KindWorkspaceRemoved
	if a.ReadOnly {
		kind = KindReadOnlyWorkspaceRemoved
	}
	return s.change(at, func(tx *sql.Tx) (string, any, error) {
		return kind, struct {
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
