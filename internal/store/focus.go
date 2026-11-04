// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// KindFocusChanged records the conversation's focus moving to a job: the job,
// the job that had it before, if any, and the witnessed message that moved
// it (04 rules 12 to 14).
const KindFocusChanged = "focus.changed"

// Focus is the conversation's focused job, the one the user's messages apply
// to unless they name another, as a change of focus made it: Job has the
// focus, Previous had it before, or none did, and Witness is the witnessed
// message that moved it. The zero Focus is a conversation no job has had
// the focus of yet.
type Focus struct {
	Job      int64     `json:"job"`
	Previous int64     `json:"previous,omitempty"`
	Witness  int64     `json:"witness"`
	At       time.Time `json:"at"`
}

// Focus returns the conversation's focused job as the latest change of focus
// left it.
func (s *Store) Focus() (Focus, error) {
	return focus(s.db)
}

func focus(q queryer) (Focus, error) {
	var f Focus
	var at string
	err := q.QueryRow(`SELECT job, previous, witness, changed_at FROM focus_changes ORDER BY id DESC LIMIT 1`).Scan(&f.Job, &f.Previous, &f.Witness, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return Focus{}, nil
	}
	if err != nil {
		return Focus{}, err
	}
	f.At, err = time.Parse(time.RFC3339Nano, at)
	return f, err
}

// FocusChanged moves the conversation's focus to job number at the user's
// word: the witnessed message witness, or with latest the latest one, which
// must be the user's own words submitted to Coordination's leader. It refuses
// a job that does not exist, and a message from before the one that gave the
// focus to the job that has it, which cannot have moved it since. It returns
// the focus and whether it moved: a job that already has the focus keeps it,
// and nothing is recorded.
func (s *Store) FocusChanged(number, witness int64, latest bool, at time.Time) (Focus, bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Focus{}, false, err
	}
	defer tx.Rollback()
	if _, err := job(tx, number); errors.Is(err, sql.ErrNoRows) {
		return Focus{}, false, fmt.Errorf("job %d does not exist; `asmai jobs` lists the jobs", number)
	} else if err != nil {
		return Focus{}, false, err
	}
	if witness, _, err = coordinationWitnessed(tx, witness, latest); err != nil {
		return Focus{}, false, err
	}
	was, err := focus(tx)
	if err != nil {
		return Focus{}, false, err
	}
	if was.Job == number {
		return was, false, nil
	}
	if witness < was.Witness {
		return Focus{}, false, fmt.Errorf("witnessed message %d came before message %d, which gave job %d the focus, so it cannot move the focus now: cite the message in which the user asked to switch", witness, was.Witness, was.Job)
	}
	f, err := focusMoved(tx, number, witness, at)
	if err != nil {
		return Focus{}, false, err
	}
	return f, true, tx.Commit()
}

// focusMoved gives job the conversation's focus, as the witnessed message
// witness asked, and journals the change.
func focusMoved(tx *sql.Tx, job, witness int64, at time.Time) (Focus, error) {
	was, err := focus(tx)
	if err != nil {
		return Focus{}, err
	}
	f := Focus{Job: job, Previous: was.Job, Witness: witness, At: at.UTC()}
	if _, err := tx.Exec(`INSERT INTO focus_changes (job, previous, witness, changed_at) VALUES (?, ?, ?, ?)`, f.Job, f.Previous, f.Witness, timestamp(at)); err != nil {
		return Focus{}, err
	}
	if _, err := appendEntry(tx, at, KindFocusChanged, f); err != nil {
		return Focus{}, err
	}
	return f, nil
}
