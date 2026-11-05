// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/fakeprovider"
)

// takeResult is what an `asmai take` printed and how it ended.
type takeResult struct {
	stdout, stderr string
	code           int
}

// startTake runs `asmai take` in the background, as a command of its own
// without a terminal, and returns the channel its result arrives on.
func startTake(t *testing.T, args ...string) <-chan takeResult {
	t.Helper()
	cmd := exec.Command(filepath.Join(asmaiBin, "asmai"), append([]string{"take"}, args...)...)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill() })
	done := make(chan takeResult, 1)
	go func() {
		err := cmd.Wait()
		code := 0
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		}
		done <- takeResult{out.String(), errOut.String(), code}
	}()
	return done
}

// resultOf waits for a take to end.
func resultOf(t *testing.T, done <-chan takeResult) takeResult {
	t.Helper()
	select {
	case r := <-done:
		return r
	case <-time.After(15 * time.Second):
		t.Fatal("asmai take did not end")
		return takeResult{}
	}
}

// takeStillWaiting reports whether done has no result after a moment.
func takeStillWaiting(done <-chan takeResult) (takeResult, bool) {
	select {
	case r := <-done:
		return r, false
	case <-time.After(700 * time.Millisecond):
		return takeResult{}, true
	}
}

// takeFactory gives the test a running factory in which Coordination's leader
// opens a job and hands it off to Engineering, whose leader plays
// engineering. Coordination's leader hands the job off a second time when the
// user's next message is "again". It returns the test's directory, and the
// terminal the user talks to Coordination in.
func takeFactory(t *testing.T, engineering []string) (testDir string, conversation *attachedTerminal) {
	t.Helper()
	gitIdentity(t)
	testDir = t.TempDir()
	t.Setenv("ASMAI_TEST_DIR", testDir)
	writeFiles(t, testDir, map[string][]string{"engineering.jsonl": engineering})
	t.Setenv(fakeprovider.ScriptEnv+"_ENGINEERING", filepath.Join(testDir, "engineering.jsonl"))
	coordination := []string{
		hookStep("SessionStart", "", ""),
		`{"screen":"\u001b[?2004h> "}`,
		`{"expect":"Please add a greeting\r"}`,
		hookStep("UserPromptSubmit", "", "Please add a greeting"),
		fakeRun(`asmai job open --message latest --repository fixture --reading 'Add a greeting' --criterion 'Greeting is printed'`, 0, "Opened job 1"),
		fakeRun(`asmai handoff send --job 1 --to engineering --outcome 'Add a greeting' --decisions none --evidence none --constraints none --permissions none --criterion 'Greeting is printed'`, 0, "dispatch 1"),
		hookStep("Stop", "", ""),
		`{"expect":"again\r"}`,
		hookStep("UserPromptSubmit", "again", "again"),
		fakeRun(`asmai handoff send --job 1 --to engineering --outcome 'Add another greeting' --decisions none --evidence none --constraints none --permissions none --criterion 'Greeting is printed'`, 0, "dispatch 2"),
		hookStep("Stop", "again", ""),
		touchFile("sent-again"),
		idleSession,
	}
	readyFactory(t, coordination)
	t.Cleanup(func() {
		if t.Failed() {
			log, _, _ := runAsmai(t, "log")
			t.Logf("the daemon's log:\n%s", log)
		}
	})
	if _, stderr, code := runAsmai(t, "start"); code != 0 {
		t.Fatal(stderr)
	}
	registerWritingFixture(t)
	conversation = inTerminal(t, 100, 25)
	waitFor(t, "Coordination's prompt", conversation.shows(">"))
	conversation.ptmx.WriteString("Please add a greeting\r")
	return testDir, conversation
}

// workingTurn is Engineering's first dispatch, which it works at until the
// test's directory holds finish-turn.
func workingTurn(more ...string) []string {
	return concat(agentSession,
		[]string{
			`{"expect":"asmai inbox --dispatch 1\r"}`,
			hookStep("UserPromptSubmit", "t1", "asmai inbox --dispatch 1"),
			touchFile("working"),
			awaitFile("finish-turn"),
			hookStep("Stop", "t1", ""),
		},
		more,
	)
}

// takenEntries returns what the journal holds of the user taking input.
func takenEntries(t *testing.T) []struct {
	Agent     string `json:"agent"`
	Terminal  string `json:"terminal"`
	How       string `json:"how"`
	MovedFrom string `json:"movedFrom"`
} {
	t.Helper()
	var taken []struct {
		Agent     string `json:"agent"`
		Terminal  string `json:"terminal"`
		How       string `json:"how"`
		MovedFrom string `json:"movedFrom"`
	}
	for _, e := range entries(t, "input.taken") {
		var one struct {
			Agent     string `json:"agent"`
			Terminal  string `json:"terminal"`
			How       string `json:"how"`
			MovedFrom string `json:"movedFrom"`
		}
		if err := json.Unmarshal(e, &one); err != nil {
			t.Fatal(err)
		}
		taken = append(taken, one)
	}
	return taken
}

// engineeringInput is who owns the input of Engineering's leader, as
// `asmai agents --json` and `asmai status` show it.
func engineeringInput(t *testing.T) string {
	t.Helper()
	var status struct {
		Leaders []struct {
			Agent string `json:"agent"`
			Input string `json:"input"`
		} `json:"leaders"`
	}
	asmaiJSON(t, &status, "status")
	for _, l := range status.Leaders {
		if l.Agent == "leader@engineering" {
			return l.Input
		}
	}
	t.Fatalf("asmai status shows no leader@engineering: %+v", status)
	return ""
}

// A take waits for the agent's turn boundary by default: the turn goes on
// uninterrupted, and the user holds the input once it has finished.
func TestATakeWaitsForTheTurnBoundaryByDefault(t *testing.T) {
	testDir, conversation := takeFactory(t, workingTurn(touchFile("turn-over"), idleSession))
	waitFor(t, "Engineering's turn", touched(testDir, "working"))

	done := startTake(t, "engineering")
	if r, waiting := takeStillWaiting(done); !waiting {
		t.Fatalf("asmai take ended at once, mid-turn, with %+v, want it waiting for the boundary", r)
	}
	if got := engineeringInput(t); got == "user" {
		t.Errorf("mid-turn, Engineering's input is already the user's")
	}
	// Nothing was typed into the turn: the script would stop at any key.
	if err := os.WriteFile(filepath.Join(testDir, "finish-turn"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r := resultOf(t, done)
	if r.code != 0 || !strings.Contains(r.stdout, "You hold leader@engineering's input: its turn finished.") {
		t.Fatalf("asmai take ended with %+v, want it to report the hold once the turn finished", r)
	}
	waitFor(t, "Engineering's script to run to its end", touched(testDir, "turn-over"))
	if got := engineeringInput(t); got != "user" {
		t.Errorf("after the take, Engineering's input is %q, want user", got)
	}
	taken := takenEntries(t)
	if len(taken) != 1 || taken[0].Agent != "leader@engineering" || taken[0].How != "boundary" {
		t.Errorf("the journal holds the takes %+v, want one at the boundary", taken)
	}
	_ = conversation
}

// With --interrupt the turn is interrupted with Esc and the user holds the
// input at once, without the turn having finished.
func TestATakeWithInterruptInterruptsTheTurn(t *testing.T) {
	engineering := concat(agentSession, []string{
		`{"expect":"asmai inbox --dispatch 1\r"}`,
		hookStep("UserPromptSubmit", "t1", "asmai inbox --dispatch 1"),
		`{"screen":"Working (esc to interrupt)\r\n"}`,
		touchFile("working"),
		`{"expect":"\u001b"}`,
		`{"screen":"Interrupted\r\n> "}`,
		touchFile("interrupted"),
		idleSession,
	})
	testDir, _ := takeFactory(t, engineering)
	waitFor(t, "Engineering's turn", touched(testDir, "working"))

	r := resultOf(t, startTake(t, "engineering", "--interrupt"))
	if r.code != 0 || !strings.Contains(r.stdout, "You hold leader@engineering's input: its turn was interrupted.") {
		t.Fatalf("asmai take --interrupt ended with %+v, want the hold reported at once", r)
	}
	waitFor(t, "Engineering to be interrupted", touched(testDir, "interrupted"))
	if got := engineeringInput(t); got != "user" {
		t.Errorf("after the take, Engineering's input is %q, want user", got)
	}
	taken := takenEntries(t)
	if len(taken) != 1 || taken[0].How != "interrupt" {
		t.Errorf("the journal holds the takes %+v, want one by interrupt", taken)
	}
}

// An automated draft the agent's input box holds is saved to the journal and
// cleared before the handover, and never typed again.
func TestATakeSavesAndClearsTheUnsentAutomatedDraft(t *testing.T) {
	engineering := workingTurn(
		inputBox("asmai inbox --dispatch 7"),
		touchFile("drafted"),
		// The take clears the box with Ctrl-L before the user holds it.
		`{"expect":"\f"}`,
		inputBox(""),
		touchFile("cleared"),
		idleSession,
	)
	testDir, _ := takeFactory(t, engineering)
	waitFor(t, "Engineering's turn", touched(testDir, "working"))
	if err := os.WriteFile(filepath.Join(testDir, "finish-turn"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the draft", touched(testDir, "drafted"))

	r := resultOf(t, startTake(t, "engineering"))
	if r.code != 0 || !strings.Contains(r.stdout, "asmai inbox --dispatch 7") {
		t.Fatalf("asmai take ended with %+v, want it to report the draft it saved", r)
	}
	waitFor(t, "the box to be cleared", touched(testDir, "cleared"))
	var saved []struct {
		Agent string `json:"agent"`
		Text  string `json:"text"`
	}
	for _, e := range entries(t, "unsent.saved") {
		var one struct {
			Agent string `json:"agent"`
			Text  string `json:"text"`
		}
		if err := json.Unmarshal(e, &one); err != nil {
			t.Fatal(err)
		}
		saved = append(saved, one)
	}
	if len(saved) != 1 || saved[0].Agent != "leader@engineering" || saved[0].Text != "asmai inbox --dispatch 7" {
		t.Errorf("the journal saved the unsent texts %+v, want the draft of leader@engineering", saved)
	}
}

// Automated messages for the agent wait in its inbox while the user holds
// its input, and are not typed into it.
func TestAutomatedMessagesWaitInTheInboxWhileTheUserHoldsTheInput(t *testing.T) {
	testDir, conversation := takeFactory(t, workingTurn(touchFile("turn-over"),
		// A nudge typed here would be a key the script is not waiting for.
		idleSession))
	waitFor(t, "Engineering's turn", touched(testDir, "working"))
	if err := os.WriteFile(filepath.Join(testDir, "finish-turn"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if r := resultOf(t, startTake(t, "engineering")); r.code != 0 {
		t.Fatalf("asmai take ended with %+v", r)
	}

	conversation.ptmx.WriteString("again\r")
	waitFor(t, "Coordination's second handoff", touched(testDir, "sent-again"))
	time.Sleep(time.Second)
	if got := strings.Join(dispatchStates(t, 0)[2], ","); got != "created" {
		t.Errorf("the message for the user's agent moved through %q, want it only created: waiting in the inbox", got)
	}
	if got := engineeringInput(t); got != "user" {
		t.Errorf("Engineering's input is %q, want it still the user's", got)
	}
	var status struct {
		Leaders []struct {
			Agent string `json:"agent"`
			State string `json:"state"`
		} `json:"leaders"`
	}
	asmaiJSON(t, &status, "status")
	for _, l := range status.Leaders {
		if l.Agent == "leader@engineering" && l.State != "running" {
			t.Errorf("Engineering's leader is %q after the message waited, want it running: nothing was typed into it", l.State)
		}
	}
}

// A newer take moves the hold: the older terminal's take, if it still waits
// for the boundary, ends, and one that had finished is moved.
func TestANewerTakeMovesTheHold(t *testing.T) {
	testDir, _ := takeFactory(t, workingTurn(touchFile("turn-over"), idleSession))
	waitFor(t, "Engineering's turn", touched(testDir, "working"))

	older := inTerminal(t, 100, 25, "take", "engineering")
	waitFor(t, "the older take to register", func() bool { return engineeringHoldPaused(t) })
	newer := inTerminal(t, 100, 25, "take", "engineering")
	if code := older.exited(t); code == 0 {
		t.Errorf("the older take, which a newer one replaced while it waited, exited %d, want it to fail", code)
	}
	if !older.shows("a newer take from " + newer.name + " holds leader@engineering's input now")() {
		t.Errorf("the older take shows %q, want it to name the newer take's terminal", older.lines())
	}
	if err := os.WriteFile(filepath.Join(testDir, "finish-turn"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if code := newer.exited(t); code != 0 {
		t.Fatalf("the newer take exited %d, want 0", code)
	}

	// Another take moves a hold that was handed over.
	last := inTerminal(t, 100, 25, "take", "engineering")
	if code := last.exited(t); code != 0 {
		t.Fatalf("the last take exited %d, want 0", code)
	}
	if !last.shows("the hold moved here from " + newer.name)() {
		t.Errorf("the last take shows %q, want it to say the hold moved from %s", last.lines(), newer.name)
	}
	taken := takenEntries(t)
	if len(taken) != 2 || taken[0].Terminal != newer.name || taken[0].How != "boundary" || taken[1].Terminal != last.name || taken[1].How != "moved" || taken[1].MovedFrom != newer.name {
		t.Errorf("the journal holds the takes %+v, want the newer terminal's at the boundary and the last one's moving it", taken)
	}
}

// engineeringHoldPaused reports whether the daemon's log says a take waits
// for Engineering's turn boundary.
func engineeringHoldPaused(t *testing.T) bool {
	t.Helper()
	log, _, _ := runAsmai(t, "log")
	return strings.Contains(log, "a take waits for the agent's turn boundary")
}

// The status line of an attached terminal shows the agent whose input the
// user holds.
func TestTheStatusLineShowsTheHeldInput(t *testing.T) {
	testDir, _ := takeFactory(t, workingTurn(touchFile("turn-over"), idleSession))
	waitFor(t, "Engineering's turn", touched(testDir, "working"))
	if err := os.WriteFile(filepath.Join(testDir, "finish-turn"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "Engineering's turn to end", touched(testDir, "turn-over"))

	watching := attachInTerminal(t, "coordination", 120, 25)
	waitFor(t, "the status line", watching.shows("observing"))
	if watching.shows("you hold")() {
		t.Fatalf("before any take, the status line shows %q, want no held input", watching.lines()[24])
	}
	if r := resultOf(t, startTake(t, "engineering")); r.code != 0 {
		t.Fatalf("asmai take ended with %+v", r)
	}
	waitFor(t, "the status line to show the held input", watching.shows("you hold leader@engineering's input"))
}

// An agent's credential cannot take: the command is the user's.
func TestAnAgentsCredentialCannotTake(t *testing.T) {
	engineering := concat(agentSession, []string{
		`{"expect":"asmai inbox --dispatch 1\r"}`,
		hookStep("UserPromptSubmit", "t1", "asmai inbox --dispatch 1"),
		fakeRun(`asmai take engineering`, 1, "an agent cannot run this command; ask the user to run `asmai take engineering`"),
		hookStep("Stop", "t1", ""),
		touchFile("tried"),
		idleSession,
	})
	testDir, _ := takeFactory(t, engineering)
	waitFor(t, "Engineering to try", touched(testDir, "tried"))
	if got := engineeringInput(t); got == "user" {
		t.Errorf("an agent's take left Engineering's input with the user")
	}
	if taken := takenEntries(t); len(taken) != 0 {
		t.Errorf("the journal holds the takes %+v after an agent tried, want none", taken)
	}
}

// Coordination's leader is the user's in the conversation already, and an
// agent that is not running cannot be taken.
func TestATakeRefusesCoordinationAndAnAgentThatIsNotRunning(t *testing.T) {
	readyFactory(t, []string{`{"screen":"\u001b[?2004h> "}`, idleSession})
	if _, stderr, code := runAsmai(t, "start"); code != 0 {
		t.Fatal(stderr)
	}
	for agent, want := range map[string]string{
		"coordination":    "asmai chat",
		"leader@planning": "is not running",
	} {
		if _, stderr, code := runAsmai(t, "take", agent); code != 1 || !strings.Contains(stderr, want) {
			t.Errorf("asmai take %s exited %d printing %q, want a refusal that says %q", agent, code, stderr, want)
		}
	}
}
