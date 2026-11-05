// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// LastLeft returns the journal ID and the time of the entry that records the
// user's last leaving of the conversation, and zero values if they never have
// (04 rule 15). The catch-up covers what changed after it.
func (s *Store) LastLeft() (id int64, at time.Time, err error) {
	var stamp string
	err = s.db.QueryRow(`SELECT id, at FROM journal WHERE kind = ? ORDER BY id DESC LIMIT 1`, KindConversationLeft).Scan(&id, &stamp)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, time.Time{}, nil
	}
	if err != nil {
		return 0, time.Time{}, err
	}
	at, err = time.Parse(time.RFC3339Nano, stamp)
	return id, at, err
}

// UnsentTextLeft returns the text the user left unsent when they left the
// conversation at the journal entry left, which the catch-up offers back
// (04 rule 16). It is empty when leaving saved none.
func (s *Store) UnsentTextLeft(left int64) ([]UnsentText, error) {
	rows, err := s.db.Query(`SELECT id, at, kind, data FROM journal WHERE kind = ? AND id > ? ORDER BY id`, KindUnsentTextSaved, left)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	texts := []UnsentText{}
	for rows.Next() {
		var id int64
		var stamp, kind, data string
		if err := rows.Scan(&id, &stamp, &kind, &data); err != nil {
			return nil, err
		}
		var u UnsentText
		if err := json.Unmarshal([]byte(data), &u); err != nil {
			return nil, err
		}
		if u.Left != left {
			continue
		}
		texts = append(texts, u)
	}
	return texts, rows.Err()
}

// EntriesAfter returns the journal entries of the given kinds with an ID
// above after, oldest first.
func (s *Store) EntriesAfter(after int64, kinds ...string) ([]Entry, error) {
	if len(kinds) == 0 {
		return []Entry{}, nil
	}
	marks := strings.TrimSuffix(strings.Repeat("?, ", len(kinds)), ", ")
	args := []any{after}
	for _, k := range kinds {
		args = append(args, k)
	}
	rows, err := s.db.Query(`SELECT id, at, kind, data FROM journal WHERE id > ? AND kind IN (`+marks+`) ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := []Entry{}
	for rows.Next() {
		var e Entry
		var stamp, data string
		if err := rows.Scan(&e.ID, &stamp, &e.Kind, &data); err != nil {
			return nil, err
		}
		if e.At, err = time.Parse(time.RFC3339Nano, stamp); err != nil {
			return nil, err
		}
		e.Data = []byte(data)
		entries = append(entries, e)
	}
	return entries, rows.Err()
}
