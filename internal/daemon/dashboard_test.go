// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/session"
	"github.com/talvor/asmai/internal/store"
)

// Every command in the Dashboard's set is denied with an agent credential,
// whichever agent holds it, and changes no store state for the user who sends
// it. That the set has no CLI subcommand is asserted beside the CLI.
func TestDashboardCommandsAreDeniedToAgentsAndChangeNothing(t *testing.T) {
	coordination := roles.LeaderOf(roles.Coordination)
	engineering := roles.LeaderOf(roles.Engineering)
	worker := roles.Address{Name: "worker-1", Role: roles.Engineering}
	d := &daemon{
		leaders: map[string]*leader{
			coordination.String(): {address: coordination, session: &session.Session{}, generation: 1},
			engineering.String():  {address: engineering, session: &session.Session{}, generation: 1},
			worker.String():       {address: worker, session: &session.Session{}, generation: 1},
		},
		sessions: map[string]sessionRef{
			"coordination": {address: coordination, generation: 1},
			"engineering":  {address: engineering, generation: 1},
			"worker":       {address: worker, generation: 1},
		},
	}
	p := stateDir(t)
	runDaemon(t, p)
	// A job detail needs a job to read.
	openJob(t, p, "fixture", store.JobOpen)
	before := journal(t, p)
	for _, command := range DashboardCommands {
		for _, credential := range []string{"coordination", "engineering", "worker"} {
			if err := d.authorize(Request{Command: command, Session: credential}); err == nil {
				t.Errorf("%s was allowed with the %s credential, want it denied", command, credential)
			}
		}
		if err := d.authorize(Request{Command: command}); err != nil {
			t.Errorf("%s was denied to the user: %v", command, err)
		}
		// A read of one entry needs an entry to read; the daemon wrote the
		// first into the journal when it started.
		if _, err := Call(p.Socket, Request{Command: command, Entry: 1, Job: 1}); err != nil {
			t.Errorf("%s: %v", command, err)
		}
	}
	if after := journal(t, p); !slices.Equal(before, after) {
		t.Errorf("the Dashboard's commands changed the journal:\nbefore %v\nafter  %v", before, after)
	}
}

func TestSnapshotListsEveryStaffedLeaderAsStoppedBeforeTheFactoryRuns(t *testing.T) {
	p := stateDir(t)
	runDaemon(t, p)
	resp, err := Call(p.Socket, Request{Command: CommandDashboardSnapshot})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Dashboard == nil || len(resp.Dashboard.Agents) != len(roles.Staffed) {
		t.Fatalf("snapshot is %+v, want one agent for each of %d staffed roles", resp.Dashboard, len(roles.Staffed))
	}
	for _, a := range resp.Dashboard.Agents {
		if a.Status != StatusStopped {
			t.Errorf("%s is %s, want stopped", a.Agent, a.Status)
		}
	}
}

func TestAnAgentsStatusFollowsTheStatedPrecedence(t *testing.T) {
	now := time.Now()
	running := store.Agent{Agent: "worker-1@engineering", Role: roles.Engineering, State: store.AgentRunning, StartedAt: now.Add(-time.Hour)}
	stopped := running
	stopped.State, stopped.EndedAt, stopped.Exit = store.AgentStopped, now.Add(-time.Minute), "exit 1"
	prompt := &WaitingPrompt{Agent: running.Agent, About: "Bash `ls`", Since: now.Add(-2 * time.Minute)}
	reconciling := store.AgentWork{Reconciling: 7, ReconcilingJob: 3, ReconcilingAt: now.Add(-3 * time.Minute), Dispatch: store.DispatchUnknown, Job: 3}
	working := store.AgentWork{Dispatch: store.DispatchWorking, At: now.Add(-4 * time.Minute), Job: 3, Kind: store.MessageAssignment, Assignment: 7, Outcome: "Add the thing\nmore"}

	for _, tt := range []struct {
		name    string
		agent   store.Agent
		live    agentLive
		work    store.AgentWork
		want    AgentStatus
		unknown bool
		job     int64
		doing   string
		input   string
	}{
		{"stopped beats everything", stopped, agentLive{input: InputUser, prompt: prompt}, merge(working, reconciling), StatusStopped, false, 0, "session ended: exit 1", ""},
		{"a prompt beats reconciliation and work", running, agentLive{input: InputAutomation, prompt: prompt}, merge(working, reconciling), StatusWaitingOnYou, false, 0, "native prompt: Bash `ls`", InputAutomation},
		{"the user's input waits on them", running, agentLive{input: InputUser, conversation: "/dev/pts/3"}, store.AgentWork{}, StatusWaitingOnYou, false, 0, "you hold its input in /dev/pts/3", InputUser},
		{"reconciliation beats working", running, agentLive{input: InputAutomation}, merge(working, reconciling), StatusReconciliation, false, 3, "assignment 7: its dispatch's outcome is not known", InputAutomation},
		{"a working dispatch is working", running, agentLive{input: InputAutomation}, working, StatusWorking, false, 3, "assignment 7: Add the thing", InputAutomation},
		{"no dispatch is idle", running, agentLive{input: InputAutomation}, store.AgentWork{}, StatusIdle, false, 0, "", InputAutomation},
		{"a stopped dispatch is idle", running, agentLive{input: InputAutomation}, store.AgentWork{Dispatch: store.DispatchStopped, Job: 3}, StatusIdle, false, 0, "", InputAutomation},
		{"a delivered dispatch is idle", running, agentLive{input: InputAutomation}, store.AgentWork{Dispatch: store.DispatchDelivered, Job: 3}, StatusIdle, false, 0, "", InputAutomation},
		{"an unknown dispatch is idle, marked", running, agentLive{input: InputAutomation}, store.AgentWork{Dispatch: store.DispatchUnknown}, StatusIdle, true, 0, "dispatch state unknown", InputAutomation},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := deriveAgent(tt.agent, tt.live, tt.work)
			if got.Status != tt.want || got.DispatchUnknown != tt.unknown || got.Job != tt.job || got.Doing != tt.doing || got.Input != tt.input {
				t.Errorf("derived %+v, want status %s unknown %v job %d doing %q input %q", got, tt.want, tt.unknown, tt.job, tt.doing, tt.input)
			}
		})
	}
	if got := deriveAgent(running, agentLive{prompt: prompt}, working).Since; !got.Equal(prompt.Since) {
		t.Errorf("waiting since %v, want the prompt's %v", got, prompt.Since)
	}
	if got := deriveAgent(stopped, agentLive{}, working).Since; !got.Equal(stopped.EndedAt) {
		t.Errorf("stopped since %v, want when it ended %v", got, stopped.EndedAt)
	}
}

func TestEveryStatusHasItsOwnWord(t *testing.T) {
	seen := map[string]AgentStatus{}
	for _, s := range []AgentStatus{StatusStopped, StatusWaitingOnYou, StatusReconciliation, StatusWorking, StatusIdle} {
		word := s.Word()
		if other, dup := seen[word]; dup || strings.TrimSpace(word) == "" {
			t.Errorf("%s reads %q, which %s already does", s, word, other)
		}
		seen[word] = s
	}
}

func merge(a, b store.AgentWork) store.AgentWork {
	a.Reconciling, a.ReconcilingJob, a.ReconcilingAt, a.Dispatch = b.Reconciling, b.ReconcilingJob, b.ReconcilingAt, b.Dispatch
	return a
}

// The change subscription ticks at once, and again when the journal grows,
// when a native prompt appears or goes, and when an input owner changes; it
// is quiet while nothing does.
func TestSubscriptionTicksOnJournalPromptAndInputOwnerChanges(t *testing.T) {
	p := stateDir(t)
	if err := os.MkdirAll(filepath.Dir(p.Store), 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(p.Store)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	worker := roles.Address{Name: "worker-1", Role: roles.Engineering}
	l := &leader{address: worker, session: &session.Session{}, generation: 1}
	work, stop := context.WithCancel(context.Background())
	defer stop()
	d := &daemon{
		work:    work,
		store:   st,
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		leaders: map[string]*leader{worker.String(): l},
	}

	dir, err := os.MkdirTemp("", "sub")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, "s"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	served := make(chan struct{})
	go func() {
		defer close(served)
		conn, err := ln.AcceptUnix()
		if err != nil {
			return
		}
		defer conn.Close()
		d.subscribe(conn, bufio.NewReader(conn))
	}()
	conn, err := net.Dial("unix", filepath.Join(dir, "s"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	r := bufio.NewReader(conn)

	tick := func(within time.Duration) (int64, bool) {
		conn.SetReadDeadline(time.Now().Add(within))
		line, err := r.ReadBytes('\n')
		if err != nil {
			return 0, false
		}
		var resp Response
		if err := json.Unmarshal(line, &resp); err != nil || resp.Change == nil {
			t.Fatalf("the daemon sent %q, want a tick", line)
		}
		return resp.Change.Journal, true
	}
	if head, ok := tick(5 * time.Second); !ok || head != 0 {
		t.Fatalf("first tick is %d (%v), want one at once with the empty journal's head 0", head, ok)
	}
	if head, ok := tick(500 * time.Millisecond); ok {
		t.Fatalf("a tick %d came with nothing changed", head)
	}

	if err := st.ConversationLeft(worker.String(), worker.Role, 1, "/dev/pts/1", "left", InputAutomation, time.Now()); err != nil {
		t.Fatal(err)
	}
	want, err := st.JournalHead()
	if err != nil || want == 0 {
		t.Fatalf("journal head %d (%v), want it to have grown", want, err)
	}
	if head, ok := tick(5 * time.Second); !ok || head != want {
		t.Errorf("tick after a journal entry is %d (%v), want the head %d", head, ok, want)
	}

	for _, change := range []struct {
		name string
		do   func()
	}{
		{"a waiting prompt appearing", func() {
			l.prompt = &WaitingPrompt{Agent: worker.String(), Generation: 1, About: "Bash `ls`", Since: time.Now()}
		}},
		{"a waiting prompt going", func() { l.prompt = nil }},
		{"the user taking the input", func() { l.userInput = true }},
		{"the input going to automation", func() { l.userInput = false }},
	} {
		d.mu.Lock()
		change.do()
		d.mu.Unlock()
		if head, ok := tick(5 * time.Second); !ok || head != want {
			t.Errorf("no tick for %s: got %d (%v), want one with head %d", change.name, head, ok, want)
		}
	}

	conn.Close()
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Error("the subscription outlived its client")
	}
}

func TestAJobsStageFollowsTheStatedPrecedence(t *testing.T) {
	counts := map[string]int{store.AssignmentActive: 2, store.AssignmentAccepted: 3, store.AssignmentNeedsReconciliation: 1}
	for _, tt := range []struct {
		name string
		work store.JobWork
		want JobStage
	}{
		{"delivered beats everything", store.JobWork{Ended: true, NeedsYou: 1, Validating: true, PullRequest: 4}, StageDelivered},
		{"a needs-you finding beats validation and delivery", store.JobWork{NeedsYou: 1, Validating: true, PullRequest: 4}, StageNeedsYou},
		{"a live validation is validating", store.JobWork{Validating: true, Validated: true}, StageValidating},
		{"a current pull request is delivering", store.JobWork{PullRequest: 4, PRCurrent: true}, StageDelivering},
		{"a push is delivering", store.JobWork{Pushed: true}, StageDelivering},
		{"an accepted validation with nothing blocking is delivering", store.JobWork{Validated: true}, StageDelivering},
		{"writing rework beats old delivery", store.JobWork{Writing: true, PRCurrent: true, Pushed: true, Validated: true}, StageWorking},
		{"an old pull request is working", store.JobWork{PullRequest: 4}, StageWorking},
		{"an accepted validation with a blocking finding is working", store.JobWork{Validated: true, OpenBlocking: 1}, StageWorking},
		{"a pushed head with a blocking finding is working", store.JobWork{Pushed: true, OpenBlocking: 1}, StageWorking},
		{"a current pull request with a blocking finding is working", store.JobWork{PullRequest: 4, PRCurrent: true, OpenBlocking: 1}, StageWorking},
		{"all delivery evidence with a blocking finding is working", store.JobWork{Validated: true, Pushed: true, PRCurrent: true, OpenBlocking: 1}, StageWorking},
		{"no validation or pull request is working", store.JobWork{}, StageWorking},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tt.work.Number, tt.work.Assignments = 7, counts
			got := deriveJob(tt.work)
			if got.Stage != tt.want || got.Job != 7 {
				t.Errorf("derived stage %s, want %s", got.Stage, tt.want)
			}
			if got.Assignments != (AssignmentCounts{Active: 2, Accepted: 3, Reconciliation: 1}) {
				t.Errorf("counted %+v", got.Assignments)
			}
		})
	}
	if got := deriveJob(store.JobWork{Number: 1, Words: "Add the thing\nin detail", Title: ""}).Title; got != "Add the thing" {
		t.Errorf("a job with no pull request is titled %q, want the first line of its words", got)
	}
	if got := deriveJob(store.JobWork{Number: 1, Words: "Add the thing", PullRequest: 9, Title: "feat: add the thing"}); got.Title != "feat: add the thing" || got.PullRequest != 9 {
		t.Errorf("a job with a pull request is %+v, want its number and title", got)
	}
}

func TestEveryStageHasItsOwnWord(t *testing.T) {
	seen := map[string]JobStage{}
	for _, s := range []JobStage{StageWorking, StageValidating, StageDelivering, StageNeedsYou, StageDelivered} {
		word := s.Word()
		if other, dup := seen[word]; dup || strings.TrimSpace(word) == "" {
			t.Errorf("%s reads %q, which %s already does", s, word, other)
		}
		seen[word] = s
	}
}

// The Waiting on you set holds native prompts, then held input, then
// needs-you findings; reconciliation and the working agents are statuses
// only.
func TestTheWaitingSetHoldsPromptsHeldInputAndFindingsAndNothingElse(t *testing.T) {
	now := time.Now()
	run := func(address roles.Address) store.Agent {
		return store.Agent{Agent: address.String(), Role: address.Role, State: store.AgentRunning, StartedAt: now.Add(-time.Hour)}
	}
	held, prompted, olderPrompt := run(roles.LeaderOf(roles.Coordination)), run(roles.LeaderOf(roles.Quality)), run(roles.LeaderOf(roles.Engineering))
	workerPrompt, reconciling, working := run(roles.WorkerOf(roles.Engineering, 1)), run(roles.WorkerOf(roles.Engineering, 3)), run(roles.WorkerOf(roles.Engineering, 4))
	lives := map[string]agentLive{
		held.Agent:         {input: InputUser, conversation: "/dev/pts/3"},
		prompted.Agent:     {input: InputAutomation, prompt: &WaitingPrompt{Agent: prompted.Agent, About: "Bash `ls`", Since: now.Add(-2 * time.Minute)}},
		olderPrompt.Agent:  {input: InputAutomation, prompt: &WaitingPrompt{Agent: olderPrompt.Agent, About: "Edit", Since: now.Add(-9 * time.Minute)}},
		workerPrompt.Agent: {input: InputAutomation, prompt: &WaitingPrompt{Agent: workerPrompt.Agent, About: "Bash `pwd`", Since: now.Add(-12 * time.Minute)}},
		reconciling.Agent:  {input: InputAutomation},
		working.Agent:      {input: InputAutomation},
	}
	works := map[string]store.AgentWork{
		reconciling.Agent: {Reconciling: 7, ReconcilingJob: 3, ReconcilingAt: now},
		working.Agent:     {Dispatch: store.DispatchWorking, Job: 3},
	}
	var agents []DashboardAgent
	for _, a := range []store.Agent{working, reconciling, held, prompted, olderPrompt, workerPrompt} {
		agents = append(agents, deriveAgent(a, lives[a.Agent], works[a.Agent]))
	}
	if agents[len(agents)-1].Status != StatusWaitingOnYou {
		t.Fatal("the worker's prompt is missing from its agent status")
	}
	findings := []store.NeedsYouFinding{{ID: 7, Job: 3, Text: "greet whom?\nmore", At: now.Add(-time.Hour)}}

	got := waitingOnYou(agents, lives, findings)
	var kinds, who []string
	for _, item := range got {
		kinds = append(kinds, item.Kind)
		who = append(who, item.Agent)
	}
	if want := []string{WaitingNativePrompt, WaitingNativePrompt, WaitingInput, WaitingFinding}; !slices.Equal(kinds, want) {
		t.Fatalf("the set is %v, want %v: %+v", kinds, want, got)
	}
	if want := []string{olderPrompt.Agent, prompted.Agent, held.Agent, ""}; !slices.Equal(who, want) {
		t.Errorf("the set is in the order of %v, want %v (oldest prompt first)", who, want)
	}
	if got[3].Finding != 7 || got[3].Job != 3 || got[3].Text != "job 3 finding 7: greet whom?" {
		t.Errorf("the finding is %+v", got[3])
	}
	if got[2].Text != "leader@coordination you hold its input in /dev/pts/3" {
		t.Errorf("the held input reads %q", got[2].Text)
	}
	if empty := waitingOnYou(nil, nil, nil); empty == nil || len(empty) != 0 {
		t.Errorf("nothing waiting is %#v, want an empty set", empty)
	}
}

// A log read pages the journal from a cursor in either direction, and an
// entry read returns the body of the message the entry caused.
func TestLogReadPagesAndEntryReadReturnsTheBody(t *testing.T) {
	p := stateDir(t)
	if err := os.MkdirAll(filepath.Dir(p.Store), 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(p.Store)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	at := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	if err := st.RepositoryAdded(store.Repository{Name: "fixture", Origin: "https://example.test/fixture", DefaultBranch: "main", Clone: "/fixture", AddedAt: at}); err != nil {
		t.Fatal(err)
	}
	d := &daemon{store: st}
	var sent []int64
	for i := range 5 {
		openJob(t, p, "fixture", store.JobOpen)
		h := store.Handoff{Job: int64(i + 1), Sender: "leader@coordination", Receiver: "leader@engineering", Outcome: "outcome\nmore", Decisions: "none", Evidence: "none", Constraints: "none", Permissions: "none", Criteria: []string{"c"}}
		if _, _, err := st.SendHandoff(h, at.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	page, err := d.dashboardLog(LogQuery{Kinds: TrafficKinds, Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 3 || !page.Older || page.Newer {
		t.Fatalf("latest page has %d entries older %v newer %v, want 3, true, false", len(page.Entries), page.Older, page.Newer)
	}
	for _, e := range page.Entries {
		sent = append(sent, e.ID)
		if e.Kind != store.KindHandoffSent || e.From != "leader@coordination" || e.To != "leader@engineering" || e.Summary != "handoff: outcome" {
			t.Errorf("entry %+v, want a handoff from Coordination to Engineering summarised by its outcome's first line", e)
		}
	}
	older, err := d.dashboardLog(LogQuery{Kinds: TrafficKinds, Before: sent[0], Limit: 3})
	if err != nil || len(older.Entries) != 2 || older.Older || !older.Newer {
		t.Fatalf("older page is %+v (%v), want 2 entries with none older and some newer", older, err)
	}
	newer, err := d.dashboardLog(LogQuery{Kinds: TrafficKinds, After: older.Entries[1].ID, Limit: 10})
	if err != nil || len(newer.Entries) != 3 || newer.Entries[0].ID != sent[0] {
		t.Fatalf("newer page is %+v (%v), want the three latest", newer, err)
	}
	job, err := d.dashboardLog(LogQuery{Job: 2})
	if err != nil || len(job.Entries) == 0 {
		t.Fatalf("job page is %+v (%v), want entries", job, err)
	}
	for _, e := range job.Entries {
		if e.Job != 2 {
			t.Errorf("entry %+v is on job %d, want job 2 only", e, e.Job)
		}
	}
	entry, err := d.dashboardEntry(sent[0])
	if err != nil || entry.Body != "outcome\nmore" || entry.Kind != store.KindHandoffSent {
		t.Errorf("entry is %+v (%v), want the handoff with its outcome as the body", entry, err)
	}
	encoded, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["data"]; ok {
		t.Errorf("entry detail exposes raw journal data: %s", encoded)
	}
	if _, err := d.dashboardEntry(1 << 40); err == nil {
		t.Error("an entry that does not exist was read")
	}
}

func TestJobOpeningSummaryUsesItsNumber(t *testing.T) {
	d := &daemon{}
	e := d.logEntry(store.Entry{Kind: store.KindJobOpened, Data: []byte(`{"number":7}`)})
	if e.Job != 7 {
		t.Errorf("job opening belongs to job %d, want 7", e.Job)
	}
}

// A job's work lists needs-you findings first, then the other findings, the
// assignments and the handoffs, each with the time it last changed.
func TestAJobsWorkListsNeedsYouFindingsFirst(t *testing.T) {
	at := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	detail := store.JobDetail{
		Findings: []store.FindingWork{
			{ID: 1, Assignment: 11, Report: 12, Kind: store.FindingAdvisory, Text: "no doc line", Commit: "old-head", CreatedAt: at, At: at},
			{ID: 2, Kind: store.FindingBlocking, Text: "flaky", Cleared: true, ClearedBy: 13, ClearedAt: at.Add(time.Minute), At: at.Add(time.Minute)},
			{ID: 3, Kind: store.FindingNeedsYou, Text: "widens ADR 0012", At: at.Add(2 * time.Minute)},
			{ID: 4, Kind: store.FindingBlocking, Text: "wrong order", At: at.Add(3 * time.Minute)},
		},
		Assignments: []store.AssignmentWork{{ID: 5, Worker: "worker-1@engineering", Owner: "leader@engineering", State: store.AssignmentNeedsReconciliation, Outcome: "Add it", ReadOnly: true, Commit: "validation-head", Skill: "review", Skills: []string{"review", "check"}, Dispatch: store.DispatchUnknown, CreatedAt: at, At: at.Add(4 * time.Minute)}},
		Handoffs:    []store.HandoffWork{{ID: 6, Sender: "leader@coordination", Receiver: "leader@engineering", State: store.HandoffAccepted, Outcome: "Add a cursor read", Decisions: "Use the journal", Evidence: "The log needs a cursor", Constraints: "Read only", Permissions: "Read the repository", Criteria: []string{"No repeated entries"}, Head: "abc123", Answer: "Accepted", CreatedAt: at, At: at.Add(5 * time.Minute)}},
	}
	items := jobItems(detail, false)
	var got []string
	for _, it := range items {
		got = append(got, fmt.Sprintf("%s %d %s", it.Kind, it.ID, it.Label))
	}
	want := []string{"finding 3 needs-you", "finding 1 advisory", "finding 2 blocking (cleared)", "finding 4 blocking", "assignment 5 needs reconciliation", "handoff 6 accepted"}
	if !slices.Equal(got, want) {
		t.Errorf("the work is %q, want %q", got, want)
	}
	if !items[0].Needs || items[1].Needs {
		t.Errorf("only the needs-you finding is marked: %+v", items[:2])
	}
	if !items[0].At.Equal(at.Add(2*time.Minute)) || !items[4].At.Equal(at.Add(4*time.Minute)) || !items[5].At.Equal(at.Add(5*time.Minute)) {
		t.Errorf("the items lost the time they last changed: %+v", items)
	}
	if items[4].Worker != "worker-1@engineering" || items[5].From != "leader@coordination" || items[5].To != "leader@engineering" {
		t.Errorf("the parties are wrong: %+v", items[4:])
	}
	if f := items[1]; f.Assignment != 11 || f.Report != 12 || f.Commit != "old-head" || !f.CreatedAt.Equal(at) {
		t.Errorf("the finding lost its stored record: %+v", f)
	}
	if f := items[2]; f.ClearedBy != 13 || !f.ClearedAt.Equal(at.Add(time.Minute)) {
		t.Errorf("the cleared finding lost its clearing record: %+v", f)
	}
	if a := items[4]; !a.ReadOnly || a.Commit != "validation-head" || a.Skill != "review" || !slices.Equal(a.Skills, []string{"review", "check"}) || !a.CreatedAt.Equal(at) {
		t.Errorf("the assignment lost its stored record: %+v", a)
	}
	if h := items[5]; h.Decisions != "Use the journal" || h.Evidence != "The log needs a cursor" || h.Constraints != "Read only" || h.Permissions != "Read the repository" || h.Head != "abc123" || h.Answer != "Accepted" || !slices.Equal(h.Criteria, []string{"No repeated entries"}) || !h.CreatedAt.Equal(at) {
		t.Errorf("the handoff lost its stored record: %+v", h)
	}
	if delivered := jobItems(detail, true); delivered[0].Needs || delivered[0].Label != store.FindingNeedsYou {
		t.Errorf("a delivered job still calls its historic finding active: %+v", delivered[0])
	}
}

func TestJobDetailReportsHandoffCompletionFromAcceptedHandoffs(t *testing.T) {
	p := stateDir(t)
	if err := os.MkdirAll(filepath.Dir(p.Store), 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(p.Store)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	at := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	if err := st.RepositoryAdded(store.Repository{Name: "fixture", Origin: "https://example.test/fixture", DefaultBranch: "main", Clone: "/fixture", AddedAt: at}); err != nil {
		t.Fatal(err)
	}
	openJob(t, p, "fixture", store.JobOpen)
	d := &daemon{store: st}
	read := func() DashboardJobDetail {
		t.Helper()
		got, err := d.dashboardJob(1)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := read(); got.Stage != StageWorking || got.HandoffComplete {
		t.Errorf("the unhanded job is %+v", got)
	}
	h, inbound, err := st.SendHandoff(store.Handoff{Job: 1, Sender: "leader@coordination", Receiver: "leader@engineering", Outcome: "Build it", Decisions: "none", Evidence: "none", Constraints: "none", Permissions: "none", Criteria: []string{"done"}}, at)
	if err != nil {
		t.Fatal(err)
	}
	if got := read(); got.HandoffComplete {
		t.Errorf("a pending handoff completed the stage: %+v", got)
	}
	if _, err := st.Inbox(h.Receiver, inbound.ID, at); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.AnswerHandoff(h.ID, h.Receiver, inbound.ID, 0, store.HandoffAccepted, "Accepted", at); err != nil {
		t.Fatal(err)
	}
	if got := read(); !got.HandoffComplete {
		t.Errorf("the accepted handoff left the stage incomplete: %+v", got)
	}
}

func TestAJobHeaderSaysHowItsPullRequestChecksAndValidationStand(t *testing.T) {
	for state, want := range map[string]store.PullRequest{
		"not ready": {State: "OPEN", NotReady: true, Draft: true},
		"draft":     {State: "OPEN", Draft: true},
		"ready":     {State: "OPEN"},
		"merged":    {State: "MERGED"},
		"closed":    {State: "CLOSED"},
	} {
		if got := pullRequestWords(want); got != state {
			t.Errorf("%+v reads %q, want %q", want, got, state)
		}
	}
	checks := &store.ChecksObservation{Verdict: "waiting", Checks: []store.CheckState{{State: "passed"}, {State: "passed"}, {State: "pending"}, {State: "failed"}}}
	if got, want := checksWords(checks), "waiting: 2 of 4 passed, 1 failed, 1 pending"; got != want {
		t.Errorf("checks read %q, want %q", got, want)
	}
	if got := checksWords(nil); got != "not observed" {
		t.Errorf("no observation reads %q", got)
	}
	for _, tt := range []struct {
		work store.JobWork
		want string
	}{
		{store.JobWork{}, "not started"},
		{store.JobWork{Validating: true, Validated: true}, "validating"},
		{store.JobWork{Validated: true, OpenBlocking: 2}, "accepted at 4f1c9a2, 2 blocking open"},
	} {
		if got := validationWords(tt.work, "4f1c9a2b0c"); got != tt.want {
			t.Errorf("%+v reads %q, want %q", tt.work, got, tt.want)
		}
	}
}
