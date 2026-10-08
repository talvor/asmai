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

const JobOpen = "open"
const MandateTestedPR = "tested-pr"

// Job holds the user's witnessed request beside Coordination's reading.
type Job struct {
	Number     int64     `json:"number"`
	Role       string    `json:"role"`
	Repository string    `json:"repository"`
	State      string    `json:"state"`
	Witness    int64     `json:"witness"`
	Words      string    `json:"words"`
	Reading    string    `json:"reading"`
	Mandate    string    `json:"mandate"`
	Criteria   []string  `json:"acceptance_criteria"`
	OpenedAt   time.Time `json:"opened_at"`
}

// JobOpened verifies a witnessed message and a registered repository in the
// same transaction that numbers and journals the job. A nudge or observation
// can never stand in for the user's words.
func (s *Store) JobOpened(job Job, latest bool, at time.Time) (Job, error) {
	if (job.Witness <= 0 && !latest) || strings.TrimSpace(job.Reading) == "" || job.Mandate != MandateTestedPR || len(job.Criteria) == 0 {
		return Job{}, errors.New("job open needs a witnessed message, a reading, the tested-pr mandate and acceptance criteria")
	}
	for _, criterion := range job.Criteria {
		if strings.TrimSpace(criterion) == "" {
			return Job{}, errors.New("each acceptance criterion must have text")
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Job{}, err
	}
	defer tx.Rollback()
	if latest {
		if err := tx.QueryRow(`SELECT id FROM journal WHERE kind = ? AND json_extract(data, '$.agent') = 'leader@coordination' ORDER BY id DESC LIMIT 1`, KindMessageWitnessed).Scan(&job.Witness); errors.Is(err, sql.ErrNoRows) {
			return Job{}, errors.New("Coordination has no witnessed message to cite")
		} else if err != nil {
			return Job{}, err
		}
	}
	var kind, data string
	if err := tx.QueryRow(`SELECT kind, data FROM journal WHERE id = ?`, job.Witness).Scan(&kind, &data); errors.Is(err, sql.ErrNoRows) {
		return Job{}, fmt.Errorf("witnessed message %d does not exist", job.Witness)
	} else if err != nil {
		return Job{}, err
	}
	if kind != KindMessageWitnessed {
		return Job{}, fmt.Errorf("journal entry %d is %s, not a witnessed message", job.Witness, kind)
	}
	var witnessed Witnessed
	if err := json.Unmarshal([]byte(data), &witnessed); err != nil {
		return Job{}, err
	}
	if witnessed.Role != "coordination" || witnessed.Agent != "leader@coordination" {
		return Job{}, fmt.Errorf("witnessed message %d was not submitted to Coordination's leader", job.Witness)
	}
	if job.Repository == "" {
		return Job{}, errors.New("a tested-PR job needs a registered repository")
	}
	if _, err := repository(tx, job.Repository); errors.Is(err, sql.ErrNoRows) {
		return Job{}, fmt.Errorf("repository %q is not registered; ask the user to run `asmai repo add <path-or-url>`", job.Repository)
	} else if err != nil {
		return Job{}, err
	}
	job.Words, job.Role, job.State, job.OpenedAt = witnessed.Text, "coordination", JobOpen, at.UTC()
	criteria, err := json.Marshal(job.Criteria)
	if err != nil {
		return Job{}, err
	}
	result, err := tx.Exec(`INSERT INTO jobs (repository, state, role, witness, words, reading, mandate, criteria, opened_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, job.Repository, job.State, job.Role, job.Witness, job.Words, job.Reading, job.Mandate, string(criteria), timestamp(at))
	if err != nil {
		return Job{}, err
	}
	job.Number, err = result.LastInsertId()
	if err != nil {
		return Job{}, err
	}
	if _, err := appendEntry(tx, at, KindJobOpened, job); err != nil {
		return Job{}, err
	}
	return job, tx.Commit()
}

func (s *Store) Job(number int64) (Job, error) {
	return job(s.db, number)
}

func job(q queryer, number int64) (Job, error) {
	var j Job
	var repo sql.NullString
	var criteria, opened string
	err := q.QueryRow(`SELECT number, repository, state, role, witness, words, reading, mandate, criteria, opened_at FROM jobs WHERE number = ?`, number).Scan(&j.Number, &repo, &j.State, &j.Role, &j.Witness, &j.Words, &j.Reading, &j.Mandate, &criteria, &opened)
	if err != nil {
		return Job{}, err
	}
	j.Repository = repo.String
	if err := json.Unmarshal([]byte(criteria), &j.Criteria); err != nil {
		return Job{}, err
	}
	if opened != "" {
		j.OpenedAt, err = time.Parse(time.RFC3339Nano, opened)
	}
	return j, err
}

func (s *Store) Jobs() ([]Job, error) {
	rows, err := s.db.Query(`SELECT number FROM jobs ORDER BY number`)
	if err != nil {
		return nil, err
	}
	var numbers []int64
	for rows.Next() {
		var n int64
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, err
		}
		numbers = append(numbers, n)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	jobs := make([]Job, 0, len(numbers))
	for _, n := range numbers {
		j, err := s.Job(n)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, nil
}

// JobEntries returns journal records belonging to this job, newest first.
func (s *Store) JobEntries(number int64) ([]Entry, error) {
	rows, err := s.db.Query(`SELECT id, at, kind, data FROM journal WHERE (kind = ? AND json_extract(data, '$.number') = ?) OR json_extract(data, '$.job') = ? ORDER BY id DESC LIMIT 20`, KindJobOpened, number, number)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := []Entry{}
	for rows.Next() {
		var e Entry
		var at, data string
		if err := rows.Scan(&e.ID, &at, &e.Kind, &data); err != nil {
			return nil, err
		}
		e.At, err = time.Parse(time.RFC3339Nano, at)
		if err != nil {
			return nil, err
		}
		e.Data = json.RawMessage(data)
		entries = append(entries, e)
	}
	return entries, rows.Err()
}
