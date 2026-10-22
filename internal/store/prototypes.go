// SPDX-License-Identifier: Apache-2.0

package store

import (
	"encoding/json"
	"fmt"
	"time"
)

// Prototype is a prototype that stays on its own assignment branch: the
// branch on origin that holds it, and the commit of the accepted result. Its
// verdict, with a pointer to that branch, is a note on the job branch (05
// rule 31).
type Prototype struct {
	Job        int64     `json:"job"`
	Assignment int64     `json:"assignment"`
	Branch     string    `json:"branch"`
	Commit     string    `json:"commit"`
	Decision   int64     `json:"decision"`
	At         time.Time `json:"at"`
}

// PrototypeKept records the owning leader's acceptance of the result of the
// writing assignment in.Assignment as a prototype that stays on its own
// assignment branch: the assignment ends, and the job branch does not move,
// so the prototype's code never reaches the job's pull request. The branch
// and the result's commit are journaled as the prototype, for the verdict
// note to point to.
func (s *Store) PrototypeKept(in Acceptance, at time.Time) (Decision, Prototype, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Decision{}, Prototype{}, err
	}
	defer tx.Rollback()
	a, err := decided(tx, in.Assignment, in.Leader, in.Reasons)
	if err != nil {
		return Decision{}, Prototype{}, err
	}
	if a.State != AssignmentSubmitted {
		return Decision{}, Prototype{}, fmt.Errorf("assignment %d is %s, so it has no result to accept", a.ID, a.State)
	}
	if a.ReadOnly || a.Scratch {
		return Decision{}, Prototype{}, fmt.Errorf("assignment %d is a read-only validation or works in a scratch workspace, so it has no assignment branch for a prototype to stay on", a.ID)
	}
	pending, err := blockingDecisions(tx, a.ID)
	if err != nil {
		return Decision{}, Prototype{}, err
	}
	if len(pending) > 0 {
		return Decision{}, Prototype{}, waitingOnDecision(a.ID, pending)
	}
	r, err := latestResult(tx, a.ID)
	if err != nil {
		return Decision{}, Prototype{}, err
	}
	if _, err := tx.Exec(`UPDATE assignments SET state = ? WHERE id = ?`, AssignmentAccepted, a.ID); err != nil {
		return Decision{}, Prototype{}, err
	}
	d, err := decide(tx, Decision{Assignment: a.ID, Job: a.Job, Kind: DecisionAccepted, Leader: in.Leader, Report: r.ID, Commit: r.Commit, Reasons: in.Reasons}, KindResultAccepted, at)
	if err != nil {
		return Decision{}, Prototype{}, err
	}
	p := Prototype{Job: a.Job, Assignment: a.ID, Branch: a.Workspace.Branch, Commit: r.Commit, Decision: d.ID, At: at.UTC()}
	if _, err := appendEntry(tx, at, KindPrototypeKept, p); err != nil {
		return Decision{}, Prototype{}, err
	}
	return d, p, tx.Commit()
}

// PrototypesKept returns the prototypes of job that stay on their assignment
// branches, oldest first.
func (s *Store) PrototypesKept(job int64) ([]Prototype, error) {
	rows, err := s.db.Query(`SELECT data FROM journal WHERE kind = ? AND json_extract(data, '$.job') = ? ORDER BY id`, KindPrototypeKept, job)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var kept []Prototype
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var p Prototype
		if err := json.Unmarshal([]byte(data), &p); err != nil {
			return nil, err
		}
		kept = append(kept, p)
	}
	return kept, rows.Err()
}
