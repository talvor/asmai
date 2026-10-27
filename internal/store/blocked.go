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

// The states of a leader's blocked report: open until Coordination resolves
// it.
const (
	BlockedOpen     = "open"
	BlockedResolved = "resolved"
)

// MessageBlockedResolved tells the leader that filed a blocked report that
// Coordination resolved it, with the note of what changed. The report itself
// reaches Coordination's leader as a message of kind ReportBlocked, as a
// worker's blocked report reaches its owning leader.
const MessageBlockedResolved = "blocked.resolved"

// BlockedReport is a leader's report to Coordination that it cannot go on for
// a reason that is not the user's choice, such as a missing build cache, a
// broken tool or an infrastructure fault. It is not a decision request: it has
// no version and no answer surface, and it holds nothing. Blocks only names
// the leader's own assignments that depend on it.
type BlockedReport struct {
	ID     int64   `json:"id"`
	Job    int64   `json:"job"`
	Leader string  `json:"leader"`
	Reason string  `json:"reason"`
	Needs  string  `json:"needs"`
	Blocks []int64 `json:"blocks,omitempty"`
	State  string  `json:"state"`
	// Note is what Coordination said changed when it resolved the report,
	// and ResolvedBy the leader that resolved it.
	Note       string    `json:"note,omitempty"`
	ResolvedBy string    `json:"resolved_by,omitempty"`
	At         time.Time `json:"at"`
	ResolvedAt time.Time `json:"resolved_at,omitzero"`
}

// BlockedReportInput is what a leader's blocked report says, and the leader
// of Coordination it goes to.
type BlockedReportInput struct {
	Job       int64
	Leader    string
	Recipient string
	Reason    string
	Needs     string
	Blocks    []int64
}

const blockedReportColumns = `id, job, leader, reason, needs, blocks, state, note, resolved_by, created_at, resolved_at`

func scanBlockedReport(row interface{ Scan(...any) error }) (BlockedReport, error) {
	var b BlockedReport
	var blocks, created string
	var resolved sql.NullString
	if err := row.Scan(&b.ID, &b.Job, &b.Leader, &b.Reason, &b.Needs, &blocks, &b.State, &b.Note, &b.ResolvedBy, &created, &resolved); err != nil {
		return BlockedReport{}, err
	}
	if err := json.Unmarshal([]byte(blocks), &b.Blocks); err != nil {
		return BlockedReport{}, err
	}
	var err error
	if b.At, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return BlockedReport{}, err
	}
	if resolved.Valid {
		if b.ResolvedAt, err = time.Parse(time.RFC3339Nano, resolved.String); err != nil {
			return BlockedReport{}, err
		}
	}
	return b, nil
}

func blockedReport(q queryer, id int64) (BlockedReport, error) {
	return scanBlockedReport(q.QueryRow(`SELECT `+blockedReportColumns+` FROM blocked_reports WHERE id = ?`, id))
}

// BlockedReport returns blocked report id.
func (s *Store) BlockedReport(id int64) (BlockedReport, error) { return blockedReport(s.db, id) }

// JobBlockedReports returns the blocked reports of job, open and resolved,
// oldest first.
func (s *Store) JobBlockedReports(job int64) ([]BlockedReport, error) {
	rows, err := s.db.Query(`SELECT `+blockedReportColumns+` FROM blocked_reports WHERE job = ? ORDER BY id`, job)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var reports []BlockedReport
	for rows.Next() {
		b, err := scanBlockedReport(rows)
		if err != nil {
			return nil, err
		}
		reports = append(reports, b)
	}
	return reports, rows.Err()
}

// BlockedReported records the blocked report in describes, with the message
// that delivers it to in.Recipient, Coordination's leader, and journals it.
// The job is open, and each assignment it names is one of the job's that the
// leader owns and that has not ended. It returns the report and the
// dispatch that carries it.
func (s *Store) BlockedReported(in BlockedReportInput, at time.Time) (BlockedReport, Dispatch, error) {
	switch {
	case in.Job <= 0:
		return BlockedReport{}, Dispatch{}, errors.New("a blocked report names its job: give --job")
	case blank(in.Reason):
		return BlockedReport{}, Dispatch{}, errors.New("a blocked report needs --reason: why you cannot go on")
	case blank(in.Needs):
		return BlockedReport{}, Dispatch{}, errors.New("a blocked report needs --needs: what would let you go on")
	case in.Leader == "" || in.Recipient == "":
		return BlockedReport{}, Dispatch{}, errors.New("a blocked report needs the leader that files it and the leader it goes to")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return BlockedReport{}, Dispatch{}, err
	}
	defer tx.Rollback()
	j, err := job(tx, in.Job)
	if errors.Is(err, sql.ErrNoRows) {
		return BlockedReport{}, Dispatch{}, fmt.Errorf("job %d does not exist; `asmai jobs` lists the jobs", in.Job)
	}
	if err != nil {
		return BlockedReport{}, Dispatch{}, err
	}
	if j.State != JobOpen {
		return BlockedReport{}, Dispatch{}, fmt.Errorf("job %d is %s, so nothing of it can be blocked", j.Number, j.State)
	}
	var blocks []int64
	for _, id := range in.Blocks {
		var assignmentJob int64
		var owner, state string
		err := tx.QueryRow(`SELECT job, owner, state FROM assignments WHERE id = ?`, id).Scan(&assignmentJob, &owner, &state)
		if errors.Is(err, sql.ErrNoRows) {
			return BlockedReport{}, Dispatch{}, fmt.Errorf("assignment %d does not exist", id)
		}
		if err != nil {
			return BlockedReport{}, Dispatch{}, err
		}
		switch {
		case owner != in.Leader:
			return BlockedReport{}, Dispatch{}, fmt.Errorf("assignment %d is owned by %s, not %s: --blocks names only your own assignments that depend on the report", id, owner, in.Leader)
		case assignmentJob != in.Job:
			return BlockedReport{}, Dispatch{}, fmt.Errorf("assignment %d is not in job %d", id, in.Job)
		case state == AssignmentAccepted || state == AssignmentCancelled:
			return BlockedReport{}, Dispatch{}, fmt.Errorf("assignment %d is %s, so no work of it depends on the report", id, state)
		}
		if !slices.Contains(blocks, id) {
			blocks = append(blocks, id)
		}
	}
	encoded, err := json.Marshal(append([]int64{}, blocks...))
	if err != nil {
		return BlockedReport{}, Dispatch{}, err
	}
	b := BlockedReport{Job: in.Job, Leader: in.Leader, Reason: strings.TrimSpace(in.Reason), Needs: strings.TrimSpace(in.Needs), Blocks: blocks, State: BlockedOpen, At: at.UTC()}
	result, err := tx.Exec(`INSERT INTO blocked_reports (job, leader, reason, needs, blocks, state, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		b.Job, b.Leader, b.Reason, b.Needs, string(encoded), b.State, timestamp(at))
	if err != nil {
		return BlockedReport{}, Dispatch{}, err
	}
	if b.ID, err = result.LastInsertId(); err != nil {
		return BlockedReport{}, Dispatch{}, err
	}
	if _, err = appendEntry(tx, at, KindBlockedReportFiled, b); err != nil {
		return BlockedReport{}, Dispatch{}, err
	}
	d, err := createMessage(tx, messageRef{blocked: b.ID}, b.Job, b.Leader, in.Recipient, ReportBlocked, b.Reason, at)
	if err != nil {
		return BlockedReport{}, Dispatch{}, err
	}
	return b, d, tx.Commit()
}

// BlockedResolved records leader resolving blocked report id with note, what
// changed, and delivers the note to the leader that filed it, and journals it.
// A report already resolved is refused. It returns the report and the
// dispatch that carries the note.
func (s *Store) BlockedResolved(id int64, leader, note string, at time.Time) (BlockedReport, Dispatch, error) {
	if blank(note) {
		return BlockedReport{}, Dispatch{}, errors.New("resolving a blocked report needs --note: what changed")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return BlockedReport{}, Dispatch{}, err
	}
	defer tx.Rollback()
	b, err := blockedReport(tx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return BlockedReport{}, Dispatch{}, fmt.Errorf("blocked report %d does not exist; `asmai brief <job>` lists a job's open blocked reports", id)
	}
	if err != nil {
		return BlockedReport{}, Dispatch{}, err
	}
	if b.State != BlockedOpen {
		return BlockedReport{}, Dispatch{}, fmt.Errorf("blocked report %d was already resolved by %s: %s", id, b.ResolvedBy, b.Note)
	}
	b.State, b.Note, b.ResolvedBy, b.ResolvedAt = BlockedResolved, strings.TrimSpace(note), leader, at.UTC()
	if _, err = tx.Exec(`UPDATE blocked_reports SET state = ?, note = ?, resolved_by = ?, resolved_at = ? WHERE id = ? AND state = ?`, b.State, b.Note, b.ResolvedBy, timestamp(at), id, BlockedOpen); err != nil {
		return BlockedReport{}, Dispatch{}, err
	}
	if _, err = appendEntry(tx, at, KindBlockedReportResolved, b); err != nil {
		return BlockedReport{}, Dispatch{}, err
	}
	d, err := createMessage(tx, messageRef{blocked: b.ID}, b.Job, leader, b.Leader, MessageBlockedResolved, b.Note, at)
	if err != nil {
		return BlockedReport{}, Dispatch{}, err
	}
	return b, d, tx.Commit()
}
