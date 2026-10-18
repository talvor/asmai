// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/talvor/asmai/internal/store"
	"github.com/talvor/asmai/internal/vt"
)

const dashboardSample = ` asmai dashboard  factory running
 1 Agents  2 Jobs  3 Log
 ▲ Waiting on you: leader@coordination native prompt: Write ` + "`/x/qualification-dashboard.txt`" + ` · +1 more
 AGENT                STATUS                 JOB    INPUT      SINCE DOING
 leader@coordination  ▲ Waiting on you       -      automation <1m   native prompt: Write
 leader@engineering   ○ Idle (unknown)       -      automation <1m   dispatch state unknown
 leader@quality       ■ Stopped              -      -          <1m   session ended
`

func TestTheDashboardIsReadAsItIsDrawn(t *testing.T) {
	v := parseDashboard(strings.Split(dashboardSample, "\n"), vt.State{})

	if !v.running || v.stopped {
		t.Errorf("running = %v, stopped = %v, want the header to say the factory runs", v.running, v.stopped)
	}
	if want := "leader@coordination native prompt: Write `/x/qualification-dashboard.txt` · +1 more"; v.banner != want {
		t.Errorf("banner = %q, want %q", v.banner, want)
	}
	if want := []string{"leader@coordination", "leader@engineering", "leader@quality"}; !slices.Equal(v.order, want) {
		t.Errorf("agents = %q, want %q", v.order, want)
	}
	for agent, want := range map[string]string{"leader@coordination": "Waiting on you", "leader@engineering": "Idle", "leader@quality": "Stopped"} {
		if got := v.agents[agent].Status; got != want {
			t.Errorf("%s status = %q, want %q", agent, got, want)
		}
	}
	if !strings.Contains(v.agents["leader@coordination"].Line, "native prompt: Write") {
		t.Errorf("the row for leader@coordination = %q, want what it is doing", v.agents["leader@coordination"].Line)
	}
}

// The status cell of a Waiting on you row is not the banner, which has a colon.
func TestARowWaitingOnYouIsNotTheBanner(t *testing.T) {
	v := parseDashboard([]string{" asmai dashboard  factory running", "", "", " leader@coordination  ▲ Waiting on you   -  user  <1m  you hold its input"}, vt.State{})

	if v.banner != "" || v.agents["leader@coordination"].Status != "Waiting on you" {
		t.Errorf("banner = %q, status = %q, want no banner and a row", v.banner, v.agents["leader@coordination"].Status)
	}
}

func TestTheStoppedScreenIsRecognisedAndHoldsNothingOfTheFactory(t *testing.T) {
	stopped := parseDashboard([]string{" asmai dashboard  factory stopped", "", "The factory is not running.", "Nothing is shown. Start it with `asmai start`."}, vt.State{})
	if !stopped.stopped || stopped.running || leftOnStoppedScreen(stopped) != "" {
		t.Errorf("stopped = %v, running = %v, left = %q, want a clean stopped screen", stopped.stopped, stopped.running, leftOnStoppedScreen(stopped))
	}
	stale := parseDashboard(strings.Split(dashboardSample, "\n"), vt.State{})
	if leftOnStoppedScreen(stale) == "" {
		t.Error("a screen of agents and a banner was taken for a clean stopped screen")
	}
	banner := parseDashboard([]string{" asmai dashboard  factory stopped", " ▲ Waiting on you: a finding"}, vt.State{})
	if !strings.Contains(leftOnStoppedScreen(banner), "banner") {
		t.Errorf("left = %q, want the banner named", leftOnStoppedScreen(banner))
	}
}

func TestTheChildrenOfAProcessAreFoundInPsOutput(t *testing.T) {
	out := ` 100     1 /usr/bin/zsh
 200   100 /tmp/asmai dashboard
 201   200 /tmp/asmai attach leader@coordination
 202   201 sleep 1
 300   100 /tmp/asmai attach leader@quality
`
	if got, want := childCommands(out, 200), []string{"/tmp/asmai attach leader@coordination"}; !slices.Equal(got, want) {
		t.Errorf("children of 200 = %q, want %q", got, want)
	}
	if got := childCommands(out, 999); len(got) != 0 {
		t.Errorf("children of 999 = %q, want none", got)
	}
}

func TestOnlyTheOneTimeYesAllowsOnce(t *testing.T) {
	for option, want := range map[string]bool{
		"1. yes": true,
		"yes":    true,
		"2. yes, allow all edits during this session (shift+tab)": false,
		"3. no, and tell claude what to do differently (esc)":     false,
		"1. yes, and don't ask again for this command":            false,
		"": false,
	} {
		if got := isAllowOnce(option); got != want {
			t.Errorf("isAllowOnce(%q) = %v, want %v", option, got, want)
		}
	}
}

func TestTheJournalSaysWhatEachLeaderIs(t *testing.T) {
	entries := runJournal(t,
		entry(t, store.KindObservation, map[string]any{"agent": "leader@coordination", "generation": 1, "event": "SessionStart"}),
		entry(t, store.KindDispatchChanged, map[string]any{"id": 1, "agent": "leader@engineering", "state": "created"}),
		entry(t, store.KindDispatchChanged, map[string]any{"id": 1, "state": "delivered"}),
		entry(t, store.KindDispatchChanged, map[string]any{"id": 1, "state": "working"}),
		entry(t, store.KindObservation, map[string]any{"agent": "leader@coordination", "generation": 1, "event": "PermissionRequest"}),
	)
	running := func(agent string, generation int) store.Agent {
		return store.Agent{Agent: agent, State: store.AgentRunning, Generation: generation}
	}
	agents := []store.Agent{running("leader@coordination", 1), running("leader@engineering", 1)}

	got := journalStatuses(entries, agents)

	for agent, want := range map[string]string{"leader@coordination": "Waiting on you", "leader@engineering": "Working", "leader@quality": "Stopped"} {
		if got[agent] != want {
			t.Errorf("%s is %q, want %q", agent, got[agent], want)
		}
	}

	// A later event of the session shows it is past the prompt, a dispatch that
	// stopped is no longer worked on, and a session that ended is Stopped.
	entries = append(entries,
		entry(t, store.KindObservation, map[string]any{"agent": "leader@coordination", "generation": 1, "event": "Stop"}),
		entry(t, store.KindDispatchChanged, map[string]any{"id": 1, "state": "stopped"}),
	)
	agents[1].State = store.AgentStopped
	got = journalStatuses(entries, agents)
	if got["leader@coordination"] != "Idle" || got["leader@engineering"] != "Stopped" {
		t.Errorf("after the prompt and the dispatch ended, coordination = %q and engineering = %q, want Idle and Stopped", got["leader@coordination"], got["leader@engineering"])
	}
}

func TestAPermissionRequestAfterTheBaselineIsFoundForTheGenerationAsked(t *testing.T) {
	entries := runJournal(t,
		entry(t, store.KindObservation, map[string]any{"agent": "leader@coordination", "generation": 1, "event": "PermissionRequest"}),
		entry(t, store.KindObservation, map[string]any{"agent": "leader@coordination", "generation": 1, "event": "Stop"}),
		entry(t, store.KindObservation, map[string]any{"agent": "leader@coordination", "generation": 2, "event": "PermissionRequest"}),
	)
	if requested, finished := permissionRequestedSince(entries, 2, "leader@coordination", 1); requested || finished {
		t.Errorf("requested = %v, finished = %v, want neither: the request is before the baseline", requested, finished)
	}
	if requested, _ := permissionRequestedSince(entries, 2, "leader@coordination", 2); !requested {
		t.Error("the request of generation 2 after the baseline was not found")
	}
	over := runJournal(t,
		entry(t, store.KindObservation, map[string]any{"agent": "leader@coordination", "generation": 1, "event": "UserPromptSubmit"}),
		entry(t, store.KindObservation, map[string]any{"agent": "leader@coordination", "generation": 1, "event": "Stop"}),
	)
	if requested, finished := permissionRequestedSince(over, 0, "leader@coordination", 1); requested || !finished {
		t.Errorf("requested = %v, finished = %v, want a turn that finished without a request", requested, finished)
	}
}

// scriptedLeader is a leader that comes up at its prompt and then plays steps.
func scriptedLeader(steps ...string) []string {
	return append([]string{
		hook("SessionStart", "", ""),
		`{"screen":"\u001b[?2004h> "}`,
	}, steps...)
}

func dashboardHarness(t *testing.T, scripts string) *Harness {
	t.Helper()
	h := harness(t, bySessionClaude{fakeClaude: signedIn, dir: scripts}, startsDirectly)
	if len(h.QualificationPaths.Socket) > 103 {
		t.Skipf("the disposable state's socket path, %s, is longer than a Unix socket's path may be on macOS", h.QualificationPaths.Socket)
	}
	t.Cleanup(func() {
		if t.Failed() {
			log, _ := os.ReadFile(h.QualificationPaths.Log)
			t.Logf("the daemon's log:\n%s", log)
		}
	})
	writeSessionScript(t, scripts, "idle-1", `{"screen":"\u001b[?2004h> "}`, `{"expect":"only typed by a test that means to"}`)
	writeSessionScript(t, scripts, "setup-1", setupTrusting...)
	return h
}

func dashboardEvidence(r Result) string { return strings.Join(r.Evidence, "\n") }

func requireEvidence(t *testing.T, r Result, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(dashboardEvidence(r), want) {
			t.Errorf("%s's evidence\n%s\ndoes not say %q", r.ID, dashboardEvidence(r), want)
		}
	}
}

// C52 hands the terminal to the real attach client of the commit under test
// and back, twice, with a scripted leader that takes no key.
func TestC52HandsTheTerminalToAttachAndGetsItBackRestored(t *testing.T) {
	scripts := t.TempDir()
	h := dashboardHarness(t, scripts)
	writeSessionScript(t, scripts, "coordination-1", scriptedLeader(`{"expect":"only typed by a test that means to"}`)...)

	r := result(t, h, "C52")

	if r.Outcome != Passed {
		t.Fatalf("C52 %s: %s", r.Outcome, r.Failure)
	}
	requireEvidence(t, r,
		"round 1: Enter on leader@coordination ran `asmai attach leader@coordination` as a child process of the dashboard",
		"round 1: attach followed the terminal to 120x36",
		"round 1: Ctrl-] ended attach, and the dashboard redrew the Agents tab on the alternate screen at 120x36",
		"round 1: the keys 2 and 1, typed with no Enter, switched the tabs, so raw mode is back",
		"round 2: Ctrl-] ended attach, and the dashboard redrew the Agents tab on the alternate screen at 120x36",
		"the keys typed in attach did not reach it, and no user message was journaled",
		"q left the dashboard with exit status 0",
	)
}

// C54 stops and restores the factory twice under a running dashboard.
func TestC54ShowsAndClearsTheStoppedMessageAcrossTwoStopsAndStarts(t *testing.T) {
	scripts := t.TempDir()
	h := dashboardHarness(t, scripts)
	writeSessionScript(t, scripts, "coordination-1", scriptedLeader(`{"expect":"only typed by a test that means to"}`)...)

	r := result(t, h, "C54")

	if r.Outcome != Passed {
		t.Fatalf("C54 %s: %s", r.Outcome, r.Failure)
	}
	requireEvidence(t, r,
		"the dashboard showed the running factory: leader@coordination, leader@engineering, leader@quality",
		"round 1: asmai stop cleared the dashboard to the stopped message alone",
		"the dashboard cleared the stopped message and showed the restored factory (Coordination's leader in generation 2)",
		"round 2: asmai stop cleared the dashboard to the stopped message alone",
		"the dashboard cleared the stopped message and showed the restored factory (Coordination's leader in generation 3)",
	)
}

func TestTheDashboardCasesRunInTheDisposableState(t *testing.T) {
	for _, id := range []string{"C52", "C53", "C54"} {
		if !disposableState(id) {
			t.Errorf("%s does not run in the disposable state, where the fixture repository and the stops it makes belong", id)
		}
	}
}

// c53Scripts are the sessions of C53 in a factory whose leaders behave as the
// real ones do when the case is done well: Coordination's leader takes the
// user's unsent key and Backspace, raises a native prompt for a file write,
// opens a job and hands it to Engineering, reads the reply, and after a
// restart reads the restoration it is given; Engineering's leader fetches the
// handoff and accepts it. The permission menu starts on a choice that is
// remembered, so the case has to move up to the one-time Yes. A prompt that
// is not raised is made the leader's reply instead when noPrompt.
func c53Scripts(t *testing.T, h *Harness, scripts string, noPrompt bool) {
	t.Helper()
	var number int64
	h.Number = func() int64 { number++; return number }
	write := nativePromptWrite(h.Number())
	exercise := h.exercisePrompt()
	number = 0
	menu := func(selected int) string {
		options := []string{"1. Yes", "2. Yes, allow all edits during this session (shift+tab)", "3. No, and tell Claude what to do differently (esc)"}
		var b strings.Builder
		b.WriteString("\\u001b[2J\\u001b[HDo you want to create qualification-dashboard.txt?\\r\\n")
		for i, option := range options {
			if i+1 == selected {
				b.WriteString(" ❯ " + option + "\\r\\n")
			} else {
				b.WriteString("   " + option + "\\r\\n")
			}
		}
		return `{"screen":"` + b.String() + `"}`
	}
	permission := `{"hook":"PermissionRequest","payload":{"hook_event_name":"PermissionRequest","session_id":"fake-session","tool_name":"Write","tool_input":{"file_path":"/state/agents/leader@coordination/qualification-dashboard.txt"}}}`
	var prompted []string
	if noPrompt {
		prompted = []string{
			fmt.Sprintf(`{"expect":%q}`, write), `{"expect":"\r"}`,
			hook("UserPromptSubmit", "", write),
			`{"screen":"\u001b[2J\u001b[HI cannot write files.\r\n> "}`,
			hook("Stop", "", ""),
		}
	} else {
		prompted = []string{
			fmt.Sprintf(`{"expect":%q}`, write), `{"expect":"\r"}`,
			hook("UserPromptSubmit", "", write),
			permission,
			menu(2),
			`{"expect":"\u001b[A"}`,
			menu(1),
			`{"expect":"\r"}`,
			scriptRun(`sleep 1`, 0, ""),
			`{"screen":"\u001b[2J\u001b[Hdone\r\n> "}`,
			hook("Stop", "", ""),
		}
	}
	steps := append([]string{`{"expect":"x"}`, `{"expect":"\u007f"}`}, prompted...)
	steps = append(steps,
		fmt.Sprintf(`{"expect":%q}`, exercise), `{"expect":"\r"}`,
		hook("UserPromptSubmit", "", exercise),
		scriptRun(`asmai job open --message latest --repository qualification-handoff --reading 'Handoff test' --criterion 'Engineering confirms'`, 0, "Opened job 1"),
		scriptRun(`asmai handoff send --job 1 --to engineering --outcome 'Acknowledge it' --decisions none --evidence none --constraints none --permissions none --criterion 'Engineering confirms'`, 0, "dispatch 1"),
		hook("Stop", "", ""),
		`{"expect":"asmai inbox --dispatch 2\r"}`,
		hook("UserPromptSubmit", "reply", "asmai inbox --dispatch 2"),
		scriptRun(`asmai inbox --dispatch 2`, 0, "(?s)accepted"),
		scriptRun(`sleep 3`, 0, ""),
		hook("Stop", "reply", ""),
		`{"expect":"only typed by a test that means to"}`,
	)
	writeSessionScript(t, scripts, "coordination-1", scriptedLeader(steps...)...)
	writeSessionScript(t, scripts, "engineering-1", scriptedLeader(
		`{"expect":"asmai inbox --dispatch 1\r"}`,
		hook("UserPromptSubmit", "handoff", "asmai inbox --dispatch 1"),
		scriptRun(`asmai inbox --dispatch 1`, 0, "(?s)handoff 1.*Acknowledge it"),
		scriptRun(`asmai handoff accept --handoff 1 --answer 'Acknowledged'`, 0, ""),
		scriptRun(`sleep 3`, 0, ""),
		hook("Stop", "handoff", ""),
		`{"expect":"only typed by a test that means to"}`,
	)...)
	writeSessionScript(t, scripts, "coordination-2", scriptedLeader(
		`{"expect":"asmai inbox --dispatch 3\r"}`,
		hook("UserPromptSubmit", "restore", "asmai inbox --dispatch 3"),
		scriptRun(`asmai inbox --dispatch 3`, 0, "(?s)restoration from daemon.*asmai brief 1"),
		scriptRun(`asmai brief 1`, 0, "Handoffs:"),
		scriptRun(`sleep 3`, 0, ""),
		hook("Stop", "restore", ""),
		`{"expect":"only typed by a test that means to"}`,
	)...)
}

// C53 against a factory whose leaders behave as the real ones do.
func TestC53ShowsTheStatusesAndTheWaitingOnYouSetOfARealFactory(t *testing.T) {
	scripts := t.TempDir()
	h := dashboardHarness(t, scripts)
	c53Scripts(t, h, scripts, false)

	r := result(t, h, "C53")

	if r.Outcome != Passed {
		t.Fatalf("C53 %s: %s", r.Outcome, r.Failure)
	}
	requireEvidence(t, r,
		"at rest the dashboard showed leader@coordination Idle, and leader@engineering and leader@quality, which have not run, Stopped",
		"while the user held leader@coordination's input in the conversation, the dashboard showed it Waiting on you",
		"raised a native permission prompt, which its PermissionRequest hook reported",
		"once the user answered the prompt and the leader's turn finished, the dashboard showed leader@coordination Idle and the banner empty",
		"Needs reconciliation is not exercised",
	)
	for _, agent := range []string{"leader@coordination", "leader@engineering", "leader@quality"} {
		requireEvidence(t, r, agent+": the dashboard and the journal agreed on")
	}
	requireEvidence(t, r, "leader@coordination: the dashboard and the journal agreed on Idle, Working, Waiting on you")
}

// A leader that never raises a native prompt shows nothing of how one is
// shown, which the case says.
func TestC53IsInconclusiveWhenTheLeaderRaisesNoNativePrompt(t *testing.T) {
	scripts := t.TempDir()
	h := dashboardHarness(t, scripts)
	c53Scripts(t, h, scripts, true)

	r := result(t, h, "C53")

	if r.Outcome != Failed || !strings.Contains(r.Failure, "inconclusive: the leader finished its turn without raising a native permission prompt") {
		t.Fatalf("C53 %s: %s, want it inconclusive without a prompt", r.Outcome, r.Failure)
	}
}
