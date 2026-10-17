// SPDX-License-Identifier: Apache-2.0

package daemon

import (
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
