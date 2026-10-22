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

// MessageSetup tells Coordination's leader that a repository registered
// without its setup has a setup job open, which it gives to a worker.
const MessageSetup = "setup"

// MandateSetup is the mandate of the job the daemon opens when a repository
// is registered that has no docs/agents/issue-tracker.md: it is authorized
// only to set the repository up for the skills, never to change its code. It
// is Coordination's own job, done by one of its workers and delivered by its
// leader like a notes job (08 rules 28 and 29), and only the daemon opens it,
// so it is not among the mandates Coordination opens a job with.
const MandateSetup = "setup"

// CIQuestion is the id of the setup question that asks whether the repository
// has CI, whose answer is for the user to declare in the configuration (08
// rule 31).
const CIQuestion = "ci"

// SetupOpening is the setup job the daemon opens as it registers a
// repository that has no setup: the user's own registration is its request,
// Words, and Reading and Criteria are the daemon's reading of it. Body is what
// Coordination's leader is told.
type SetupOpening struct {
	Words    string
	Reading  string
	Criteria []string
	Body     string
}

// RepositoryAddedWithSetup records that r was registered, as RepositoryAdded
// does, and in the same step opens the setup job of r's repository, citing
// the registration the user ran, and tells Coordination's leader with a
// message of kind MessageSetup whose dispatch it returns (05 rule 4, 08 rule
// 28). It refuses a name that is already registered, and then opens nothing.
func (s *Store) RepositoryAddedWithSetup(r Repository, setup SetupOpening) (Job, Dispatch, error) {
	if strings.TrimSpace(setup.Reading) == "" || len(setup.Criteria) == 0 {
		return Job{}, Dispatch{}, errors.New("a setup job has a reading and acceptance criteria")
	}
	r.AddedAt = r.AddedAt.UTC()
	tx, err := s.db.Begin()
	if err != nil {
		return Job{}, Dispatch{}, err
	}
	defer tx.Rollback()
	registered, err := repositoryAdded(tx, r)
	if err != nil {
		return Job{}, Dispatch{}, err
	}
	criteria, err := json.Marshal(setup.Criteria)
	if err != nil {
		return Job{}, Dispatch{}, err
	}
	job := Job{Repository: r.Name, State: JobOpen, Role: "coordination", Witness: registered, Words: setup.Words, Reading: setup.Reading, Mandate: MandateSetup, Criteria: setup.Criteria, OpenedAt: r.AddedAt}
	result, err := tx.Exec(`INSERT INTO jobs (repository, state, role, witness, words, reading, mandate, criteria, opened_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, job.Repository, job.State, job.Role, job.Witness, job.Words, job.Reading, job.Mandate, string(criteria), timestamp(r.AddedAt))
	if err != nil {
		return Job{}, Dispatch{}, err
	}
	if job.Number, err = result.LastInsertId(); err != nil {
		return Job{}, Dispatch{}, err
	}
	if _, err := appendEntry(tx, r.AddedAt, KindJobOpened, job); err != nil {
		return Job{}, Dispatch{}, err
	}
	dispatch, err := createMessage(tx, messageRef{}, job.Number, Daemon, CoordinationLeader, MessageSetup, setup.Body, r.AddedAt)
	if err != nil {
		return Job{}, Dispatch{}, err
	}
	return job, dispatch, tx.Commit()
}

// SetupAnswer is the user's answer to one question of a repository's setup,
// recorded for the repository as they gave it: the option they chose and the
// words they wrote. It is a repository instruction until the setup pull
// request merges (08 rule 30). Cites is the Lavish answer that holds it.
type SetupAnswer struct {
	ID         int64     `json:"id"`
	Repository string    `json:"repository"`
	Job        int64     `json:"job"`
	Request    int64     `json:"request"`
	Version    int       `json:"version"`
	Question   string    `json:"question"`
	Title      string    `json:"title"`
	Choice     string    `json:"choice,omitempty"`
	Notes      string    `json:"notes,omitempty"`
	Cites      int64     `json:"cites"`
	At         time.Time `json:"at"`
}

// Words writes the answer as the user gave it: the option chosen, then their
// own words.
func (a SetupAnswer) Words() string {
	return QuestionAnswer{Choice: a.Choice, Notes: a.Notes}.Words()
}

// recordSetupAnswers records the answers the user gave on the page of page,
// which is a request of a setup job, as setup answers of the job's
// repository, so that they apply at once. It records nothing for any other
// job's request.
func recordSetupAnswers(tx *sql.Tx, page DecisionRequest, cites int64, answers []QuestionAnswer, at time.Time) error {
	// What a review of the drafts answers is not a setup answer: it approves
	// the files, which carry the setup once they merge.
	if page.Kind == RequestReview {
		return nil
	}
	j, err := job(tx, page.Job)
	if err != nil {
		return err
	}
	if j.Mandate != MandateSetup {
		return nil
	}
	for _, a := range answers {
		title := a.Question
		for _, q := range page.Questions {
			if q.ID == a.Question {
				title = q.Title
			}
		}
		recorded := SetupAnswer{Repository: j.Repository, Job: j.Number, Request: page.Request, Version: page.Version, Question: a.Question, Title: title, Choice: a.Choice, Notes: a.Notes, Cites: cites, At: at.UTC()}
		result, err := tx.Exec(`INSERT INTO setup_answers (repository, job, request, version, question, title, choice, notes, cites, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			recorded.Repository, recorded.Job, recorded.Request, recorded.Version, recorded.Question, recorded.Title, recorded.Choice, recorded.Notes, recorded.Cites, timestamp(at))
		if err != nil {
			return err
		}
		if recorded.ID, err = result.LastInsertId(); err != nil {
			return err
		}
		if _, err := appendEntry(tx, at, KindSetupAnswered, recorded); err != nil {
			return err
		}
	}
	return nil
}

// SetupAnswers returns the answers the user gave to the setup questions of
// repository, one for each question, in the order the questions were first
// answered. When the user answered a question again, as for a new version of
// the request that asked it, the latest answer stands. Each is a repository
// instruction until the setup pull request merges.
func (s *Store) SetupAnswers(repository string) ([]SetupAnswer, error) {
	return setupAnswers(s.db, repository)
}

func setupAnswers(q querier, repository string) ([]SetupAnswer, error) {
	rows, err := q.Query(`SELECT id, repository, job, request, version, question, title, choice, notes, cites, created_at FROM setup_answers WHERE repository = ? ORDER BY id`, repository)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	answers := []SetupAnswer{}
	for rows.Next() {
		var a SetupAnswer
		var at string
		if err := rows.Scan(&a.ID, &a.Repository, &a.Job, &a.Request, &a.Version, &a.Question, &a.Title, &a.Choice, &a.Notes, &a.Cites, &at); err != nil {
			return nil, err
		}
		if a.At, err = time.Parse(time.RFC3339Nano, at); err != nil {
			return nil, fmt.Errorf("reading a setup answer's time: %w", err)
		}
		if i := slices.IndexFunc(answers, func(earlier SetupAnswer) bool { return earlier.Question == a.Question }); i >= 0 {
			answers[i] = a
			continue
		}
		answers = append(answers, a)
	}
	return answers, rows.Err()
}

// SetupNoticed tells Coordination's leader something about the setup job number, with
// a message of kind MessageSetup with body, whose dispatch it returns.
func (s *Store) SetupNoticed(number int64, body string, at time.Time) (Dispatch, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Dispatch{}, err
	}
	defer tx.Rollback()
	if j, err := job(tx, number); err != nil {
		return Dispatch{}, err
	} else if j.Mandate != MandateSetup {
		return Dispatch{}, fmt.Errorf("job %d is not the setup of a repository", number)
	}
	dispatch, err := createMessage(tx, messageRef{}, number, Daemon, CoordinationLeader, MessageSetup, body, at)
	if err != nil {
		return Dispatch{}, err
	}
	return dispatch, tx.Commit()
}

// CIDeclared reads the answers of a repository's setup for the one to the CI
// question: whether the user said the repository has CI. known is false until
// they chose an option, yes or no, for it.
func CIDeclared(answers []SetupAnswer) (known, hasCI bool) {
	for _, a := range answers {
		if a.Question != CIQuestion {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(a.Choice)) {
		case "yes":
			known, hasCI = true, true
		case "no":
			known, hasCI = true, false
		}
	}
	return known, hasCI
}
