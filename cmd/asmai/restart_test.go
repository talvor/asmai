// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/fakeprovider"
	"github.com/talvor/asmai/internal/statedir"
	"github.com/talvor/asmai/internal/store"
)

// The provider's own session the first run's worker is in, which the daemon
// learns from the session's hooks.
const nativeSession = "native-session-of-worker1"

// nativeHook is a hook payload the fake delivers for a session of the
// provider, with the identifier of its native session and the transcript the
// provider keeps.
func nativeHook(event, promptID, prompt, transcript string) string {
	payload := map[string]string{"hook_event_name": event, "session_id": nativeSession, "transcript_path": transcript}
	if promptID != "" {
		payload["prompt_id"] = promptID
	}
	if prompt != "" {
		payload["prompt"] = prompt
	}
	encoded, _ := json.Marshal(payload)
	return fmt.Sprintf(`{"hook":%q,"payload":%s}`, event, encoded)
}

// restartRun is a factory that has run until the point a test stops it: Coordination
// opened job 1 and handed it to Engineering, whose leader accepted it and gave
// the one writing assignment to worker1. The dispatches are numbered in the
// order they are made: 1 the handoff, 2 its answer, 3 the assignment.
type restartRun struct {
	testDir    string
	stateDir   string
	transcript string
	fixture    writingFixture
}

// busyLeader keeps Engineering's leader in a turn of its own after it has
// assigned the work, until the factory is stopped, so that what is sent to it
// waits unfetched in its inbox.
var busyLeader = []string{fakeRun(`while [ ! -e "$ASMAI_TEST_DIR/never" ]; do sleep 0.02; done`, 0, "")}

// firstRun runs a factory to that point, and then until the test's own
// condition: worker is what the worker does after it fetches its assignment,
// leader what Engineering's leader does in its turn after it has assigned it,
// which ends the turn if it is nothing, and secondHandoff has Coordination
// send Engineering a second handoff, which waits unfetched for a busy
// leader. It returns once marker, a file the scripts touch, exists in the
// run's test directory.
func firstRun(t *testing.T, worker func(transcript string) []string, leader []string, secondHandoff bool, marker string) restartRun {
	t.Helper()
	gitIdentity(t)
	testDir := t.TempDir()
	t.Setenv("ASMAI_TEST_DIR", testDir)
	transcript := filepath.Join(testDir, "worker-transcript.jsonl")
	if err := os.WriteFile(transcript, []byte(`{"type":"user"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	engineering := []string{
		hookStep("SessionStart", "", ""),
		`{"screen":"\u001b[?2004h> "}`,
		`{"expect":"asmai inbox --dispatch 1\r"}`,
		hookStep("UserPromptSubmit", "handoff", "asmai inbox --dispatch 1"),
		fakeRun(`asmai inbox --dispatch 1`, 0, "Add a greeting"),
		fakeRun(`asmai handoff accept --handoff 1`, 0, "reply dispatch 2"),
		fakeRun(`asmai assign --job 1 --outcome 'Add greeting.txt, saying hello' --criterion 'greeting.txt says hello'`, 0, "assignment 1 of job 1 given to worker1@engineering; dispatch 3"),
		fakeRun(`touch "$ASMAI_TEST_DIR/assigned"`, 0, ""),
	}
	engineering = append(engineering, leader...)
	if leader == nil {
		engineering = append(engineering, hookStep("Stop", "handoff", ""), `{"expect":"only typed by a test that means to"}`)
	}
	t.Setenv(fakeprovider.ScriptEnv+"_ENGINEERING", writeScript(t, "engineering-first", engineering))

	workerScript := append([]string{
		nativeHook("SessionStart", "", "", transcript),
		`{"screen":"\u001b[?2004h> "}`,
		`{"expect":"asmai inbox --dispatch 3\r"}`,
		nativeHook("UserPromptSubmit", "assignment", "asmai inbox --dispatch 3", transcript),
		fakeRun(`asmai inbox --dispatch 3`, 0, "(?s)assignment 1 \\| job 1 \\| leader@engineering -> worker1@engineering \\| active.*Add greeting.txt"),
		fakeRun(`printf '%s' "$ASMAI_SESSION" > "$ASMAI_TEST_DIR/worker-credential"`, 0, ""),
	}, worker(transcript)...)
	t.Setenv(fakeprovider.ScriptEnv+"_WORKER", writeScript(t, "worker-first", workerScript))

	coordination := []string{
		hookStep("SessionStart", "", ""),
		`{"screen":"> "}`,
		`{"expect":"Please add a greeting\r"}`,
		hookStep("UserPromptSubmit", "", "Please add a greeting"),
		fakeRun(`asmai job open --message latest --repository fixture --reading 'Add a greeting' --criterion 'Greeting is printed'`, 0, "Opened job 1"),
		fakeRun(`asmai handoff send --job 1 --to engineering --outcome 'Add a greeting' --decisions none --evidence none --constraints none --permissions none --criterion 'Greeting is printed'`, 0, "dispatch 1"),
		hookStep("Stop", "", ""),
		`{"expect":"asmai inbox --dispatch 2\r"}`,
		hookStep("UserPromptSubmit", "reply", "asmai inbox --dispatch 2"),
		fakeRun(`asmai inbox --dispatch 2`, 0, "accepted from leader@engineering"),
	}
	if secondHandoff {
		coordination = append(coordination,
			fakeRun(`while [ ! -e "$ASMAI_TEST_DIR/assigned" ]; do sleep 0.02; done`, 0, ""),
			fakeRun(`asmai handoff send --job 1 --to engineering --outcome 'Second request' --decisions none --evidence none --constraints none --permissions none --criterion Answered`, 0, "dispatch 4"),
		)
	}
	coordination = append(coordination, hookStep("Stop", "reply", ""), `{"expect":"only typed by a test that means to"}`)
	stateDir := readyFactory(t, coordination)
	t.Cleanup(func() {
		if t.Failed() {
			log, _, _ := runAsmai(t, "log")
			t.Logf("the daemon's log:\n%s", log)
		}
	})
	if _, stderr, code := runAsmai(t, "start"); code != 0 {
		t.Fatal(stderr)
	}
	fixture := registerWritingFixture(t)
	a := inTerminal(t, 100, 25)
	waitFor(t, "Coordination prompt", a.shows(">"))
	a.ptmx.WriteString("Please add a greeting\r")
	waitFor(t, "the run to reach the point it is stopped at", touched(testDir, marker))
	if secondHandoff {
		waitFor(t, "the second handoff", func() bool { return len(journaled(t, "handoff.sent")) == 2 })
	}
	return restartRun{testDir: testDir, stateDir: stateDir, transcript: transcript, fixture: fixture}
}

// stopFactory runs `asmai stop`, which must succeed.
func stopFactory(t *testing.T) {
	t.Helper()
	if stdout, stderr, code := runAsmai(t, "stop"); code != 0 || !strings.Contains(stdout, "Stopped the factory") {
		t.Fatalf("asmai stop exited %d printing %q and %q", code, stdout, stderr)
	}
}

// inStore opens the store of the stopped factory, which only its daemon
// opens while it runs, and runs f with it.
func inStore(t *testing.T, stateDir string, f func(s *store.Store)) {
	t.Helper()
	s, err := store.Open(statedir.At(stateDir).Store)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	f(s)
}

// secondRun starts the stopped factory again with scripts for its agents'
// next sessions: coordination and engineering for the leaders, and resumed
// for a worker that resumes its native session.
func secondRun(t *testing.T, coordination, engineering, resumed []string) {
	t.Helper()
	t.Setenv(fakeprovider.ScriptEnv, writeScript(t, "coordination-second", coordination))
	t.Setenv(fakeprovider.ScriptEnv+"_ENGINEERING", writeScript(t, "engineering-second", engineering))
	if resumed != nil {
		t.Setenv(fakeprovider.ResumedScriptEnv, writeScript(t, "worker-resumed", resumed))
	}
	if stdout, stderr, code := runAsmai(t, "start"); code != 0 {
		t.Fatalf("asmai start exited %d printing %q and %q", code, stdout, stderr)
	}
}

// dispatchStates returns the states each dispatch moved through in the
// journal after the entry with ID after, by dispatch.
func dispatchStates(t *testing.T, after int64) map[int64][]string {
	t.Helper()
	states := map[int64][]string{}
	for _, e := range journaled(t, "dispatch.changed") {
		if e.ID <= after {
			continue
		}
		d := decode[struct {
			ID    int64  `json:"id"`
			State string `json:"state"`
		}](t, e)
		states[d.ID] = append(states[d.ID], d.State)
	}
	return states
}

// lastEntry returns the ID of the last journal entry.
func lastEntry(t *testing.T) int64 {
	t.Helper()
	var j struct {
		Journal []journalEntry `json:"journal"`
	}
	asmaiJSON(t, &j, "export")
	return j.Journal[len(j.Journal)-1].ID
}

// asAgent runs asmai as the agent session whose credential is credential, the
// way the agent's own commands and hooks do, and returns what it printed and
// its exit code.
func asAgent(t *testing.T, credential, stdin string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(filepath.Join(asmaiBin, "asmai"), args...)
	cmd.Env = append(os.Environ(), "ASMAI_SESSION="+credential)
	cmd.Stdin = strings.NewReader(stdin)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	if exit, ok := err.(*exec.ExitError); ok {
		return out.String(), errOut.String(), exit.ExitCode()
	}
	if err != nil {
		t.Fatal(err)
	}
	return out.String(), errOut.String(), 0
}

// stoppedWorker is the first run's worker when it stops at a boundary after
// it has committed some of the greeting: its dispatch, 3, stops in its first
// turn, which it ends without a result.
func stoppedWorker(transcript string) []string {
	return []string{
		fakeRun(`printf 'draft\n' > greeting.txt && git add -A && git commit -q -m 'Start the greeting' && asmai effect commit "$(git rev-parse HEAD)"`, 0, "effect 1 recorded: commit"),
		nativeHook("Stop", "assignment", "", transcript),
		fakeRun(`touch "$ASMAI_TEST_DIR/worker-stopped"`, 0, ""),
		`{"expect":"only typed by a test that means to"}`,
	}
}

// restoration is what a restored leader's fetch of its restoration message
// shows: the job whose brief it loads.
const restoration = "(?s)restoration from daemon.*Load `asmai brief 1`"

func TestWorkContinuesAcrossAStopAndAStartWithDispatchesAndResultsStillCorrelated(t *testing.T) {
	const branch = "asmai/job-1/1"
	// The first run: the worker commits a draft and ends its turn without a
	// result, and the Engineering leader, still in its turn, has a second
	// handoff waiting unfetched in its inbox when the factory is stopped.
	r := firstRun(t, stoppedWorker, busyLeader, true, "worker-stopped")
	if got := len(journaled(t, "daemon.started")); got != 1 {
		t.Fatalf("the daemon started %d times before the stop", got)
	}

	// ---- asmai stop stops every agent session and persists the state.
	stopFactory(t)
	inStore(t, r.stateDir, func(s *store.Store) {
		agents, err := s.Agents()
		if err != nil {
			t.Fatal(err)
		}
		generations := map[string]int{}
		for _, a := range agents {
			generations[a.Agent] = a.Generation
			if a.State != store.AgentStopped {
				t.Errorf("%s is %s after asmai stop, want every agent session stopped", a.Agent, a.State)
			}
		}
		if want := map[string]int{"leader@coordination": 1, "leader@engineering": 1, "worker1@engineering": 1}; fmt.Sprint(generations) != fmt.Sprint(want) {
			t.Errorf("the agents are %v, want %v", generations, want)
		}
		pending, err := s.PendingLeaders()
		if err != nil || !slices.Contains(pending, "leader@engineering") {
			t.Errorf("the agents with queued messages are %v (%v), want the Engineering leader's handoff to survive", pending, err)
		}
		queued, err := s.NextDispatch("leader@engineering")
		if err != nil || queued.ID != 4 || queued.State != store.DispatchCreated {
			t.Errorf("Engineering's next dispatch is %+v (%v), want the second handoff's dispatch 4, created and unfetched", queued, err)
		}
		handoffs, err := s.HandoffsForJob(1)
		if err != nil || len(handoffs) != 2 || handoffs[0].State != store.HandoffAccepted || handoffs[1].State != store.HandoffPending {
			t.Errorf("the job's handoffs are %+v (%v), want the first accepted and the second still open", handoffs, err)
		}
		w, err := s.WorkerDispatch(1)
		if err != nil || w.Assignment.State != store.AssignmentActive || w.Dispatch.ID != 3 || w.Dispatch.State != store.DispatchStopped || !w.Fetched {
			t.Errorf("the worker's latest dispatch is %+v (%v), want dispatch 3 of the active assignment 1, stopped after it was fetched", w, err)
		}
		native, err := s.ResumableSession(1)
		if err != nil || native.ID != nativeSession || native.Transcript != r.transcript {
			t.Errorf("the worker's native session is %+v (%v), want %s with its transcript %s", native, err, nativeSession, r.transcript)
		}
	})

	// ---- The next asmai start continues the work.
	coordination := []string{
		hookStep("SessionStart", "", ""),
		`{"screen":"\u001b[?2004h> "}`,
		`{"expect":"asmai inbox --dispatch 6\r"}`,
		hookStep("UserPromptSubmit", "restore", "asmai inbox --dispatch 6"),
		fakeRun(`asmai inbox --dispatch 6`, 0, restoration),
		fakeRun(`asmai brief 1`, 0, "(?s)Handoffs:\n- 1 leader@coordination -> leader@engineering: accepted"),
		hookStep("Stop", "restore", ""),
		`{"expect":"asmai inbox --dispatch 8\r"}`,
		hookStep("UserPromptSubmit", "reply", "asmai inbox --dispatch 8"),
		fakeRun(`asmai inbox --dispatch 8`, 0, "accepted from leader@engineering"),
		hookStep("Stop", "reply", ""),
		fakeRun(`touch "$ASMAI_TEST_DIR/coordination-done"`, 0, ""),
		`{"expect":"only typed by a test that means to"}`,
	}
	engineering := []string{
		hookStep("SessionStart", "", ""),
		`{"screen":"\u001b[?2004h> "}`,
		// What was queued before the stop is nudged first, then the
		// restoration, which names the brief to load.
		`{"expect":"asmai inbox --dispatch 4\r"}`,
		hookStep("UserPromptSubmit", "second", "asmai inbox --dispatch 4"),
		fakeRun(`asmai inbox --dispatch 4`, 0, "(?s)handoff 2 .*Second request"),
		fakeRun(`asmai handoff accept --handoff 2`, 0, "reply dispatch 8"),
		fakeRun(`touch "$ASMAI_TEST_DIR/engineering-accepted"`, 0, ""),
		hookStep("Stop", "second", ""),
		`{"expect":"asmai inbox --dispatch 7\r"}`,
		hookStep("UserPromptSubmit", "restore", "asmai inbox --dispatch 7"),
		fakeRun(`asmai inbox --dispatch 7`, 0, restoration),
		fakeRun(`asmai brief 1`, 0, "(?s)Assignments:\n- 1 worker1@engineering \\(owner leader@engineering\\) (active|submitted) on branch "+branch),
		hookStep("Stop", "restore", ""),
		// The worker's result reaches the leader in the dispatch the worker
		// was given after the restart.
		`{"expect":"asmai inbox --dispatch 9\r"}`,
		hookStep("UserPromptSubmit", "result", "asmai inbox --dispatch 9"),
		fakeRun(`asmai inbox --dispatch 9`, 0, "(?s)result from worker1@engineering.*result 1 \\| assignment 1 \\| worker1@engineering"),
		waitForWorker,
		fakeRun(`asmai accept --assignment 1 --reason 'greeting.txt says hello: the check passed at the commit'`, 0, "assignment 1 accepted by leader@engineering"),
		hookStep("Stop", "result", ""),
		fakeRun(`touch "$ASMAI_TEST_DIR/leader-done"`, 0, ""),
		`{"expect":"only typed by a test that means to"}`,
	}
	resumed := []string{
		nativeHook("SessionStart", "", "", r.transcript),
		`{"screen":"\u001b[?2004h> "}`,
		`{"expect":"asmai inbox --dispatch 5\r"}`,
		nativeHook("UserPromptSubmit", "resumption", "asmai inbox --dispatch 5", r.transcript),
		fakeRun(`asmai inbox --dispatch 5`, 0, "(?s)resumption from daemon.*Resumes dispatch 3.*assignment 1 \\| job 1 \\| leader@engineering -> worker1@engineering \\| active"),
		// Fetching the dispatch that stopped again is safe, and changes
		// nothing about it.
		fakeRun(`asmai inbox --dispatch 3`, 0, "assignment 1"),
		fakeRun(`test "$(cat greeting.txt)" = draft && printf 'hello\n' > greeting.txt && printf '#!/bin/sh\ntest "$(cat greeting.txt)" = hello\n' > check.sh && chmod +x check.sh && ./check.sh && git add -A && git commit -q -m 'Finish the greeting' && asmai effect commit "$(git rev-parse HEAD)"`, 0, "effect 2 recorded: commit"),
		fakeRun(`git merge --no-edit asmai/job-1-add-a-greeting`, 0, "up to date"),
		fakeRun(`git push -q origin `+branch+` && asmai effect push "origin/`+branch+`@$(git rev-parse HEAD)"`, 0, "effect 3 recorded: push"),
		fakeRun(`while [ ! -e "$ASMAI_TEST_DIR/engineering-accepted" ]; do sleep 0.02; done`, 0, ""),
		fakeRun(resultFlagsForGreeting, 0, "result recorded for assignment 1 at commit [0-9a-f]{40}; sent to leader@engineering as dispatch 9"),
		nativeHook("Stop", "resumption", "", r.transcript),
		fakeRun(`touch "$ASMAI_TEST_DIR/worker-done"`, 0, ""),
		`{"expect":"only typed by a test that means to"}`,
	}
	secondRun(t, coordination, engineering, resumed)
	waitFor(t, "the leaders to be told what to load", touched(r.testDir, "coordination-done"))
	waitFor(t, "the worker's result to be accepted", touched(r.testDir, "leader-done"))
	waitFor(t, "the accepted worker to stop", func() bool { return workerOf(t, "worker1@engineering").State == "stopped" })

	// ---- Coordination and each leader with open work are restored, each in
	// a new generation, and the worker is run again in its native session.
	starts := journaled(t, "daemon.started")
	if len(starts) != 2 {
		t.Fatalf("the daemon started %d times, want 2", len(starts))
	}
	restart := starts[1].ID
	type session struct {
		Agent      string   `json:"agent"`
		Generation int      `json:"generation"`
		Args       []string `json:"args"`
		Assignment int64    `json:"assignment"`
		Resumes    string   `json:"resumes"`
	}
	sessions := map[string][]session{}
	for _, e := range journaled(t, "session.started") {
		s := decode[session](t, e)
		sessions[s.Agent] = append(sessions[s.Agent], s)
	}
	for _, agent := range []string{"leader@coordination", "leader@engineering", "worker1@engineering"} {
		got := sessions[agent]
		if len(got) != 2 || got[0].Generation != 1 || got[1].Generation != 2 {
			t.Fatalf("%s started %+v, want its generations 1 and 2", agent, got)
		}
		if slices.Contains(got[0].Args, "--resume") {
			t.Errorf("%s's first session resumed one: %q", agent, got[0].Args)
		}
	}
	for _, leader := range []string{"leader@coordination", "leader@engineering"} {
		if got := sessions[leader][1]; slices.Contains(got.Args, "--resume") || got.Resumes != "" {
			t.Errorf("%s's new session resumed a provider session, %+v; a leader loads its brief instead", leader, got)
		}
	}
	worker := sessions["worker1@engineering"][1]
	if i := slices.Index(worker.Args, "--resume"); i < 0 || i+1 >= len(worker.Args) || worker.Args[i+1] != nativeSession || worker.Resumes != nativeSession || worker.Assignment != 1 {
		t.Errorf("the worker's second session is %+v, want it to resume %s for assignment 1", worker, nativeSession)
	}
	for _, flag := range []string{"--model", "--append-system-prompt", "--settings"} {
		if !slices.Contains(worker.Args, flag) {
			t.Errorf("the resumed worker was started without %s: %q", flag, worker.Args)
		}
	}
	if got := workerOf(t, "worker1@engineering"); got.Generation != 2 || got.Provider != "claude-code" {
		t.Errorf("the worker is %+v, want generation 2 on the same provider, Claude Code", got)
	}

	// ---- It continues with a new dispatch recorded as a resumption of the
	// one that stopped, and that one is left as it was.
	resumptions := journaled(t, "assignment.resumed")
	if len(resumptions) != 1 {
		t.Fatalf("assignments resumed: %+v", resumptions)
	}
	resumption := decode[struct {
		Assignment    int64  `json:"assignment"`
		Job           int64  `json:"job"`
		Worker        string `json:"worker"`
		Resumes       int64  `json:"resumes"`
		Dispatch      int64  `json:"dispatch"`
		NativeSession string `json:"native_session"`
	}](t, resumptions[0])
	if resumption.Assignment != 1 || resumption.Job != 1 || resumption.Worker != "worker1@engineering" || resumption.Resumes != 3 || resumption.Dispatch != 5 || resumption.NativeSession != nativeSession {
		t.Errorf("the resumption is %+v", resumption)
	}
	states := dispatchStates(t, restart)
	if got := states[3]; len(got) != 0 {
		t.Errorf("dispatch 3 changed to %v after the restart: evidence from before it never completes a new dispatch", got)
	}
	if got := strings.Join(states[5], ","); got != "created,unknown,nudged,delivered,working,stopped" {
		t.Errorf("the resumption's states are %s", got)
	}
	if got := strings.Join(states[4], ","); got != "unknown,nudged,delivered,working,stopped" {
		t.Errorf("the second handoff's dispatch moved through %s after the restart, want it delivered once the leader was restored", got)
	}

	// ---- Every result and effect stays tied to the dispatch it came from.
	results := journaled(t, "result.submitted")
	if len(results) != 1 {
		t.Fatalf("results submitted: %+v", results)
	}
	submitted := decode[struct {
		Assignment int64  `json:"assignment"`
		Dispatch   int64  `json:"dispatch"`
		Agent      string `json:"agent"`
		Effects    []struct {
			ID       int64 `json:"id"`
			Dispatch int64 `json:"dispatch"`
		} `json:"effects"`
	}](t, results[0])
	if submitted.Assignment != 1 || submitted.Dispatch != 5 || submitted.Agent != "worker1@engineering" {
		t.Errorf("the result is %+v, want it tied to the resumption, dispatch 5", submitted)
	}
	for _, e := range submitted.Effects {
		if e.Dispatch != 5 {
			t.Errorf("the result lists effect %d of dispatch %d, which is not the dispatch it came from", e.ID, e.Dispatch)
		}
	}
	var effectDispatches []int64
	for _, e := range journaled(t, "effect.recorded") {
		effectDispatches = append(effectDispatches, decode[struct {
			Dispatch int64 `json:"dispatch"`
		}](t, e).Dispatch)
	}
	if want := []int64{3, 5, 5}; !slices.Equal(effectDispatches, want) {
		t.Errorf("the effects are tied to dispatches %v, want %v", effectDispatches, want)
	}
	if got := len(journaled(t, "result.accepted")); got != 1 {
		t.Errorf("results accepted: %d", got)
	}

	// ---- A call or a hook of the session from before the restart is
	// refused, and changes nothing.
	credential, err := os.ReadFile(filepath.Join(r.testDir, "worker-credential"))
	if err != nil || len(credential) == 0 {
		t.Fatalf("the first run's worker credential: %q (%v)", credential, err)
	}
	last := lastEntry(t)
	for _, call := range []struct {
		stdin string
		args  []string
		want  string
	}{
		{`{"hook_event_name":"Stop","prompt_id":"assignment","session_id":"` + nativeSession + `"}`, []string{"hook"}, "unknown or superseded"},
		{"", []string{"inbox", "--dispatch", "5"}, "unknown or superseded"},
		{"", []string{"result", "--evidence", "x", "--test", "none", "--check", "none", "--gap", "none", "--pr-section", "x"}, "unknown or superseded"},
		{"", []string{"effect", "commit", "0000000000000000000000000000000000000000"}, "unknown or superseded"},
	} {
		_, stderr, code := asAgent(t, string(credential), call.stdin, call.args...)
		if code == 0 || !strings.Contains(stderr, call.want) {
			t.Errorf("asmai %s with the credential of the session from before the restart exited %d printing %q, want a refusal saying %q", strings.Join(call.args, " "), code, stderr, call.want)
		}
	}
	if got := lastEntry(t); got != last {
		t.Errorf("the calls of the session from before the restart changed the journal: its last entry is %d, was %d", got, last)
	}
}

func TestAnAssignmentWhoseNativeSessionCannotBeResumedNeedsReconciliationAndItsOwnerIsTold(t *testing.T) {
	r := firstRun(t, stoppedWorker, nil, false, "worker-stopped")
	stopFactory(t)

	// Claude Code has no record of the session: it says so and exits.
	t.Setenv(fakeprovider.ResumeFailsEnv, "1")
	coordination := []string{
		hookStep("SessionStart", "", ""),
		`{"screen":"\u001b[?2004h> "}`,
		`{"expect":"asmai inbox --dispatch 5\r"}`,
		hookStep("UserPromptSubmit", "restore", "asmai inbox --dispatch 5"),
		fakeRun(`asmai inbox --dispatch 5`, 0, restoration),
		hookStep("Stop", "restore", ""),
		`{"expect":"only typed by a test that means to"}`,
	}
	engineering := []string{
		hookStep("SessionStart", "", ""),
		`{"screen":"\u001b[?2004h> "}`,
		`{"expect":"asmai inbox --dispatch 6\r"}`,
		hookStep("UserPromptSubmit", "restore", "asmai inbox --dispatch 6"),
		fakeRun(`asmai inbox --dispatch 6`, 0, restoration),
		hookStep("Stop", "restore", ""),
		// The owning leader is told that the assignment needs reconciliation,
		// and holds it: it can be neither accepted nor cancelled yet.
		`{"expect":"asmai inbox --dispatch 7\r"}`,
		hookStep("UserPromptSubmit", "reconcile", "asmai inbox --dispatch 7"),
		fakeRun(`asmai inbox --dispatch 7`, 0, "(?s)reconciliation from daemon.*Assignment 1 of job 1 needs reconciliation: the native session of worker1@engineering cannot be resumed.*assignment 1 \\| job 1 \\| leader@engineering -> worker1@engineering \\| needs_reconciliation"),
		fakeRun(`asmai brief 1`, 0, "(?s)Assignments:\n- 1 worker1@engineering \\(owner leader@engineering\\) needs_reconciliation on branch asmai/job-1/1"),
		fakeRun(`asmai accept --assignment 1 --reason x`, 1, "is needs_reconciliation, so it has no result to accept"),
		fakeRun(`asmai cancel --assignment 1 --reason x`, 1, "is needs_reconciliation, so it cannot be cancelled"),
		hookStep("Stop", "reconcile", ""),
		fakeRun(`touch "$ASMAI_TEST_DIR/leader-told"`, 0, ""),
		`{"expect":"only typed by a test that means to"}`,
	}
	secondRun(t, coordination, engineering, nil)
	waitFor(t, "the owning leader to be told", touched(r.testDir, "leader-told"))

	held := journaled(t, "assignment.needs_reconciliation")
	if len(held) != 1 {
		t.Fatalf("assignments held for reconciliation: %+v", held)
	}
	h := decode[struct {
		Assignment int64  `json:"assignment"`
		Worker     string `json:"worker"`
		Owner      string `json:"owner"`
		Dispatch   int64  `json:"dispatch"`
		Reason     string `json:"reason"`
	}](t, held[0])
	if h.Assignment != 1 || h.Worker != "worker1@engineering" || h.Owner != "leader@engineering" || h.Dispatch != 4 || !strings.Contains(h.Reason, "cannot be resumed") {
		t.Errorf("the assignment was held as %+v, want assignment 1 with its resumption, dispatch 4", h)
	}
	if got := len(journaled(t, "assignment.resumed")); got != 1 {
		t.Errorf("assignments resumed: %d, want the one resumption that could not start", got)
	}
	states := dispatchStates(t, 0)
	if got := strings.Join(states[4], ","); got != "created,unknown" {
		t.Errorf("the resumption that could not start moved through %s, want it unknown", got)
	}
	// The worker is not started again and again for it.
	time.Sleep(300 * time.Millisecond)
	if got := workerOf(t, "worker1@engineering"); got.Generation != 2 || got.State != "stopped" || !strings.Contains(got.Exit, "exit status 1") {
		t.Errorf("the worker is %+v, want its second session, which could not resume, ended with exit status 1 and no third", got)
	}
	if len(journaled(t, "result.submitted")) != 0 {
		t.Error("a result was submitted for an assignment that needs reconciliation")
	}
}

func TestAWorkerWhoseTurnWasCutOffByTheStopNeedsReconciliationAndIsNotRunAgain(t *testing.T) {
	cutOff := func(transcript string) []string {
		return []string{
			fakeRun(`touch "$ASMAI_TEST_DIR/worker-working"; while [ ! -e "$ASMAI_TEST_DIR/never" ]; do sleep 0.02; done`, 0, ""),
		}
	}
	r := firstRun(t, cutOff, nil, false, "worker-working")
	stopFactory(t)

	coordination := []string{
		hookStep("SessionStart", "", ""),
		`{"screen":"\u001b[?2004h> "}`,
		`{"expect":"asmai inbox --dispatch 5\r"}`,
		hookStep("UserPromptSubmit", "restore", "asmai inbox --dispatch 5"),
		fakeRun(`asmai inbox --dispatch 5`, 0, restoration),
		hookStep("Stop", "restore", ""),
		`{"expect":"only typed by a test that means to"}`,
	}
	engineering := []string{
		hookStep("SessionStart", "", ""),
		`{"screen":"\u001b[?2004h> "}`,
		`{"expect":"asmai inbox --dispatch 4\r"}`,
		hookStep("UserPromptSubmit", "reconcile", "asmai inbox --dispatch 4"),
		fakeRun(`asmai inbox --dispatch 4`, 0, "(?s)reconciliation from daemon.*Assignment 1 of job 1 needs reconciliation: its dispatch 3 was working when the factory stopped"),
		hookStep("Stop", "reconcile", ""),
		`{"expect":"asmai inbox --dispatch 6\r"}`,
		hookStep("UserPromptSubmit", "restore", "asmai inbox --dispatch 6"),
		fakeRun(`asmai inbox --dispatch 6`, 0, restoration),
		fakeRun(`touch "$ASMAI_TEST_DIR/leader-told"`, 0, ""),
		hookStep("Stop", "restore", ""),
		`{"expect":"only typed by a test that means to"}`,
	}
	secondRun(t, coordination, engineering, nil)
	waitFor(t, "the owning leader to be told", touched(r.testDir, "leader-told"))

	if got := len(journaled(t, "assignment.resumed")); got != 0 {
		t.Errorf("an assignment whose worker did not stop at a boundary was resumed %d times", got)
	}
	held := journaled(t, "assignment.needs_reconciliation")
	if len(held) != 1 || decode[struct {
		Dispatch int64 `json:"dispatch"`
	}](t, held[0]).Dispatch != 3 {
		t.Fatalf("assignments held for reconciliation: %+v", held)
	}
	if got := strings.Join(dispatchStates(t, 0)[3], ","); got != "created,unknown,nudged,delivered,working,unknown" {
		t.Errorf("the cut off dispatch moved through %s, want it unknown at the end", got)
	}
	worker := workerOf(t, "worker1@engineering")
	if worker.Generation != 1 || worker.State != "stopped" {
		t.Errorf("the worker is %+v, want it stopped in its first generation, not run again", worker)
	}
}

// A cancellation that had not completed when the factory stopped, because
// its worker had stopped without its assignment branch on origin, continues
// after the restart: the worker is run again in its session, in a dispatch
// that resumes the one that stopped, and the assignment ends once it has
// pushed and stopped.
func TestACancellationThatWasNotCompleteWhenTheFactoryStoppedContinuesAfterTheStart(t *testing.T) {
	const branch = "asmai/job-1/1"
	// Engineering's leader cancels the assignment once its worker has
	// fetched it, so the worker is cancelled in the dispatch after it.
	leader := []string{
		fakeRun(`while [ ! -e "$ASMAI_TEST_DIR/worker-credential" ]; do sleep 0.02; done`, 0, ""),
		fakeRun(`asmai cancel --assignment 1 --reason 'the user dropped the greeting'`, 0, "cancellation requested"),
		hookStep("Stop", "handoff", ""),
		`{"expect":"only typed by a test that means to"}`,
	}
	worker := func(transcript string) []string {
		return []string{
			nativeHook("Stop", "assignment", "", transcript),
			`{"expect":"asmai inbox --dispatch 4\r"}`,
			nativeHook("UserPromptSubmit", "cancellation", "asmai inbox --dispatch 4", transcript),
			fakeRun(`asmai inbox --dispatch 4`, 0, "(?s)cancellation from leader@engineering.*the user dropped the greeting"),
			// It commits but does not push, so the cancellation is not
			// complete when its turn stops.
			fakeRun(`printf 'draft\n' > notes.txt && git add -A && git commit -q -m 'Unpushed work' && asmai effect commit "$(git rev-parse HEAD)"`, 0, "effect 1 recorded: commit"),
			nativeHook("Stop", "cancellation", "", transcript),
			fakeRun(`touch "$ASMAI_TEST_DIR/worker-stopped"`, 0, ""),
			`{"expect":"only typed by a test that means to"}`,
		}
	}
	r := firstRun(t, worker, leader, false, "worker-stopped")
	if got := len(journaled(t, "assignment.cancelling")); got != 1 {
		t.Fatalf("cancellations requested: %d", got)
	}
	if got := len(journaled(t, "assignment.cancelled")); got != 0 {
		t.Fatalf("a cancellation without a confirmed push was recorded: %d", got)
	}
	stopFactory(t)

	coordination := []string{
		hookStep("SessionStart", "", ""),
		`{"screen":"\u001b[?2004h> "}`,
		`{"expect":"only typed by a test that means to"}`,
	}
	engineering := []string{
		hookStep("SessionStart", "", ""),
		`{"screen":"\u001b[?2004h> "}`,
		`{"expect":"only typed by a test that means to"}`,
	}
	resumed := []string{
		nativeHook("SessionStart", "", "", r.transcript),
		`{"screen":"\u001b[?2004h> "}`,
		`{"expect":"asmai inbox --dispatch 5\r"}`,
		nativeHook("UserPromptSubmit", "resumption", "asmai inbox --dispatch 5", r.transcript),
		fakeRun(`asmai inbox --dispatch 5`, 0, "(?s)resumption from daemon.*Resumes dispatch 4.*Your owning leader has cancelled assignment 1.*cancelling"),
		fakeRun(`git push -q origin `+branch+` && asmai effect push "origin/`+branch+`@$(git rev-parse HEAD)" && git rev-parse HEAD > "$ASMAI_TEST_DIR/final-commit"`, 0, "effect 2 recorded: push"),
		nativeHook("Stop", "resumption", "", r.transcript),
		fakeRun(`touch "$ASMAI_TEST_DIR/worker-pushed"`, 0, ""),
		`{"expect":"only typed by a test that means to"}`,
	}
	secondRun(t, coordination, engineering, resumed)
	waitFor(t, "the resumed worker to push", touched(r.testDir, "worker-pushed"))
	waitFor(t, "the cancellation to complete", func() bool { return len(journaled(t, "assignment.cancelled")) == 1 })
	waitFor(t, "the cancelled worker to stop", func() bool { return workerOf(t, "worker1@engineering").State == "stopped" })

	data, err := os.ReadFile(filepath.Join(r.testDir, "final-commit"))
	if err != nil {
		t.Fatal(err)
	}
	final := strings.TrimSpace(string(data))
	if got := gitIn(t, r.fixture.origin, "rev-parse", "refs/heads/"+branch); got != final {
		t.Errorf("origin's assignment branch is at %s, want the worker's commit %s", got, final)
	}
	cancelled := decode[struct {
		Assignment int64  `json:"assignment"`
		Commit     string `json:"commit"`
		Pushed     bool   `json:"pushed"`
	}](t, journaled(t, "assignment.cancelled")[0])
	if cancelled.Assignment != 1 || cancelled.Commit != final || !cancelled.Pushed {
		t.Errorf("the assignment ended as %+v, want it cancelled with %s pushed", cancelled, final)
	}
	if got := len(journaled(t, "assignment.resumed")); got != 1 {
		t.Errorf("assignments resumed: %d", got)
	}
	if got := len(journaled(t, "assignment.needs_reconciliation")); got != 0 {
		t.Errorf("assignments held for reconciliation: %d", got)
	}
}
