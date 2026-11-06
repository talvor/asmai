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

// InterventionMessage is a message the user typed and submitted in an agent's terminal
// while they held its input, word for word.
type InterventionMessage struct {
	ID       int64  `json:"id"`
	Terminal string `json:"terminal"`
	Text     string `json:"text"`
}

// Intervention is the record of the user holding an agent's input, from their
// first take to the release (03 rule 38): the messages they typed word for
// word, their answers to native prompts, the automated drafts cleared from
// the input box, the effects the agent recorded meanwhile, and the note they
// gave at release.
type Intervention struct {
	Agent      string                `json:"agent"`
	Generation int                   `json:"generation"`
	Messages   []InterventionMessage `json:"messages"`
	Answers    []PromptAnswer        `json:"answers"`
	Drafts     []string              `json:"drafts"`
	Effects    []Effect              `json:"effects"`
	Note       string                `json:"note,omitempty"`
}

// Typed reports whether the user typed any message, which supersedes the
// dispatch the agent worked on (03 rule 37).
func (i Intervention) Typed() bool { return len(i.Messages) > 0 }

// Release is the user releasing generation of agent's input, from Terminal,
// with an optional Note. Unsent is the text the user left unsent in the input
// box, which the release cleared. Fallback is who the record goes to when the
// agent has no assignment of its own to be reconciled.
type Release struct {
	Agent      string
	Role       string
	Generation int
	Terminal   string
	Note       string
	Unsent     string
	Fallback   string
}

// Released is what releasing an agent's input did: the intervention record,
// the dispatch the user's message superseded and the assignment that needs
// reconciliation for it, if any, and the owning leader's message with the
// record, whose dispatch is Told when there is one.
type Released struct {
	Intervention Intervention
	Superseded   int64
	Assignment   int64
	Owner        string
	Told         Dispatch
}

// interventionOf reads the record of the hold the user has on generation of
// agent's input: everything journaled since the first take after the last
// release.
func interventionOf(tx *sql.Tx, agent string, generation int) (Intervention, error) {
	i := Intervention{Agent: agent, Generation: generation, Messages: []InterventionMessage{}, Answers: []PromptAnswer{}, Drafts: []string{}, Effects: []Effect{}}
	var start sql.NullInt64
	err := tx.QueryRow(`SELECT MIN(id) FROM journal WHERE kind = ? AND json_extract(data, '$.agent') = ? AND json_extract(data, '$.generation') = ?
		AND id > COALESCE((SELECT MAX(id) FROM journal WHERE kind = ? AND json_extract(data, '$.agent') = ?), 0)`,
		KindInputTaken, agent, generation, KindInputReleased, agent).Scan(&start)
	if err != nil {
		return Intervention{}, err
	}
	if !start.Valid {
		return Intervention{}, errors.New("the user does not hold this agent's input")
	}
	rows, err := tx.Query(`SELECT id, kind, data FROM journal WHERE id > ? AND kind IN (?, ?, ?, ?) AND json_extract(data, '$.agent') = ? ORDER BY id`,
		start.Int64, KindMessageWitnessed, KindPromptAnswered, KindDraftCleared, KindEffectRecorded, agent)
	if err != nil {
		return Intervention{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var kind, data string
		if err := rows.Scan(&id, &kind, &data); err != nil {
			return Intervention{}, err
		}
		switch kind {
		case KindMessageWitnessed:
			var w Witnessed
			if err := json.Unmarshal([]byte(data), &w); err != nil {
				return Intervention{}, err
			}
			if w.Generation == generation {
				i.Messages = append(i.Messages, InterventionMessage{ID: id, Terminal: w.Terminal, Text: w.Text})
			}
		case KindPromptAnswered:
			var a PromptAnswer
			if err := json.Unmarshal([]byte(data), &a); err != nil {
				return Intervention{}, err
			}
			if a.Generation == generation {
				i.Answers = append(i.Answers, a)
			}
		case KindDraftCleared:
			var c DraftCleared
			if err := json.Unmarshal([]byte(data), &c); err != nil {
				return Intervention{}, err
			}
			i.Drafts = append(i.Drafts, c.Text)
		case KindEffectRecorded:
			var e Effect
			if err := json.Unmarshal([]byte(data), &e); err != nil {
				return Intervention{}, err
			}
			if e.Generation == generation {
				i.Effects = append(i.Effects, e)
			}
		}
	}
	return i, rows.Err()
}

// InputReleased journals that the user released generation of r.Agent's
// input, with the intervention record. If they only answered native prompts,
// nothing else changes: the same dispatch continues. If they typed any
// message, the agent's dispatch is superseded: when the agent is a worker
// with an active assignment, the dispatch is closed and the assignment needs
// reconciliation, and the owning leader is sent the record in a message; it
// reconciles and sends the work again in a new dispatch, which counts toward
// the assignment's dispatch limit (03 rules 36 to 38).
func (s *Store) InputReleased(r Release, at time.Time) (Released, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Released{}, err
	}
	defer tx.Rollback()
	out := Released{}
	out.Intervention, err = interventionOf(tx, r.Agent, r.Generation)
	if err != nil {
		return Released{}, err
	}
	out.Intervention.Note = r.Note
	entry := map[string]any{
		"agent": r.Agent, "role": r.Role, "generation": r.Generation, "terminal": r.Terminal, "note": r.Note,
		"messages": len(out.Intervention.Messages), "answers": len(out.Intervention.Answers), "drafts": len(out.Intervention.Drafts), "effects": len(out.Intervention.Effects),
	}
	if r.Unsent != "" {
		entry["unsent"] = r.Unsent
	}
	if !out.Intervention.Typed() {
		if _, err := appendEntry(tx, at, KindInputReleased, entry); err != nil {
			return Released{}, err
		}
		return out, tx.Commit()
	}

	var assignment, job int64
	var state, owner string
	err = tx.QueryRow(`SELECT id, job, state, owner FROM assignments WHERE worker = ? AND NOT `+endedAssignment+` ORDER BY id DESC LIMIT 1`, r.Agent).Scan(&assignment, &job, &state, &owner)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		assignment, owner = 0, r.Fallback
		err = tx.QueryRow(`SELECT job FROM messages WHERE recipient = ? ORDER BY id DESC LIMIT 1`, r.Agent).Scan(&job)
		if errors.Is(err, sql.ErrNoRows) {
			job, err = 0, nil
		}
	}
	if err != nil {
		return Released{}, err
	}
	reconcile := assignment != 0 && (state == AssignmentActive || state == AssignmentCancelling)
	if reconcile {
		w, err := workerDispatch(tx, assignment)
		if err != nil {
			return Released{}, fmt.Errorf("assignment %d: %w", assignment, err)
		}
		out.Superseded = w.Dispatch.ID
		if w.Dispatch.State != DispatchStopped || w.Report == "" {
			if _, err := tx.Exec(`UPDATE dispatches SET state = ?, closed = ?, updated_at = ? WHERE id = ?`, DispatchStopped, ClosedSuperseded, timestamp(at), w.Dispatch.ID); err != nil {
				return Released{}, err
			}
			if _, err := appendEntry(tx, at, KindDispatchChanged, map[string]any{
				"id": w.Dispatch.ID, "agent": r.Agent, "generation": w.Dispatch.Generation, "from": w.Dispatch.State, "state": DispatchStopped, "closed": ClosedSuperseded, "assignment": assignment,
			}); err != nil {
				return Released{}, err
			}
		}
		if _, err := tx.Exec(`UPDATE assignments SET state = ? WHERE id = ?`, AssignmentNeedsReconciliation, assignment); err != nil {
			return Released{}, err
		}
		if _, err := appendEntry(tx, at, KindAssignmentNeedsReconciliation, map[string]any{
			"assignment": assignment, "job": job, "worker": r.Agent, "owner": owner, "dispatch": w.Dispatch.ID,
			"reason": "the user typed to its worker while they held its input",
		}); err != nil {
			return Released{}, err
		}
		entry["superseded"], entry["assignment"] = w.Dispatch.ID, assignment
	}
	if _, err := appendEntry(tx, at, KindInputReleased, entry); err != nil {
		return Released{}, err
	}
	out.Assignment, out.Owner = assignment, owner
	if owner != "" && job != 0 {
		out.Told, err = createMessage(tx, messageRef{assignment: assignment}, job, Daemon, owner, MessageIntervention, out.Intervention.Text(out.Superseded, assignment), at)
		if err != nil {
			return Released{}, err
		}
	}
	return out, tx.Commit()
}

// Text is the record as the owning leader reads it. superseded is the
// dispatch the user's messages took the place of, and assignment the
// assignment that needs reconciliation for it; either is zero for none.
func (i Intervention) Text(superseded, assignment int64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The user took the input of %s, typed to it and released it. This is the intervention record.\n", i.Agent)
	if assignment != 0 {
		fmt.Fprintf(&b, "Its dispatch %d is superseded by the user's messages, and assignment %d needs reconciliation. Look at `asmai brief` and the effects below, decide what the user's words change, then send the assignment to its worker again with `asmai redispatch --assignment %d --reason <why>`: a new dispatch, which counts toward the assignment's dispatch limit. The worker is not restarted until you do. The user's words do not change the assignment's scope or priority; you keep control of both.\n", superseded, assignment, assignment)
	} else {
		b.WriteString("The user's words do not change any assignment's scope or priority; you keep control of both.\n")
	}
	b.WriteString("\nThe user's messages, word for word:\n")
	for _, m := range i.Messages {
		fmt.Fprintf(&b, "- (message %d, from %s)\n%s\n", m.ID, m.Terminal, indent(m.Text))
	}
	b.WriteString("\nThe user's answers to native prompts:\n")
	if len(i.Answers) == 0 {
		b.WriteString("- none\n")
	}
	for _, a := range i.Answers {
		fmt.Fprintf(&b, "- prompt: %s\n  keys typed: %q\n", strings.ReplaceAll(strings.TrimSpace(a.Prompt), "\n", "\n    "), a.Keys)
	}
	b.WriteString("\nAutomated drafts cleared from the input box:\n")
	if len(i.Drafts) == 0 {
		b.WriteString("- none\n")
	}
	for _, d := range i.Drafts {
		fmt.Fprintf(&b, "-\n%s\n", indent(d))
	}
	b.WriteString("\nEffects recorded meanwhile:\n")
	if len(i.Effects) == 0 {
		b.WriteString("- none\n")
	}
	for _, e := range i.Effects {
		fmt.Fprintf(&b, "- %s %s (effect %d, dispatch %d)\n", e.Kind, e.Ref, e.ID, e.Dispatch)
	}
	if i.Note != "" {
		fmt.Fprintf(&b, "\nThe user's note at release:\n%s\n", indent(i.Note))
	}
	return b.String()
}

func indent(text string) string {
	return "    " + strings.ReplaceAll(text, "\n", "\n    ")
}
