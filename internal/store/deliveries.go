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

// MessageDelivery tells Coordination's leader that a job is delivered: its
// pull request is ready for review, and the job has ended. It is about a job,
// not an assignment or a handoff.
const MessageDelivery = "delivery"

// CheckState is one check on the head of a job's pull request, as GitHub had
// it when the daemon looked: its name, GitHub's status and conclusion, and
// what AsmAI makes of it, pending, passed or failed.
type CheckState struct {
	Name       string `json:"name"`
	Status     string `json:"status,omitempty"`
	Conclusion string `json:"conclusion,omitempty"`
	State      string `json:"state"`
	URL        string `json:"url,omitempty"`
}

// ChecksObservation is the checks on the head of a job's pull request, as the
// daemon read them through gh, with its verdict on them: green, red or
// waiting. A head with no checks is waiting, never green.
type ChecksObservation struct {
	ID      int64        `json:"id"`
	Job     int64        `json:"job"`
	Number  int          `json:"number"`
	Head    string       `json:"head"`
	Checks  []CheckState `json:"checks"`
	Verdict string       `json:"verdict"`
	At      time.Time    `json:"at"`
}

// Delivery is a job delivered: its pull request, marked ready for review
// once every check on its head was green, the head commit and the time. AsmAI
// never merges: ready never means merged.
type Delivery struct {
	Job    int64        `json:"job"`
	Number int          `json:"number"`
	URL    string       `json:"url"`
	Head   string       `json:"head"`
	Checks []CheckState `json:"checks"`
	At     time.Time    `json:"at"`
}

// ChecksObserved records o, unless the latest observation of o.Job is of the
// same head with the same checks in the same states, and journals it. It
// returns whether it recorded it.
func (s *Store) ChecksObserved(o ChecksObservation, at time.Time) (ChecksObservation, bool, error) {
	if o.Checks == nil {
		o.Checks = []CheckState{}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return ChecksObservation{}, false, err
	}
	defer tx.Rollback()
	last, err := latestChecks(tx, o.Job)
	if err == nil && last.Number == o.Number && last.Head == o.Head && last.Verdict == o.Verdict && slices.Equal(last.Checks, o.Checks) {
		return last, false, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return ChecksObservation{}, false, err
	}
	checks, err := json.Marshal(o.Checks)
	if err != nil {
		return ChecksObservation{}, false, err
	}
	o.At = at.UTC()
	result, err := tx.Exec(`INSERT INTO check_observations (job, number, head, checks, verdict, created_at) VALUES (?, ?, ?, ?, ?, ?)`, o.Job, o.Number, o.Head, string(checks), o.Verdict, timestamp(at))
	if err != nil {
		return ChecksObservation{}, false, err
	}
	if o.ID, err = result.LastInsertId(); err != nil {
		return ChecksObservation{}, false, err
	}
	if _, err := appendEntry(tx, at, KindChecksObserved, o); err != nil {
		return ChecksObservation{}, false, err
	}
	return o, true, tx.Commit()
}

// LatestChecks returns the latest observation of the checks on the head of
// job's pull request, or sql.ErrNoRows.
func (s *Store) LatestChecks(job int64) (ChecksObservation, error) {
	return latestChecks(s.db, job)
}

func latestChecks(q queryer, job int64) (ChecksObservation, error) {
	var o ChecksObservation
	var checks, at string
	err := q.QueryRow(`SELECT id, job, number, head, checks, verdict, created_at FROM check_observations WHERE job = ? ORDER BY id DESC LIMIT 1`, job).Scan(&o.ID, &o.Job, &o.Number, &o.Head, &checks, &o.Verdict, &at)
	if err != nil {
		return ChecksObservation{}, err
	}
	if err := json.Unmarshal([]byte(checks), &o.Checks); err != nil {
		return ChecksObservation{}, err
	}
	o.At, err = time.Parse(time.RFC3339Nano, at)
	return o, err
}

// TestedPR returns nil when head meets every condition of job's tested pull
// request but its CI: the job is open, with no assignment still live; Quality
// validated head, whose handoff the daemon fixes only once the job branch has
// taken in the base, and no blocking finding is left open; and the daemon's
// latest push of the job branch pushed head. Otherwise it says which
// condition is not met.
func (s *Store) TestedPR(job int64, head string) error {
	return testedPR(s.db, job, head)
}

func testedPR(q querier, number int64, head string) error {
	j, err := job(q, number)
	if err != nil {
		return err
	}
	if j.State != JobOpen {
		return fmt.Errorf("job %d is %s", number, j.State)
	}
	var live int64
	if err := q.QueryRow(`SELECT COALESCE(MIN(id), 0) FROM assignments WHERE job = ? AND NOT `+endedAssignment, number).Scan(&live); err != nil {
		return err
	}
	if live != 0 {
		return fmt.Errorf("assignment %d of job %d has not ended", live, number)
	}
	var result, pushed string
	err = q.QueryRow(`SELECT result, commit_id FROM pushes WHERE job = ? ORDER BY id DESC LIMIT 1`, number).Scan(&result, &pushed)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("the daemon has not pushed job %d's branch", number)
	}
	if err != nil {
		return err
	}
	if result != PushPushed || pushed != head {
		return fmt.Errorf("the daemon's latest push of job %d's branch is %s at %s, not pushed at %s", number, result, pushed, head)
	}
	var validated int64
	if err := q.QueryRow(`SELECT COALESCE(MAX(a.id), 0) FROM assignments a JOIN workspaces w ON w.assignment = a.id WHERE a.job = ? AND a.read_only = 1 AND a.state = ? AND w.base = ?`, number, AssignmentAccepted, head).Scan(&validated); err != nil {
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

// JobDelivered records d, the delivery of d.Job, once the daemon has marked
// its pull request ready for review with every check on d.Head green. In the
// same step it checks that d.Head meets every other condition of the job's
// tested pull request (TestedPR), ends the job, and tells Coordination's
// leader, with a message of kind MessageDelivery with body, whose dispatch is
// returned. It journals the delivery and the job's end.
func (s *Store) JobDelivered(d Delivery, body string, at time.Time) (Delivery, Dispatch, error) {
	if d.Number <= 0 || d.URL == "" || d.Head == "" || len(d.Checks) == 0 {
		return Delivery{}, Dispatch{}, errors.New("a delivery has its pull request, its head and the checks green on it")
	}
	for _, c := range d.Checks {
		if c.State != "passed" {
			return Delivery{}, Dispatch{}, fmt.Errorf("check %s on %s is %s, not passed", c.Name, d.Head, c.State)
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Delivery{}, Dispatch{}, err
	}
	defer tx.Rollback()
	if err := testedPR(tx, d.Job, d.Head); err != nil {
		return Delivery{}, Dispatch{}, err
	}
	var number int
	if err := tx.QueryRow(`SELECT number FROM pull_requests WHERE job = ? ORDER BY id DESC LIMIT 1`, d.Job).Scan(&number); err != nil {
		return Delivery{}, Dispatch{}, fmt.Errorf("job %d has no pull request recorded: %w", d.Job, err)
	}
	if number != d.Number {
		return Delivery{}, Dispatch{}, fmt.Errorf("the pull request job %d follows is #%d, not #%d", d.Job, number, d.Number)
	}
	checks, err := json.Marshal(d.Checks)
	if err != nil {
		return Delivery{}, Dispatch{}, err
	}
	d.At = at.UTC()
	if _, err := tx.Exec(`INSERT INTO deliveries (job, number, url, head, checks, created_at) VALUES (?, ?, ?, ?, ?, ?)`, d.Job, d.Number, d.URL, d.Head, string(checks), timestamp(at)); err != nil {
		return Delivery{}, Dispatch{}, err
	}
	if _, err := appendEntry(tx, at, KindJobDelivered, d); err != nil {
		return Delivery{}, Dispatch{}, err
	}
	if _, err := tx.Exec(`UPDATE jobs SET state = ? WHERE number = ?`, JobEnded, d.Job); err != nil {
		return Delivery{}, Dispatch{}, err
	}
	if _, err := appendEntry(tx, at, KindJobEnded, map[string]any{"job": d.Job, "state": JobEnded, "by": "delivery"}); err != nil {
		return Delivery{}, Dispatch{}, err
	}
	dispatch, err := createMessage(tx, messageRef{}, d.Job, Daemon, "leader@coordination", MessageDelivery, body, at)
	if err != nil {
		return Delivery{}, Dispatch{}, err
	}
	return d, dispatch, tx.Commit()
}

// JobDelivery returns job's delivery, or sql.ErrNoRows.
func (s *Store) JobDelivery(job int64) (Delivery, error) {
	var d Delivery
	var checks, at string
	err := s.db.QueryRow(`SELECT job, number, url, head, checks, created_at FROM deliveries WHERE job = ?`, job).Scan(&d.Job, &d.Number, &d.URL, &d.Head, &checks, &at)
	if err != nil {
		return Delivery{}, err
	}
	if err := json.Unmarshal([]byte(checks), &d.Checks); err != nil {
		return Delivery{}, err
	}
	d.At, err = time.Parse(time.RFC3339Nano, at)
	return d, err
}

// JobLocalRemoved journals that the daemon removed what was local to job,
// which has ended: the paths and the branches of AsmAI's clone in removed.
// Its branches stay on origin.
func (s *Store) JobLocalRemoved(job int64, removed []string, at time.Time) error {
	return s.change(at, func(tx *sql.Tx) (string, any, error) {
		return KindJobLocalRemoved, map[string]any{"job": job, "removed": removed, "origin": "kept"}, nil
	})
}

// LocalRemoved reports whether the daemon has removed what was local to job.
func (s *Store) LocalRemoved(job int64) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT count(*) FROM journal WHERE kind = ? AND json_extract(data, '$.job') = ?`, KindJobLocalRemoved, job).Scan(&n)
	return n > 0, err
}

// CheckNames lists the names of checks, comma-separated.
func CheckNames(checks []CheckState) string {
	names := make([]string, 0, len(checks))
	for _, c := range checks {
		names = append(names, c.Name)
	}
	return strings.Join(names, ", ")
}
