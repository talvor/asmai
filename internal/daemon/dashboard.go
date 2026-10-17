// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/store"
)

// The Dashboard's calls (ADR 0014). They only read, have no CLI subcommand
// and are not on the agent allow list, so authorize denies them to every agent
// session. DashboardCommands lists them for the guard test.
const (
	// CommandDashboardSnapshot is the Dashboard's read of the factory: every
	// agent with its derived status.
	CommandDashboardSnapshot = "dashboard.snapshot"
)

// DashboardCommands are the commands only the Dashboard sends.
var DashboardCommands = []string{CommandDashboardSnapshot}

// AgentStatus is what an agent is doing, as the Dashboard shows it. The
// daemon derives it, so every client agrees.
type AgentStatus string

// The statuses, in the precedence they are derived in: an agent has the first
// that holds.
const (
	StatusStopped        AgentStatus = "stopped"
	StatusWaitingOnYou   AgentStatus = "waiting"
	StatusReconciliation AgentStatus = "reconciliation"
	StatusWorking        AgentStatus = "working"
	StatusIdle           AgentStatus = "idle"
)

// Word is how the status reads.
func (s AgentStatus) Word() string {
	switch s {
	case StatusStopped:
		return "Stopped"
	case StatusWaitingOnYou:
		return "Waiting on you"
	case StatusReconciliation:
		return "Needs reconciliation"
	case StatusWorking:
		return "Working"
	default:
		return "Idle"
	}
}

// DashboardAgent is one agent in the snapshot.
type DashboardAgent struct {
	Agent  string      `json:"agent"`
	Role   string      `json:"role"`
	Status AgentStatus `json:"status"`
	// DispatchUnknown marks an Idle agent whose latest dispatch is in the
	// unknown state: it is not known whether the agent is working.
	DispatchUnknown bool `json:"dispatch_unknown,omitempty"`
	// Job is the job the agent is on, zero for none.
	Job int64 `json:"job,omitempty"`
	// Input is who owns a running agent's input: InputAutomation, InputUser
	// or InputUnknown; empty for a stopped agent.
	Input string `json:"input,omitempty"`
	// Since is when the agent came to this status, zero when not known.
	Since time.Time `json:"since,omitzero"`
	// Doing is what the agent is doing, in words.
	Doing string `json:"doing,omitempty"`
}

// DashboardSnapshot is the Dashboard's read of the factory.
type DashboardSnapshot struct {
	Agents []DashboardAgent `json:"agents"`
}

// agentLive is what the daemon knows of a running agent in memory.
type agentLive struct {
	input        string
	conversation string
	prompt       *WaitingPrompt
}

// deriveAgent derives the status of agent a from what is known of it. This is
// the one derivation of the status vocabulary; the Dashboard only shows it.
func deriveAgent(a store.Agent, live agentLive, w store.AgentWork) DashboardAgent {
	out := DashboardAgent{Agent: a.Agent, Role: a.Role, Since: w.At}
	if out.Since.IsZero() {
		out.Since = a.StartedAt
	}
	if a.State == store.AgentRunning {
		out.Input = live.input
	}
	switch {
	case a.State != store.AgentRunning:
		out.Status = StatusStopped
		out.Since = a.EndedAt
		out.Doing = "session ended"
		if a.Exit != "" {
			out.Doing += ": " + a.Exit
		}
	case live.prompt != nil || live.input == InputUser:
		out.Status = StatusWaitingOnYou
		if w.Dispatch == store.DispatchWorking {
			out.Job = w.Job
		}
		if live.prompt != nil {
			out.Since = live.prompt.Since
			out.Doing = "native prompt"
			if live.prompt.About != "" {
				out.Doing += ": " + live.prompt.About
			}
		} else {
			out.Doing = "you hold its input"
			if live.conversation != "" {
				out.Doing += " in " + live.conversation
			}
		}
	case w.Reconciling != 0:
		out.Status = StatusReconciliation
		out.Job, out.Since = w.ReconcilingJob, w.ReconcilingAt
		out.Doing = fmt.Sprintf("assignment %d: its dispatch's outcome is not known", w.Reconciling)
	case w.Dispatch == store.DispatchWorking:
		out.Status = StatusWorking
		out.Job, out.Doing = w.Job, doing(w)
	default:
		out.Status = StatusIdle
		if w.Dispatch == store.DispatchUnknown {
			out.DispatchUnknown = true
			out.Doing = "dispatch state unknown"
		}
	}
	return out
}

// doing says what a dispatch asked of an agent: its assignment or handoff's
// outcome, or the kind of message.
func doing(w store.AgentWork) string {
	outcome, _, _ := strings.Cut(strings.TrimSpace(w.Outcome), "\n")
	switch {
	case w.Assignment != 0 && outcome != "":
		return fmt.Sprintf("assignment %d: %s", w.Assignment, outcome)
	case w.Assignment != 0:
		return fmt.Sprintf("assignment %d: %s", w.Assignment, w.Kind)
	case outcome != "":
		return w.Kind + ": " + outcome
	default:
		return w.Kind
	}
}

// dashboardSnapshot reads the factory for the Dashboard: every agent the store
// knows, and every staffed leader that has not run yet, as Stopped.
func (d *daemon) dashboardSnapshot() (DashboardSnapshot, error) {
	agents, err := d.store.Agents()
	if err != nil {
		return DashboardSnapshot{}, fmt.Errorf("reading the agents: %w", err)
	}
	works, err := d.store.AgentWorks()
	if err != nil {
		return DashboardSnapshot{}, fmt.Errorf("reading what the agents do: %w", err)
	}
	for _, role := range roles.Staffed {
		address := roles.LeaderOf(role)
		if !slices.ContainsFunc(agents, func(a store.Agent) bool { return a.Agent == address.String() }) {
			agents = append(agents, store.Agent{Agent: address.String(), Role: role, State: store.AgentStopped})
		}
	}
	slices.SortFunc(agents, func(a, b store.Agent) int { return strings.Compare(a.Agent, b.Agent) })

	lives := map[string]agentLive{}
	d.mu.Lock()
	conversation := d.conversation
	for name, l := range d.leaders {
		if l.session == nil {
			continue
		}
		live := agentLive{input: InputAutomation}
		if l.inputUnknown {
			live.input = InputUnknown
		} else if l.userInput {
			live.input = InputUser
		}
		if conversation != nil && conversation.leader == l {
			live.conversation = conversation.terminal
		}
		if l.prompt != nil && l.prompt.Generation == l.generation {
			p := *l.prompt
			live.prompt = &p
		}
		lives[name] = live
	}
	d.mu.Unlock()

	snapshot := DashboardSnapshot{Agents: []DashboardAgent{}}
	for _, a := range agents {
		snapshot.Agents = append(snapshot.Agents, deriveAgent(a, lives[a.Agent], works[a.Agent]))
	}
	return snapshot, nil
}
