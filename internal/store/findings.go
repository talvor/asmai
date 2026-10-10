// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// The kinds of finding a validation reports. A blocking finding must be
// fixed before delivery, and only a later validation or the user's decision
// clears it. An advisory finding is reported and needs no fix. A needs-you
// finding challenges the user's stated intent or a recorded decision, so it
// goes to the user.
const (
	FindingBlocking = "blocking"
	FindingAdvisory = "advisory"
	FindingNeedsYou = "needs-you"
)

// FindingKinds are the kinds of finding, in the order a report lists them.
var FindingKinds = []string{FindingBlocking, FindingAdvisory, FindingNeedsYou}

// FindingInput is a finding as a Quality worker reports it.
type FindingInput struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}

// ValidationInput is what a Quality worker says in a validation report,
// beside the checks it re-ran and its evidence and gaps.
type ValidationInput struct {
	// Findings are what the review found. It is given even when empty, so
	// that a report with none says so.
	Findings []FindingInput `json:"findings"`
	// Resolved are the earlier blocking findings of the job that the worker
	// confirms are resolved at this commit, and StillOpen those it finds
	// are not. Each earlier blocking finding that is still open is in one.
	Resolved  []int64 `json:"resolved,omitempty"`
	StillOpen []int64 `json:"still_open,omitempty"`
}

// Finding is a problem a validation reported, recorded when Quality's leader
// accepted the report.
type Finding struct {
	ID         int64  `json:"id"`
	Job        int64  `json:"job"`
	Assignment int64  `json:"assignment"`
	Report     int64  `json:"report"`
	Kind       string `json:"kind"`
	Text       string `json:"text"`
	// Commit is the exact head commit the validation found it at.
	Commit string `json:"commit"`
	// ClearedBy is the validation report that confirmed a blocking finding
	// resolved. It is zero while the finding is open.
	ClearedBy int64 `json:"cleared_by,omitempty"`
}

// Open reports whether f is a blocking finding that no validation has
// cleared.
func (f Finding) Open() bool {
	return f.Kind == FindingBlocking && f.ClearedBy == 0
}

// Intent is what a read-only assignment is given of its job, so that its
// worker reviews against the user's intent: the user's words, Coordination's
// reading of them, the job's mandate and its acceptance criteria.
type Intent struct {
	Words    string   `json:"words"`
	Reading  string   `json:"reading"`
	Mandate  string   `json:"mandate"`
	Criteria []string `json:"acceptance_criteria"`
}

// checkFinding refuses a finding of no known kind or with no text.
func checkFinding(f FindingInput) error {
	if !slices.Contains(FindingKinds, f.Kind) {
		return fmt.Errorf("a finding is %s, not %q", strings.Join(FindingKinds, ", "), f.Kind)
	}
	if strings.TrimSpace(f.Text) == "" {
		return errors.New("a finding has text saying what is wrong")
	}
	return nil
}

// checkValidation checks a validation report against the open blocking
// findings of its job: the worker names its findings, and says for each open
// blocking finding whether it is resolved at this commit.
func checkValidation(q querier, a Assignment, in ReportInput) error {
	v := in.Validation
	if v == nil {
		return errors.New("a validation report names its findings: give each as --finding '<blocking|advisory|needs-you>: <text>', or --finding none")
	}
	if len(in.Evidence) == 0 {
		return errors.New("a validation report links its evidence: what shows the checks ran at the commit")
	}
	for _, evidence := range in.Evidence {
		if strings.TrimSpace(evidence) == "" {
			return errors.New("validation evidence cannot be blank")
		}
	}
	for _, f := range v.Findings {
		if err := checkFinding(f); err != nil {
			return err
		}
	}
	open, err := openBlocking(q, a.Job)
	if err != nil {
		return err
	}
	isOpen := map[int64]bool{}
	for _, f := range open {
		isOpen[f.ID] = true
	}
	accounted := map[int64]bool{}
	for _, list := range [][]int64{v.Resolved, v.StillOpen} {
		for _, id := range list {
			if !isOpen[id] {
				return fmt.Errorf("finding %d is not an open blocking finding of job %d", id, a.Job)
			}
			if accounted[id] {
				return fmt.Errorf("finding %d is named twice: it is resolved or still open", id)
			}
			accounted[id] = true
		}
	}
	var missing []string
	for _, f := range open {
		if !accounted[f.ID] {
			missing = append(missing, fmt.Sprintf("%d (%s)", f.ID, f.Text))
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("a validation says whether each earlier blocking finding is resolved at its commit; name each with --resolved <id>, or --unresolved <id> when it is still open: %s", strings.Join(missing, "; "))
	}
	return nil
}

// querier is what the readers of findings need of a database or a
// transaction.
type querier interface {
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

const findingColumns = `id, job, assignment, report, kind, text, commit_id, COALESCE(cleared_by, 0)`

func findings(q querier, where string, args ...any) ([]Finding, error) {
	rows, err := q.Query(`SELECT `+findingColumns+` FROM findings WHERE `+where+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []Finding{}
	for rows.Next() {
		var f Finding
		if err := rows.Scan(&f.ID, &f.Job, &f.Assignment, &f.Report, &f.Kind, &f.Text, &f.Commit, &f.ClearedBy); err != nil {
			return nil, err
		}
		list = append(list, f)
	}
	return list, rows.Err()
}

// openBlocking returns the blocking findings of job that no validation has
// cleared, oldest first.
func openBlocking(q querier, job int64) ([]Finding, error) {
	return findings(q, `job = ? AND kind = 'blocking' AND cleared_by IS NULL`, job)
}

// OpenFindings returns the blocking findings of job that no validation has
// cleared, oldest first.
func (s *Store) OpenFindings(job int64) ([]Finding, error) {
	return openBlocking(s.db, job)
}

// Findings returns every finding of job, oldest first, with whether and by
// which report each blocking one was cleared.
func (s *Store) Findings(job int64) ([]Finding, error) {
	return findings(s.db, `job = ?`, job)
}

// ValidationAccepted records the acceptance by Quality's leader of the
// validation report its read-only assignment is submitted with, and journals
// it with the findings it records and the earlier blocking findings it
// clears. The assignment ends, and the report is sent to deliverTo, the
// delivery owner, as a message with a dispatch of its own, which is
// returned. Nothing but a validation clears a blocking finding.
func (s *Store) ValidationAccepted(in Acceptance, deliverTo string, at time.Time) (Decision, Dispatch, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Decision{}, Dispatch{}, err
	}
	defer tx.Rollback()
	a, err := decided(tx, in.Assignment, in.Leader, in.Reasons)
	if err != nil {
		return Decision{}, Dispatch{}, err
	}
	if !a.ReadOnly {
		return Decision{}, Dispatch{}, fmt.Errorf("assignment %d is a writing assignment, which has no validation to accept", a.ID)
	}
	if a.State != AssignmentSubmitted {
		return Decision{}, Dispatch{}, fmt.Errorf("assignment %d is %s, so it has no validation report to accept", a.ID, a.State)
	}
	r, err := latestResult(tx, a.ID)
	if err != nil {
		return Decision{}, Dispatch{}, err
	}
	if r.Validation == nil {
		return Decision{}, Dispatch{}, fmt.Errorf("assignment %d has no validation report", a.ID)
	}
	recorded := []Finding{}
	for _, f := range r.Validation.Findings {
		result, err := tx.Exec(`INSERT INTO findings (job, assignment, report, kind, text, commit_id, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			a.Job, a.ID, r.ID, f.Kind, f.Text, r.Commit, timestamp(at))
		if err != nil {
			return Decision{}, Dispatch{}, err
		}
		id, err := result.LastInsertId()
		if err != nil {
			return Decision{}, Dispatch{}, err
		}
		recorded = append(recorded, Finding{ID: id, Job: a.Job, Assignment: a.ID, Report: r.ID, Kind: f.Kind, Text: f.Text, Commit: r.Commit})
	}
	for _, id := range r.Validation.Resolved {
		result, err := tx.Exec(`UPDATE findings SET cleared_by = ?, cleared_at = ? WHERE id = ? AND job = ? AND kind = 'blocking' AND cleared_by IS NULL`,
			r.ID, timestamp(at), id, a.Job)
		if err != nil {
			return Decision{}, Dispatch{}, err
		}
		if n, err := result.RowsAffected(); err != nil || n != 1 {
			return Decision{}, Dispatch{}, fmt.Errorf("finding %d is not an open blocking finding of job %d", id, a.Job)
		}
	}
	if _, err := tx.Exec(`UPDATE assignments SET state = ? WHERE id = ?`, AssignmentAccepted, a.ID); err != nil {
		return Decision{}, Dispatch{}, err
	}
	if _, err := appendEntry(tx, at, KindValidationAccepted, map[string]any{
		"job": a.Job, "assignment": a.ID, "report": r.ID, "commit": r.Commit,
		"findings": recorded, "cleared": nonNilIDs(r.Validation.Resolved), "still_open": nonNilIDs(r.Validation.StillOpen),
	}); err != nil {
		return Decision{}, Dispatch{}, err
	}
	d, err := decide(tx, Decision{Assignment: a.ID, Job: a.Job, Kind: DecisionAccepted, Leader: in.Leader, Report: r.ID, Commit: r.Commit, Reasons: in.Reasons}, KindResultAccepted, at)
	if err != nil {
		return Decision{}, Dispatch{}, err
	}
	dispatch, err := createMessage(tx, messageRef{assignment: a.ID, report: r.ID}, a.Job, in.Leader, deliverTo, MessageValidation, validationSummary(r, recorded), at)
	if err != nil {
		return Decision{}, Dispatch{}, err
	}
	return d, dispatch, tx.Commit()
}

func nonNilIDs(ids []int64) []int64 {
	if ids == nil {
		return []int64{}
	}
	return ids
}

// validationSummary is the line a validation report is delivered with.
func validationSummary(r Report, recorded []Finding) string {
	counts := map[string]int{}
	for _, f := range recorded {
		counts[f.Kind]++
	}
	return fmt.Sprintf("Validation of commit %s: %d blocking, %d advisory, %d needs-you finding(s); %d earlier blocking finding(s) resolved, %d still open",
		r.Commit, counts[FindingBlocking], counts[FindingAdvisory], counts[FindingNeedsYou], len(r.Validation.Resolved), len(r.Validation.StillOpen))
}
