// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// The journal entries about a dispatch's terminal recording (03 rules 41
// and 43). KindRecordingStarted records where it is, KindRecordingFinished
// its size once the terminal stopped being recorded for the dispatch, and
// KindRecordingDeleted its deletion, once its job had ended for the
// retention period.
const (
	KindRecordingStarted  = "recording.started"
	KindRecordingFinished = "recording.finished"
	KindRecordingDeleted  = "recording.deleted"
)

// Recording is the compressed recording of the terminal of the agent a
// dispatch went to, kept as a file in the state directory.
type Recording struct {
	Dispatch int64     `json:"dispatch"`
	Agent    string    `json:"agent"`
	Job      int64     `json:"job"`
	Path     string    `json:"path"`
	Size     int64     `json:"size"`
	Started  time.Time `json:"started_at"`
	// Deleted is when the recording's file was deleted, or zero.
	Deleted time.Time `json:"deleted_at,omitzero"`
}

const recordingColumns = `r.dispatch, d.agent, m.job, r.path, r.size, r.started_at, COALESCE(r.deleted_at, '')`

func scanRecording(row interface{ Scan(...any) error }) (Recording, error) {
	var r Recording
	var started, deleted string
	if err := row.Scan(&r.Dispatch, &r.Agent, &r.Job, &r.Path, &r.Size, &started, &deleted); err != nil {
		return Recording{}, err
	}
	var err error
	if r.Started, err = time.Parse(time.RFC3339Nano, started); err != nil {
		return Recording{}, err
	}
	if deleted != "" {
		r.Deleted, err = time.Parse(time.RFC3339Nano, deleted)
	}
	return r, err
}

// Recording returns the recording of dispatch, and whether it has one.
func (s *Store) Recording(dispatch int64) (Recording, bool, error) {
	r, err := scanRecording(s.db.QueryRow(`SELECT `+recordingColumns+` FROM recordings r JOIN dispatches d ON d.id = r.dispatch JOIN messages m ON m.id = d.message WHERE r.dispatch = ?`, dispatch))
	if errors.Is(err, sql.ErrNoRows) {
		return Recording{}, false, nil
	}
	return r, err == nil, err
}

// RecordingStarted records that the terminal of dispatch's agent is recorded
// in the file at path, and journals where. A dispatch typed to its agent
// again, as after the user interrupted it, keeps the recording it has, and
// nothing more is journaled. It returns the dispatch's recording.
func (s *Store) RecordingStarted(dispatch int64, path string, at time.Time) (Recording, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Recording{}, err
	}
	defer tx.Rollback()
	row := tx.QueryRow(`SELECT `+recordingColumns+` FROM recordings r JOIN dispatches d ON d.id = r.dispatch JOIN messages m ON m.id = d.message WHERE r.dispatch = ?`, dispatch)
	if r, err := scanRecording(row); err == nil {
		return r, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return Recording{}, err
	}
	if _, err := tx.Exec(`INSERT INTO recordings (dispatch, path, started_at) VALUES (?, ?, ?)`, dispatch, path, timestamp(at)); err != nil {
		return Recording{}, fmt.Errorf("dispatch %d cannot be recorded: %w", dispatch, err)
	}
	r, err := scanRecording(tx.QueryRow(`SELECT `+recordingColumns+` FROM recordings r JOIN dispatches d ON d.id = r.dispatch JOIN messages m ON m.id = d.message WHERE r.dispatch = ?`, dispatch))
	if err != nil {
		return Recording{}, err
	}
	if _, err := appendEntry(tx, at, KindRecordingStarted, map[string]any{"dispatch": r.Dispatch, "agent": r.Agent, "job": r.Job, "path": path}); err != nil {
		return Recording{}, err
	}
	return r, tx.Commit()
}

// RecordingFinished records that size bytes are in dispatch's recording, now
// that its terminal is no longer recorded for it, and journals it.
func (s *Store) RecordingFinished(dispatch, size int64, at time.Time) error {
	return s.change(at, func(tx *sql.Tx) (string, any, error) {
		result, err := tx.Exec(`UPDATE recordings SET size = ? WHERE dispatch = ?`, size, dispatch)
		if err != nil {
			return "", nil, err
		}
		if n, err := result.RowsAffected(); err != nil || n != 1 {
			return "", nil, fmt.Errorf("dispatch %d has no recording to finish (%v)", dispatch, err)
		}
		return KindRecordingFinished, map[string]any{"dispatch": dispatch, "size": size}, nil
	})
}

// ExpiredRecordings returns the recordings whose files are not yet deleted
// and whose job ended at or before cutoff: delivered, ended on its leader's
// acceptance, replaced or cancelled.
func (s *Store) ExpiredRecordings(cutoff time.Time) ([]Recording, error) {
	rows, err := s.db.Query(`SELECT `+recordingColumns+`, (
			SELECT MAX(at) FROM journal WHERE kind IN (?, ?) AND json_extract(data, '$.job') = m.job
		) FROM recordings r JOIN dispatches d ON d.id = r.dispatch JOIN messages m ON m.id = d.message
		JOIN jobs j ON j.number = m.job
		WHERE r.deleted_at IS NULL AND j.state IN (?, ?) ORDER BY r.dispatch`, KindJobEnded, KindJobCancelled, JobEnded, JobCancelled)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var expired []Recording
	for rows.Next() {
		var r Recording
		var started, deleted string
		var ended sql.NullString
		if err := rows.Scan(&r.Dispatch, &r.Agent, &r.Job, &r.Path, &r.Size, &started, &deleted, &ended); err != nil {
			return nil, err
		}
		if !ended.Valid {
			continue
		}
		end, err := time.Parse(time.RFC3339Nano, ended.String)
		if err != nil {
			return nil, err
		}
		if end.After(cutoff) {
			continue
		}
		if r.Started, err = time.Parse(time.RFC3339Nano, started); err != nil {
			return nil, err
		}
		expired = append(expired, r)
	}
	return expired, rows.Err()
}

// RecordingDeleted records that dispatch's recording file was deleted, and
// journals it. The recording stays known, with where it was.
func (s *Store) RecordingDeleted(dispatch int64, at time.Time) error {
	return s.change(at, func(tx *sql.Tx) (string, any, error) {
		var path string
		err := tx.QueryRow(`SELECT path FROM recordings WHERE dispatch = ? AND deleted_at IS NULL`, dispatch).Scan(&path)
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil, nil
		}
		if err != nil {
			return "", nil, err
		}
		if _, err := tx.Exec(`UPDATE recordings SET deleted_at = ? WHERE dispatch = ?`, timestamp(at), dispatch); err != nil {
			return "", nil, err
		}
		return KindRecordingDeleted, map[string]any{"dispatch": dispatch, "path": path}, nil
	})
}
