// SPDX-License-Identifier: Apache-2.0

package store

import (
	"errors"
	"fmt"
	"time"
)

// EffectPullRequest is the kind of effect the delivery owner records for a
// pull request it opens or updates, naming it by its URL.
const EffectPullRequest = "pull-request"

// PullRequest is a job's pull request as GitHub had it when the daemon looked
// it up for the delivery owner's effect naming it: the pull request the
// daemon follows. Each effect naming it records it again, and the latest is
// the one followed.
type PullRequest struct {
	ID     int64  `json:"id"`
	Job    int64  `json:"job"`
	Number int    `json:"number"`
	URL    string `json:"url"`
	Title  string `json:"title"`
	// Draft is whether it is a draft, and NotReady whether its title starts
	// with "[not ready]", as it does when GitHub refused a draft.
	Draft    bool `json:"draft"`
	NotReady bool `json:"not_ready"`
	// Branch is the job branch it is from, and Head that branch's commit on
	// GitHub.
	Branch string `json:"branch"`
	Head   string `json:"head"`
	// State is GitHub's: OPEN, CLOSED or MERGED.
	State string `json:"state"`
	// Leader is the delivery owner that recorded it, Dispatch the dispatch
	// it was working on, and Effect the effect it recorded.
	Leader   string    `json:"leader"`
	Dispatch int64     `json:"dispatch"`
	Effect   int64     `json:"effect"`
	At       time.Time `json:"at"`
}

// DispatchJob returns the job of the message dispatch delivers.
func (s *Store) DispatchJob(dispatch int64) (int64, error) {
	var job int64
	err := s.db.QueryRow(`SELECT m.job FROM dispatches d JOIN messages m ON m.id = d.message WHERE d.id = ?`, dispatch).Scan(&job)
	return job, err
}

// PullRequestRecorded records the effect of kind EffectPullRequest and ref
// that generation of p.Leader's session made in p.Dispatch, and p, the pull
// request it names as GitHub had it, from p.Job's branch. It journals both.
func (s *Store) PullRequestRecorded(p PullRequest, generation int, ref string, at time.Time) (PullRequest, Effect, error) {
	if p.Number <= 0 {
		return PullRequest{}, Effect{}, errors.New("a pull request has a number")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return PullRequest{}, Effect{}, err
	}
	defer tx.Rollback()
	job, assignment, err := currentDispatch(tx, p.Dispatch, p.Leader, generation)
	if err != nil {
		return PullRequest{}, Effect{}, err
	}
	if job != p.Job {
		return PullRequest{}, Effect{}, fmt.Errorf("dispatch %d is of job %d, not job %d", p.Dispatch, job, p.Job)
	}
	b, err := jobBranch(tx, p.Job)
	if err != nil {
		return PullRequest{}, Effect{}, fmt.Errorf("job %d has no job branch: %w", p.Job, err)
	}
	if b.Name != p.Branch {
		return PullRequest{}, Effect{}, fmt.Errorf("pull request #%d is from %s, not job %d's branch %s", p.Number, p.Branch, p.Job, b.Name)
	}
	e := Effect{Job: job, Assignment: assignment, Dispatch: p.Dispatch, Agent: p.Leader, Generation: generation, Kind: EffectPullRequest, Ref: ref, At: at.UTC()}
	if err := insertEffect(tx, &e); err != nil {
		return PullRequest{}, Effect{}, err
	}
	p.Effect, p.At = e.ID, at.UTC()
	result, err := tx.Exec(`INSERT INTO pull_requests (job, number, url, title, draft, not_ready, branch, head, state, leader, dispatch, effect, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.Job, p.Number, p.URL, p.Title, p.Draft, p.NotReady, p.Branch, p.Head, p.State, p.Leader, p.Dispatch, p.Effect, timestamp(at))
	if err != nil {
		return PullRequest{}, Effect{}, err
	}
	if p.ID, err = result.LastInsertId(); err != nil {
		return PullRequest{}, Effect{}, err
	}
	if _, err := appendEntry(tx, at, KindEffectRecorded, e); err != nil {
		return PullRequest{}, Effect{}, err
	}
	if _, err := appendEntry(tx, at, KindPullRequestRecorded, p); err != nil {
		return PullRequest{}, Effect{}, err
	}
	return p, e, tx.Commit()
}

// PullRequests returns every record of job's pull requests, oldest first.
func (s *Store) PullRequests(job int64) ([]PullRequest, error) {
	rows, err := s.db.Query(`SELECT id, job, number, url, title, draft, not_ready, branch, head, state, leader, dispatch, effect, created_at FROM pull_requests WHERE job = ? ORDER BY id`, job)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	prs := []PullRequest{}
	for rows.Next() {
		var p PullRequest
		var at string
		if err := rows.Scan(&p.ID, &p.Job, &p.Number, &p.URL, &p.Title, &p.Draft, &p.NotReady, &p.Branch, &p.Head, &p.State, &p.Leader, &p.Dispatch, &p.Effect, &at); err != nil {
			return nil, err
		}
		if p.At, err = time.Parse(time.RFC3339Nano, at); err != nil {
			return nil, err
		}
		prs = append(prs, p)
	}
	return prs, rows.Err()
}
