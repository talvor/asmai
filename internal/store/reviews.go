// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// How a validation by Quality goes (06 rules 5, 6 and 12). The first
// validation of a finished job branch is a full review, with the repository's
// checks re-run and both axes of code-review. Every later validation of the
// branch is at a head that makes the earlier evidence stale: a fix, or a merge
// whose conflicts a worker resolved, is reviewed as changes, for what changed
// since the last validated commit, and confirms each earlier blocking finding;
// a clean merge of a moved base needs only the checks. An early review is of
// one assignment's result, before the job branch is finished, at the request of
// a leader; it validates no head of the job.
const (
	ReviewFull    = "full"
	ReviewChanges = "changes"
	ReviewChecks  = "checks"
	ReviewEarly   = "early"
)

// JobReviews are the reviews of the finished job branch, which make a head
// validated.
var JobReviews = []string{ReviewFull, ReviewChanges, ReviewChecks}

// LastValidated returns the commit of job's job branch that Quality's leader
// last accepted a validation of, empty when there is none. An early review of
// an assignment's result validates no head and is not it.
func (s *Store) LastValidated(job int64) (string, error) {
	var commit string
	err := s.db.QueryRow(`SELECT w.base FROM assignments a JOIN workspaces w ON w.assignment = a.id
		WHERE a.job = ? AND a.read_only = 1 AND a.state = ? AND a.review != ? ORDER BY a.id DESC LIMIT 1`, job, AssignmentAccepted, ReviewEarly).Scan(&commit)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return commit, err
}

// ValidatedHead returns nil when Quality validated head, the head of job's
// job branch, and no blocking finding of the job is left open. Any later
// commit makes that evidence stale, so a head that Quality has not validated
// is refused, whatever was validated before it. Otherwise it says which
// condition is not met.
func (s *Store) ValidatedHead(job int64, head string) error {
	return validatedHead(s.db, job, head)
}

func validatedHead(q querier, number int64, head string) error {
	var validated int64
	if err := q.QueryRow(`SELECT COALESCE(MAX(a.id), 0) FROM assignments a JOIN workspaces w ON w.assignment = a.id WHERE a.job = ? AND a.read_only = 1 AND a.state = ? AND a.review != ? AND w.base = ?`, number, AssignmentAccepted, ReviewEarly, head).Scan(&validated); err != nil {
		return err
	}
	if validated == 0 {
		return fmt.Errorf("Quality has not validated %s, the head of job %d", head, number)
	}
	open, err := openBlocking(q, number)
	if err != nil {
		return err
	}
	if len(open) > 0 {
		return fmt.Errorf("blocking finding %d of job %d is still open", open[0].ID, number)
	}
	return nil
}

// checkReviewed refuses an early review of an assignment that is not a
// writing assignment of job with a result at commit.
func checkReviewed(q querier, job, id int64, commit string) error {
	a, err := reviewedAssignment(q, job, id)
	if err != nil {
		return err
	}
	r, err := latestResult(q, a.ID)
	if err != nil {
		return fmt.Errorf("assignment %d has no result to review yet: %w", a.ID, err)
	}
	if r.Commit != commit {
		return fmt.Errorf("the result of assignment %d is at %s, not %s", a.ID, r.Commit, commit)
	}
	return nil
}

// reviewedAssignment returns the writing assignment of job that an early
// review is of.
func reviewedAssignment(q querier, job, id int64) (Assignment, error) {
	a, err := assignment(q, id)
	if errors.Is(err, sql.ErrNoRows) {
		return Assignment{}, fmt.Errorf("assignment %d does not exist", id)
	}
	if err != nil {
		return Assignment{}, err
	}
	switch {
	case a.Job != job:
		return Assignment{}, fmt.Errorf("assignment %d belongs to job %d, not job %d", a.ID, a.Job, job)
	case a.ReadOnly || a.Scratch:
		return Assignment{}, fmt.Errorf("assignment %d is not a writing assignment with a commit, so it has no result to review", a.ID)
	case a.State == AssignmentCancelled:
		return Assignment{}, fmt.Errorf("assignment %d was cancelled, so it has no result to review", a.ID)
	}
	return a, nil
}

// EarlyReviewRequest returns the latest handoff to Quality's leader that
// asked for an early review of assignment of job and that its leader
// accepted, or sql.ErrNoRows.
func (s *Store) EarlyReviewRequest(job, assignment int64) (Handoff, error) {
	var id int64
	err := s.db.QueryRow(`SELECT id FROM handoffs WHERE job = ? AND assignment = ? AND review = ? AND state = ? ORDER BY id DESC LIMIT 1`, job, assignment, ReviewEarly, HandoffAccepted).Scan(&id)
	if err != nil {
		return Handoff{}, err
	}
	return handoff(s.db, id)
}
