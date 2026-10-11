// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// The results of the delivery owner asking for a job branch's push: pushed,
// or refused because origin's job branch has commits AsmAI did not make.
const (
	PushPushed  = "pushed"
	PushRefused = "refused"
)

// Push is the delivery owner asking the daemon to push a job branch, and what
// came of it. A refused push is the outside change it found.
type Push struct {
	ID     int64  `json:"id"`
	Job    int64  `json:"job"`
	Branch string `json:"branch"`
	// Commit is the job branch's tip when the push was asked for: the commit
	// pushed, when it was.
	Commit string `json:"commit"`
	Result string `json:"result"`
	// Outside are the commits on origin AsmAI did not make, newest first,
	// and Found where they were found; MergedIn is the commit of the later
	// push that took them in, once one has. A pushed push has none.
	Outside  []string `json:"outside"`
	Found    string   `json:"found,omitempty"`
	MergedIn string   `json:"merged_in,omitempty"`
	// Leader is the delivery owner that asked, and Dispatch the dispatch it
	// was working on. Effect is the push's effect, which the daemon records
	// as its own, tagged with that dispatch.
	Leader   string    `json:"leader"`
	Dispatch int64     `json:"dispatch"`
	Effect   int64     `json:"effect,omitempty"`
	At       time.Time `json:"at"`
}

// OutsideChangeFound records that the push of p.Branch, which generation of
// p.Leader's session asked for in p.Dispatch, was refused because origin's
// branch has the commits p.Outside AsmAI did not make, and journals the
// outside change.
func (s *Store) OutsideChangeFound(p Push, generation int, at time.Time) (Push, error) {
	if len(p.Outside) == 0 {
		return Push{}, errors.New("an outside change has the commits AsmAI did not make")
	}
	p.Result, p.Found, p.Effect, p.MergedIn = PushRefused, "origin/"+p.Branch, 0, ""
	err := s.change(at, func(tx *sql.Tx) (string, any, error) {
		if err := recordPush(tx, &p, generation, at); err != nil {
			return "", nil, err
		}
		return KindOutsideChange, p, nil
	})
	return p, err
}

// JobBranchPushed records that the daemon pushed p.Branch at p.Commit, which
// generation of p.Leader's session asked for in p.Dispatch, records the push
// as the daemon's own effect tagged with that dispatch, and journals both.
// tookIn are the earlier outside changes of the job whose commits p.Commit
// has, which are marked merged in by it.
func (s *Store) JobBranchPushed(p Push, generation int, tookIn []int64, at time.Time) (Push, error) {
	p.Result, p.Outside, p.Found, p.MergedIn = PushPushed, nil, "", ""
	tx, err := s.db.Begin()
	if err != nil {
		return Push{}, err
	}
	defer tx.Rollback()
	e := Effect{Job: p.Job, Dispatch: p.Dispatch, Agent: Daemon, Kind: "push", Ref: "origin/" + p.Branch + "@" + p.Commit, At: at.UTC()}
	if err := insertEffect(tx, &e); err != nil {
		return Push{}, err
	}
	p.Effect = e.ID
	if err := recordPush(tx, &p, generation, at); err != nil {
		return Push{}, err
	}
	for _, id := range tookIn {
		result, err := tx.Exec(`UPDATE pushes SET merged_in = ? WHERE id = ? AND job = ? AND result = ? AND merged_in = ''`, p.Commit, id, p.Job, PushRefused)
		if err != nil {
			return Push{}, err
		}
		if n, err := result.RowsAffected(); err != nil || n != 1 {
			return Push{}, fmt.Errorf("push %d is not an outside change of job %d still to be taken in", id, p.Job)
		}
	}
	if _, err := appendEntry(tx, at, KindEffectRecorded, e); err != nil {
		return Push{}, err
	}
	data := struct {
		Push
		TookIn []int64 `json:"took_in"`
	}{p, nonNilIDs(tookIn)}
	if _, err := appendEntry(tx, at, KindJobBranchPushed, data); err != nil {
		return Push{}, err
	}
	return p, tx.Commit()
}

// recordPush checks that p is of the job's branch at its recorded tip, asked
// for in the dispatch generation of p.Leader's session is working on, and
// inserts it, setting its ID.
func recordPush(tx *sql.Tx, p *Push, generation int, at time.Time) error {
	if _, _, err := currentDispatch(tx, p.Dispatch, p.Leader, generation); err != nil {
		return err
	}
	b, err := jobBranch(tx, p.Job)
	if err != nil {
		return fmt.Errorf("job %d has no job branch: %w", p.Job, err)
	}
	if b.Name != p.Branch || b.Tip != p.Commit {
		return fmt.Errorf("job %d's branch is %s at %s, not %s at %s", p.Job, b.Name, b.Tip, p.Branch, p.Commit)
	}
	if p.Outside == nil {
		p.Outside = []string{}
	}
	outside, err := json.Marshal(p.Outside)
	if err != nil {
		return err
	}
	p.At = at.UTC()
	result, err := tx.Exec(`INSERT INTO pushes (job, branch, commit_id, result, outside, found, leader, dispatch, effect, merged_in, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULLIF(?, 0), ?, ?)`,
		p.Job, p.Branch, p.Commit, p.Result, string(outside), p.Found, p.Leader, p.Dispatch, p.Effect, p.MergedIn, timestamp(at))
	if err != nil {
		return err
	}
	p.ID, err = result.LastInsertId()
	return err
}

// Pushes returns every push asked for of job, oldest first.
func (s *Store) Pushes(job int64) ([]Push, error) {
	rows, err := s.db.Query(`SELECT id, job, branch, commit_id, result, outside, found, merged_in, leader, dispatch, effect, created_at FROM pushes WHERE job = ? ORDER BY id`, job)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	pushes := []Push{}
	for rows.Next() {
		var p Push
		var outside, at string
		var effect sql.NullInt64
		if err := rows.Scan(&p.ID, &p.Job, &p.Branch, &p.Commit, &p.Result, &outside, &p.Found, &p.MergedIn, &p.Leader, &p.Dispatch, &effect, &at); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(outside), &p.Outside); err != nil {
			return nil, err
		}
		p.Effect = effect.Int64
		if p.At, err = time.Parse(time.RFC3339Nano, at); err != nil {
			return nil, err
		}
		pushes = append(pushes, p)
	}
	return pushes, rows.Err()
}
