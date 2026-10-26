// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// MessageJobCancelled tells a leader that a job it has work in was cancelled
// at the user's word, and Coordination's leader what the cancellation did.
const MessageJobCancelled = "job.cancelled"

// KindJobCancelled records a job cancelled at the user's word: the witnessed
// message whose words are the reason, and what the cancellation did.
const KindJobCancelled = "job.cancelled"

// EffectPullRequestClosed is the daemon's own effect of closing a cancelled
// job's open pull request, with a comment giving the reason.
const EffectPullRequestClosed = "pull-request-closed"

// JobCancel is Coordination's leader cancelling a job at the user's word.
type JobCancel struct {
	Job int64
	// Witness is the witnessed message holding the user's words, or with
	// Latest the latest one submitted to Coordination's leader.
	Witness int64
	Latest  bool
	// Closed is the job's pull request the daemon closed for the
	// cancellation, nil when it closed none.
	Closed *PullRequest
}

// JobCancellation is what cancelling a job did.
type JobCancellation struct {
	Job Job `json:"job"`
	// Cancelling are the assignments that were live or needed
	// reconciliation: each is cancelling until its worker has pushed its
	// assignment branch and stopped.
	Cancelling []Assignment `json:"cancelling"`
	// Discarded are the unreviewed results, of assignments that were
	// submitted, which no leader accepts now.
	Discarded []int64 `json:"discarded"`
	// Withdrawn are the decision requests that were pending.
	Withdrawn []DecisionRequest `json:"withdrawn"`
	// Closed is the daemon's effect of closing the pull request, nil when it
	// closed none.
	Closed *Effect `json:"closed,omitempty"`
	// Dispatches are the cancellations to the workers and the messages to
	// the leaders, to nudge.
	Dispatches []Dispatch `json:"dispatches"`
}

// CancelReason returns the reason for cancelling a job: the user's own words
// in the witnessed message witness, or with latest the latest one, submitted
// to Coordination's leader, with the message's journal ID. It refuses a
// message that is not such words, or has no words to give as the reason.
func (s *Store) CancelReason(witness int64, latest bool) (int64, string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, "", err
	}
	defer tx.Rollback()
	return cancelReason(tx, witness, latest)
}

func cancelReason(tx *sql.Tx, witness int64, latest bool) (int64, string, error) {
	witness, witnessed, err := coordinationWitnessed(tx, witness, latest)
	if err != nil {
		return 0, "", err
	}
	reason := strings.TrimSpace(witnessed.Text)
	if reason == "" {
		return 0, "", fmt.Errorf("witnessed message %d has no words to give as the reason: cite the message in which the user asked for the cancellation", witness)
	}
	return witness, reason, nil
}

// CheckJobCancellable returns job if it can be cancelled, or why not: a job
// that does not exist, has ended or is already cancelled is refused.
func (s *Store) CheckJobCancellable(number int64) (Job, error) {
	return cancellableJob(s.db, number)
}

func cancellableJob(q queryer, number int64) (Job, error) {
	j, err := job(q, number)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, fmt.Errorf("job %d does not exist; `asmai jobs` lists the jobs", number)
	}
	if err != nil {
		return Job{}, err
	}
	switch j.State {
	case JobCancelled:
		return Job{}, fmt.Errorf("job %d is already cancelled (witnessed message %d): nothing is left to stop", j.Number, j.CancelWitness)
	case JobEnded:
		return Job{}, fmt.Errorf("job %d is finished: it has ended, so there is nothing left to cancel", j.Number)
	}
	return j, nil
}

// JobCancelled cancels the job c names at the user's word, in one step that
// journals it: the job ends as cancelled with the user's own words as the
// reason; every active or submitted assignment, and every one that needs
// reconciliation, is cancelling, with its worker told to push its assignment
// branch and stop, and the results submitted but not reviewed are discarded
// with it; the pending decision requests are
// withdrawn, which ends their pages; and the daemon's closing of the pull
// request is recorded as its effect. Coordination's leader is told what the
// cancellation did, and every other leader with work in the job that it was
// cancelled. Nothing changes when it is refused.
func (s *Store) JobCancelled(c JobCancel, at time.Time) (JobCancellation, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return JobCancellation{}, err
	}
	defer tx.Rollback()
	j, err := cancellableJob(tx, c.Job)
	if err != nil {
		return JobCancellation{}, err
	}
	witness, reason, err := cancelReason(tx, c.Witness, c.Latest)
	if err != nil {
		return JobCancellation{}, err
	}
	if _, err := tx.Exec(`UPDATE jobs SET state = ?, cancel_reason = ?, cancel_witness = ? WHERE number = ?`, JobCancelled, reason, witness, j.Number); err != nil {
		return JobCancellation{}, err
	}
	j.State, j.CancelReason, j.CancelWitness = JobCancelled, reason, witness
	out := JobCancellation{Job: j, Cancelling: []Assignment{}, Discarded: []int64{}, Dispatches: []Dispatch{}}

	ids, err := int64s(tx, `SELECT id FROM assignments WHERE job = ? ORDER BY id`, j.Number)
	if err != nil {
		return JobCancellation{}, err
	}
	reasons := []string{fmt.Sprintf("Job %d is cancelled at the user's word, in witnessed message %d: %q. Commit whatever you have, push your assignment branch, record the push, and stop. Submit no result.", j.Number, witness, reason)}
	for _, id := range ids {
		a, err := assignment(tx, id)
		if err != nil {
			return JobCancellation{}, err
		}
		switch a.State {
		case AssignmentActive, AssignmentSubmitted, AssignmentNeedsReconciliation:
		default:
			continue
		}
		if a.State == AssignmentSubmitted {
			r, err := latestResult(tx, a.ID)
			if err != nil {
				return JobCancellation{}, err
			}
			out.Discarded = append(out.Discarded, r.ID)
		}
		_, dispatch, err := cancelAssignment(tx, a, CoordinationLeader, reasons, at)
		if err != nil {
			return JobCancellation{}, err
		}
		a.State = AssignmentCancelling
		out.Cancelling = append(out.Cancelling, a)
		out.Dispatches = append(out.Dispatches, dispatch)
	}
	if out.Withdrawn, err = withdrawJobDecisions(tx, j.Number, at); err != nil {
		return JobCancellation{}, err
	}

	cancelling := make([]int64, len(out.Cancelling))
	for i, a := range out.Cancelling {
		cancelling[i] = a.ID
	}
	withdrawn := make([]int64, len(out.Withdrawn))
	for i, r := range out.Withdrawn {
		withdrawn[i] = r.Request
	}
	entry := map[string]any{"job": j.Number, "witness": witness, "reason": reason, "by": CoordinationLeader,
		"cancelling": cancelling, "discarded": out.Discarded, "withdrawn": withdrawn}
	if c.Closed != nil {
		entry["pull_request"] = c.Closed.Number
	}
	if _, err := appendEntry(tx, at, KindJobCancelled, entry); err != nil {
		return JobCancellation{}, err
	}

	told, err := createMessage(tx, messageRef{}, j.Number, Daemon, CoordinationLeader, MessageJobCancelled, cancellationSummary(out, c.Closed, cancelling, withdrawn), at)
	if err != nil {
		return JobCancellation{}, err
	}
	out.Dispatches = append(out.Dispatches, told)
	if c.Closed != nil {
		e := Effect{Job: j.Number, Dispatch: told.ID, Agent: Daemon, Kind: EffectPullRequestClosed, Ref: c.Closed.URL, At: at.UTC()}
		if err := insertEffect(tx, &e); err != nil {
			return JobCancellation{}, err
		}
		if _, err := appendEntry(tx, at, KindEffectRecorded, e); err != nil {
			return JobCancellation{}, err
		}
		out.Closed = &e
	}
	leaders, err := texts(tx, `SELECT owner FROM assignments WHERE job = ?
		UNION SELECT receiver FROM handoffs WHERE job = ? UNION SELECT sender FROM handoffs WHERE job = ?
		ORDER BY 1`, j.Number, j.Number, j.Number)
	if err != nil {
		return JobCancellation{}, err
	}
	notice := fmt.Sprintf("Job %d is cancelled at the user's word, in witnessed message %d: %q. "+
		"Stop assigning, accepting and delivering its work: the daemon refuses new work for it, discards its unreviewed results, and has told the workers of its live assignments to push their assignment branches and stop; each ends as cancelled once the daemon confirms the push. "+
		"Its branches stay on origin.", j.Number, witness, reason)
	for _, l := range leaders {
		if l == CoordinationLeader || !strings.HasPrefix(l, "leader@") {
			continue
		}
		d, err := createMessage(tx, messageRef{}, j.Number, Daemon, l, MessageJobCancelled, notice, at)
		if err != nil {
			return JobCancellation{}, err
		}
		out.Dispatches = append(out.Dispatches, d)
	}
	return out, tx.Commit()
}

// cancellationSummary tells Coordination's leader what cancelling the job did.
func cancellationSummary(c JobCancellation, closed *PullRequest, cancelling, withdrawn []int64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Job %d is cancelled at the user's word, in witnessed message %d: %q.", c.Job.Number, c.Job.CancelWitness, c.Job.CancelReason)
	if closed != nil {
		fmt.Fprintf(&b, " Its pull request #%d %s is closed with a comment giving the reason.", closed.Number, closed.URL)
	} else {
		b.WriteString(" It had no open pull request to close.")
	}
	if len(cancelling) > 0 {
		fmt.Fprintf(&b, " Assignments %v are cancelling: their workers push their assignment branches and stop, and each ends as cancelled once the daemon confirms the push.", cancelling)
	}
	if len(c.Discarded) > 0 {
		fmt.Fprintf(&b, " The unreviewed results (reports %v) are discarded.", c.Discarded)
	}
	if len(withdrawn) > 0 {
		fmt.Fprintf(&b, " Decision requests %v are withdrawn, and their Lavish pages end.", withdrawn)
	}
	fmt.Fprintf(&b, " Every branch stays on origin. Tell the user, in a line tagged with job %d.", c.Job.Number)
	return b.String()
}

// int64s returns the single integer column query selects.
func int64s(tx *sql.Tx, query string, args ...any) ([]int64, error) {
	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var n int64
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// texts returns the single text column query selects.
func texts(tx *sql.Tx, query string, args ...any) ([]string, error) {
	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
