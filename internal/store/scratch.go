// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ScratchResultAccepted records the owning leader's acceptance of the result
// of the assignment in.Assignment, which works in a scratch workspace: the
// assignment ends, and nothing else moves, as a job with no repository has no
// job branch. With in.EndsJob the leader accepts the job's outcome against its
// mandate (06 rule 39): in the same step the job ends, and Coordination's
// leader is told with a message of kind MessageDelivery with body, which says
// where the results are, and whose dispatch is returned. A job that has
// another assignment that has not ended has not got its outcome yet.
func (s *Store) ScratchResultAccepted(in Acceptance, body string, at time.Time) (Decision, *Dispatch, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Decision{}, nil, err
	}
	defer tx.Rollback()
	a, err := decided(tx, in.Assignment, in.Leader, in.Reasons)
	if err != nil {
		return Decision{}, nil, err
	}
	if !a.Scratch {
		return Decision{}, nil, fmt.Errorf("assignment %d works in a repository's workspace, so its acceptance moves the job branch", a.ID)
	}
	if a.State != AssignmentSubmitted {
		return Decision{}, nil, fmt.Errorf("assignment %d is %s, so it has no result to accept", a.ID, a.State)
	}
	pending, err := blockingDecisions(tx, a.ID)
	if err != nil {
		return Decision{}, nil, err
	}
	if len(pending) > 0 {
		return Decision{}, nil, waitingOnDecision(a.ID, pending)
	}
	r, err := latestResult(tx, a.ID)
	if err != nil {
		return Decision{}, nil, err
	}
	if in.EndsJob {
		if body == "" {
			return Decision{}, nil, errors.New("a job's end tells Coordination where the results are")
		}
		var live int64
		if err := tx.QueryRow(`SELECT COALESCE(MIN(id), 0) FROM assignments WHERE job = ? AND id <> ? AND NOT `+endedAssignment, a.Job, a.ID).Scan(&live); err != nil {
			return Decision{}, nil, err
		}
		if live != 0 {
			return Decision{}, nil, fmt.Errorf("assignment %d of job %d has not ended, so its outcome is not in: accept or cancel it first, or accept this result without ending the job", live, a.Job)
		}
	}
	if _, err := tx.Exec(`UPDATE assignments SET state = ? WHERE id = ?`, AssignmentAccepted, a.ID); err != nil {
		return Decision{}, nil, err
	}
	d, err := decide(tx, Decision{Assignment: a.ID, Job: a.Job, Kind: DecisionAccepted, Leader: in.Leader, Report: r.ID, Reasons: in.Reasons}, KindResultAccepted, at)
	if err != nil {
		return Decision{}, nil, err
	}
	if !in.EndsJob {
		return d, nil, tx.Commit()
	}
	if _, err := tx.Exec(`UPDATE jobs SET state = ? WHERE number = ?`, JobEnded, a.Job); err != nil {
		return Decision{}, nil, err
	}
	if _, err := appendEntry(tx, at, KindJobEnded, map[string]any{"job": a.Job, "state": JobEnded, "by": "acceptance", "assignment": a.ID}); err != nil {
		return Decision{}, nil, err
	}
	dispatch, err := createMessage(tx, messageRef{}, a.Job, Daemon, "leader@coordination", MessageDelivery, body, at)
	if err != nil {
		return Decision{}, nil, err
	}
	return d, &dispatch, tx.Commit()
}

// ScratchCleaned journals that the daemon removed the scratch workspaces of
// job, which has no repository and has ended, at the user's command: the paths
// in removed. A job's scratch workspaces are kept until then, and a job that
// is still open keeps them (05 rule 44).
func (s *Store) ScratchCleaned(job int64, removed []string, at time.Time) error {
	return s.change(at, func(tx *sql.Tx) (string, any, error) {
		j, err := jobOf(tx, job)
		if err != nil {
			return "", nil, err
		}
		if j.Repository != "" {
			return "", nil, fmt.Errorf("job %d targets repository %s, so it has no scratch workspaces", job, j.Repository)
		}
		if !j.Ended() {
			return "", nil, fmt.Errorf("job %d is still open", job)
		}
		return KindScratchCleaned, map[string]any{"job": job, "removed": removed}, nil
	})
}

// CleanedScratch reports whether the scratch workspaces of job have been
// removed.
func (s *Store) CleanedScratch(job int64) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT count(*) FROM journal WHERE kind = ? AND json_extract(data, '$.job') = ?`, KindScratchCleaned, job).Scan(&n)
	return n > 0, err
}

// jobOf returns job number, or an error that says it does not exist.
func jobOf(q queryer, number int64) (Job, error) {
	j, err := job(q, number)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, fmt.Errorf("job %d does not exist", number)
	}
	return j, err
}
