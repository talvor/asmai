// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// NativeSession is the provider's own record of one agent session: the
// identifier its hooks reported and the transcript it keeps, which a later
// session of the same assignment resumes.
type NativeSession struct {
	Agent      string
	Generation int
	// Provider is the provider the session ran on.
	Provider string
	// Assignment is the assignment a worker's session carried, and zero for a
	// leader's.
	Assignment int64
	ID         string
	Transcript string
}

// SessionIdentified records the provider's identifier of generation of
// agent's session, and where the provider keeps its transcript, as its hooks
// reported them, and journals it. Nothing is recorded, or journaled, when it
// is what the store already holds, so every hook of a session may report it.
func (s *Store) SessionIdentified(agent string, generation int, id, transcript string, at time.Time) error {
	if id == "" {
		return nil
	}
	return s.change(at, func(tx *sql.Tx) (string, any, error) {
		var current, kept, provider string
		var assignment sql.NullInt64
		err := tx.QueryRow(`SELECT session_id, transcript, provider, assignment FROM native_sessions WHERE agent = ? AND generation = ?`, agent, generation).
			Scan(&current, &kept, &provider, &assignment)
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil, nil
		}
		if err != nil {
			return "", nil, err
		}
		if transcript == "" {
			transcript = kept
		}
		if current == id && kept == transcript {
			return "", nil, nil
		}
		if _, err := tx.Exec(`UPDATE native_sessions SET session_id = ?, transcript = ? WHERE agent = ? AND generation = ?`, id, transcript, agent, generation); err != nil {
			return "", nil, err
		}
		return KindSessionIdentified, map[string]any{
			"agent": agent, "generation": generation, "provider": provider, "assignment": assignment.Int64,
			"native_session": id, "transcript": transcript,
		}, nil
	})
}

// ResumableSession returns the latest session of the worker that carried
// assignment whose provider identified it, or sql.ErrNoRows.
func (s *Store) ResumableSession(assignment int64) (NativeSession, error) {
	n := NativeSession{Assignment: assignment}
	err := s.db.QueryRow(`SELECT agent, generation, provider, session_id, transcript FROM native_sessions WHERE assignment = ? AND session_id <> '' ORDER BY generation DESC LIMIT 1`, assignment).
		Scan(&n.Agent, &n.Generation, &n.Provider, &n.ID, &n.Transcript)
	return n, err
}

// WorkerDispatch is the latest dispatch of an assignment's worker: the one
// that gave it the assignment, returned it with a rejection, or resumed it,
// and what became of it.
type WorkerDispatch struct {
	Assignment Assignment
	Dispatch   Dispatch
	// Kind is the kind of message the dispatch carries.
	Kind string
	// Fetched says the worker fetched the message, which is the evidence it
	// was delivered.
	Fetched bool
	// Report is the kind of the report the dispatch ended in, or empty when
	// it ended in none.
	Report string
}

// WorkerDispatch returns assignment's worker's latest work or delivered
// decision-answer dispatch, or sql.ErrNoRows.
func (s *Store) WorkerDispatch(assignment int64) (WorkerDispatch, error) {
	return workerDispatch(s.db, assignment)
}

func workerDispatch(q querier, id int64) (WorkerDispatch, error) {
	var w WorkerDispatch
	var resumes sql.NullInt64
	err := q.QueryRow(`SELECT d.id, d.message, d.agent, d.generation, d.state, d.transcript, d.resumes, d.native_session, m.kind, m.fetched_at IS NOT NULL,
			COALESCE((SELECT r.kind FROM reports r WHERE r.dispatch = d.id ORDER BY r.id LIMIT 1), '')
		FROM messages m JOIN dispatches d ON d.message = m.id JOIN assignments a ON a.id = m.assignment
		WHERE m.assignment = ? AND m.recipient = a.worker AND (m.kind != ? OR m.fetched_at IS NOT NULL) ORDER BY m.id DESC LIMIT 1`, id, MessageDecisionAnswer).
		Scan(&w.Dispatch.ID, &w.Dispatch.Message, &w.Dispatch.Agent, &w.Dispatch.Generation, &w.Dispatch.State, &w.Dispatch.Transcript,
			&resumes, &w.Dispatch.NativeSession, &w.Kind, &w.Fetched, &w.Report)
	if err != nil {
		return WorkerDispatch{}, err
	}
	w.Dispatch.Resumes = resumes.Int64
	w.Assignment, err = assignment(q, id)
	return w, err
}

// WorkersInState returns, for each assignment in one of states, its worker's
// latest dispatch, by assignment. A restart looks at the assignments whose
// workers have work in hand: the active and the cancelling.
func (s *Store) WorkersInState(states ...string) ([]WorkerDispatch, error) {
	if len(states) == 0 {
		return nil, nil
	}
	marks := strings.TrimSuffix(strings.Repeat("?, ", len(states)), ", ")
	args := make([]any, len(states))
	for i, state := range states {
		args[i] = state
	}
	rows, err := s.db.Query(`SELECT id FROM assignments WHERE state IN (`+marks+`) ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	var workers []WorkerDispatch
	for _, id := range ids {
		w, err := s.WorkerDispatch(id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		workers = append(workers, w)
	}
	return workers, nil
}

// AssignmentResumed continues the active or cancelling assignment id after a
// restart: its worker's latest dispatch, previous, stopped at a boundary, and
// the worker is run again in the provider's session nativeSession with a new
// dispatch that resumes it. The dispatch is a message to the worker with body, and
// the resumption is journaled with it. The dispatch is returned.
func (s *Store) AssignmentResumed(id, previous int64, nativeSession, body string, at time.Time) (Dispatch, error) {
	if nativeSession == "" {
		return Dispatch{}, errors.New("a resumption names the provider's session it resumes")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Dispatch{}, err
	}
	defer tx.Rollback()
	w, err := workerDispatch(tx, id)
	if err != nil {
		return Dispatch{}, fmt.Errorf("assignment %d: %w", id, err)
	}
	a := w.Assignment
	switch {
	case a.State != AssignmentActive && a.State != AssignmentCancelling:
		return Dispatch{}, fmt.Errorf("assignment %d is %s, so it is not resumed", a.ID, a.State)
	case w.Dispatch.ID != previous || w.Dispatch.State != DispatchStopped:
		return Dispatch{}, fmt.Errorf("dispatch %d is not the stopped dispatch of %s's assignment %d", previous, a.Worker, a.ID)
	}
	d, err := createResumingMessage(tx, messageRef{assignment: a.ID}, a.Job, Daemon, a.Worker, MessageResumption, body, at, previous, nativeSession)
	if err != nil {
		return Dispatch{}, err
	}
	if _, err := appendEntry(tx, at, KindAssignmentResumed, map[string]any{
		"assignment": a.ID, "job": a.Job, "worker": a.Worker, "owner": a.Owner,
		"resumes": previous, "dispatch": d.ID, "native_session": nativeSession,
	}); err != nil {
		return Dispatch{}, err
	}
	return d, tx.Commit()
}

// CancellationRestarted continues the cancelling assignment id of a cancelled
// job after a restart that cannot resume its worker's session, in a new
// cancellation dispatch with body, which starts the worker in a new session.
// previous is the worker's dispatch the stop left; if it was still on its way
// or at work, it becomes unknown. A job cancel ends every assignment as
// cancelled, so a cancelled job's assignment whose workspace is still there
// is started again with this rather than held for reconciliation; one whose
// workspace is gone ends with CancelledWithoutWorkspace instead.
func (s *Store) CancellationRestarted(id, previous int64, body string, at time.Time) (Dispatch, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Dispatch{}, err
	}
	defer tx.Rollback()
	w, err := workerDispatch(tx, id)
	if err != nil {
		return Dispatch{}, fmt.Errorf("assignment %d: %w", id, err)
	}
	a := w.Assignment
	j, err := job(tx, a.Job)
	if err != nil {
		return Dispatch{}, fmt.Errorf("job %d: %w", a.Job, err)
	}
	switch {
	case a.State != AssignmentCancelling || j.State != JobCancelled:
		return Dispatch{}, fmt.Errorf("assignment %d is %s in job %d, which is %s: only a cancelling assignment of a cancelled job has its cancellation started again", a.ID, a.State, j.Number, j.State)
	case w.Dispatch.ID != previous:
		return Dispatch{}, fmt.Errorf("dispatch %d is not the latest dispatch of %s's assignment %d", previous, a.Worker, a.ID)
	}
	switch w.Dispatch.State {
	case DispatchCreated, DispatchNudged, DispatchDelivered, DispatchWorking:
		if _, err := tx.Exec(`UPDATE dispatches SET state = ?, updated_at = ? WHERE id = ?`, DispatchUnknown, timestamp(at), previous); err != nil {
			return Dispatch{}, err
		}
		if _, err := appendEntry(tx, at, KindDispatchChanged, map[string]any{
			"id": previous, "agent": w.Dispatch.Agent, "generation": w.Dispatch.Generation, "from": w.Dispatch.State, "state": DispatchUnknown, "assignment": a.ID,
		}); err != nil {
			return Dispatch{}, err
		}
	}
	d, err := createMessage(tx, messageRef{assignment: a.ID}, a.Job, Daemon, a.Worker, MessageCancellation, body, at)
	if err != nil {
		return Dispatch{}, err
	}
	if _, err := appendEntry(tx, at, KindCancellationRestarted, map[string]any{
		"assignment": a.ID, "job": a.Job, "worker": a.Worker, "owner": a.Owner, "previous": previous, "dispatch": d.ID,
	}); err != nil {
		return Dispatch{}, err
	}
	return d, tx.Commit()
}

// CancelledWithoutWorkspace ends the cancelling assignment id of a cancelled
// job, or one that needs reconciliation, as cancelled with no push, because
// why: its workspace is gone, so its worker has nothing to push from and is
// never started for it. A job cancel ends every assignment as cancelled, so
// this is how one with no workspace ends; its branch, if pushed earlier,
// stays on origin. Each of the worker's dispatches for it still on its way
// or at work becomes unknown, and no message about it waits for the worker.
func (s *Store) CancelledWithoutWorkspace(id int64, why string, at time.Time) (Assignment, error) {
	if why == "" {
		return Assignment{}, errors.New("an assignment cancelled without a push says why")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Assignment{}, err
	}
	defer tx.Rollback()
	a, err := assignment(tx, id)
	if err != nil {
		return Assignment{}, fmt.Errorf("assignment %d: %w", id, err)
	}
	j, err := job(tx, a.Job)
	if err != nil {
		return Assignment{}, fmt.Errorf("job %d: %w", a.Job, err)
	}
	if (a.State != AssignmentCancelling && a.State != AssignmentNeedsReconciliation) || j.State != JobCancelled {
		return Assignment{}, fmt.Errorf("assignment %d is %s in job %d, which is %s: only an assignment of a cancelled job that is cancelling or needs reconciliation ends cancelled without a push", a.ID, a.State, j.Number, j.State)
	}
	rows, err := tx.Query(`SELECT d.id, d.agent, d.generation, d.state FROM dispatches d JOIN messages m ON m.id = d.message
		WHERE m.assignment = ? AND m.recipient = ? AND d.state IN (?,?,?,?) ORDER BY d.id`,
		a.ID, a.Worker, DispatchCreated, DispatchNudged, DispatchDelivered, DispatchWorking)
	if err != nil {
		return Assignment{}, err
	}
	type open struct {
		id         int64
		agent      string
		generation int
		state      string
	}
	var waiting []open
	for rows.Next() {
		var o open
		if err := rows.Scan(&o.id, &o.agent, &o.generation, &o.state); err != nil {
			rows.Close()
			return Assignment{}, err
		}
		waiting = append(waiting, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Assignment{}, err
	}
	for _, o := range waiting {
		if _, err := tx.Exec(`UPDATE dispatches SET state = ?, updated_at = ? WHERE id = ?`, DispatchUnknown, timestamp(at), o.id); err != nil {
			return Assignment{}, err
		}
		if _, err := appendEntry(tx, at, KindDispatchChanged, map[string]any{
			"id": o.id, "agent": o.agent, "generation": o.generation, "from": o.state, "state": DispatchUnknown, "assignment": a.ID,
		}); err != nil {
			return Assignment{}, err
		}
	}
	if _, err := tx.Exec(`UPDATE assignments SET state = ? WHERE id = ?`, AssignmentCancelled, a.ID); err != nil {
		return Assignment{}, err
	}
	from := a.State
	a.State = AssignmentCancelled
	if _, err := appendEntry(tx, at, KindAssignmentCancelled, map[string]any{
		"assignment": a.ID, "job": a.Job, "worker": a.Worker, "from": from,
		"commit": "", "pushed": false, "workspace_gone": true, "workspace": a.Workspace.Path, "reason": why,
	}); err != nil {
		return Assignment{}, err
	}
	return a, tx.Commit()
}

// AssignmentNeedsReconciliation holds the active or cancelling assignment id
// for its owning leader to reconcile, because why: its worker's turn was cut off, or
// its native session cannot be resumed. dispatch is the worker's dispatch
// whose outcome is unknown; if it was still on its way or at work, it becomes
// unknown. The owning leader is told with a message with body, whose dispatch
// is returned. The worker's number and workspace stay held.
func (s *Store) AssignmentNeedsReconciliation(id, dispatch int64, why, body string, at time.Time) (Dispatch, error) {
	if why == "" {
		return Dispatch{}, errors.New("an assignment that needs reconciliation says why")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Dispatch{}, err
	}
	defer tx.Rollback()
	a, err := assignment(tx, id)
	if err != nil {
		return Dispatch{}, fmt.Errorf("assignment %d: %w", id, err)
	}
	if a.State != AssignmentActive && a.State != AssignmentCancelling {
		return Dispatch{}, fmt.Errorf("assignment %d is %s, so it cannot need reconciliation", a.ID, a.State)
	}
	if _, err := tx.Exec(`UPDATE assignments SET state = ? WHERE id = ?`, AssignmentNeedsReconciliation, a.ID); err != nil {
		return Dispatch{}, err
	}
	if dispatch != 0 {
		var agent, state string
		var generation int
		err := tx.QueryRow(`SELECT agent, state, generation FROM dispatches WHERE id = ?`, dispatch).Scan(&agent, &state, &generation)
		if err != nil {
			return Dispatch{}, fmt.Errorf("dispatch %d: %w", dispatch, err)
		}
		switch state {
		case DispatchCreated, DispatchNudged, DispatchDelivered, DispatchWorking:
			if _, err := tx.Exec(`UPDATE dispatches SET state = ?, updated_at = ? WHERE id = ?`, DispatchUnknown, timestamp(at), dispatch); err != nil {
				return Dispatch{}, err
			}
			if _, err := appendEntry(tx, at, KindDispatchChanged, map[string]any{
				"id": dispatch, "agent": agent, "generation": generation, "from": state, "state": DispatchUnknown, "assignment": a.ID,
			}); err != nil {
				return Dispatch{}, err
			}
		}
	}
	if _, err := appendEntry(tx, at, KindAssignmentNeedsReconciliation, map[string]any{
		"assignment": a.ID, "job": a.Job, "worker": a.Worker, "owner": a.Owner, "dispatch": dispatch, "reason": why,
	}); err != nil {
		return Dispatch{}, err
	}
	told, err := createMessage(tx, messageRef{assignment: a.ID}, a.Job, Daemon, a.Owner, MessageReconciliation, body, at)
	if err != nil {
		return Dispatch{}, err
	}
	return told, tx.Commit()
}

// LeaderWork is a job a leader has open work in.
type LeaderWork struct {
	Leader string
	Job    int64
}

// OpenLeaderWork returns each leader that has open work in an open job, with
// the job, by leader and job. A leader has open work in a job when a handoff
// to it is open, it owns an assignment that has not ended, a decision it
// requested or one it answers is pending, or a message waits in its inbox;
// Coordination has it in every open job, as the role responsible for the
// user's conversation and overall delivery progress.
func (s *Store) OpenLeaderWork() ([]LeaderWork, error) {
	rows, err := s.db.Query(`SELECT DISTINCT w.leader, w.job FROM (
			SELECT receiver AS leader, job FROM handoffs WHERE state = ?
			UNION SELECT owner, job FROM assignments WHERE state IN (?, ?, ?, ?)
			UNION SELECT m.recipient, m.job FROM messages m JOIN dispatches d ON d.message = m.id
				WHERE m.fetched_at IS NULL AND d.state IN (?, ?, ?) AND m.recipient LIKE 'leader@%'
			UNION SELECT requester, job FROM decision_requests WHERE state = ? AND requester LIKE 'leader@%'
			UNION SELECT recipient, job FROM decision_requests WHERE state = ? AND recipient LIKE 'leader@%'
			UNION SELECT 'leader@' || role, number FROM jobs
		) w JOIN jobs j ON j.number = w.job WHERE j.state = ? ORDER BY w.leader, w.job`,
		HandoffPending,
		AssignmentActive, AssignmentSubmitted, AssignmentCancelling, AssignmentNeedsReconciliation,
		DispatchCreated, DispatchNudged, DispatchUnknown,
		RequestPending, RequestPending,
		JobOpen)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var work []LeaderWork
	for rows.Next() {
		var w LeaderWork
		if err := rows.Scan(&w.Leader, &w.Job); err != nil {
			return nil, err
		}
		work = append(work, w)
	}
	return work, rows.Err()
}

// LeaderRestored tells leader, which a start restored, to load the brief of
// each job it has open work in, in one message with body that it fetches as it
// does any other. The message is about job, the first of them. A message of
// the kind still waiting for the leader is not made again, and then false is
// returned.
func (s *Store) LeaderRestored(leader string, job int64, body string, at time.Time) (Dispatch, bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Dispatch{}, false, err
	}
	defer tx.Rollback()
	var waiting int
	err = tx.QueryRow(`SELECT count(*) FROM messages m JOIN dispatches d ON d.message = m.id
		WHERE m.recipient = ? AND m.kind = ? AND m.fetched_at IS NULL AND d.state IN (?, ?, ?)`,
		leader, MessageRestoration, DispatchCreated, DispatchNudged, DispatchUnknown).Scan(&waiting)
	if err != nil {
		return Dispatch{}, false, err
	}
	if waiting > 0 {
		if _, err := tx.Exec(`UPDATE messages SET job = ?, body = ? WHERE recipient = ? AND kind = ? AND fetched_at IS NULL AND id IN (
			SELECT m.id FROM messages m JOIN dispatches d ON d.message = m.id
			WHERE m.recipient = ? AND m.kind = ? AND m.fetched_at IS NULL AND d.state IN (?, ?, ?)
		)`, job, body, leader, MessageRestoration, leader, MessageRestoration, DispatchCreated, DispatchNudged, DispatchUnknown); err != nil {
			return Dispatch{}, false, err
		}
		if err := tx.Commit(); err != nil {
			return Dispatch{}, false, err
		}
		return Dispatch{}, false, nil
	}
	d, err := createMessage(tx, messageRef{}, job, Daemon, leader, MessageRestoration, body, at)
	if err != nil {
		return Dispatch{}, false, err
	}
	return d, true, tx.Commit()
}
