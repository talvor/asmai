// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/session"
	"github.com/talvor/asmai/internal/store"
)

const permissionRequest = `{"hook_event_name": "PermissionRequest", "tool_name": "Bash", "tool_input": {"command": "rm -rf build"}}`

// stalled gives the first assignment's worker a session that records the first
// key typed into it, and says under which policy it started.
func (f twoResults) stalled(t *testing.T, managedOnly bool) (agent string, ref sessionRef, capture string) {
	t.Helper()
	agent = f.assignments[0].Worker
	capture = filepath.Join(t.TempDir(), "typed")
	const script = `stty raw -echo; dd bs=1 count=1 of="$ASMAI_CAPTURE" 2>/dev/null; sleep 60`
	sess, err := session.Start(session.Spec{Path: "/bin/sh", Args: []string{"-c", script}, Env: append(os.Environ(), "ASMAI_CAPTURE="+capture), Dir: t.TempDir(), Columns: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Stop(100 * time.Millisecond) })
	f.d.mu.Lock()
	l := f.d.leaders[agent]
	l.session, l.assignment, l.managedOnly = sess, f.assignments[0].ID, managedOnly
	f.d.mu.Unlock()
	return agent, f.d.sessions["worker0"], capture
}

// typed waits for the key the stalled worker's terminal received, and says
// that none came when it waits in vain.
func typed(t *testing.T, capture string) (string, bool) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if got, err := os.ReadFile(capture); err == nil && len(got) > 0 {
			return string(got), true
		}
	}
	return "", false
}

// nothingTyped says whether the terminal received nothing, after a wait long
// enough for a key to have arrived.
func nothingTyped(t *testing.T, capture string) bool {
	t.Helper()
	time.Sleep(300 * time.Millisecond)
	got, err := os.ReadFile(capture)
	return err != nil || len(got) == 0
}

func (f twoResults) pendingPrompts(t *testing.T) []store.DecisionRequest {
	t.Helper()
	pending, err := f.s.PendingDecisions()
	if err != nil {
		t.Fatal(err)
	}
	var prompts []store.DecisionRequest
	for _, r := range pending {
		if r.Kind == store.RequestPrompt {
			prompts = append(prompts, r)
		}
	}
	return prompts
}

// Under the managed policy a native prompt at an agent without the user's
// terminal is raised as a decision request to the user, which Coordination
// presents. The daemon types an answer into the agent's terminal only after
// the user's own witnessed message after it was shown, and never answers the
// prompt itself.
func TestUnderThePolicyANativePromptAtAWorkerIsRaisedToTheUserAndOnlyTheirWitnessedAnswerIsRelayed(t *testing.T) {
	for _, tt := range []struct {
		choice string
		keys   string
	}{
		{store.PromptAllow, "\r"},
		{store.PromptDeny, "\x1b"},
	} {
		t.Run(tt.choice, func(t *testing.T) {
			f := newTwoResults(t)
			f.withWorkers(t)
			agent, ref, capture := f.stalled(t, true)

			f.d.promptObserved(ref, "PermissionRequest", json.RawMessage(permissionRequest))
			prompts := f.pendingPrompts(t)
			if len(prompts) != 1 {
				t.Fatalf("the user has the prompts %+v pending, want one", prompts)
			}
			r := prompts[0]
			if r.Requester != agent || r.Recipient != store.ToUser || r.Job != f.job.Number || r.Surface != store.SurfaceConversation || r.Version != 1 ||
				!strings.Contains(r.Content, "rm -rf build") || !strings.Contains(r.Content, agent) ||
				len(r.Questions) != 1 || strings.Join(r.Questions[0].Options, "|") != store.PromptAllow+"|"+store.PromptDeny || r.Questions[0].Recommendation != "" {
				t.Fatalf("request = %+v", r)
			}
			// Coordination, which presents it, is told in a dispatch of its own.
			if dispatch, err := f.s.NextDispatch("leader@coordination"); err != nil {
				t.Fatalf("Coordination was not told: %v", err)
			} else if inbox, err := f.s.Inbox("leader@coordination", dispatch.ID, dispatch.ID, time.Now()); err != nil || len(inbox) != 1 || inbox[0].RequestData == nil || inbox[0].RequestData.Request != r.Request {
				t.Fatalf("Coordination's inbox = %+v (%v)", inbox, err)
			}
			// The status shows it as the user's to decide, not as one nobody can answer.
			waiting := f.d.waitingPrompts()
			if len(waiting) != 1 || waiting[0].Request != r.Request || strings.Contains(waiting[0].String(), "nobody can answer") || !strings.Contains(waiting[0].String(), "decision request") {
				t.Errorf("waiting prompts = %+v", waiting)
			}

			// Nothing is typed before the user answers: not when it is shown,
			// not for an answer that cites a nudge, and not for an agent's
			// own word.
			if _, err := f.d.decisionPresent(Request{Session: "coordination", Command: CommandDecisionPresent, Request: r.Request, Version: 1}); err != nil {
				t.Fatal(err)
			}
			if _, _, _, err := f.d.conversationAnswer(Request{Session: "worker0", Command: CommandDecisionRecord, Request: r.Request, Version: 1, LatestWitness: true, Choice: tt.choice, Reading: "yes"}); err == nil {
				t.Fatal("the stalled worker answered its own prompt")
			}
			if _, _, err := f.d.decisionAnswer(Request{Session: "credential", Command: CommandDecisionAnswer, Request: r.Request, Version: 1, Answer: store.PromptAllow}); err == nil {
				t.Fatal("a leader answered the user's prompt")
			}
			if err := f.s.Observed("leader@coordination", "coordination", 1, "UserPromptSubmit", json.RawMessage(`{}`), time.Now()); err != nil {
				t.Fatal(err)
			}
			journal, err := f.s.Journal()
			if err != nil {
				t.Fatal(err)
			}
			if _, _, _, err := f.d.conversationAnswer(Request{Session: "coordination", Command: CommandDecisionRecord, Request: r.Request, Version: 1, Cites: journal[len(journal)-1].ID, Choice: tt.choice, Reading: "yes"}); err == nil {
				t.Fatal("a nudge answered the prompt")
			}
			if !nothingTyped(t, capture) {
				t.Fatal("the daemon typed into the agent's terminal before the user answered")
			}
			if got := journaledOf(t, f.s, store.KindPromptRelayed); got != 0 {
				t.Fatalf("%d relays journaled before the user answered", got)
			}

			// The user's own message after it was shown answers it, and the
			// daemon relays that answer, once.
			if _, err := f.d.decisionPresent(Request{Session: "coordination", Command: CommandDecisionPresent, Request: r.Request, Version: 1}); err != nil {
				t.Fatal(err)
			}
			said := f.userSays(t, "that is fine")
			_, answered, dispatch, err := f.d.conversationAnswer(Request{Session: "coordination", Command: CommandDecisionRecord, Request: r.Request, Version: 1, Cites: said, Choice: tt.choice, Reading: "the user said to go on"})
			if err != nil {
				t.Fatal(err)
			}
			if answered.State != store.RequestAnswered || answered.Cites != said || dispatch != nil {
				t.Errorf("answered = %+v, dispatch = %+v, want it answered by the witnessed message with no dispatch to the agent", answered, dispatch)
			}
			if got, ok := typed(t, capture); !ok || got != tt.keys {
				t.Fatalf("the agent's terminal received %q (%v), want %q", got, ok, tt.keys)
			}
			if got := journaledOf(t, f.s, store.KindPromptRelayed); got != 1 {
				t.Errorf("%d relays journaled, want 1", got)
			}
			if waiting := f.d.waitingPrompts(); len(waiting) != 0 {
				t.Errorf("the status still shows %+v after the user's answer was relayed", waiting)
			}
			if _, _, _, err := f.d.conversationAnswer(Request{Session: "coordination", Command: CommandDecisionRecord, Request: r.Request, Version: 1, Cites: said, Choice: tt.choice, Reading: "again"}); err == nil {
				t.Error("the prompt was answered twice")
			}
		})
	}
}

func journaledOf(t *testing.T, s *store.Store, kind string) int {
	t.Helper()
	journal, err := s.Journal()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range journal {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

// Without the policy nothing changes: the prompt is reported as one nobody
// can answer, no request is raised and nothing is relayed.
func TestWithoutThePolicyANativePromptIsOnlyReported(t *testing.T) {
	f := newTwoResults(t)
	f.withWorkers(t)
	_, ref, capture := f.stalled(t, false)

	f.d.promptObserved(ref, "PermissionRequest", json.RawMessage(permissionRequest))
	if prompts := f.pendingPrompts(t); len(prompts) != 0 {
		t.Fatalf("the user is asked %+v without the policy", prompts)
	}
	waiting := f.d.waitingPrompts()
	if len(waiting) != 1 || waiting[0].Request != 0 || waiting[0].String() != "worker1@engineering waits at a native prompt nobody can answer: Bash `rm -rf build`" {
		t.Errorf("waiting prompts = %+v", waiting)
	}
	if _, err := f.s.NextDispatch("leader@coordination"); err == nil {
		t.Error("Coordination was told of a prompt without the policy")
	}
	if !nothingTyped(t, capture) {
		t.Error("the daemon typed into the agent's terminal")
	}
}

// Coordination's own prompt is the user's, in the conversation, and an agent
// whose job the daemon cannot tell is only reported.
func TestOnlyAnAgentWithAJobAndNotCoordinationRaisesAPromptRequest(t *testing.T) {
	f := newTwoResults(t)
	f.withWorkers(t)
	f.d.leaders["leader@coordination"].managedOnly = true
	f.d.promptObserved(sessionRef{address: roles.LeaderOf(roles.Coordination), generation: 1}, "PermissionRequest", json.RawMessage(permissionRequest))
	// Engineering's leader has no dispatch it is working on, so no job.
	f.d.leaders["leader@engineering"].managedOnly = true
	f.d.promptObserved(sessionRef{address: roles.LeaderOf(roles.Engineering), generation: 1}, "PermissionRequest", json.RawMessage(permissionRequest))
	if prompts := f.pendingPrompts(t); len(prompts) != 0 {
		t.Fatalf("the user is asked %+v", prompts)
	}
	waiting := f.d.waitingPrompts()
	if len(waiting) != 1 || waiting[0].Agent != "leader@engineering" || !strings.Contains(waiting[0].String(), "nobody can answer") {
		t.Errorf("waiting prompts = %+v, want Engineering's leader's, reported as before", waiting)
	}
}

// Once the agent is past the prompt, the user is no longer asked: the request
// is withdrawn, and an answer typed against it is refused and relays nothing.
func TestAPromptTheAgentIsPastIsWithdrawnAndRelaysNothing(t *testing.T) {
	for name, past := range map[string]func(f twoResults, ref sessionRef){
		"another event of the session": func(f twoResults, ref sessionRef) {
			f.d.promptObserved(ref, "PostToolUse", json.RawMessage(`{}`))
		},
		"the session ending": func(f twoResults, ref sessionRef) {
			f.d.mu.Lock()
			defer f.d.mu.Unlock()
			f.d.promptEnded(f.d.leaders[ref.address.String()])
		},
		"another prompt": func(f twoResults, ref sessionRef) {
			f.d.promptObserved(ref, "PermissionRequest", json.RawMessage(`{"hook_event_name": "PermissionRequest", "tool_name": "Edit", "tool_input": {"file_path": "/etc/hosts"}}`))
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newTwoResults(t)
			f.withWorkers(t)
			_, ref, capture := f.stalled(t, true)
			f.d.promptObserved(ref, "PermissionRequest", json.RawMessage(permissionRequest))
			first := f.pendingPrompts(t)[0]
			if _, err := f.d.decisionPresent(Request{Session: "coordination", Command: CommandDecisionPresent, Request: first.Request, Version: 1}); err != nil {
				t.Fatal(err)
			}

			past(f, ref)
			said := f.userSays(t, "that is fine")
			if _, _, _, err := f.d.conversationAnswer(Request{Session: "coordination", Command: CommandDecisionRecord, Request: first.Request, Version: 1, Cites: said, Choice: store.PromptAllow, Reading: "go on"}); err == nil || !strings.Contains(err.Error(), "withdrawn") {
				t.Fatalf("answering the prompt the agent is past = %v", err)
			}
			if !nothingTyped(t, capture) {
				t.Fatal("the daemon typed an answer to a prompt the agent was past")
			}
			if name == "another prompt" {
				if prompts := f.pendingPrompts(t); len(prompts) != 1 || prompts[0].Request == first.Request || !strings.Contains(prompts[0].Content, "/etc/hosts") {
					t.Errorf("the user's prompts are %+v, want only the new one", prompts)
				}
			} else if prompts := f.pendingPrompts(t); len(prompts) != 0 {
				t.Errorf("the user still has %+v pending", prompts)
			}
		})
	}
}

// An answer to a prompt that belongs to a session that is gone is recorded
// as the user's, but types nothing into the agent that runs now.
func TestAnAnswerToAPromptOfAnEarlierSessionIsNotRelayedToTheNextOne(t *testing.T) {
	f := newTwoResults(t)
	f.withWorkers(t)
	agent, ref, capture := f.stalled(t, true)
	f.d.promptObserved(ref, "PermissionRequest", json.RawMessage(permissionRequest))
	r := f.pendingPrompts(t)[0]
	if _, err := f.d.decisionPresent(Request{Session: "coordination", Command: CommandDecisionPresent, Request: r.Request, Version: 1}); err != nil {
		t.Fatal(err)
	}
	// The session restarted without the daemon seeing the prompt end.
	f.d.mu.Lock()
	f.d.leaders[agent].generation = 2
	f.d.mu.Unlock()
	said := f.userSays(t, "that is fine")
	if _, _, _, err := f.d.conversationAnswer(Request{Session: "coordination", Command: CommandDecisionRecord, Request: r.Request, Version: 1, Cites: said, Choice: store.PromptAllow, Reading: "go on"}); err != nil {
		t.Fatal(err)
	}
	if !nothingTyped(t, capture) {
		t.Fatal("the daemon typed an earlier session's answer into the next session")
	}
	if got := journaledOf(t, f.s, store.KindPromptRelayed); got != 0 {
		t.Errorf("%d relays journaled", got)
	}
}

func TestAnAgentCannotRaiseOrRenewAPromptRequestItself(t *testing.T) {
	f := newTwoResults(t)
	f.withWorkers(t)
	question := []store.Question{{ID: "prompt", Title: "Go on?", Options: []string{store.PromptAllow, store.PromptDeny}}}
	for _, session := range []string{"credential", "worker0"} {
		_, _, err := f.d.decisionRequest(Request{Session: session, Command: CommandDecisionRequest, Job: f.job.Number, Kind: store.RequestPrompt, Questions: question})
		if err == nil || !strings.Contains(err.Error(), "daemon") {
			t.Errorf("%s raising a prompt request = %v, want a refusal that the daemon raises it", session, err)
		}
	}
}
