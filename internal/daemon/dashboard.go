// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
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
	// CommandDashboardSubscribe keeps the connection open and sends a tick
	// carrying the latest journal ID, at once and again whenever the journal
	// grows or a waiting prompt or an input owner changes, which are not
	// journaled. The Dashboard re-reads on a tick and derives nothing from it.
	CommandDashboardSubscribe = "dashboard.subscribe"
)

// DashboardCommands are the commands only the Dashboard sends.
var DashboardCommands = []string{CommandDashboardSnapshot, CommandDashboardSubscribe}

// DashboardChange is one tick of the change subscription.
type DashboardChange struct {
	// Journal is the ID of the latest journal entry, zero for an empty journal.
	Journal int64 `json:"journal"`
}

// changePoll is how often the subscription looks for a change. Ticks are
// coalesced by the Dashboard, so this only bounds how late one can be.
const changePoll = 100 * time.Millisecond

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

// JobStage is where a job is, as the Dashboard shows it. The daemon derives
// it, so every client agrees.
type JobStage string

// The stages, in the precedence they are derived in: a job is at the first
// that holds.
const (
	StageDelivered  JobStage = "delivered"
	StageNeedsYou   JobStage = "needs-you"
	StageValidating JobStage = "validating"
	StageDelivering JobStage = "delivering"
	StageWorking    JobStage = "working"
)

// Word is how the stage reads.
func (s JobStage) Word() string {
	switch s {
	case StageDelivered:
		return "Delivered"
	case StageNeedsYou:
		return "Needs you"
	case StageValidating:
		return "Validating"
	case StageDelivering:
		return "Delivering"
	default:
		return "Working"
	}
}

// AssignmentCounts is how many of a job's assignments are in each state.
type AssignmentCounts struct {
	Active         int `json:"active,omitempty"`
	Submitted      int `json:"submitted,omitempty"`
	Reconciliation int `json:"reconciliation,omitempty"`
	Cancelling     int `json:"cancelling,omitempty"`
	Accepted       int `json:"accepted,omitempty"`
	Cancelled      int `json:"cancelled,omitempty"`
}

// DashboardJob is one job in the snapshot.
type DashboardJob struct {
	Job   int64    `json:"job"`
	Stage JobStage `json:"stage"`
	// Assignments counts the job's assignments by state.
	Assignments AssignmentCounts `json:"assignments"`
	// PullRequest is the number of the job's pull request, zero for none.
	PullRequest int `json:"pull_request,omitempty"`
	// Title is the pull request's title, or the first line of the user's
	// request while the job has no pull request.
	Title string `json:"title"`
}

// The kinds of item the factory cannot continue without the user for, in the
// order the Waiting on you set lists them. A not-ready or red-CI pull request
// and an assignment needing reconciliation are statuses, not in the set.
const (
	// WaitingNativePrompt is a native permission prompt waiting in a leader's session.
	WaitingNativePrompt = "prompt"
	// WaitingInput is an agent whose input the user still holds.
	WaitingInput = "input"
	// WaitingDecision is a pending decision request. Its place is reserved:
	// the decision request record is not daemon state yet, so none is listed.
	WaitingDecision = "decision"
	// WaitingFinding is a needs-you finding of a job not yet ended.
	WaitingFinding = "finding"
)

// WaitingItem is one thing the factory cannot continue without the user for.
type WaitingItem struct {
	Kind string `json:"kind"`
	// Agent is the agent that waits, for a prompt or held input.
	Agent string `json:"agent,omitempty"`
	// Job is the job the item is about, zero for none.
	Job int64 `json:"job,omitempty"`
	// Finding is the finding's ID, for a needs-you finding.
	Finding int64 `json:"finding,omitempty"`
	// Text says what waits, in words.
	Text string `json:"text"`
	// Since is when it began to wait.
	Since time.Time `json:"since,omitzero"`
}

// DashboardSnapshot is the Dashboard's read of the factory.
type DashboardSnapshot struct {
	Agents []DashboardAgent `json:"agents"`
	Jobs   []DashboardJob   `json:"jobs"`
	// Waiting is the Waiting on you set, in kind order and, within a kind,
	// oldest first. It is empty when nothing waits.
	Waiting []WaitingItem `json:"waiting"`
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

// deriveJob derives the stage of a job from what its records say. This is the
// one derivation of the stage vocabulary; the Dashboard only shows it.
func deriveJob(w store.JobWork) DashboardJob {
	out := DashboardJob{Job: w.Number, PullRequest: w.PullRequest, Title: w.Title}
	if out.Title == "" {
		out.Title, _, _ = strings.Cut(strings.TrimSpace(w.Words), "\n")
	}
	out.Assignments = AssignmentCounts{
		Active:         w.Assignments[store.AssignmentActive],
		Submitted:      w.Assignments[store.AssignmentSubmitted],
		Reconciliation: w.Assignments[store.AssignmentNeedsReconciliation],
		Cancelling:     w.Assignments[store.AssignmentCancelling],
		Accepted:       w.Assignments[store.AssignmentAccepted],
		Cancelled:      w.Assignments[store.AssignmentCancelled],
	}
	switch {
	case w.Ended:
		out.Stage = StageDelivered
	case w.NeedsYou > 0:
		out.Stage = StageNeedsYou
	case w.Validating:
		out.Stage = StageValidating
	case !w.Writing && w.OpenBlocking == 0 && (w.Pushed || w.PRCurrent || w.Validated):
		out.Stage = StageDelivering
	default:
		out.Stage = StageWorking
	}
	return out
}

// dashboardSnapshot reads the factory for the Dashboard: every agent the store
// knows, and every staffed leader that has not run yet, as Stopped, and every
// job with its stage.
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

	lives := d.liveAgents()

	jobs, err := d.store.JobWorks()
	if err != nil {
		return DashboardSnapshot{}, fmt.Errorf("reading the jobs: %w", err)
	}
	snapshot := DashboardSnapshot{Agents: []DashboardAgent{}, Jobs: []DashboardJob{}}
	for _, w := range jobs {
		snapshot.Jobs = append(snapshot.Jobs, deriveJob(w))
	}
	for _, a := range agents {
		snapshot.Agents = append(snapshot.Agents, deriveAgent(a, lives[a.Agent], works[a.Agent]))
	}
	needsYou, err := d.store.NeedsYouFindings()
	if err != nil {
		return DashboardSnapshot{}, fmt.Errorf("reading the needs-you findings: %w", err)
	}
	snapshot.Waiting = waitingOnYou(snapshot.Agents, lives, needsYou)
	return snapshot, nil
}

// liveAgents is what the daemon knows in memory of each running agent, by
// address.
func (d *daemon) liveAgents() map[string]agentLive {
	lives := map[string]agentLive{}
	d.mu.Lock()
	defer d.mu.Unlock()
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
		if d.conversation != nil && d.conversation.leader == l {
			live.conversation = d.conversation.terminal
		}
		if l.prompt != nil && l.prompt.Generation == l.generation {
			p := *l.prompt
			live.prompt = &p
		}
		lives[name] = live
	}
	return lives
}

// liveMark is a string that differs whenever a waiting prompt or an input
// owner does, which the journal does not record.
func (d *daemon) liveMark() string {
	lives := d.liveAgents()
	names := make([]string, 0, len(lives))
	for name := range lives {
		names = append(names, name)
	}
	slices.Sort(names)
	var b strings.Builder
	for _, name := range names {
		l := lives[name]
		fmt.Fprintf(&b, "%s|%s|%s", name, l.input, l.conversation)
		if l.prompt != nil {
			fmt.Fprintf(&b, "|%s|%d", l.prompt.About, l.prompt.Since.UnixNano())
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// subscribe answers a change subscription on conn: a tick now and a tick at
// each change, until the Dashboard goes or the daemon stops. It only reads.
func (d *daemon) subscribe(conn *net.UnixConn, r io.Reader) {
	conn.SetDeadline(time.Time{})
	gone := make(chan struct{})
	go func() {
		io.Copy(io.Discard, r)
		close(gone)
	}()
	head, mark := int64(-1), ""
	ticker := time.NewTicker(changePoll)
	defer ticker.Stop()
	for {
		latest, err := d.store.JournalHead()
		if err != nil {
			d.log.Error("reading the journal head", "error", err.Error())
			conn.SetWriteDeadline(time.Now().Add(time.Second))
			reply(conn, Response{Error: "reading the journal head: " + err.Error()})
			return
		}
		if now := d.liveMark(); latest != head || now != mark {
			head, mark = latest, now
			line, _ := json.Marshal(Response{Change: &DashboardChange{Journal: latest}})
			conn.SetWriteDeadline(time.Now().Add(time.Second))
			if _, err := conn.Write(append(line, '\n')); err != nil {
				return
			}
		}
		select {
		case <-ticker.C:
		case <-gone:
			return
		case <-d.work.Done():
			return
		}
	}
}

// waitingOnYou derives the Waiting on you set: the agents the snapshot derived
// as waiting, with a native prompt before held input, then the needs-you
// findings. Pending decision requests have a reserved, empty position.
func waitingOnYou(agents []DashboardAgent, lives map[string]agentLive, findings []store.NeedsYouFinding) []WaitingItem {
	var items []WaitingItem
	for _, kind := range []string{WaitingNativePrompt, WaitingInput} {
		for _, a := range agents {
			if a.Status != StatusWaitingOnYou || (lives[a.Agent].prompt != nil) != (kind == WaitingNativePrompt) || (kind == WaitingNativePrompt && a.Agent != roles.LeaderOf(a.Role).String()) {
				continue
			}
			items = append(items, WaitingItem{Kind: kind, Agent: a.Agent, Job: a.Job, Text: a.Agent + " " + a.Doing, Since: a.Since})
		}
	}
	for _, f := range findings {
		text, _, _ := strings.Cut(strings.TrimSpace(f.Text), "\n")
		items = append(items, WaitingItem{Kind: WaitingFinding, Job: f.Job, Finding: f.ID, Text: fmt.Sprintf("job %d finding %d: %s", f.Job, f.ID, text), Since: f.At})
	}
	rank := func(kind string) int {
		return slices.Index([]string{WaitingNativePrompt, WaitingInput, WaitingDecision, WaitingFinding}, kind)
	}
	slices.SortStableFunc(items, func(a, b WaitingItem) int {
		if c := rank(a.Kind) - rank(b.Kind); c != 0 {
			return c
		}
		return a.Since.Compare(b.Since)
	})
	if items == nil {
		items = []WaitingItem{}
	}
	return items
}
