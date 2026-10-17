// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bufio"
	"context"
	"encoding/json"
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
		if _, err := Call(p.Socket, Request{Command: command}); err != nil {
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

// The Waiting on you set holds native prompts, then held input, then
// needs-you findings; reconciliation and the working agents are statuses
// only.
func TestTheWaitingSetHoldsPromptsHeldInputAndFindingsAndNothingElse(t *testing.T) {
	now := time.Now()
	run := func(name string) store.Agent {
		return store.Agent{Agent: name, Role: roles.Engineering, State: store.AgentRunning, StartedAt: now.Add(-time.Hour)}
	}
	held, prompted, olderPrompt, reconciling, working := run("leader@coordination"), run("worker-1@quality"), run("worker-2@quality"), run("worker-3@engineering"), run("worker-4@engineering")
	lives := map[string]agentLive{
		held.Agent:        {input: InputUser, conversation: "/dev/pts/3"},
		prompted.Agent:    {input: InputAutomation, prompt: &WaitingPrompt{Agent: prompted.Agent, About: "Bash `ls`", Since: now.Add(-2 * time.Minute)}},
		olderPrompt.Agent: {input: InputAutomation, prompt: &WaitingPrompt{Agent: olderPrompt.Agent, About: "Edit", Since: now.Add(-9 * time.Minute)}},
		reconciling.Agent: {input: InputAutomation},
		working.Agent:     {input: InputAutomation},
	}
	works := map[string]store.AgentWork{
		reconciling.Agent: {Reconciling: 7, ReconcilingJob: 3, ReconcilingAt: now},
		working.Agent:     {Dispatch: store.DispatchWorking, Job: 3},
	}
	var agents []DashboardAgent
	for _, a := range []store.Agent{working, reconciling, held, prompted, olderPrompt} {
		agents = append(agents, deriveAgent(a, lives[a.Agent], works[a.Agent]))
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
	if len(pendingDecisions()) != 0 {
		t.Error("a pending decision is listed, but the daemon has no state for one")
	}
}
