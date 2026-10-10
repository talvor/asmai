// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/providers"
	"github.com/talvor/asmai/internal/store"
)

// runJournal builds a journal with ids in order from the entries it is given.
func runJournal(t *testing.T, entries ...store.Entry) []store.Entry {
	t.Helper()
	for i := range entries {
		entries[i].ID = int64(i + 1)
	}
	return entries
}

func entry(t *testing.T, kind string, data map[string]any) store.Entry {
	t.Helper()
	return journalEntry(t, 0, kind, data)
}

// beforeTheStop is a factory with a job open, a handoff to Engineering that
// its leader fetched and has not answered, and Coordination's reply to an
// earlier one that has stopped.
func beforeTheStop(t *testing.T) []store.Entry {
	t.Helper()
	return []store.Entry{
		entry(t, store.KindDaemonStarted, map[string]any{"previous": "new"}),
		entry(t, store.KindSessionStarted, map[string]any{"agent": "leader@coordination", "generation": 1}),
		entry(t, store.KindSessionStarted, map[string]any{"agent": "leader@engineering", "generation": 1}),
		entry(t, store.KindJobOpened, map[string]any{"number": 1}),
		entry(t, store.KindHandoffSent, map[string]any{"id": 1, "receiver": "leader@engineering"}),
		entry(t, store.KindMessageCreated, map[string]any{"dispatch": 1, "recipient": "leader@engineering", "kind": "handoff"}),
		entry(t, store.KindDispatchChanged, map[string]any{"id": 1, "agent": "leader@engineering", "state": "created"}),
		entry(t, store.KindDispatchChanged, map[string]any{"id": 1, "state": "delivered"}),
		entry(t, store.KindMessageCreated, map[string]any{"dispatch": 2, "recipient": "leader@coordination", "kind": "accepted"}),
		entry(t, store.KindDispatchChanged, map[string]any{"id": 2, "agent": "leader@coordination", "state": "stopped"}),
		entry(t, store.KindSessionEnded, map[string]any{"agent": "leader@coordination", "generation": 1, "by": "asmai stop"}),
		entry(t, store.KindSessionEnded, map[string]any{"agent": "leader@engineering", "generation": 1, "by": "asmai stop"}),
		entry(t, store.KindDaemonStopped, map[string]any{"by": "asmai stop"}),
	}
}

// afterTheStart is the start that follows: both leaders restored, each told
// to load its brief, and Engineering's open handoff nudged again.
func afterTheStart(t *testing.T) []store.Entry {
	t.Helper()
	nudge := func(agent string, generation int, dispatch string) store.Entry {
		return entry(t, store.KindObservation, map[string]any{"agent": agent, "generation": generation, "event": "UserPromptSubmit", "payload": map[string]any{"prompt": "asmai inbox --dispatch " + dispatch}})
	}
	return []store.Entry{
		entry(t, store.KindDaemonStarted, map[string]any{"previous": "stopped"}),
		entry(t, store.KindMessageCreated, map[string]any{"dispatch": 3, "recipient": "leader@coordination", "kind": "restoration"}),
		entry(t, store.KindDispatchChanged, map[string]any{"id": 3, "agent": "leader@coordination", "state": "created"}),
		entry(t, store.KindMessageCreated, map[string]any{"dispatch": 4, "recipient": "leader@engineering", "kind": "restoration"}),
		entry(t, store.KindDispatchChanged, map[string]any{"id": 4, "agent": "leader@engineering", "state": "created"}),
		entry(t, store.KindSessionStarted, map[string]any{"agent": "leader@coordination", "generation": 2}),
		entry(t, store.KindSessionStarted, map[string]any{"agent": "leader@engineering", "generation": 2}),
		nudge("leader@coordination", 2, "3"),
		entry(t, store.KindMessageFetched, map[string]any{"dispatch": 3, "agent": "leader@coordination"}),
		nudge("leader@engineering", 2, "4"),
		entry(t, store.KindMessageFetched, map[string]any{"dispatch": 4, "agent": "leader@engineering"}),
	}
}

func evidenceOf(t *testing.T, entries ...store.Entry) *restartEvidence {
	t.Helper()
	journal := runJournal(t, entries...)
	e := newRestartEvidence(lastStop(journal))
	for _, entry := range journal {
		e.record(entry)
	}
	return e
}

func TestARestartThatKeepsDispatchesCorrelatedHasNoProblems(t *testing.T) {
	e := evidenceOf(t, append(beforeTheStop(t), afterTheStart(t)...)...)

	problems, _ := e.problems([]string{"leader@coordination", "leader@engineering"})

	if len(problems) != 0 {
		t.Fatalf("a restart that kept everything has problems: %v", problems)
	}
	want := []string{"leader@coordination", "leader@engineering"}
	if !slices.Equal(e.restore, want) {
		t.Errorf("the leaders to restore are %v, want %v: Coordination, and Engineering whose handoff was open", e.restore, want)
	}
	if got := e.restoredBy("leader@engineering"); !slices.Equal(got, []int64{4}) {
		t.Errorf("Engineering's delivered, acknowledged dispatches are %v, want its restoration, 4", got)
	}
}

func TestAnAnsweredHandoffIsNoOpenWorkForEngineering(t *testing.T) {
	before := beforeTheStop(t)
	before = slices.Insert(before, len(before)-1, entry(t, store.KindHandoffAnswered, map[string]any{"id": 1}))
	e := evidenceOf(t, before...)

	if want := []string{"leader@coordination"}; !slices.Equal(e.restore, want) {
		t.Errorf("the leaders to restore are %v, want only %v", e.restore, want)
	}
}

func TestAnOpenAssignmentIsOpenWorkForItsOwner(t *testing.T) {
	before := beforeTheStop(t)
	before = slices.Insert(before, len(before)-1,
		entry(t, store.KindHandoffAnswered, map[string]any{"id": 1}),
		entry(t, store.KindAssignmentCreated, map[string]any{"id": 1, "owner": "leader@engineering", "worker": "worker1@engineering"}))
	e := evidenceOf(t, before...)

	if want := []string{"leader@coordination", "leader@engineering"}; !slices.Equal(e.restore, want) {
		t.Errorf("the leaders to restore are %v, want %v", e.restore, want)
	}
	if e.live != 1 {
		t.Errorf("%d assignments were live at the stop, want 1", e.live)
	}
}

func TestADispatchThatStoppedBeforeTheRestartMustNotChangeAfterIt(t *testing.T) {
	after := append(afterTheStart(t), entry(t, store.KindDispatchChanged, map[string]any{"id": 2, "generation": 2, "from": "stopped", "state": "working"}))
	e := evidenceOf(t, append(beforeTheStop(t), after...)...)

	problems, _ := e.problems([]string{"leader@coordination", "leader@engineering"})

	if len(problems) != 1 || !strings.Contains(problems[0], "dispatch 2 had stopped before the restart, and was changed after it") {
		t.Errorf("the problems are %v, want the stopped dispatch's change", problems)
	}
}

func TestALeaderThatIsNotRestoredOrNotDeliveredItsNudgeIsReportedAndWaitedFor(t *testing.T) {
	after := afterTheStart(t)
	// Engineering's session started, but its nudge was not acknowledged.
	after = slices.DeleteFunc(after, func(e store.Entry) bool {
		return e.Kind == store.KindObservation && strings.Contains(string(e.Data), "leader@engineering")
	})
	e := evidenceOf(t, append(beforeTheStop(t), after...)...)

	problems, waiting := e.problems([]string{"leader@coordination", "leader@engineering"})

	if !waiting || len(problems) != 1 || !strings.Contains(problems[0], "leader@engineering was restored in generation 2, but none of its dispatches was acknowledged") {
		t.Errorf("the problems are %v (waiting %v), want Engineering's missing acknowledgment", problems, waiting)
	}

	e = evidenceOf(t, beforeTheStop(t)...)
	if problems, waiting := e.problems([]string{"leader@coordination"}); !waiting || len(problems) != 1 || !strings.Contains(problems[0], "no start after the stop") {
		t.Errorf("before the start the problems are %v (waiting %v)", problems, waiting)
	}
}

func TestAStopThatDidNotEndEverySessionIsAProblem(t *testing.T) {
	before := slices.DeleteFunc(beforeTheStop(t), func(e store.Entry) bool {
		return e.Kind == store.KindSessionEnded && strings.Contains(string(e.Data), "leader@engineering")
	})
	e := evidenceOf(t, append(before, afterTheStart(t)...)...)

	problems, _ := e.problems([]string{"leader@coordination", "leader@engineering"})

	joined := strings.Join(problems, "; ")
	if !strings.Contains(joined, "the session of leader@engineering was not ended by asmai stop") || !strings.Contains(joined, "the stop left sessions with no recorded end: leader@engineering") {
		t.Errorf("the problems are %v, want the engineering session left running", problems)
	}
}

func TestAStopByASignalIsNotACleanStopByTheUser(t *testing.T) {
	before := beforeTheStop(t)
	before[len(before)-1] = entry(t, store.KindDaemonStopped, map[string]any{"by": "signal"})
	e := evidenceOf(t, append(before, afterTheStart(t)...)...)

	problems, _ := e.problems(nil)

	if len(problems) == 0 || !strings.Contains(problems[0], `stopped by "signal", not by asmai stop`) {
		t.Errorf("the problems are %v", problems)
	}
}

func TestEveryResultIsTiedToADispatchThatGaveItsWorkerTheAssignment(t *testing.T) {
	after := afterTheStart(t)
	after = append(after,
		entry(t, store.KindMessageCreated, map[string]any{"dispatch": 5, "recipient": "worker1@engineering", "kind": "resumption"}),
		entry(t, store.KindResultSubmitted, map[string]any{"assignment": 1, "dispatch": 5, "agent": "worker1@engineering"}),
		entry(t, store.KindResultSubmitted, map[string]any{"assignment": 1, "dispatch": 1, "agent": "worker1@engineering"}),
	)
	e := evidenceOf(t, append(beforeTheStop(t), after...)...)

	problems, _ := e.problems([]string{"leader@coordination", "leader@engineering"})

	if len(problems) != 1 || !strings.Contains(problems[0], "result of assignment 1 is tied to dispatch 1, which is not a dispatch that gave worker1@engineering the assignment") {
		t.Errorf("the problems are %v, want only the result tied to the handoff's dispatch", problems)
	}
}

// Everything the factory's earlier cases left in the same state directory
// is known to the case; their stops and starts are not its own.
func TestAnEarlierCasesStopIsNotTheStopOfTheCase(t *testing.T) {
	earlier := []store.Entry{
		entry(t, store.KindDaemonStarted, map[string]any{"previous": "new"}),
		entry(t, store.KindHandoffSent, map[string]any{"id": 1, "receiver": "leader@engineering"}),
		entry(t, store.KindJobOpened, map[string]any{"number": 1}),
		entry(t, store.KindDaemonStopped, map[string]any{"by": "asmai stop"}),
	}
	journal := append(earlier, append(beforeTheStop(t), afterTheStart(t)...)...)
	e := evidenceOf(t, journal...)

	if problems, _ := e.problems([]string{"leader@coordination", "leader@engineering"}); len(problems) != 0 {
		t.Errorf("problems with an earlier case's stop before: %v", problems)
	}
}

func TestCoordinationIsBusyWhileADispatchForItIsOnItsWayOrBeingWorkedOn(t *testing.T) {
	created := entry(t, store.KindDispatchChanged, map[string]any{"id": 7, "agent": "leader@coordination", "state": "created"})
	other := entry(t, store.KindDispatchChanged, map[string]any{"id": 8, "agent": "leader@engineering", "state": "created"})
	for state, busy := range map[string]bool{"unknown": true, "nudged": true, "delivered": true, "working": true, "stopped": false} {
		moved := entry(t, store.KindDispatchChanged, map[string]any{"id": 7, "generation": 1, "from": "created", "state": state})
		if got := coordinationBusy([]store.Entry{created, other, moved}); got != busy {
			t.Errorf("with its dispatch %s, Coordination is busy: %v, want %v", state, got, busy)
		}
	}
	if coordinationBusy([]store.Entry{other}) {
		t.Error("Coordination is busy for a dispatch to Engineering")
	}
}

// bySessionClaude stands in for the pinned Claude Code with a wrapper that
// plays the scripted fake with the script of the leader's nth session: it
// counts the sessions each kind of agent has started, so that a leader
// restored by a start plays a different script than the one before the stop.
type bySessionClaude struct {
	fakeClaude
	dir string
}

func (p bySessionClaude) Install(_ context.Context, f *Factory, pins providers.Pins) error {
	platform, _ := providers.Platform(runtime.GOOS, runtime.GOARCH)
	plan, err := pins.ClaudeCodePlan(platform, f.paths.Providers)
	if err != nil {
		return err
	}
	wrapper := fmt.Sprintf(`#!/bin/sh
dir=%q
case "$*" in
*"You are Coordination's leader in AsmAI"*) role=coordination ;;
*"You are Engineering's leader in AsmAI"*) role=engineering ;;
*) role=idle ;;
esac
n=0
[ -f "$dir/$role.count" ] && n=$(cat "$dir/$role.count")
n=$((n + 1))
echo "$n" > "$dir/$role.count"
script="$dir/$role-$n.jsonl"
[ -f "$script" ] || script="$dir/$role-1.jsonl"
ASMAI_FAKE_PROVIDER_SCRIPT="$script" exec %q "$@"
`, p.dir, fakeProvider)
	if err := os.MkdirAll(filepath.Dir(plan.Path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(plan.Path, []byte(wrapper), 0o755); err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(wrapper))
	_, err = daemon.Call(f.paths.Socket, daemon.Request{Command: daemon.CommandProviderInstalled, Provider: &store.ProviderInstall{
		Name: plan.Name, Version: plan.Version, Path: plan.Path, SHA256: hex.EncodeToString(sum[:]),
	}})
	return err
}

func writeSessionScript(t *testing.T, dir, name string, steps ...string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name+".jsonl"), []byte(strings.Join(steps, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func hook(event, promptID, prompt string) string {
	payload := map[string]string{"hook_event_name": event, "session_id": "fake-session", "transcript_path": "/fake/transcript.jsonl"}
	if promptID != "" {
		payload["prompt_id"] = promptID
	}
	if prompt != "" {
		payload["prompt"] = prompt
	}
	encoded, _ := json.Marshal(payload)
	return fmt.Sprintf(`{"hook":%q,"payload":%s}`, event, encoded)
}

func scriptRun(command string, status int, output string) string {
	return fmt.Sprintf(`{"run":%q,"status":%d,"output":%q}`, command, status, output)
}

func TestC19StopsTheFactoryStartsItAgainAndRequiresTheRestoredLeadersToBeDeliveredToInTheirNewSessions(t *testing.T) {
	scripts := t.TempDir()
	h := harness(t, bySessionClaude{fakeClaude: signedIn, dir: scripts}, startsDirectly)
	if len(h.QualificationPaths.Socket) > 103 {
		t.Skipf("the disposable state's socket path, %s, is longer than a Unix socket's path may be on macOS", h.QualificationPaths.Socket)
	}
	h.Host = "asmai-vm"
	h.Number = func() int64 { return 19 }
	prompt := h.exercisePrompt()
	t.Cleanup(func() {
		if t.Failed() {
			log, _ := os.ReadFile(h.QualificationPaths.Log)
			t.Logf("the daemon's log:\n%s", log)
		}
	})

	writeSessionScript(t, scripts, "idle-1", `{"screen":"\u001b[?2004h> "}`, `{"expect":"only typed by a test that means to"}`)
	// Before the stop, Coordination opens the job and hands it to
	// Engineering, whose leader fetches it and stays in its turn.
	writeSessionScript(t, scripts, "coordination-1",
		hook("SessionStart", "", ""),
		`{"screen":"\u001b[?2004h> "}`,
		fmt.Sprintf(`{"expect":%q}`, prompt),
		`{"expect":"\r"}`,
		hook("UserPromptSubmit", "", prompt),
		scriptRun(`asmai job open --message latest --repository qualification-handoff --reading 'Handoff test' --criterion 'Engineering confirms'`, 0, "Opened job 1"),
		scriptRun(`asmai handoff send --job 1 --to engineering --outcome 'Acknowledge it' --decisions none --evidence none --constraints none --permissions none --criterion 'Engineering confirms'`, 0, "dispatch 1"),
		hook("Stop", "", ""),
		`{"expect":"only typed by a test that means to"}`,
	)
	writeSessionScript(t, scripts, "engineering-1",
		hook("SessionStart", "", ""),
		`{"screen":"\u001b[?2004h> "}`,
		`{"expect":"asmai inbox --dispatch 1\r"}`,
		hook("UserPromptSubmit", "handoff", "asmai inbox --dispatch 1"),
		scriptRun(`asmai inbox --dispatch 1`, 0, "(?s)handoff 1.*Acknowledge it"),
		scriptRun(`sleep 600`, 0, ""),
	)
	// After the start, both are restored: Coordination is told to load the
	// brief of the open job, and Engineering to load it and then to answer
	// the handoff that was still open.
	writeSessionScript(t, scripts, "coordination-2",
		hook("SessionStart", "", ""),
		`{"screen":"\u001b[?2004h> "}`,
		`{"expect":"asmai inbox --dispatch 2\r"}`,
		hook("UserPromptSubmit", "restore", "asmai inbox --dispatch 2"),
		scriptRun(`asmai inbox --dispatch 2`, 0, "(?s)restoration from daemon.*asmai brief 1"),
		scriptRun(`asmai brief 1`, 0, "(?s)Handoffs:\n- 1 leader@coordination -> leader@engineering: pending"),
		hook("Stop", "restore", ""),
		`{"expect":"only typed by a test that means to"}`,
	)
	writeSessionScript(t, scripts, "engineering-2",
		hook("SessionStart", "", ""),
		`{"screen":"\u001b[?2004h> "}`,
		`{"expect":"asmai inbox --dispatch 3\r"}`,
		hook("UserPromptSubmit", "restore", "asmai inbox --dispatch 3"),
		scriptRun(`asmai inbox --dispatch 3`, 0, "(?s)restoration from daemon.*asmai brief 1"),
		hook("Stop", "restore", ""),
		`{"expect":"asmai inbox --dispatch 1\r"}`,
		hook("UserPromptSubmit", "handoff-again", "asmai inbox --dispatch 1"),
		scriptRun(`asmai inbox --dispatch 1`, 0, "(?s)handoff 1.*Acknowledge it"),
		hook("Stop", "handoff-again", ""),
		`{"expect":"only typed by a test that means to"}`,
	)

	r := result(t, h, "C19")

	if r.Outcome != Passed {
		t.Fatalf("C19 %s: %s", r.Outcome, r.Failure)
	}
	evidence := strings.Join(r.Evidence, "\n")
	for _, want := range []string{
		"leader@coordination (generation 1) was ended by asmai stop",
		"leader@engineering (generation 1) was ended by asmai stop",
		"leader@coordination was restored in generation 2",
		"leader@engineering was restored in generation 2",
		"no dispatch that had stopped before the restart changed after it",
	} {
		if !strings.Contains(evidence, want) {
			t.Errorf("C19's evidence\n%s\ndoes not say %q", evidence, want)
		}
	}
}
