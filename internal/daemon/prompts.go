// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/store"
)

// permissionRequested is the Claude Code hook event a session reports as it
// raises a native permission prompt.
const permissionRequested = "PermissionRequest"

// promptCheck is how often an attach stream looks for a change in the native
// prompts agents wait at.
const promptCheck = time.Second

// WaitingPrompt is a native permission prompt an agent's session waits at.
// Only Coordination's terminal takes the user's keys. A prompt in any other
// session is something its rules did not foresee.
type WaitingPrompt struct {
	Agent      string `json:"agent"`
	Generation int    `json:"generation"`
	// About is what the session asks to do, as its hook reported it: the
	// tool, and the command for a shell command.
	About string    `json:"about"`
	Since time.Time `json:"since"`
	// Request is the decision request the daemon raised for the user through
	// Coordination under the managed policy, and zero where it raised none.
	Request int64 `json:"request,omitempty"`
}

// String is how `asmai status` and the status line show the prompt.
func (p WaitingPrompt) String() string {
	text := p.Agent + " waits at a native prompt nobody can answer"
	if p.Request != 0 {
		text = fmt.Sprintf("%s waits at a native prompt the user decides (decision request %d)", p.Agent, p.Request)
	}
	if p.About != "" {
		text += ": " + p.About
	}
	return text
}

// promptAbout describes what a PermissionRequest payload asks to do.
func promptAbout(payload json.RawMessage) string {
	var fields struct {
		Tool  string `json:"tool_name"`
		Input struct {
			Command  string `json:"command"`
			FilePath string `json:"file_path"`
		} `json:"tool_input"`
	}
	json.Unmarshal(payload, &fields)
	about := fields.Tool
	if target := cmp.Or(fields.Input.Command, fields.Input.FilePath); target != "" {
		// The status line has one row.
		target, _, _ = strings.Cut(target, "\n")
		about = fmt.Sprintf("%s `%s`", about, target)
	}
	return strings.TrimSpace(about)
}

// promptObserved notes what an event ref's session reported says of the
// native prompts it waits at: a permission request starts one, and any later
// event of the session shows it is past it. Under the managed policy, a
// prompt at an agent without the user's terminal is raised to the user
// through Coordination, and withdrawn when the agent is past it. d.mu is not
// held.
func (d *daemon) promptObserved(ref sessionRef, event string, payload json.RawMessage) {
	if notify := d.promptNoted(ref, event, payload); notify != "" {
		d.nudge(notify)
	}
}

// promptNoted is promptObserved up to the nudge of the agent it asks to
// present a request it raised, which it returns.
func (d *daemon) promptNoted(ref sessionRef, event string, payload json.RawMessage) (notify string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	l := d.leaders[ref.address.String()]
	if l == nil || l.session == nil || l.generation != ref.generation {
		return ""
	}
	d.promptEnded(l)
	if event != permissionRequested {
		return ""
	}
	l.prompt = &WaitingPrompt{Agent: ref.address.String(), Generation: ref.generation, About: promptAbout(payload), Since: time.Now()}
	if ref.address != roles.LeaderOf(roles.Coordination) {
		d.log.Warn("an agent waits at a native permission prompt nobody can answer", "agent", l.prompt.Agent, "generation", l.prompt.Generation, "about", l.prompt.About)
		if l.managedOnly {
			return d.promptRaised(l)
		}
	}
	return ""
}

// promptEnded forgets the native prompt l's session waited at, and withdraws
// the request raised for it if the user has not answered it. d.mu is held.
func (d *daemon) promptEnded(l *leader) {
	p := l.prompt
	l.prompt = nil
	if p == nil || p.Request == 0 {
		return
	}
	withdrawn, err := d.store.PromptRequestWithdrawn(p.Request, time.Now())
	if err != nil {
		d.log.Error("withdrawing the request for a native prompt the agent is past", "agent", p.Agent, "request", p.Request, "error", err.Error())
		return
	}
	if withdrawn {
		d.log.Info("the agent is past its native prompt; its request to the user is withdrawn", "agent", p.Agent, "request", p.Request)
	}
}

// promptRaised raises the native prompt l's session waits at, which nobody
// can answer in its terminal, as a decision request to the user through
// Coordination, in the job the agent works on, and returns the agent that
// presents it. The user decides it: nothing is ever typed into the prompt
// until their witnessed answer is recorded (relayPrompt). d.mu is held.
func (d *daemon) promptRaised(l *leader) (notify string) {
	p := l.prompt
	job := d.promptJob(l)
	if job == 0 {
		d.log.Warn("a native prompt is not raised to the user: the agent works on no job it could be asked about", "agent", p.Agent)
		return ""
	}
	coordination := roles.LeaderOf(roles.Coordination)
	r, _, err := d.store.DecisionRequested(store.DecisionRequestInput{
		Job: job, Requester: p.Agent, Recipient: store.ToUser, Notify: coordination.String(), Assignment: l.assignment, Kind: store.RequestPrompt,
		Content:   fmt.Sprintf("%s waits at a native permission prompt in its own terminal, which nobody else can answer, and its work stops there until it is answered. It asks to: %s.\nThe daemon never answers it: your answer is typed into the agent's terminal as it stands, and nothing else is.", p.Agent, cmp.Or(p.About, "do something its permissions do not allow")),
		Questions: []store.Question{{ID: "prompt", Title: "Let " + p.Agent + " do this once?", Body: p.About, Options: []string{store.PromptAllow, store.PromptDeny}}},
	}, time.Now())
	if err != nil {
		d.log.Error("raising a native prompt to the user", "agent", p.Agent, "job", job, "error", err.Error())
		return ""
	}
	p.Request = r.Request
	d.log.Info("a native prompt is raised to the user through Coordination", "agent", p.Agent, "request", r.Request, "job", job, "about", p.About)
	if err := d.ensureAgent(coordination); err != nil {
		d.log.Error("starting the agent a native prompt's request goes to", "agent", coordination.String(), "request", r.Request, "error", err.Error())
	}
	return coordination.String()
}

// promptJob returns the job the agent of l works on: its assignment's, or its
// current dispatch's. It is zero for an agent working on neither.
func (d *daemon) promptJob(l *leader) int64 {
	if l.assignment != 0 {
		if a, err := d.store.Assignment(l.assignment); err == nil {
			return a.Job
		}
	}
	if l.currentDispatch != 0 {
		if job, err := d.store.DispatchJob(l.currentDispatch); err == nil {
			return job
		}
	}
	return 0
}

// The keys typed into the terminal of an agent at a native prompt for the
// user's answer to it: Enter takes the prompt's default, to go on once, and
// Escape cancels it. Neither adds an allow rule.
var promptKeys = map[string][]byte{
	store.PromptAllow: []byte("\r"),
	store.PromptDeny:  []byte("\x1b"),
}

// PromptKeys returns the keys the daemon types into an agent's terminal for
// the user's choice on a native prompt request, store.PromptAllow or
// store.PromptDeny, which qualification types into a real Claude Code prompt
// to show they answer it as the relay says.
func PromptKeys(choice string) ([]byte, bool) {
	keys, ok := promptKeys[choice]
	return slices.Clone(keys), ok
}

// relayPrompt types the user's answer to r, a native prompt request they
// answered with a witnessed message that the store checked, into the terminal
// of the agent that waits at the prompt, if its session is still the one that
// raised it and still waits there. The daemon never calls it for anything
// else. d.mu is not held.
func (d *daemon) relayPrompt(r store.DecisionRequest, choice string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	keys, ok := promptKeys[choice]
	l := d.leaders[r.Requester]
	switch {
	case !ok:
		d.log.Error("a native prompt's answer is neither of its options, so nothing is relayed", "request", r.Request, "choice", choice)
	case l == nil || l.session == nil || l.prompt == nil || l.prompt.Request != r.Request || l.prompt.Generation != l.generation:
		d.log.Warn("the agent no longer waits at the native prompt the user answered, so nothing is relayed", "agent", r.Requester, "request", r.Request)
	default:
		if err := l.session.Input(keys); err != nil {
			d.log.Error("typing the user's answer into the agent's terminal", "agent", r.Requester, "request", r.Request, "error", err.Error())
			return
		}
		// The user has decided it: the agent no longer waits on them.
		l.prompt = nil
		d.log.Info("the user's answer to a native prompt is relayed to the agent's terminal", "agent", r.Requester, "request", r.Request, "choice", choice, "cites", r.Cites)
		if err := d.store.PromptRelayed(r, choice, l.generation, time.Now()); err != nil {
			d.log.Error("journaling the relay of a native prompt's answer", "agent", r.Requester, "request", r.Request, "error", err.Error())
		}
	}
}

// waitingPrompts returns the native prompts running sessions wait at that
// nobody can answer, by agent. d.mu is not held.
func (d *daemon) waitingPrompts() []WaitingPrompt {
	d.mu.Lock()
	defer d.mu.Unlock()
	var prompts []WaitingPrompt
	for _, l := range d.leaders {
		if l.session != nil && l.address != roles.LeaderOf(roles.Coordination) && l.prompt != nil && l.prompt.Generation == l.generation {
			prompts = append(prompts, *l.prompt)
		}
	}
	slices.SortFunc(prompts, func(a, b WaitingPrompt) int { return strings.Compare(a.Agent, b.Agent) })
	return prompts
}

// promptLines returns the waitingPrompts as the status line shows them.
func (d *daemon) promptLines() []string {
	lines := []string{}
	for _, p := range d.waitingPrompts() {
		lines = append(lines, p.String())
	}
	return lines
}
