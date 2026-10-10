// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/talvor/asmai/internal/store"
)

// restartEvidence is what a run's journal says about a clean stop and the
// start that followed it, folded in entry by entry. The journal is the record
// of what the real provider's sessions did, so the case reads it and never
// the sessions themselves.
type restartEvidence struct {
	phase restartPhase
	// stop is the entry that journals the stop the case made, which is the
	// last: an earlier case's stops and starts, in the same state directory,
	// are not it, though what they left, such as a handoff still open, is
	// the case's to restore.
	stop int64

	// Before the stop.
	state    map[int64]string // each dispatch's latest state
	agent    map[int64]string // the agent each dispatch is for
	kind     map[int64]string // the kind of message each dispatch carries
	running  map[string]int   // the agent sessions started and not ended, by generation
	handoffs map[int64]string // the open handoffs, by the leader they are to
	owned    map[int64]string // the assignments that have not ended, by owner
	jobs     int

	// The stop.
	stoppedBy   string
	endedByStop map[string]int
	// restore is the leaders whose roles had open work when the factory
	// stopped, and live the assignments that had not ended.
	restore []string
	live    int

	// After the start that followed it.
	previous string
	started  map[string]int // the agent sessions the start made, by generation
	made     map[int64]string
	reopened []int64
	acked    map[int64]bool
	fetched  map[int64]bool
	resumed  []int64
	held     []int64

	// Results, which are tied to the dispatch each came from throughout.
	results []resultEvidence
}

type restartPhase int

const (
	beforeStop restartPhase = iota
	afterStop
	afterStart
)

// resultEvidence is a result as the journal records it.
type resultEvidence struct {
	Assignment int64
	Dispatch   int64
	Agent      string
}

func newRestartEvidence(stop int64) *restartEvidence {
	return &restartEvidence{
		stop:  stop,
		state: map[int64]string{}, agent: map[int64]string{}, kind: map[int64]string{},
		running: map[string]int{}, handoffs: map[int64]string{}, owned: map[int64]string{},
		endedByStop: map[string]int{}, started: map[string]int{}, made: map[int64]string{},
		acked: map[int64]bool{}, fetched: map[int64]bool{},
	}
}

// record folds one journal entry into the evidence.
func (e *restartEvidence) record(entry store.Entry) {
	var d struct {
		Agent      string `json:"agent"`
		Generation int    `json:"generation"`
		Event      string `json:"event"`
		Payload    struct {
			Prompt string `json:"prompt"`
		} `json:"payload"`
		ID         int64  `json:"id"`
		State      string `json:"state"`
		Dispatch   int64  `json:"dispatch"`
		Recipient  string `json:"recipient"`
		Kind       string `json:"kind"`
		By         string `json:"by"`
		Previous   string `json:"previous"`
		Receiver   string `json:"receiver"`
		Handoff    int64  `json:"handoff"`
		Assignment int64  `json:"assignment"`
		Owner      string `json:"owner"`
		Role       string `json:"role"`
	}
	json.Unmarshal(entry.Data, &d)
	switch entry.Kind {
	case store.KindDaemonStopped:
		if e.phase == beforeStop && entry.ID == e.stop {
			e.phase = afterStop
			e.stoppedBy = d.By
			e.restore = e.mustRestore()
			e.live = len(e.owned)
		}
	case store.KindDaemonStarted:
		if e.phase == afterStop {
			e.phase = afterStart
			e.previous = d.Previous
		}
	case store.KindJobOpened:
		e.jobs++
	case store.KindHandoffSent:
		var h struct {
			ID       int64  `json:"id"`
			Receiver string `json:"receiver"`
		}
		json.Unmarshal(entry.Data, &h)
		e.handoffs[h.ID] = h.Receiver
	case store.KindHandoffAnswered:
		var h struct {
			ID int64 `json:"id"`
		}
		json.Unmarshal(entry.Data, &h)
		delete(e.handoffs, h.ID)
	case store.KindAssignmentCreated:
		var a struct {
			ID    int64  `json:"id"`
			Owner string `json:"owner"`
		}
		json.Unmarshal(entry.Data, &a)
		e.owned[a.ID] = a.Owner
	case store.KindResultAccepted, store.KindAssignmentCancelled:
		delete(e.owned, d.Assignment)
	case store.KindSessionStarted:
		if e.phase == afterStart {
			e.started[d.Agent] = d.Generation
		} else {
			e.running[d.Agent] = d.Generation
		}
	case store.KindSessionEnded:
		if e.phase == beforeStop && e.running[d.Agent] == d.Generation {
			delete(e.running, d.Agent)
			if d.By == "asmai stop" {
				e.endedByStop[d.Agent] = d.Generation
			}
		}
	case store.KindMessageCreated:
		e.kind[d.Dispatch] = d.Kind
		e.agent[d.Dispatch] = d.Recipient
		if e.phase == afterStart {
			e.made[d.Dispatch] = d.Recipient
		}
	case store.KindDispatchChanged:
		if d.Agent != "" {
			e.agent[d.ID] = d.Agent
		}
		if e.phase == afterStart && e.state[d.ID] == store.DispatchStopped && e.made[d.ID] == "" {
			// What stopped before the restart is never taken up again.
			e.reopened = append(e.reopened, d.ID)
		}
		e.state[d.ID] = d.State
	case store.KindMessageFetched:
		if e.phase == afterStart {
			e.fetched[d.Dispatch] = true
		}
	case store.KindObservation:
		if e.phase == afterStart && d.Event == "UserPromptSubmit" && e.started[d.Agent] == d.Generation {
			for id, agent := range e.agent {
				if agent == d.Agent && d.Payload.Prompt == fmt.Sprintf("asmai inbox --dispatch %d", id) {
					e.acked[id] = true
				}
			}
		}
	case store.KindAssignmentResumed:
		if e.phase == afterStart {
			e.resumed = append(e.resumed, d.Assignment)
		}
	case store.KindAssignmentNeedsReconciliation:
		if e.phase == afterStart {
			e.held = append(e.held, d.Assignment)
		}
	case store.KindResultSubmitted:
		var r struct {
			Assignment int64  `json:"assignment"`
			Dispatch   int64  `json:"dispatch"`
			Agent      string `json:"agent"`
		}
		json.Unmarshal(entry.Data, &r)
		e.results = append(e.results, resultEvidence{Assignment: r.Assignment, Dispatch: r.Dispatch, Agent: r.Agent})
	}
}

// delivered reports whether dispatch was delivered after the start: its
// message was fetched, or an agent's fetch moved it to delivered, which the
// journal records as a change of its state while it was acknowledged.
func (e *restartEvidence) delivered(dispatch int64) bool {
	return e.fetched[dispatch] || e.state[dispatch] == store.DispatchDelivered || e.state[dispatch] == store.DispatchWorking || e.state[dispatch] == store.DispatchStopped
}

// mustRestore returns the leaders the start has to restore: Coordination
// whenever a job is open, and Engineering when a handoff to it was open or it
// owned an assignment that had not ended, which is its open work.
func (e *restartEvidence) mustRestore() []string {
	var leaders []string
	if e.jobs > 0 {
		leaders = append(leaders, "leader@coordination")
	}
	for _, receiver := range e.handoffs {
		leaders = append(leaders, receiver)
	}
	for _, owner := range e.owned {
		leaders = append(leaders, owner)
	}
	slices.Sort(leaders)
	return slices.Compact(leaders)
}

// restoredBy returns the dispatches after the start that agent acknowledged
// by their exact nudge in its new session and that were then delivered.
func (e *restartEvidence) restoredBy(agent string) []int64 {
	var ids []int64
	for id, a := range e.agent {
		if a == agent && e.acked[id] && e.delivered(id) {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

// problems returns what the journal shows went wrong with the stop and the
// start, or what it does not yet show: waiting is true while the only thing
// missing is more time for the restored leaders to answer their nudges.
func (e *restartEvidence) problems(stoppedAgents []string) (problems []string, waiting bool) {
	if e.phase == beforeStop {
		return []string{"the journal shows no stop"}, false
	}
	if e.stoppedBy != "asmai stop" {
		problems = append(problems, fmt.Sprintf("the daemon was stopped by %q, not by asmai stop", e.stoppedBy))
	}
	for _, agent := range stoppedAgents {
		if e.endedByStop[agent] == 0 {
			problems = append(problems, fmt.Sprintf("the session of %s was not ended by asmai stop", agent))
		}
	}
	if len(e.running) > 0 {
		var left []string
		for agent := range e.running {
			left = append(left, agent)
		}
		slices.Sort(left)
		problems = append(problems, fmt.Sprintf("the stop left sessions with no recorded end: %s", strings.Join(left, ", ")))
	}
	if e.phase == afterStop {
		return append(problems, "the journal shows no start after the stop"), true
	}
	if e.previous != store.StateStopped {
		problems = append(problems, fmt.Sprintf("the start after the stop found the factory %q, not stopped", e.previous))
	}
	for _, id := range e.reopened {
		problems = append(problems, fmt.Sprintf("dispatch %d had stopped before the restart, and was changed after it", id))
	}
	for _, r := range e.results {
		kind, ok := e.kind[r.Dispatch]
		if !ok || e.agent[r.Dispatch] != r.Agent || (kind != store.MessageAssignment && kind != store.MessageRejection && kind != store.MessageResumption) {
			problems = append(problems, fmt.Sprintf("the result of assignment %d is tied to dispatch %d, which is not a dispatch that gave %s the assignment", r.Assignment, r.Dispatch, r.Agent))
		}
	}
	for _, leader := range e.restore {
		switch {
		case e.started[leader] == 0:
			problems = append(problems, fmt.Sprintf("%s, whose role has open work, was not restored by the start", leader))
			waiting = true
		case len(e.restoredBy(leader)) == 0:
			problems = append(problems, fmt.Sprintf("%s was restored in generation %d, but none of its dispatches was acknowledged by its exact nudge in that session and delivered", leader, e.started[leader]))
			waiting = true
		}
	}
	return problems, waiting
}

// c19 qualifies that dispatches and results stay correlated and reconcilable
// across restarts (02 rules 23 to 25): the case drives the real provider's
// leaders through the user's handoff, stops the factory as the user does and
// starts it again, and requires from the journal that every agent session was
// ended by the stop, that Coordination and each leader with open work were
// restored in a new generation and delivered a dispatch by its exact nudge in
// it, and that nothing which stopped before the restart was changed after it.
// What the stop did to workers, and the results they submitted, is read from
// the same journal when the leaders gave any.
func (h *Harness) c19(ctx context.Context, f *Factory, r *Result) error {
	e, err := h.exerciseHandoff(ctx, f, r)
	if err != nil {
		return err
	}
	r.observe("dispatch %d, the handoff to Engineering, was acknowledged by its exact nudge and fetched before the stop", e.dispatch)
	agents, err := f.Agents()
	if err != nil {
		return err
	}
	var running []string
	generations := map[string]int{}
	for _, a := range agents {
		generations[a.Agent] = a.Generation
		if a.State == store.AgentRunning {
			running = append(running, a.Agent)
		}
	}
	if !slices.Contains(running, "leader@coordination") {
		return errors.New("Coordination's leader was not running before the stop")
	}

	if err := f.Stop(ctx); err != nil {
		return err
	}
	if _, up := f.Running(); up {
		return errors.New("the daemon still answers after asmai stop")
	}
	res, code, err := f.Start(ctx)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("asmai start after the stop failed its checks: %s", failedChecks(res.Checks))
	}

	deadline := time.Now().Add(h.Wait)
	var evidence *restartEvidence
	var problems []string
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, err := factoryJournal(f)
		if err != nil {
			return err
		}
		evidence = newRestartEvidence(lastStop(entries))
		for _, entry := range entries {
			evidence.record(entry)
		}
		var waiting bool
		problems, waiting = evidence.problems(running)
		if len(problems) == 0 {
			break
		}
		if !waiting || time.Now().After(deadline) {
			return fmt.Errorf("the restart did not keep dispatches correlated: %s", strings.Join(problems, "; "))
		}
		time.Sleep(h.Poll)
	}

	for _, agent := range running {
		r.observe("%s (generation %d) was ended by asmai stop", agent, generations[agent])
	}
	for _, leader := range evidence.restore {
		ids := evidence.restoredBy(leader)
		if evidence.started[leader] <= generations[leader] {
			return fmt.Errorf("%s was restored in generation %d, which is not a new generation after %d", leader, evidence.started[leader], generations[leader])
		}
		r.observe("%s was restored in generation %d, and its dispatches %v were each acknowledged by their exact nudge in it and delivered", leader, evidence.started[leader], ids)
	}
	if len(evidence.reopened) == 0 {
		r.observe("no dispatch that had stopped before the restart changed after it")
	}
	switch {
	case len(evidence.resumed) > 0 || len(evidence.held) > 0:
		r.observe("the start resumed assignments %v and held assignments %v for reconciliation", evidence.resumed, evidence.held)
	case evidence.live == 0:
		r.observe("no worker carried an assignment at the stop, so its resumption was not exercised with the real provider; the development test exercises it with the fake")
	}
	for _, result := range evidence.results {
		r.observe("the result of assignment %d is tied to dispatch %d, which gave %s its assignment", result.Assignment, result.Dispatch, result.Agent)
	}
	return nil
}

// lastStop returns the ID of the last daemon stop in entries, or zero.
func lastStop(entries []store.Entry) int64 {
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Kind == store.KindDaemonStopped {
			return entries[i].ID
		}
	}
	return 0
}
