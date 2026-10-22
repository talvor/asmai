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
	// CommandDashboardLog reads a page of the journal as one-line entries,
	// from a cursor in either direction, narrowed by job, agent and kind.
	CommandDashboardLog = "dashboard.log"
	// CommandDashboardEntry reads one journal entry with the body of the
	// message or decision it records.
	CommandDashboardEntry = "dashboard.entry"
	// CommandDashboardJob reads one job's detail: its header, findings,
	// assignments and handoffs, each with the time it last changed.
	CommandDashboardJob = "dashboard.job"
)

// DashboardCommands are the commands only the Dashboard sends.
var DashboardCommands = []string{CommandDashboardSnapshot, CommandDashboardSubscribe, CommandDashboardLog, CommandDashboardEntry, CommandDashboardJob}

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

// TrafficKinds are the journal kinds of agent traffic: handoffs, assignments,
// results, blocked reports, and acceptances and rejections. The Log tab shows
// them by default and the whole journal on request.
var TrafficKinds = []string{
	store.KindHandoffSent, store.KindAssignmentCreated, store.KindResultSubmitted,
	store.KindBlockedReported, store.KindResultAccepted, store.KindResultRejected, store.KindResultReturned,
}

// The least and most entries a log read returns, and what it returns when the
// request does not say.
const (
	logLimitDefault = 300
	logLimitMax     = 1000
)

// LogQuery is a log read. With neither Before nor After it reads the latest
// entries; with Before, the entries older than that journal ID; with After,
// the entries newer than it. Kinds, Job and Agent narrow it when set.
type LogQuery struct {
	Kinds  []string `json:"kinds,omitempty"`
	Job    int64    `json:"job,omitempty"`
	Agent  string   `json:"agent,omitempty"`
	Before int64    `json:"before,omitempty"`
	After  int64    `json:"after,omitempty"`
	Limit  int      `json:"limit,omitempty"`
}

// LogEntry is one journal entry as a one-line summary.
type LogEntry struct {
	ID   int64     `json:"id"`
	At   time.Time `json:"at"`
	Kind string    `json:"kind"`
	// Job is the job the entry is about, zero for none.
	Job int64 `json:"job,omitempty"`
	// From and To are the agents the entry runs between, either may be empty.
	From    string `json:"from,omitempty"`
	To      string `json:"to,omitempty"`
	Summary string `json:"summary"`
}

// DashboardLog is a page of the journal, oldest first. Older and Newer say
// whether entries that match the query lie beyond the page in each direction.
type DashboardLog struct {
	Entries []LogEntry `json:"entries"`
	Older   bool       `json:"older"`
	Newer   bool       `json:"newer"`
}

// DashboardEntry is one journal entry with its summary and message body.
type DashboardEntry struct {
	LogEntry
	Body string `json:"body"`
}

// entryFields are the fields of a journal entry's data that a summary reads.
type entryFields struct {
	ID         int64    `json:"id"`
	Job        int64    `json:"job"`
	Number     int64    `json:"number"`
	Assignment int64    `json:"assignment"`
	Sender     string   `json:"sender"`
	Receiver   string   `json:"receiver"`
	Recipient  string   `json:"recipient"`
	Owner      string   `json:"owner"`
	Worker     string   `json:"worker"`
	Leader     string   `json:"leader"`
	Agent      string   `json:"agent"`
	Kind       string   `json:"kind"`
	Outcome    string   `json:"outcome"`
	Reason     string   `json:"reason"`
	Reasons    []string `json:"reasons"`
}

// firstLine is the first non-blank line of s.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

// logEntry summarises e in a line. The summary is the one derivation of the
// log's words; the Dashboard only shows it.
func (d *daemon) logEntry(e store.Entry) LogEntry {
	var f entryFields
	json.Unmarshal(e.Data, &f)
	out := LogEntry{ID: e.ID, At: e.At, Kind: e.Kind, Job: f.Job, Summary: e.Kind}
	if e.Kind == store.KindJobOpened {
		out.Job = f.Number
	}
	out.From = cmpFirst(f.Sender, f.Owner, f.Leader, f.Agent)
	out.To = cmpFirst(f.Receiver, f.Recipient, f.Worker)
	with := func(prefix, detail string) string {
		if detail = firstLine(detail); detail != "" {
			return prefix + ": " + detail
		}
		return prefix
	}
	// The assignment a report or a decision is about says who the other end is.
	other := func() store.Assignment {
		a, _ := d.store.Assignment(f.Assignment)
		return a
	}
	switch e.Kind {
	case store.KindHandoffSent:
		out.Summary = with("handoff", f.Outcome)
	case store.KindAssignmentCreated:
		out.Summary = with(fmt.Sprintf("assignment %d", f.ID), f.Outcome)
	case store.KindResultSubmitted:
		out.To = other().Owner
		out.Summary = fmt.Sprintf("result of assignment %d", f.Assignment)
	case store.KindBlockedReported:
		out.To = other().Owner
		out.Summary = with(fmt.Sprintf("blocked on assignment %d", f.Assignment), f.Reason)
	case store.KindResultAccepted:
		out.To = other().Worker
		out.Summary = with(fmt.Sprintf("accepted assignment %d", f.Assignment), strings.Join(f.Reasons, " "))
	case store.KindResultRejected:
		out.To = other().Worker
		out.Summary = with(fmt.Sprintf("rejected assignment %d", f.Assignment), strings.Join(f.Reasons, " "))
	case store.KindResultReturned:
		out.To = other().Worker
		out.Summary = fmt.Sprintf("returned assignment %d: the job branch moved after its result", f.Assignment)
	case store.KindMessageCreated:
		out.Summary = f.Kind + " message"
	}
	return out
}

// cmpFirst is the first of names that is not empty.
func cmpFirst(names ...string) string {
	for _, n := range names {
		if n != "" {
			return n
		}
	}
	return ""
}

// dashboardLog reads a page of the journal for the Log tab.
func (d *daemon) dashboardLog(q LogQuery) (DashboardLog, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = logLimitDefault
	}
	entries, older, newer, err := d.store.JournalPage(store.JournalQuery{Kinds: q.Kinds, Job: q.Job, Agent: q.Agent, Before: q.Before, After: q.After, Limit: min(limit, logLimitMax)})
	if err != nil {
		return DashboardLog{}, fmt.Errorf("reading the journal: %w", err)
	}
	page := DashboardLog{Entries: []LogEntry{}, Older: older, Newer: newer}
	for _, e := range entries {
		page.Entries = append(page.Entries, d.logEntry(e))
	}
	return page, nil
}

// dashboardEntry reads one journal entry with its body.
func (d *daemon) dashboardEntry(id int64) (DashboardEntry, error) {
	entries, _, _, err := d.store.JournalPage(store.JournalQuery{Before: id + 1, Limit: 1})
	if err != nil {
		return DashboardEntry{}, fmt.Errorf("reading the journal entry: %w", err)
	}
	if len(entries) == 0 || entries[0].ID != id {
		return DashboardEntry{}, fmt.Errorf("journal entry %d does not exist", id)
	}
	body, err := d.store.MessageBodyOf(entries[0])
	if err != nil {
		return DashboardEntry{}, fmt.Errorf("reading the body of journal entry %d: %w", id, err)
	}
	return DashboardEntry{LogEntry: d.logEntry(entries[0]), Body: body}, nil
}

// DashboardJobDetail is one job as the Dashboard's job detail shows it: the
// header, and the job's work with the time each item last changed.
type DashboardJobDetail struct {
	Job             int64    `json:"job"`
	Title           string   `json:"title"`
	Stage           JobStage `json:"stage"`
	HandoffComplete bool     `json:"handoff_complete"`
	// Branch and Head are the job branch and its tip, empty for a job with no
	// repository.
	Branch string `json:"branch,omitempty"`
	Head   string `json:"head,omitempty"`
	// PullRequest is the pull request's number, zero for none, and
	// PullRequestState says where it is in words.
	PullRequest      int    `json:"pull_request,omitempty"`
	PullRequestState string `json:"pull_request_state,omitempty"`
	// CI and Validation say in words how the checks on the pull request's
	// head and Quality's validation stand.
	CI         string `json:"ci"`
	Validation string `json:"validation"`
	// Work is the job's findings, needs-you first, then its assignments, then
	// its handoffs, each oldest first. The Dashboard lists it in this order.
	Work []JobItem `json:"work"`
}

// The kinds of item in a job's work.
const (
	ItemFinding    = "finding"
	ItemAssignment = "assignment"
	ItemHandoff    = "handoff"
)

// JobItem is one finding, assignment or handoff of a job.
type JobItem struct {
	Kind string `json:"kind"`
	ID   int64  `json:"id"`
	// Label says which sort of the kind it is: a finding's kind, or the state
	// of an assignment or a handoff, in words.
	Label string `json:"label"`
	// Text is what the item says in full: a finding's text, or the outcome of
	// an assignment or a handoff.
	Text string `json:"text"`
	// From and To are a handoff's sender and receiver; Worker is an
	// assignment's worker, whom Enter attaches to.
	From   string `json:"from,omitempty"`
	To     string `json:"to,omitempty"`
	Worker string `json:"worker,omitempty"`
	// Criteria are acceptance criteria; Dispatch is an assignment's worker
	// dispatch, and the other fields describe a handoff.
	Criteria    []string  `json:"criteria,omitempty"`
	Dispatch    string    `json:"dispatch,omitempty"`
	Answer      string    `json:"answer,omitempty"`
	Decisions   string    `json:"decisions,omitempty"`
	Evidence    string    `json:"evidence,omitempty"`
	Constraints string    `json:"constraints,omitempty"`
	Permissions string    `json:"permissions,omitempty"`
	Head        string    `json:"head,omitempty"`
	Commit      string    `json:"commit,omitempty"`
	Assignment  int64     `json:"assignment,omitempty"`
	Report      int64     `json:"report,omitempty"`
	ClearedBy   int64     `json:"cleared_by,omitempty"`
	ReadOnly    bool      `json:"read_only,omitempty"`
	Skill       string    `json:"skill,omitempty"`
	Skills      []string  `json:"skills,omitempty"`
	CreatedAt   time.Time `json:"created_at,omitzero"`
	ClearedAt   time.Time `json:"cleared_at,omitzero"`
	// Needs marks a needs-you finding.
	Needs bool `json:"needs,omitempty"`
	// At is when the item last changed.
	At time.Time `json:"at"`
}

// stateWords reads a stored state as words.
func stateWords(s string) string { return strings.ReplaceAll(s, "_", " ") }

// pullRequestWords says where a pull request is.
func pullRequestWords(p store.PullRequest) string {
	switch {
	case p.State == "MERGED":
		return "merged"
	case p.State == "CLOSED":
		return "closed"
	case p.NotReady:
		return "not ready"
	case p.Draft:
		return "draft"
	default:
		return "ready"
	}
}

// checksWords says how the checks on a pull request's head stand.
func checksWords(o *store.ChecksObservation) string {
	if o == nil {
		return "not observed"
	}
	var passed, failed, pending int
	for _, c := range o.Checks {
		switch c.State {
		case "passed":
			passed++
		case "failed":
			failed++
		default:
			pending++
		}
	}
	words := fmt.Sprintf("%s: %d of %d passed", o.Verdict, passed, len(o.Checks))
	if failed > 0 {
		words += fmt.Sprintf(", %d failed", failed)
	}
	if pending > 0 {
		words += fmt.Sprintf(", %d pending", pending)
	}
	return words
}

// validationWords says how Quality's validation of the job stands.
func validationWords(w store.JobWork, head string) string {
	short := head[:min(len(head), 7)]
	var words string
	switch {
	case w.Validating:
		words = "validating"
	case w.Validated:
		words = "accepted at " + short
	case w.LastValidated != "":
		// A later commit made the evidence stale: the head is validated
		// again, and its checks re-run, before it is delivered.
		words = fmt.Sprintf("stale: validated at %s, the head is now %s", w.LastValidated[:min(len(w.LastValidated), 7)], short)
	default:
		words = "not started"
	}
	if w.OpenBlocking > 0 {
		words += fmt.Sprintf(", %d blocking open", w.OpenBlocking)
	}
	return words
}

// dashboardJob reads one job for the Dashboard's job detail. The stage, the
// order of the work and the words are the daemon's; the Dashboard only shows
// them.
func (d *daemon) dashboardJob(number int64) (DashboardJobDetail, error) {
	work, detail, err := d.store.JobWorkDetail(number)
	if err != nil {
		return DashboardJobDetail{}, fmt.Errorf("reading job %d: %w", number, err)
	}
	job := deriveJob(work)
	out := DashboardJobDetail{
		Job: number, Title: job.Title, Stage: job.Stage, Branch: detail.Branch, Head: detail.Head,
		PullRequest: job.PullRequest, CI: "no pull request", Validation: validationWords(work, detail.Head),
		HandoffComplete: slices.ContainsFunc(detail.Handoffs, func(h store.HandoffWork) bool { return h.State == store.HandoffAccepted }),
	}
	if detail.PullRequest != nil {
		out.PullRequestState = pullRequestWords(*detail.PullRequest)
		out.CI = checksWords(detail.Checks)
	}
	out.Work = jobItems(detail, work.Ended)
	return out, nil
}

// jobItems lists a job's work: needs-you findings first, then the remaining
// findings in record order, then assignments and handoffs.
func jobItems(detail store.JobDetail, ended bool) []JobItem {
	items := []JobItem{}
	findings := slices.Clone(detail.Findings)
	slices.SortStableFunc(findings, func(a, b store.FindingWork) int {
		if a.Kind == store.FindingNeedsYou && b.Kind != store.FindingNeedsYou {
			return -1
		}
		if b.Kind == store.FindingNeedsYou && a.Kind != store.FindingNeedsYou {
			return 1
		}
		return 0
	})
	for _, f := range findings {
		label := f.Kind
		if f.Cleared {
			label += " (cleared)"
		}
		items = append(items, JobItem{Kind: ItemFinding, ID: f.ID, Label: label, Text: f.Text, Commit: f.Commit, Assignment: f.Assignment, Report: f.Report, ClearedBy: f.ClearedBy, CreatedAt: f.CreatedAt, ClearedAt: f.ClearedAt, Needs: !ended && f.Kind == store.FindingNeedsYou, At: f.At})
	}
	for _, a := range detail.Assignments {
		items = append(items, JobItem{Kind: ItemAssignment, ID: a.ID, Label: stateWords(a.State), Text: a.Outcome, From: a.Owner, Worker: a.Worker, Criteria: a.Criteria, Dispatch: a.Dispatch, ReadOnly: a.ReadOnly, Commit: a.Commit, Skill: a.Skill, Skills: a.Skills, CreatedAt: a.CreatedAt, At: a.At})
	}
	for _, h := range detail.Handoffs {
		items = append(items, JobItem{Kind: ItemHandoff, ID: h.ID, Label: stateWords(h.State), Text: h.Outcome, From: h.Sender, To: h.Receiver, Criteria: h.Criteria, Answer: h.Answer, Decisions: h.Decisions, Evidence: h.Evidence, Constraints: h.Constraints, Permissions: h.Permissions, Head: h.Head, CreatedAt: h.CreatedAt, At: h.At})
	}
	return items
}
