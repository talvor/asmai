// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	HandoffPending    = "pending"
	HandoffAccepted   = "accepted"
	HandoffClarified  = "clarify"
	HandoffDeclined   = "declined"
	DispatchCreated   = "created"
	DispatchNudged    = "nudged"
	DispatchDelivered = "delivered"
	DispatchWorking   = "working"
	DispatchStopped   = "stopped"
	DispatchUnknown   = "unknown"
)

type Handoff struct {
	ID          int64    `json:"id"`
	Job         int64    `json:"job"`
	Sender      string   `json:"sender"`
	Receiver    string   `json:"receiver"`
	Outcome     string   `json:"outcome"`
	Decisions   string   `json:"decisions"`
	Evidence    string   `json:"evidence"`
	Constraints string   `json:"constraints"`
	Permissions string   `json:"permissions"`
	Criteria    []string `json:"acceptance_criteria"`
	State       string   `json:"state"`
	Answer      string   `json:"answer,omitempty"`
}

type Message struct {
	ID          int64    `json:"id"`
	Handoff     int64    `json:"handoff"`
	Job         int64    `json:"job"`
	Sender      string   `json:"sender"`
	Recipient   string   `json:"recipient"`
	Kind        string   `json:"kind"`
	Body        string   `json:"body"`
	Dispatch    int64    `json:"dispatch"`
	State       string   `json:"dispatch_state"`
	Transcript  string   `json:"transcript,omitempty"`
	Fetched     bool     `json:"fetched"`
	HandoffData *Handoff `json:"handoff_data,omitempty"`
}

type Dispatch struct {
	ID         int64  `json:"id"`
	Message    int64  `json:"message"`
	Agent      string `json:"agent"`
	Generation int    `json:"generation"`
	State      string `json:"state"`
	Transcript string `json:"transcript,omitempty"`
}

func (s *Store) SendHandoff(h Handoff, at time.Time) (Handoff, Dispatch, error) {
	if h.Job <= 0 || h.Sender == "" || h.Receiver == "" || strings.TrimSpace(h.Outcome) == "" || len(h.Criteria) == 0 {
		return Handoff{}, Dispatch{}, errors.New("handoff needs a job, sender, receiving leader, outcome and acceptance criteria")
	}
	for _, criterion := range h.Criteria {
		if strings.TrimSpace(criterion) == "" {
			return Handoff{}, Dispatch{}, errors.New("handoff acceptance criteria cannot be empty")
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Handoff{}, Dispatch{}, err
	}
	defer tx.Rollback()
	if _, err = job(tx, h.Job); err != nil {
		return Handoff{}, Dispatch{}, fmt.Errorf("job %d: %w", h.Job, err)
	}
	criteria, _ := json.Marshal(h.Criteria)
	h.State = HandoffPending
	result, err := tx.Exec(`INSERT INTO handoffs(job,sender,receiver,outcome,decisions,evidence,constraints,permissions,criteria,state,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, h.Job, h.Sender, h.Receiver, h.Outcome, h.Decisions, h.Evidence, h.Constraints, h.Permissions, string(criteria), h.State, timestamp(at))
	if err != nil {
		return Handoff{}, Dispatch{}, err
	}
	h.ID, err = result.LastInsertId()
	if err != nil {
		return Handoff{}, Dispatch{}, err
	}
	if _, err = appendEntry(tx, at, KindHandoffSent, h); err != nil {
		return Handoff{}, Dispatch{}, err
	}
	d, err := createMessage(tx, h.ID, h.Job, h.Sender, h.Receiver, "handoff", h.Outcome, at)
	if err != nil {
		return Handoff{}, Dispatch{}, err
	}
	return h, d, tx.Commit()
}

func createMessage(tx *sql.Tx, handoff, job int64, sender, recipient, kind, body string, at time.Time) (Dispatch, error) {
	result, err := tx.Exec(`INSERT INTO messages(handoff,job,sender,recipient,kind,body) VALUES(?,?,?,?,?,?)`, handoff, job, sender, recipient, kind, body)
	if err != nil {
		return Dispatch{}, err
	}
	message, err := result.LastInsertId()
	if err != nil {
		return Dispatch{}, err
	}
	result, err = tx.Exec(`INSERT INTO dispatches(message,agent,state,updated_at) VALUES(?,?,?,?)`, message, recipient, DispatchCreated, timestamp(at))
	if err != nil {
		return Dispatch{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return Dispatch{}, err
	}
	d := Dispatch{ID: id, Message: message, Agent: recipient, State: DispatchCreated}
	if _, err = appendEntry(tx, at, KindMessageCreated, map[string]any{"message": message, "handoff": handoff, "job": job, "sender": sender, "recipient": recipient, "kind": kind, "dispatch": id}); err != nil {
		return Dispatch{}, err
	}
	if _, err = appendEntry(tx, at, KindDispatchChanged, d); err != nil {
		return Dispatch{}, err
	}
	return d, nil
}

func (s *Store) AnswerHandoff(id int64, agent, state, answer string, at time.Time) (Handoff, Dispatch, error) {
	if state != HandoffAccepted && state != HandoffClarified && state != HandoffDeclined {
		return Handoff{}, Dispatch{}, errors.New("answer must accept, clarify or decline")
	}
	if (state == HandoffDeclined || state == HandoffClarified) && strings.TrimSpace(answer) == "" {
		return Handoff{}, Dispatch{}, errors.New("clarification and decline need a reason or question")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Handoff{}, Dispatch{}, err
	}
	defer tx.Rollback()
	h, err := handoff(tx, id)
	if err != nil {
		return Handoff{}, Dispatch{}, err
	}
	if h.Receiver != agent {
		return Handoff{}, Dispatch{}, errors.New("only the receiving leader can answer this handoff")
	}
	if h.State != HandoffPending {
		return Handoff{}, Dispatch{}, fmt.Errorf("handoff %d was already answered", id)
	}
	var inbound int64
	var fetched sql.NullString
	err = tx.QueryRow(`SELECT d.id,m.fetched_at FROM dispatches d JOIN messages m ON m.id=d.message WHERE m.handoff=? AND m.kind='handoff'`, id).Scan(&inbound, &fetched)
	if err != nil {
		return Handoff{}, Dispatch{}, err
	}
	if !fetched.Valid {
		return Handoff{}, Dispatch{}, errors.New("fetch the handoff with asmai inbox before answering")
	}
	if _, err = tx.Exec(`UPDATE handoffs SET state=?,answer=? WHERE id=?`, state, answer, id); err != nil {
		return Handoff{}, Dispatch{}, err
	}
	h.State, h.Answer = state, answer
	if _, err = appendEntry(tx, at, KindHandoffAnswered, h); err != nil {
		return Handoff{}, Dispatch{}, err
	}
	if _, err = tx.Exec(`UPDATE dispatches SET state=?,updated_at=? WHERE id=? AND state=?`, DispatchWorking, timestamp(at), inbound, DispatchDelivered); err != nil {
		return Handoff{}, Dispatch{}, err
	}
	if _, err = appendEntry(tx, at, KindDispatchChanged, map[string]any{"id": inbound, "state": DispatchWorking, "agent": agent}); err != nil {
		return Handoff{}, Dispatch{}, err
	}
	d, err := createMessage(tx, id, h.Job, agent, h.Sender, state, answer, at)
	if err != nil {
		return Handoff{}, Dispatch{}, err
	}
	return h, d, tx.Commit()
}

func handoff(q queryer, id int64) (Handoff, error) {
	var h Handoff
	var criteria string
	err := q.QueryRow(`SELECT id,job,sender,receiver,outcome,decisions,evidence,constraints,permissions,criteria,state,answer FROM handoffs WHERE id=?`, id).Scan(&h.ID, &h.Job, &h.Sender, &h.Receiver, &h.Outcome, &h.Decisions, &h.Evidence, &h.Constraints, &h.Permissions, &criteria, &h.State, &h.Answer)
	if err != nil {
		return Handoff{}, err
	}
	err = json.Unmarshal([]byte(criteria), &h.Criteria)
	return h, err
}
func (s *Store) Handoff(id int64) (Handoff, error) { return handoff(s.db, id) }

func (s *Store) Inbox(agent string, only int64, at time.Time) ([]Message, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT m.id,m.handoff,m.job,m.sender,m.recipient,m.kind,m.body,d.id,d.state,d.transcript,m.fetched_at FROM messages m JOIN dispatches d ON d.message=m.id WHERE m.recipient=? AND (?=0 OR d.id=?) ORDER BY m.id`, agent, only, only)
	if err != nil {
		return nil, err
	}
	var messages []Message
	for rows.Next() {
		var m Message
		var fetched sql.NullString
		if err = rows.Scan(&m.ID, &m.Handoff, &m.Job, &m.Sender, &m.Recipient, &m.Kind, &m.Body, &m.Dispatch, &m.State, &m.Transcript, &fetched); err != nil {
			break
		}
		m.Fetched = fetched.Valid
		messages = append(messages, m)
	}
	if e := rows.Err(); err == nil {
		err = e
	}
	rows.Close()
	if err != nil {
		return nil, err
	}
	if only > 0 && len(messages) == 0 {
		return nil, fmt.Errorf("dispatch %d is not in %s's inbox", only, agent)
	}
	for i := range messages {
		m := &messages[i]
		h, e := handoff(tx, m.Handoff)
		if e != nil {
			return nil, e
		}
		m.HandoffData = &h
		if m.Fetched {
			continue
		}
		if _, err = tx.Exec(`UPDATE messages SET fetched_at=? WHERE id=?`, timestamp(at), m.ID); err != nil {
			return nil, err
		}
		if _, err = appendEntry(tx, at, KindMessageFetched, map[string]any{"message": m.ID, "dispatch": m.Dispatch, "agent": agent, "job": m.Job}); err != nil {
			return nil, err
		}
		if _, err = tx.Exec(`UPDATE dispatches SET state=?,updated_at=? WHERE id=?`, DispatchDelivered, timestamp(at), m.Dispatch); err != nil {
			return nil, err
		}
		m.Fetched = true
		m.State = DispatchDelivered
		if _, err = appendEntry(tx, at, KindDispatchChanged, map[string]any{"id": m.Dispatch, "state": DispatchDelivered, "agent": agent}); err != nil {
			return nil, err
		}
	}
	return messages, tx.Commit()
}

func (s *Store) NextDispatch(agent string) (Dispatch, error) {
	var d Dispatch
	err := s.db.QueryRow(`SELECT d.id,d.message,d.agent,d.generation,d.state,d.transcript FROM dispatches d JOIN messages m ON m.id=d.message WHERE d.agent=? AND m.fetched_at IS NULL AND d.state=? ORDER BY d.id LIMIT 1`, agent, DispatchCreated).Scan(&d.ID, &d.Message, &d.Agent, &d.Generation, &d.State, &d.Transcript)
	return d, err
}

func (s *Store) PendingLeaders() ([]string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT m.recipient FROM messages m JOIN dispatches d ON d.message=m.id WHERE m.fetched_at IS NULL AND d.state IN (?,?) ORDER BY m.recipient`, DispatchCreated, DispatchUnknown)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var leaders []string
	for rows.Next() {
		var agent string
		if err = rows.Scan(&agent); err != nil {
			return nil, err
		}
		leaders = append(leaders, agent)
	}
	return leaders, rows.Err()
}

// ChangeDispatch commits one correlated transition. The daemon holds its
// session lock while using this method, so an old generation cannot win.
func (s *Store) ChangeDispatch(id int64, generation int, from, to, transcript string, at time.Time) error {
	return s.change(at, func(tx *sql.Tx) (string, any, error) {
		result, err := tx.Exec(`UPDATE dispatches SET state=?,generation=?,transcript=CASE WHEN ?='' THEN transcript ELSE ? END,updated_at=? WHERE id=? AND state=? AND (generation=0 OR generation=?)`, to, generation, transcript, transcript, timestamp(at), id, from, generation)
		if err != nil {
			return "", nil, err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return "", nil, err
		}
		if n != 1 {
			return "", nil, fmt.Errorf("dispatch %d is not %s in generation %d", id, from, generation)
		}
		return KindDispatchChanged, map[string]any{"id": id, "generation": generation, "from": from, "state": to, "transcript": transcript}, nil
	})
}

// ObservedAutomated journals the provider's exact prompt-submission payload
// and the dispatch transition together. The transition cites that evidence.
func (s *Store) ObservedAutomated(d Dispatch, role string, payload json.RawMessage, transcript string, at time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	observation, err := appendEntry(tx, at, KindObservation, map[string]any{"agent": d.Agent, "role": role, "generation": d.Generation, "event": "UserPromptSubmit", "payload": payload})
	if err != nil {
		return err
	}
	result, err := tx.Exec(`UPDATE dispatches SET state=?,transcript=CASE WHEN ?='' THEN transcript ELSE ? END,updated_at=? WHERE id=? AND agent=? AND generation=? AND state=?`, DispatchNudged, transcript, transcript, timestamp(at), d.ID, d.Agent, d.Generation, DispatchUnknown)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("dispatch %d is not an unacknowledged nudge in generation %d", d.ID, d.Generation)
	}
	_, err = appendEntry(tx, at, KindDispatchChanged, map[string]any{"id": d.ID, "agent": d.Agent, "generation": d.Generation, "from": DispatchUnknown, "state": DispatchNudged, "transcript": transcript, "observation": observation})
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) StopDispatchIfWorking(id int64, generation int, transcript string, at time.Time) error {
	var state string
	err := s.db.QueryRow(`SELECT state FROM dispatches WHERE id=? AND generation=?`, id, generation).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if state != DispatchWorking {
		return nil
	}
	return s.ChangeDispatch(id, generation, DispatchWorking, DispatchStopped, transcript, at)
}
