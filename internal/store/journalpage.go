// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"
)

// JournalQuery picks a page of the journal. Entries are read in a direction
// from a cursor: the latest page when neither Before nor After is set, the
// entries older than Before, or the entries newer than After.
type JournalQuery struct {
	// Kinds, Job and Agent narrow the page; a zero value does not. Agent
	// matches an entry that names the agent as its sender, recipient, owner,
	// worker, leader or reporter.
	Kinds []string
	Job   int64
	Agent string
	// Before and After are journal IDs, the cursor into the journal in each
	// direction.
	Before, After int64
	// Limit is the most entries a page holds.
	Limit int
}

// agentFields are the fields of a journal entry's data that name an agent.
var agentFields = []string{"sender", "receiver", "recipient", "owner", "worker", "leader", "agent"}

// where is the condition on journal rows that q's filters make, with its
// arguments; it is "1" when q has no filter.
func (q JournalQuery) where() (string, []any) {
	conds, args := []string{"1"}, []any{}
	if len(q.Kinds) > 0 {
		conds = append(conds, "kind IN ("+strings.TrimSuffix(strings.Repeat("?,", len(q.Kinds)), ",")+")")
		for _, k := range q.Kinds {
			args = append(args, k)
		}
	}
	if q.Job != 0 {
		conds = append(conds, "(json_extract(data, '$.job') = ? OR (kind = 'job.opened' AND json_extract(data, '$.number') = ?))")
		args = append(args, q.Job, q.Job)
	}
	if q.Agent != "" {
		var names []string
		for _, f := range agentFields {
			names = append(names, "json_extract(data, '$."+f+"') = ?")
			args = append(args, q.Agent)
		}
		names = append(names, "EXISTS (SELECT 1 FROM assignments WHERE assignments.id = json_extract(journal.data, '$.assignment') AND (assignments.owner = ? OR assignments.worker = ?))")
		args = append(args, q.Agent, q.Agent)
		conds = append(conds, "("+strings.Join(names, " OR ")+")")
	}
	return strings.Join(conds, " AND "), args
}

// JournalPage reads a page of the journal, oldest first, and says whether
// entries that match remain before and after it.
func (s *Store) JournalPage(q JournalQuery) (entries []Entry, older, newer bool, err error) {
	where, filter := q.where()
	query, args, order := `SELECT id, at, kind, data FROM journal WHERE `+where, append([]any{}, filter...), ` ORDER BY id DESC LIMIT ?`
	switch {
	case q.After > 0:
		query += ` AND id > ?`
		order = ` ORDER BY id ASC LIMIT ?`
		args = append(args, q.After)
	case q.Before > 0:
		query += ` AND id < ?`
		args = append(args, q.Before)
	}
	rows, err := s.db.Query(query+order, append(args, max(q.Limit, 1))...)
	if err != nil {
		return nil, false, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var e Entry
		var at, data string
		if err := rows.Scan(&e.ID, &at, &e.Kind, &data); err != nil {
			return nil, false, false, err
		}
		if e.At, err = time.Parse(time.RFC3339Nano, at); err != nil {
			return nil, false, false, err
		}
		e.Data = json.RawMessage(data)
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, false, false, err
	}
	if len(entries) == 0 {
		return entries, false, false, nil
	}
	if q.After == 0 {
		slices.Reverse(entries)
	}
	exists := func(cond string, id int64) (bool, error) {
		var one int
		err := s.db.QueryRow(`SELECT 1 FROM journal WHERE `+where+` AND `+cond+` LIMIT 1`, append(append([]any{}, filter...), id)...).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return err == nil, err
	}
	if older, err = exists("id < ?", entries[0].ID); err != nil {
		return nil, false, false, err
	}
	if newer, err = exists("id > ?", entries[len(entries)-1].ID); err != nil {
		return nil, false, false, err
	}
	return entries, older, newer, nil
}

// MessageBodyOf is the body of the message that a journal entry caused, or
// of the decision it records: a handoff's outcome, an assignment's outcome, a
// result's PR section, a blocked report's reason, a message as it was sent,
// or the reasons for accepting or rejecting a result. It is empty when the
// entry carries none.
func (s *Store) MessageBodyOf(e Entry) (string, error) {
	var data struct {
		ID      int64    `json:"id"`
		Message int64    `json:"message"`
		Reasons []string `json:"reasons"`
	}
	if err := json.Unmarshal(e.Data, &data); err != nil {
		return "", nil
	}
	var query string
	var id int64
	switch e.Kind {
	case KindMessageCreated:
		query, id = `SELECT body FROM messages WHERE id = ?`, data.Message
	case KindHandoffSent:
		query, id = `SELECT body FROM messages WHERE handoff = ? AND kind = 'handoff' ORDER BY id LIMIT 1`, data.ID
	case KindAssignmentCreated:
		query, id = `SELECT body FROM messages WHERE assignment = ? AND kind = 'assignment' ORDER BY id LIMIT 1`, data.ID
	case KindResultSubmitted, KindBlockedReported:
		query, id = `SELECT body FROM messages WHERE report = ? ORDER BY id LIMIT 1`, data.ID
	case KindResultAccepted, KindResultRejected:
		return strings.Join(data.Reasons, "\n"), nil
	default:
		return "", nil
	}
	var body string
	err := s.db.QueryRow(query, id).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return body, err
}
