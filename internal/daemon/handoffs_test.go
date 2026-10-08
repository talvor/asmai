// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/providers"
	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/session"
	"github.com/talvor/asmai/internal/store"
)

func addPendingDispatch(t *testing.T, s *store.Store, agent, state string, generation int) int64 {
	t.Helper()
	at := time.Now()
	const terminal = "/dev/pts/test"
	if err := s.ConversationEntered("leader@coordination", roles.Coordination, 1, terminal, at); err != nil {
		t.Fatal(err)
	}
	_, witness, err := s.ObservedWitnessed(store.Witnessed{Agent: "leader@coordination", Role: roles.Coordination, Generation: 1, Terminal: terminal, Text: "open a test job"}, "UserPromptSubmit", json.RawMessage(`{"prompt":"open a test job"}`), at)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RepositoryAdded(store.Repository{Name: "fixture", Origin: "https://example.test/fixture", DefaultBranch: "main", Clone: "/fixture", AddedAt: at}); err != nil {
		t.Fatal(err)
	}
	job, err := s.JobOpened(store.Job{Repository: "fixture", Witness: witness, Reading: "test", Mandate: store.MandateTestedPR, Criteria: []string{"complete"}}, false, at)
	if err != nil {
		t.Fatal(err)
	}
	_, dispatch, err := s.SendHandoff(store.Handoff{Job: job.Number, Sender: "leader@coordination", Receiver: agent, Outcome: "outcome", Decisions: "none", Evidence: "none", Constraints: "none", Permissions: "none", Criteria: []string{"complete"}}, at)
	if err != nil {
		t.Fatal(err)
	}
	if dispatch.State != store.DispatchCreated {
		t.Fatalf("new dispatch is %s, want created", dispatch.State)
	}
	if state != store.DispatchCreated {
		if err := s.ChangeDispatch(dispatch.ID, generation, store.DispatchCreated, state, "", at); err != nil {
			t.Fatal(err)
		}
	}
	return dispatch.ID
}

func TestLeavingCoordinationConversationNudgesPendingReply(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	const agent = "leader@coordination"
	at := time.Now()
	if _, err := s.SessionStarted(store.SessionStart{Agent: agent, Role: roles.Coordination, Provider: "claude-code", Version: "v", Model: "m", Executable: "/bin/sh", Dir: t.TempDir(), PID: 1, At: at}); err != nil {
		t.Fatal(err)
	}
	const script = `IFS= read -r line; printf '%s' "$line" > "$ASMAI_CAPTURE"; sleep 60`
	capture := filepath.Join(t.TempDir(), "input")
	sess, err := session.Start(session.Spec{Path: "/bin/sh", Args: []string{"-c", script}, Env: append(os.Environ(), "ASMAI_CAPTURE="+capture), Dir: t.TempDir(), Columns: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Stop(100 * time.Millisecond) })
	dispatch := addPendingDispatch(t, s, agent, store.DispatchCreated, 1)
	l := &leader{address: roles.LeaderOf(roles.Coordination), session: sess, generation: 1, ready: true, userInput: true}
	c := &conversation{leader: l, session: sess, generation: 1, terminal: "/dev/pts/test"}
	d := &daemon{leaders: map[string]*leader{agent: l}, store: s, log: slog.New(slog.NewTextHandler(io.Discard, nil)), conversation: c}
	d.leave(c, LeftByDetaching)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got, err := os.ReadFile(capture); err == nil {
			if want := fmt.Sprintf("asmai inbox --dispatch %d", dispatch); string(got) != want {
				t.Fatalf("the departed conversation typed %q, want %q", got, want)
			}
			if _, err := s.NextDispatch(agent); err == nil {
				t.Fatal("the unacknowledged dispatch became eligible for another nudge")
			}
			pending, err := s.PendingLeaders()
			if err != nil || len(pending) != 1 || pending[0] != agent {
				t.Fatalf("the unacknowledged dispatch recovery lists %v (%v), want %s", pending, err, agent)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("leaving the conversation did not type the pending reply nudge")
}

func TestExitedRecipientRestartsForPendingDispatch(t *testing.T) {
	root := t.TempDir()
	configFile := filepath.Join(root, "config.toml")
	var config strings.Builder
	for _, role := range []string{roles.Coordination, roles.Engineering, roles.Quality} {
		config.WriteString("[roles." + role + "]\nleader_provider = \"claude\"\nleader_model = \"opus\"\nworker_provider = \"claude\"\nworker_model = \"opus\"\n")
	}
	if err := os.WriteFile(configFile, []byte(config.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	paths := stateDir(t)
	if err := os.MkdirAll(filepath.Dir(filepath.Join(paths.Providers, "claude-code", "v", "claude")), 0o700); err != nil {
		t.Fatal(err)
	}
	provider := filepath.Join(paths.Providers, "claude-code", "v", "claude")
	if err := os.WriteFile(provider, []byte("#!/bin/sh\nexec sleep 60\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(paths.Store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	install := store.ProviderInstall{Name: providers.ClaudeCode, Version: "v", Path: provider, SHA256: strings.Repeat("a", 64), InstalledAt: time.Now()}
	if _, err := s.ProviderInstalled(install); err != nil {
		t.Fatal(err)
	}
	const agent = "leader@engineering"
	d := &daemon{cfg: Config{Paths: paths, ConfigFile: configFile, Pins: providers.Pins{ClaudeCode: providers.Pin{Version: "v"}}}, leaders: map[string]*leader{}, sessions: map[string]sessionRef{}, store: s, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	l := d.leader(roles.Engineering)
	t.Cleanup(func() {
		d.mu.Lock()
		d.stopping = true
		current := l.session
		d.mu.Unlock()
		if current != nil {
			current.Stop(100 * time.Millisecond)
		}
		d.agents.Wait()
	})
	d.mu.Lock()
	if err := d.ensureRoleLeader(roles.Engineering); err != nil {
		d.mu.Unlock()
		t.Fatal(err)
	}
	d.mu.Unlock()
	dispatchID := addPendingDispatch(t, s, agent, store.DispatchUnknown, 1)
	if err := s.ObservedAutomated(store.Dispatch{ID: dispatchID, Agent: agent, Generation: 1}, roles.Engineering, json.RawMessage(`{"prompt":"asmai inbox --dispatch 1"}`), "transcript", time.Now()); err != nil {
		t.Fatal(err)
	}
	if messages, err := s.Inbox(agent, dispatchID, time.Now()); err != nil || len(messages) != 1 || !messages[0].Fetched {
		t.Fatalf("fetching the pending handoff returned %+v (%v)", messages, err)
	}
	old := l.session
	old.Stop(100 * time.Millisecond)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		d.mu.Lock()
		generation, current := l.generation, l.session
		d.mu.Unlock()
		if generation == 2 && current != nil {
			dispatch, err := s.NextDispatch(agent)
			if err != nil {
				t.Fatal(err)
			}
			if dispatch.ID != dispatchID || dispatch.Generation != 2 || dispatch.State != store.DispatchDelivered {
				t.Fatalf("the recovered dispatch is %+v, want dispatch %d in generation 2 delivered", dispatch, dispatchID)
			}
			d.mu.Lock()
			l.ready = true
			d.mu.Unlock()
			d.nudge(agent)
			ref := sessionRef{address: l.address, generation: 2}
			prompt := fmt.Sprintf("asmai inbox --dispatch %d", dispatchID)
			if matched, err := d.automatedSubmitted(ref, prompt, "recovery-prompt", "transcript", json.RawMessage(`{"prompt":"asmai inbox --dispatch 1"}`)); err != nil || !matched {
				t.Fatalf("acknowledging the recovery nudge matched=%v, err=%v", matched, err)
			}
			messages, err := s.Inbox(agent, dispatchID, time.Now())
			if err != nil || len(messages) != 1 || messages[0].State != store.DispatchDelivered {
				t.Fatalf("re-fetching the handoff returned %+v (%v), want delivered", messages, err)
			}
			h, err := s.Handoff(messages[0].Handoff)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.AnswerHandoff(h.ID, agent, l.currentDispatch, 2, store.HandoffAccepted, "accepted", time.Now()); err != nil {
				t.Fatalf("answering the recovered handoff with its current dispatch failed: %v", err)
			}
			if err := d.stopCurrentDispatch(ref, "recovery-prompt", "transcript"); err != nil {
				t.Fatalf("stopping the recovered handoff dispatch: %v", err)
			}
			d.mu.Lock()
			d.stopping = true
			d.mu.Unlock()
			current.Stop(100 * time.Millisecond)
			d.agents.Wait()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the exited recipient was not restarted for its pending dispatch")
}

func TestOnlyTheCurrentFetchedReplyStartsWorking(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	at := time.Now()
	for _, start := range []store.SessionStart{
		{Agent: "leader@coordination", Role: roles.Coordination, Provider: "claude-code", Version: "v", Model: "m", Executable: "/p", Dir: "/c", PID: 1, At: at},
		{Agent: "leader@engineering", Role: roles.Engineering, Provider: "claude-code", Version: "v", Model: "m", Executable: "/p", Dir: "/e", PID: 2, At: at},
	} {
		if _, err := s.SessionStarted(start); err != nil {
			t.Fatal(err)
		}
	}
	inbound := addPendingDispatch(t, s, "leader@engineering", store.DispatchCreated, 1)
	if err := s.ChangeDispatch(inbound, 1, store.DispatchCreated, store.DispatchUnknown, "transcript", at); err != nil {
		t.Fatal(err)
	}
	incoming := store.Dispatch{ID: inbound, Agent: "leader@engineering", Generation: 1}
	if err := s.ObservedAutomated(incoming, roles.Engineering, json.RawMessage(`{"prompt":"asmai inbox --dispatch 1"}`), "transcript", at); err != nil {
		t.Fatal(err)
	}
	messages, err := s.Inbox("leader@engineering", inbound, at)
	if err != nil {
		t.Fatal(err)
	}
	h, err := s.Handoff(messages[0].Handoff)
	if err != nil {
		t.Fatal(err)
	}
	_, reply, err := s.AnswerHandoff(h.ID, "leader@engineering", inbound, 1, store.HandoffAccepted, "accepted", at)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ChangeDispatch(reply.ID, 1, store.DispatchCreated, store.DispatchUnknown, "transcript", at); err != nil {
		t.Fatal(err)
	}
	reply.Generation = 1
	if err := s.ObservedAutomated(reply, roles.Coordination, json.RawMessage(`{"prompt":"asmai inbox --dispatch 2"}`), "transcript", at); err != nil {
		t.Fatal(err)
	}
	messages, err = s.Inbox("leader@coordination", reply.ID, at)
	if err != nil || len(messages) != 1 || messages[0].State != store.DispatchDelivered {
		t.Fatalf("fetching the reply returned %+v (%v), want delivered", messages, err)
	}
	d := &daemon{store: s}
	l := &leader{currentDispatch: inbound}
	ref := sessionRef{address: roles.LeaderOf(roles.Coordination), generation: 1}
	if err := d.workFetchedReply(l, ref, &messages[0]); err != nil {
		t.Fatal(err)
	}
	if messages[0].State != store.DispatchDelivered {
		t.Fatalf("a different dispatch moved the reply to %s, want delivered", messages[0].State)
	}
	l.currentDispatch = reply.ID
	if err := d.workFetchedReply(l, ref, &messages[0]); err != nil {
		t.Fatal(err)
	}
	if messages[0].State != store.DispatchWorking {
		t.Fatalf("the current reply moved to %s, want working", messages[0].State)
	}
	messages, err = s.Inbox("leader@coordination", reply.ID, at)
	if err != nil || len(messages) != 1 || messages[0].State != store.DispatchWorking {
		t.Fatalf("re-reading the reply returned %+v (%v), want the same working dispatch", messages, err)
	}
	if err := d.workFetchedReply(l, ref, &messages[0]); err != nil {
		t.Fatalf("re-reading the current reply was not idempotent: %v", err)
	}
	if stopped, err := s.StopDispatchIfWorking(reply.ID, ref.generation, "transcript", at); err != nil || !stopped {
		t.Fatalf("stopping the working reply returned stopped=%v, err=%v", stopped, err)
	}
	messages, err = s.Inbox("leader@coordination", reply.ID, at)
	if err != nil || len(messages) != 1 || messages[0].State != store.DispatchStopped {
		t.Fatalf("the correlated Stop left the reply %+v (%v), want stopped", messages, err)
	}
}

// A handoff the recipient has fetched but not answered stays eligible for
// another nudge. It must not starve a later handoff to the same leader: the
// qualification run sends two handoffs in one session and needs the second
// acknowledged.
func TestAnsweredNudgeDoesNotStarveLaterDispatch(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	const agent = "leader@engineering"
	at := time.Now()
	if _, err := s.SessionStarted(store.SessionStart{Agent: agent, Role: roles.Engineering, Provider: "claude-code", Version: "v", Model: "m", Executable: "/bin/sh", Dir: t.TempDir(), PID: 1, At: at}); err != nil {
		t.Fatal(err)
	}
	sess, err := session.Start(session.Spec{Path: "/bin/sh", Args: []string{"-c", "sleep 60"}, Env: os.Environ(), Dir: t.TempDir(), Columns: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Stop(100 * time.Millisecond) })
	first := addPendingDispatch(t, s, agent, store.DispatchCreated, 1)
	l := &leader{address: roles.LeaderOf(roles.Engineering), session: sess, generation: 1, ready: true}
	d := &daemon{leaders: map[string]*leader{agent: l}, store: s, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ref := sessionRef{address: l.address, generation: 1}
	deliver := func(dispatch int64, promptID string) {
		t.Helper()
		d.nudge(agent)
		prompt := fmt.Sprintf("asmai inbox --dispatch %d", dispatch)
		d.mu.Lock()
		pending := l.pendingPrompt
		d.mu.Unlock()
		if pending != prompt {
			t.Fatalf("the daemon nudged %q, want %q", pending, prompt)
		}
		if matched, err := d.automatedSubmitted(ref, prompt, promptID, "transcript", json.RawMessage(`{"prompt":"`+prompt+`"}`)); err != nil || !matched {
			t.Fatalf("acknowledging %q matched=%v, err=%v", prompt, matched, err)
		}
		if messages, err := s.Inbox(agent, dispatch, time.Now()); err != nil || len(messages) != 1 || !messages[0].Fetched {
			t.Fatalf("fetching dispatch %d returned %+v (%v)", dispatch, messages, err)
		}
		if err := d.stopCurrentDispatch(ref, promptID, "transcript"); err != nil {
			t.Fatal(err)
		}
		d.mu.Lock()
		l.ready = true
		d.mu.Unlock()
	}
	deliver(first, "prompt-1")

	_, witness, err := s.ObservedWitnessed(store.Witnessed{Agent: "leader@coordination", Role: roles.Coordination, Generation: 1, Terminal: "/dev/pts/test", Text: "open a second job"}, "UserPromptSubmit", json.RawMessage(`{"prompt":"open a second job"}`), at)
	if err != nil {
		t.Fatal(err)
	}
	job, err := s.JobOpened(store.Job{Repository: "fixture", Witness: witness, Reading: "second", Mandate: store.MandateTestedPR, Criteria: []string{"complete"}}, false, at)
	if err != nil {
		t.Fatal(err)
	}
	_, later, err := s.SendHandoff(store.Handoff{Job: job.Number, Sender: "leader@coordination", Receiver: agent, Outcome: "outcome", Decisions: "none", Evidence: "none", Constraints: "none", Permissions: "none", Criteria: []string{"complete"}}, at)
	if err != nil {
		t.Fatal(err)
	}
	deliver(later.ID, "prompt-2")
	// Both handoffs remain unanswered, so both keep being nudged in turn.
	deliver(first, "prompt-3")
	deliver(later.ID, "prompt-4")
}

// The qualification run's first handoff is nudged and acknowledged but its
// leader never fetches it before the second handoff is created. The second
// dispatch must still get its nudge ahead of another nudge for the first.
func TestAcknowledgedUnfetchedDispatchDoesNotStarveLaterDispatch(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	const agent = "leader@engineering"
	at := time.Now()
	if _, err := s.SessionStarted(store.SessionStart{Agent: agent, Role: roles.Engineering, Provider: "claude-code", Version: "v", Model: "m", Executable: "/bin/sh", Dir: t.TempDir(), PID: 1, At: at}); err != nil {
		t.Fatal(err)
	}
	sess, err := session.Start(session.Spec{Path: "/bin/sh", Args: []string{"-c", "sleep 60"}, Env: os.Environ(), Dir: t.TempDir(), Columns: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Stop(100 * time.Millisecond) })
	first := addPendingDispatch(t, s, agent, store.DispatchCreated, 1)
	l := &leader{address: roles.LeaderOf(roles.Engineering), session: sess, generation: 1, ready: true}
	d := &daemon{leaders: map[string]*leader{agent: l}, store: s, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ref := sessionRef{address: l.address, generation: 1}
	nudge := func(dispatch int64, promptID string) {
		t.Helper()
		d.nudge(agent)
		prompt := fmt.Sprintf("asmai inbox --dispatch %d", dispatch)
		d.mu.Lock()
		pending := l.pendingPrompt
		d.mu.Unlock()
		if pending != prompt {
			t.Fatalf("the daemon nudged %q, want %q", pending, prompt)
		}
		if matched, err := d.automatedSubmitted(ref, prompt, promptID, "transcript", json.RawMessage(`{"prompt":"`+prompt+`"}`)); err != nil || !matched {
			t.Fatalf("acknowledging %q matched=%v, err=%v", prompt, matched, err)
		}
		if err := d.stopCurrentDispatch(ref, promptID, "transcript"); err != nil {
			t.Fatal(err)
		}
		d.mu.Lock()
		l.ready = true
		d.mu.Unlock()
	}
	nudge(first, "prompt-1")

	_, witness, err := s.ObservedWitnessed(store.Witnessed{Agent: "leader@coordination", Role: roles.Coordination, Generation: 1, Terminal: "/dev/pts/test", Text: "open a second job"}, "UserPromptSubmit", json.RawMessage(`{"prompt":"open a second job"}`), at)
	if err != nil {
		t.Fatal(err)
	}
	job, err := s.JobOpened(store.Job{Repository: "fixture", Witness: witness, Reading: "second", Mandate: store.MandateTestedPR, Criteria: []string{"complete"}}, false, at)
	if err != nil {
		t.Fatal(err)
	}
	_, later, err := s.SendHandoff(store.Handoff{Job: job.Number, Sender: "leader@coordination", Receiver: agent, Outcome: "outcome", Decisions: "none", Evidence: "none", Constraints: "none", Permissions: "none", Criteria: []string{"complete"}}, at)
	if err != nil {
		t.Fatal(err)
	}
	nudge(later.ID, "prompt-2")
}
