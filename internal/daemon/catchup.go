// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/talvor/asmai/internal/lavish"
	"github.com/talvor/asmai/internal/providers"
	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/store"
)

// The kinds of event the catch-up and `asmai status` tell the user of, in the
// order they are listed (04 rule 17, 07 rule 33).
const (
	// EventWaiting is something that waits on the user: a decision request
	// pending with them, with its Lavish link when it is answered there, or
	// the native prompt Coordination's leader waits at.
	EventWaiting = "waiting"
	// EventFinished is a job that ended since the user last left.
	EventFinished = "finished"
	// EventFailed is a job whose pull request's checks went red since the
	// user last left and are red still.
	EventFailed = "failed"
	// EventPaused is a job the user paused.
	EventPaused = "paused"
	// EventHold is an agent held, with its cause.
	EventHold = "hold"
	// EventTerminal is an agent waiting at a native prompt in its own
	// terminal.
	EventTerminal = "terminal"
	// EventInput is an agent whose input the user still holds.
	EventInput = "input"
	// EventSignIn is the provider's sign-in lost, with the native command
	// that signs it in again.
	EventSignIn = "sign-in"
)

// Event is one thing the user is told of when they return to the
// conversation, and by `asmai status`.
type Event struct {
	Kind  string `json:"kind"`
	Job   int64  `json:"job,omitempty"`
	Agent string `json:"agent,omitempty"`
	// Text says what happened or waits, in words.
	Text string `json:"text"`
	// Link is the Lavish page a waiting decision request is answered in.
	Link string `json:"link,omitempty"`
	// Command is the command the user runs themselves: the provider's own
	// sign-in command for a lost sign-in.
	Command string `json:"command,omitempty"`
}

// CatchUp is what the attach client prints before the live conversation when
// the user returns to it (04 rule 17): the events since they last left,
// drawn from the journal and the daemon's state, and the text they left
// unsent, offered back.
type CatchUp struct {
	Events []Event `json:"events"`
	// Unsent is the text the user's last leaving saved from Coordination's
	// input box. It is never typed again.
	Unsent []store.UnsentText `json:"unsent"`
	// LavishPort is the port of the Lavish server when an event has a
	// link, for the command that forwards it.
	LavishPort int `json:"lavish_port,omitempty"`
}

// events returns what the user is told of: what waits on them, the jobs that
// ended, failed or paused, the agents held with their causes, the agents
// waiting in their own terminals, the input the user still holds, and a
// lost sign-in. The journal's entries after the one that records the user's
// last leaving, last, say which jobs ended or failed since; with none, the
// user has not left before and no job changed since. The same events are in
// the catch-up and in `asmai status`.
func (d *daemon) events(last int64) ([]Event, error) {
	var events []Event

	pending, err := d.store.PendingDecisions()
	if err != nil {
		return nil, fmt.Errorf("reading the pending decisions: %w", err)
	}
	// A worker's native prompt that raised a decision request is told of
	// with it.
	prompts := d.waitingPrompts()
	raised := map[int64]WaitingPrompt{}
	for _, p := range prompts {
		if p.Request != 0 {
			raised[p.Request] = p
		}
	}
	if p := d.coordinationPrompt(); p != nil {
		events = append(events, Event{Kind: EventWaiting, Agent: p.Agent, Text: p.String()})
	}
	for _, r := range pending {
		text := decisionLine(r)
		if p, ok := raised[r.Request]; ok {
			text = store.JobTag(r.Job) + " " + p.String()
		}
		events = append(events, Event{Kind: EventWaiting, Job: r.Job, Text: text, Link: r.PageURL})
	}
	findings, err := d.store.NeedsYouFindings()
	if err != nil {
		return nil, fmt.Errorf("reading the needs-you findings: %w", err)
	}
	for _, item := range waitingOnYou(nil, nil, findings) {
		events = append(events, Event{Kind: EventWaiting, Job: item.Job, Text: item.Text})
	}

	jobs, err := d.jobEvents(last)
	if err != nil {
		return nil, err
	}
	events = append(events, jobs...)

	lives := d.liveAgents()
	d.mu.Lock()
	checks := slices.Clone(d.lastChecks)
	heldAgent := d.lastHeldAgent
	leader := d.leaders[heldAgent]
	heldByChecks := leader == nil || leader.session == nil
	d.mu.Unlock()
	if heldAgent != "" && heldByChecks {
		for _, c := range checks {
			if !c.OK {
				events = append(events, Event{Kind: EventHold, Agent: heldAgent, Text: fmt.Sprintf("%s is held, not started: %s: %s (fix: %s)", heldAgent, c.Name, c.Problem, c.Fix)})
			}
		}
	}
	works, err := d.store.AgentWorks()
	if err != nil {
		return nil, fmt.Errorf("reading what the agents do: %w", err)
	}
	running := slices.Sorted(maps.Keys(lives))
	for _, name := range slices.Sorted(maps.Keys(works)) {
		if w := works[name]; w.Reconciling != 0 {
			events = append(events, Event{Kind: EventHold, Job: w.ReconcilingJob, Agent: name, Text: fmt.Sprintf("%s is held: assignment %d's dispatch has an outcome nobody knows", name, w.Reconciling)})
		}
	}
	for _, name := range running {
		if lives[name].input == InputUnknown {
			events = append(events, Event{Kind: EventHold, Agent: name, Text: name + " is held: its input is at a state AsmAI does not know, so nothing is typed over it"})
		}
	}
	for _, p := range prompts {
		if p.Request == 0 {
			events = append(events, Event{Kind: EventTerminal, Agent: p.Agent, Text: p.String()})
		}
	}
	for _, name := range running {
		if lives[name].input == InputUser && lives[name].conversation == "" {
			events = append(events, Event{Kind: EventInput, Agent: name, Text: "you still hold " + name + "'s input"})
		}
	}
	if e := d.signInEvent(); e != nil {
		events = append(events, *e)
	}
	return events, nil
}

// jobEvents tells of the jobs that ended and the jobs whose checks went red
// after the journal entry last, and of every job that is paused now.
func (d *daemon) jobEvents(last int64) ([]Event, error) {
	var events []Event
	described := func(number int64) (store.Job, string) {
		j, err := d.store.Job(number)
		if err != nil {
			return store.Job{}, fmt.Sprintf("job %d", number)
		}
		title, _, _ := strings.Cut(strings.TrimSpace(j.Words), "\n")
		return j, fmt.Sprintf("job %d (%s)", number, title)
	}
	if last != 0 {
		entries, err := d.store.EntriesAfter(last, store.KindJobEnded, store.KindJobCancelled, store.KindChecksObserved)
		if err != nil {
			return nil, fmt.Errorf("reading the journal since you last left: %w", err)
		}
		told := map[string]bool{}
		for _, e := range entries {
			var data struct {
				Job     int64  `json:"job"`
				Verdict string `json:"verdict"`
			}
			if json.Unmarshal(e.Data, &data) != nil || data.Job == 0 {
				continue
			}
			kind := EventFinished
			if e.Kind == store.KindChecksObserved {
				kind = EventFailed
				latest, err := d.store.LatestChecks(data.Job)
				if err != nil || latest.Verdict != "red" {
					continue
				}
				if _, head, ok := d.watched(data.Job); !ok || latest.Head != head {
					continue
				}
				if j, err := d.store.Job(data.Job); err != nil || j.Ended() {
					continue
				}
			}
			if told[fmt.Sprintf("%s %d", kind, data.Job)] {
				continue
			}
			told[fmt.Sprintf("%s %d", kind, data.Job)] = true
			j, what := described(data.Job)
			switch {
			case kind == EventFailed:
				events = append(events, Event{Kind: kind, Job: data.Job, Text: what + " failed: the checks on its pull request are red"})
			case e.Kind == store.KindJobCancelled || j.State == store.JobCancelled:
				events = append(events, Event{Kind: kind, Job: data.Job, Text: what + " was cancelled"})
			default:
				events = append(events, Event{Kind: kind, Job: data.Job, Text: what + " finished"})
			}
		}
	}
	jobs, err := d.store.Jobs()
	if err != nil {
		return nil, fmt.Errorf("reading the jobs: %w", err)
	}
	for _, j := range jobs {
		if j.State == store.JobPaused {
			_, what := described(j.Number)
			events = append(events, Event{Kind: EventPaused, Job: j.Number, Text: what + " is paused"})
		}
	}
	rank := func(e Event) int {
		return slices.Index([]string{EventFinished, EventFailed, EventPaused}, e.Kind)
	}
	slices.SortStableFunc(events, func(a, b Event) int { return rank(a) - rank(b) })
	return events, nil
}

// signInEvent tells of the pinned Claude Code's sign-in when it is lost, or
// is not the user's Claude subscription, with the native command that signs
// in again (04 rule 18).
func (d *daemon) signInEvent() *Event {
	install, err := d.claudeCode(d.cfg.Pins.ClaudeCode.Version)
	if err != nil {
		return nil
	}
	check := SignInCheck(install.Path)
	if check.OK {
		return nil
	}
	return &Event{Kind: EventSignIn, Agent: roles.LeaderOf(roles.Coordination).String(), Text: "Claude Code's sign-in is lost: " + check.Problem, Command: providers.ShellQuote(install.Path) + " auth login"}
}

// catchUp returns what the user returns to: the events since they last left
// and the text they left unsent.
func (d *daemon) catchUp() (CatchUp, error) {
	last, _, err := d.store.LastLeft()
	if err != nil {
		return CatchUp{}, fmt.Errorf("reading when you last left: %w", err)
	}
	events, err := d.events(last)
	if err != nil {
		return CatchUp{}, err
	}
	c := CatchUp{Events: events, Unsent: []store.UnsentText{}}
	if last != 0 {
		if c.Unsent, err = d.store.UnsentTextLeft(last); err != nil {
			return CatchUp{}, fmt.Errorf("reading the text you left unsent: %w", err)
		}
	}
	if slices.ContainsFunc(events, func(e Event) bool { return e.Link != "" }) {
		c.LavishPort = lavish.Port
		if d.lavish != nil {
			c.LavishPort = d.lavish.Port
		}
	}
	return c, nil
}
