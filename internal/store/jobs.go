// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

const JobOpen = "open"

// JobPaused is the state of a job the user paused. Its pending decision
// requests stay answerable.
const JobPaused = "paused"

// JobCancelled is the state of a job the user cancelled through Coordination's
// leader, with their own words as the reason. Like an ended job, it takes no
// new work and is never reopened; its assignments that were live end as
// cancelled once their workers have pushed their branches and stopped.
const JobCancelled = "cancelled"

// Ended reports whether the job has ended: delivered, replaced, ended on its
// leader's acceptance, or cancelled.
func (j Job) Ended() bool {
	return j.State == JobEnded || j.State == JobCancelled
}

// The mandates a job has. A tested-PR job is authorized to inspect, plan,
// research, make scoped edits, run checks, commit, push and open and update
// its pull request. A planning-only job is authorized only to produce its
// planning outcome, and a research-only job only its research outcome: neither
// is handed to Engineering, and each goes only to its own role (01 rule 17).
const (
	MandateTestedPR     = "tested-pr"
	MandatePlanningOnly = "planning-only"
	MandateResearchOnly = "research-only"
)

// Mandates are the mandates Coordination opens a job with. The setup mandate
// is not among them: only the daemon opens that job, as a repository is
// registered (MandateSetup).
var Mandates = []string{MandateTestedPR, MandatePlanningOnly, MandateResearchOnly}

// Notes reports whether the job is a notes job: a job in a repository whose
// mandate is planning-only, research-only or setup, so that its outcome is
// files that are no code change of the repository's and its delivery is its
// own leader's, Planning's, Research's or Coordination's, not Engineering's
// (06 rule 16, 08 rule 29).
func (j Job) Notes() bool {
	return j.Repository != "" && (j.Mandate == MandatePlanningOnly || j.Mandate == MandateResearchOnly || j.Mandate == MandateSetup)
}

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
	// Follows is the job this one is a follow-up to, or zero (02 rule 20).
	Follows int64 `json:"follows,omitempty"`
	// CancelReason is the user's own words a cancelled job was cancelled
	// for, and CancelWitness the witnessed message that holds them.
	CancelReason  string `json:"cancel_reason,omitempty"`
	CancelWitness int64  `json:"cancel_witness,omitempty"`
}

// JobOpened verifies a witnessed message and a registered repository in the
// same transaction that numbers and journals the job. A nudge or observation
// can never stand in for the user's words.
func (s *Store) JobOpened(job Job, latest bool, at time.Time) (Job, error) {
	if (job.Witness <= 0 && !latest) || strings.TrimSpace(job.Reading) == "" || !slices.Contains(Mandates, job.Mandate) || len(job.Criteria) == 0 {
		return Job{}, fmt.Errorf("job open needs a witnessed message, a reading, a mandate (%s or %s) and acceptance criteria", strings.Join(Mandates[:len(Mandates)-1], ", "), Mandates[len(Mandates)-1])
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
	var witnessed Witnessed
	if job.Witness, witnessed, err = coordinationWitnessed(tx, job.Witness, latest); err != nil {
		return Job{}, err
	}
	// Only a research-only job may target no repository, and its assignments
	// then work in scratch workspaces (02 rule 19, 05 rule 8). A job that
	// delivers a tested pull request needs one, and so does a planning-only
	// job, whose workers write notes into a repository's clone.
	if job.Repository == "" && job.Mandate != MandateResearchOnly {
		return Job{}, fmt.Errorf("a %s job needs a registered repository", job.Mandate)
	}
	if job.Repository != "" {
		if _, err := repository(tx, job.Repository); errors.Is(err, sql.ErrNoRows) {
			return Job{}, fmt.Errorf("repository %q is not registered; ask the user to run `asmai repo add <path-or-url>`", job.Repository)
		} else if err != nil {
			return Job{}, err
		}
	}
	job.Words, job.Role, job.State, job.OpenedAt = witnessed.Text, "coordination", JobOpen, at.UTC()
	criteria, err := json.Marshal(job.Criteria)
	if err != nil {
		return Job{}, err
	}
	result, err := tx.Exec(`INSERT INTO jobs (repository, state, role, witness, words, reading, mandate, criteria, opened_at) VALUES (NULLIF(?, ''), ?, ?, ?, ?, ?, ?, ?, ?)`, job.Repository, job.State, job.Role, job.Witness, job.Words, job.Reading, job.Mandate, string(criteria), timestamp(at))
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

// coordinationWitnessed returns the witnessed message witness, or with latest
// the latest one, which must be the user's own words submitted to
// Coordination's leader: a nudge or observation never stands in for them.
func coordinationWitnessed(tx *sql.Tx, witness int64, latest bool) (int64, Witnessed, error) {
	if latest {
		if err := tx.QueryRow(`SELECT id FROM journal WHERE kind = ? AND json_extract(data, '$.agent') = 'leader@coordination' ORDER BY id DESC LIMIT 1`, KindMessageWitnessed).Scan(&witness); errors.Is(err, sql.ErrNoRows) {
			return 0, Witnessed{}, errors.New("Coordination has no witnessed message to cite")
		} else if err != nil {
			return 0, Witnessed{}, err
		}
	}
	var kind, data string
	if err := tx.QueryRow(`SELECT kind, data FROM journal WHERE id = ?`, witness).Scan(&kind, &data); errors.Is(err, sql.ErrNoRows) {
		return 0, Witnessed{}, fmt.Errorf("witnessed message %d does not exist", witness)
	} else if err != nil {
		return 0, Witnessed{}, err
	}
	if kind != KindMessageWitnessed {
		return 0, Witnessed{}, fmt.Errorf("journal entry %d is %s, not a witnessed message", witness, kind)
	}
	var witnessed Witnessed
	if err := json.Unmarshal([]byte(data), &witnessed); err != nil {
		return 0, Witnessed{}, err
	}
	if witnessed.Role != "coordination" || witnessed.Agent != "leader@coordination" {
		return 0, Witnessed{}, fmt.Errorf("witnessed message %d was not submitted to Coordination's leader", witness)
	}
	return witness, witnessed, nil
}

// JobLinked links the follow-up job to the job it follows, and journals it.
// The follow-up is a job opened after its predecessor, still open and not
// linked to another, and in the same repository when both have one (02 rule 20).
func (s *Store) JobLinked(number, follows int64, at time.Time) (Job, error) {
	var linked Job
	err := s.change(at, func(tx *sql.Tx) (string, any, error) {
		j, err := job(tx, number)
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil, fmt.Errorf("job %d does not exist", number)
		}
		if err != nil {
			return "", nil, err
		}
		predecessor, err := job(tx, follows)
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil, fmt.Errorf("job %d does not exist", follows)
		}
		if err != nil {
			return "", nil, err
		}
		// Linking again records nothing, so that Coordination can ask the
		// daemon to settle the follow-up's job branch again.
		if j.Follows == follows {
			linked = j
			return "", nil, nil
		}
		switch {
		case follows >= number:
			return "", nil, fmt.Errorf("job %d is not a follow-up to job %d: a follow-up is a new job linked to one opened before it", number, follows)
		case j.State != JobOpen:
			return "", nil, fmt.Errorf("job %d is %s, so it cannot be linked", number, j.State)
		case j.Follows != 0:
			return "", nil, fmt.Errorf("job %d already follows job %d", number, j.Follows)
		case j.Repository != predecessor.Repository:
			return "", nil, fmt.Errorf("job %d targets %s and job %d targets %s: a follow-up is in the same repository as the job it follows", number, repositoryName(j.Repository), follows, repositoryName(predecessor.Repository))
		}
		if _, err := tx.Exec(`UPDATE jobs SET follows = ? WHERE number = ?`, follows, number); err != nil {
			return "", nil, err
		}
		j.Follows = follows
		linked = j
		return KindJobLinked, map[string]any{"job": number, "follows": follows}, nil
	})
	return linked, err
}

// repositoryName is a repository for a sentence, or "no repository".
func repositoryName(name string) string {
	if name == "" {
		return "no repository"
	}
	return name
}

func (s *Store) Job(number int64) (Job, error) {
	return job(s.db, number)
}

func job(q queryer, number int64) (Job, error) {
	var j Job
	var repo sql.NullString
	var criteria, opened string
	err := q.QueryRow(`SELECT number, repository, state, role, witness, words, reading, mandate, criteria, opened_at, follows, cancel_reason, cancel_witness FROM jobs WHERE number = ?`, number).Scan(&j.Number, &repo, &j.State, &j.Role, &j.Witness, &j.Words, &j.Reading, &j.Mandate, &criteria, &opened, &j.Follows, &j.CancelReason, &j.CancelWitness)
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
