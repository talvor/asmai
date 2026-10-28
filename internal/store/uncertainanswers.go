// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// AnyGeneration is the generation to give UncertainAnswer for an answer typed
// in any session of the worker, as after a restart, when every session ended.
const AnyGeneration = math.MaxInt

// UncertainAnswer returns the earliest decision answer to assignment's worker
// whose nudge was typed in generation of its session, or in any earlier one,
// and never acknowledged: its dispatch is unknown and its message unfetched,
// so nobody can tell whether the worker saw it. NextDispatch never offers it
// again, so a worker whose session exits in that window, or a factory that
// stops and starts in it, leaves it for its owning leader to recover, and a
// later exit or restart still finds it. It returns sql.ErrNoRows when there
// is none.
func (s *Store) UncertainAnswer(assignment int64, generation int) (Dispatch, error) {
	return uncertainAnswer(s.db, assignment, generation)
}

func uncertainAnswer(q querier, assignment int64, generation int) (Dispatch, error) {
	var d Dispatch
	err := q.QueryRow(`SELECT d.id, d.message, d.agent, d.generation, d.state, d.transcript
		FROM messages m JOIN dispatches d ON d.message = m.id JOIN assignments a ON a.id = m.assignment
		WHERE m.assignment = ? AND m.recipient = a.worker AND m.kind = ? AND m.fetched_at IS NULL
		AND d.state = ? AND d.generation <= ? ORDER BY d.id LIMIT 1`,
		assignment, MessageDecisionAnswer, DispatchUnknown, generation).
		Scan(&d.ID, &d.Message, &d.Agent, &d.Generation, &d.State, &d.Transcript)
	return d, err
}

// uncertainHold is an uncertain decision answer of an assignment held for
// reconciliation, which only its owning leader recovers.
type uncertainHold struct {
	assignment Assignment
	dispatch   Dispatch
	message    Message
}

// heldUncertainAnswer reads dispatch as the uncertain decision answer of an
// assignment that needs reconciliation and that leader owns, for leader to
// recover with reasons.
func heldUncertainAnswer(tx *sql.Tx, dispatch int64, leader string, reasons []string) (uncertainHold, error) {
	var h uncertainHold
	var assignment, request sql.NullInt64
	var fetched bool
	err := tx.QueryRow(`SELECT d.id, d.message, d.agent, d.generation, d.state, m.kind, m.assignment, m.request, m.job, m.sender, m.recipient, m.body, m.fetched_at IS NOT NULL
		FROM dispatches d JOIN messages m ON m.id = d.message WHERE d.id = ?`, dispatch).
		Scan(&h.dispatch.ID, &h.dispatch.Message, &h.dispatch.Agent, &h.dispatch.Generation, &h.dispatch.State,
			&h.message.Kind, &assignment, &request, &h.message.Job, &h.message.Sender, &h.message.Recipient, &h.message.Body, &fetched)
	if errors.Is(err, sql.ErrNoRows) {
		return h, fmt.Errorf("dispatch %d does not exist", dispatch)
	}
	if err != nil {
		return h, err
	}
	h.message.ID, h.message.Assignment, h.message.Request = h.dispatch.Message, assignment.Int64, request.Int64
	if h.message.Kind != MessageDecisionAnswer || !assignment.Valid {
		return h, fmt.Errorf("dispatch %d carries a %s message, not a decision answer to an assignment's worker", dispatch, h.message.Kind)
	}
	if h.assignment, err = decided(tx, assignment.Int64, leader, reasons); err != nil {
		return h, err
	}
	if h.message.Recipient != h.assignment.Worker || fetched || h.dispatch.State != DispatchUnknown || h.assignment.State != AssignmentNeedsReconciliation {
		return h, fmt.Errorf("dispatch %d is not an uncertain decision answer of an assignment that needs reconciliation: it is %s, and assignment %d is %s", dispatch, h.dispatch.State, h.assignment.ID, h.assignment.State)
	}
	return h, nil
}

// stopHeldAnswer stops the uncertain dispatch of h, which a redelivery or a
// retirement ends: it is never offered to the worker again.
func stopHeldAnswer(tx *sql.Tx, h uncertainHold, at time.Time) error {
	if _, err := tx.Exec(`UPDATE dispatches SET state = ?, updated_at = ? WHERE id = ?`, DispatchStopped, timestamp(at), h.dispatch.ID); err != nil {
		return err
	}
	_, err := appendEntry(tx, at, KindDispatchChanged, map[string]any{
		"id": h.dispatch.ID, "agent": h.dispatch.Agent, "generation": h.dispatch.Generation, "from": DispatchUnknown, "state": DispatchStopped, "assignment": h.assignment.ID,
	})
	return err
}

// AnswerRedelivered delivers the uncertain decision answer of dispatch, whose
// assignment needs reconciliation and which leader owns, to its worker again,
// with leader's reasons, in a new dispatch of the same answer to the same
// request, which is returned with the assignment, active again. The
// uncertain dispatch is stopped, never reused.
func (s *Store) AnswerRedelivered(dispatch int64, leader string, reasons []string, at time.Time) (Assignment, Dispatch, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Assignment{}, Dispatch{}, err
	}
	defer tx.Rollback()
	h, err := heldUncertainAnswer(tx, dispatch, leader, reasons)
	if err != nil {
		return Assignment{}, Dispatch{}, err
	}
	a := h.assignment
	if err := stopHeldAnswer(tx, h, at); err != nil {
		return Assignment{}, Dispatch{}, err
	}
	if _, err := tx.Exec(`UPDATE assignments SET state = ? WHERE id = ?`, AssignmentActive, a.ID); err != nil {
		return Assignment{}, Dispatch{}, err
	}
	a.State = AssignmentActive
	body := fmt.Sprintf("%[1]s delivers this decision answer to you again, in a new dispatch: your session exited before acknowledging it in dispatch %[2]d, which is not reused.\nReasons from %[1]s:\n%[3]s\n\n"+
		"Load `asmai brief %[4]d`, look at your workspace and at the effects you have recorded, and continue assignment %[5]d from where you stopped, with the answer below. Never repeat an effect you already made.\n\n%[6]s",
		leader, h.dispatch.ID, strings.Join(reasons, "\n"), a.Job, a.ID, h.message.Body)
	again, err := createMessage(tx, messageRef{assignment: a.ID, request: h.message.Request}, h.message.Job, h.message.Sender, a.Worker, MessageDecisionAnswer, body, at)
	if err != nil {
		return Assignment{}, Dispatch{}, err
	}
	if _, err := appendEntry(tx, at, KindDecisionAnswerRedelivered, map[string]any{
		"assignment": a.ID, "job": a.Job, "worker": a.Worker, "owner": a.Owner, "request": h.message.Request,
		"previous": h.dispatch.ID, "dispatch": again.ID, "reasons": reasons,
	}); err != nil {
		return Assignment{}, Dispatch{}, err
	}
	return a, again, tx.Commit()
}

// AnswerRetired retires the uncertain decision answer of dispatch, whose
// assignment needs reconciliation and which leader owns, with leader's
// reasons: the dispatch is stopped and never delivered, and the retirement is
// journaled with the answer it retires. The assignment stays held, for its
// owner to send it to its worker again, and is returned.
func (s *Store) AnswerRetired(dispatch int64, leader string, reasons []string, at time.Time) (Assignment, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Assignment{}, err
	}
	defer tx.Rollback()
	h, err := heldUncertainAnswer(tx, dispatch, leader, reasons)
	if err != nil {
		return Assignment{}, err
	}
	a := h.assignment
	if err := stopHeldAnswer(tx, h, at); err != nil {
		return Assignment{}, err
	}
	if _, err := appendEntry(tx, at, KindDecisionAnswerRetired, map[string]any{
		"assignment": a.ID, "job": a.Job, "worker": a.Worker, "owner": a.Owner, "request": h.message.Request,
		"dispatch": h.dispatch.ID, "message": h.message.ID, "body": h.message.Body, "reasons": reasons,
	}); err != nil {
		return Assignment{}, err
	}
	return a, tx.Commit()
}
