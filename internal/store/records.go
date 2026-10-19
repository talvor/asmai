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

// Check reports what d lacks to be a decision record. A record names the
// accountable leader and the choice before anything else, and a human
// decision cites the witnessed message it came from.
func (d DecisionRecord) Check() error {
	switch {
	case strings.TrimSpace(d.Leader) == "":
		return errors.New("a decision names the accountable leader")
	case strings.TrimSpace(d.Choice) == "":
		return errors.New("a decision names its choice")
	case d.Job <= 0:
		return errors.New("a decision is about a job")
	case !slices.Contains(Sources, d.Source):
		return fmt.Errorf("a decision's source is %s, not %q", strings.Join(Sources, " or "), d.Source)
	case strings.TrimSpace(d.ReliedOn) == "":
		return errors.New("a decision names the mandate or grant it relied on")
	case strings.TrimSpace(d.Rationale) == "":
		return errors.New("a decision gives its rationale")
	case strings.TrimSpace(d.Evidence) == "":
		return errors.New("a decision gives its evidence")
	case d.Source == SourceHuman && d.Cites <= 0:
		return errors.New("a human decision cites the witnessed message it came from")
	case d.Source == SourceDelegated && d.Cites != 0:
		return errors.New("a delegated decision cites no witnessed message: only a human decision has one")
	}
	if slices.ContainsFunc(d.Assumptions, blank) {
		return errors.New("each assumption must have text")
	}
	if slices.ContainsFunc(d.ReopenWhen, blank) {
		return errors.New("each condition that would reopen a decision must have text")
	}
	return nil
}

func nullIfZero(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

func blank(text string) bool { return strings.TrimSpace(text) == "" }

// DecisionRecorded records d, the decision of d.Leader's session of
// generation, and journals it. d.Dispatch is the dispatch the leader is
// working on, if it is working on one, and the job must be open. A human decision cites a witnessed message, which
// must exist. Routine work needs no record: nothing else in the store asks
// for one.
func (s *Store) DecisionRecorded(d DecisionRecord, generation int, at time.Time) (DecisionRecord, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return DecisionRecord{}, err
	}
	defer tx.Rollback()
	if d, err = insertDecisionRecord(tx, d, generation, at); err != nil {
		return DecisionRecord{}, err
	}
	return d, tx.Commit()
}

// insertDecisionRecord checks and inserts d in tx, and journals it.
func insertDecisionRecord(tx *sql.Tx, d DecisionRecord, generation int, at time.Time) (DecisionRecord, error) {
	if d.Source == "" {
		d.Source = SourceDelegated
	}
	if err := d.Check(); err != nil {
		return DecisionRecord{}, err
	}
	j, err := job(tx, d.Job)
	if errors.Is(err, sql.ErrNoRows) {
		return DecisionRecord{}, fmt.Errorf("job %d does not exist", d.Job)
	}
	if err != nil {
		return DecisionRecord{}, err
	}
	if j.State != JobOpen {
		return DecisionRecord{}, fmt.Errorf("job %d is %s, so it takes no new decision", j.Number, j.State)
	}
	// A leader in a conversation, such as Coordination's, has no dispatch;
	// otherwise the record is tagged with the one the leader is working on.
	if d.Dispatch != 0 {
		if _, _, err := currentDispatch(tx, d.Dispatch, d.Leader, generation); err != nil {
			return DecisionRecord{}, err
		}
	}
	if d.Source == SourceHuman {
		var kind string
		if err := tx.QueryRow(`SELECT kind FROM journal WHERE id = ?`, d.Cites).Scan(&kind); errors.Is(err, sql.ErrNoRows) {
			return DecisionRecord{}, fmt.Errorf("witnessed message %d does not exist", d.Cites)
		} else if err != nil {
			return DecisionRecord{}, err
		}
		if kind != KindMessageWitnessed {
			return DecisionRecord{}, fmt.Errorf("journal entry %d is %s, not a witnessed message", d.Cites, kind)
		}
	}
	d.Assumptions, d.ReopenWhen, d.At = nonNil(d.Assumptions), nonNil(d.ReopenWhen), at.UTC()
	assumptions, err := json.Marshal(d.Assumptions)
	if err != nil {
		return DecisionRecord{}, err
	}
	reopen, err := json.Marshal(d.ReopenWhen)
	if err != nil {
		return DecisionRecord{}, err
	}
	result, err := tx.Exec(`INSERT INTO decision_records (job, leader, source, cites, relied_on, choice, rationale, evidence, assumptions, reopen_when, dispatch, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.Job, d.Leader, d.Source, d.Cites, d.ReliedOn, d.Choice, d.Rationale, d.Evidence, string(assumptions), string(reopen), nullIfZero(d.Dispatch), timestamp(at))
	if err != nil {
		return DecisionRecord{}, err
	}
	if d.ID, err = result.LastInsertId(); err != nil {
		return DecisionRecord{}, err
	}
	if _, err := appendEntry(tx, at, KindDecisionRecorded, d); err != nil {
		return DecisionRecord{}, err
	}
	return d, nil
}

const decisionRecordColumns = `id, job, leader, source, cites, relied_on, choice, rationale, evidence, assumptions, reopen_when, COALESCE(dispatch, 0), created_at`

func scanDecisionRecords(q querier, where string, args ...any) ([]DecisionRecord, error) {
	rows, err := q.Query(`SELECT `+decisionRecordColumns+` FROM decision_records WHERE `+where+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := []DecisionRecord{}
	for rows.Next() {
		var d DecisionRecord
		var assumptions, reopen, at string
		if err := rows.Scan(&d.ID, &d.Job, &d.Leader, &d.Source, &d.Cites, &d.ReliedOn, &d.Choice, &d.Rationale, &d.Evidence, &assumptions, &reopen, &d.Dispatch, &at); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(assumptions), &d.Assumptions); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(reopen), &d.ReopenWhen); err != nil {
			return nil, err
		}
		if d.At, err = time.Parse(time.RFC3339Nano, at); err != nil {
			return nil, err
		}
		records = append(records, d)
	}
	return records, rows.Err()
}

// DecisionRecord returns the decision record id, or sql.ErrNoRows.
func (s *Store) DecisionRecord(id int64) (DecisionRecord, error) {
	records, err := scanDecisionRecords(s.db, `id = ?`, id)
	if err != nil {
		return DecisionRecord{}, err
	}
	if len(records) == 0 {
		return DecisionRecord{}, sql.ErrNoRows
	}
	return records[0], nil
}

// CheckRecords checks that ids are distinct decision records of job, which its
// owning leader may pass on in an assignment or a handoff.
func (s *Store) CheckRecords(job int64, ids []int64) error {
	_, err := passedRecords(s.db, job, ids)
	return err
}

// passedRecords checks that ids are distinct decision records of job, the
// ones its owning leader passes on, and returns them in the order given.
func passedRecords(q querier, job int64, ids []int64) ([]DecisionRecord, error) {
	passed := make([]DecisionRecord, 0, len(ids))
	for _, id := range ids {
		if slices.ContainsFunc(passed, func(r DecisionRecord) bool { return r.ID == id }) {
			return nil, fmt.Errorf("decision record %d is passed twice", id)
		}
		records, err := scanDecisionRecords(q, `id = ?`, id)
		if err != nil {
			return nil, err
		}
		if len(records) == 0 {
			return nil, fmt.Errorf("decision record %d does not exist; `asmai brief %d` lists the job's decisions", id, job)
		}
		if records[0].Job != job {
			return nil, fmt.Errorf("decision record %d is of job %d, not job %d: only a job's own decisions are passed on", id, records[0].Job, job)
		}
		passed = append(passed, records[0])
	}
	return passed, nil
}

// recordIDs is the IDs of records, for the column that holds them.
func recordIDs(records []DecisionRecord) []int64 {
	ids := make([]int64, 0, len(records))
	for _, r := range records {
		ids = append(ids, r.ID)
	}
	return ids
}

// loadRecords reads the IDs a record column holds and the records they name,
// in the order they were passed.
func loadRecords(q querier, column string) (ids []int64, records []DecisionRecord, err error) {
	if err := json.Unmarshal([]byte(column), &ids); err != nil {
		return nil, nil, err
	}
	if len(ids) == 0 {
		return nil, nil, nil
	}
	for _, id := range ids {
		found, err := scanDecisionRecords(q, `id = ?`, id)
		if err != nil {
			return nil, nil, err
		}
		if len(found) == 0 {
			return nil, nil, fmt.Errorf("decision record %d is missing", id)
		}
		records = append(records, found[0])
	}
	return ids, records, nil
}

// Text writes d with every field of rule 25, one to a line: the first line
// starts with first and the others with rest.
func (d DecisionRecord) Text(first, rest string) string {
	source := d.Source
	if d.Source == SourceHuman {
		source = fmt.Sprintf("human, citing witnessed message %d", d.Cites)
	}
	list := func(items []string) string {
		if len(items) == 0 {
			return "none"
		}
		return strings.Join(items, "; ")
	}
	return fmt.Sprintf("%sdecision %d | job %d | %s | accountable leader %s | %s\n%sChoice: %s\n%sRelied on: %s\n%sRationale: %s\n%sEvidence: %s\n%sAssumptions: %s\n%sReopen when: %s\n",
		first, d.ID, d.Job, source, d.Leader, d.At.Format(time.RFC3339),
		rest, d.Choice, rest, d.ReliedOn, rest, d.Rationale, rest, d.Evidence,
		rest, list(d.Assumptions), rest, list(d.ReopenWhen))
}
