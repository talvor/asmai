// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/fakeprovider"
)

// inputBox is a screen step drawing Claude Code's input box as it draws it:
// between two horizontal rules, its prompt mark and text, or its faint
// placeholder when there is none, with the cursor after the text.
func inputBox(text string) string {
	rule := strings.Repeat("─", 60)
	typed := text
	if text == "" {
		typed = "\x1b[2mTry \"fix lint errors\"\x1b[22m"
	}
	screen := fmt.Sprintf("\x1b[?2004h\x1b[2J\x1b[H%s\r\n❯ %s\r\n%s\x1b[2;%dH", rule, typed, rule, len(text)+3)
	step, _ := json.Marshal(struct {
		Screen string `json:"screen"`
	}{screen})
	return string(step)
}

// replyingFactory gives the test a running factory whose Coordination's
// leader plays coordination, and whose Engineering's leader, once the test's
// directory holds release, accepts the handoff Coordination sends it, which
// sends Coordination a reply as dispatch 2.
func replyingFactory(t *testing.T, dir string, coordination []string) {
	t.Helper()
	engineering := []string{
		`{"hook":"SessionStart","payload":{"hook_event_name":"SessionStart"}}`,
		fakeRun(`touch "$ASMAI_TEST_DIR/handed-off"; while [ ! -f "$ASMAI_TEST_DIR/release" ]; do sleep 0.02; done`, 0, ""),
		`{"screen":"\u001b[?2004h> "}`,
		`{"expect":"asmai inbox --dispatch 1\r"}`,
		`{"hook":"UserPromptSubmit","payload":{"hook_event_name":"UserPromptSubmit","prompt_id":"nudge-1","prompt":"asmai inbox --dispatch 1"}}`,
		fakeRun(`asmai inbox --dispatch 1`, 0, "handoff 1"),
		fakeRun(`asmai handoff accept --handoff 1`, 0, "reply dispatch 2"),
		`{"hook":"Stop","payload":{"hook_event_name":"Stop","prompt_id":"nudge-1"}}`,
		fakeRun(`touch "$ASMAI_TEST_DIR/replied"`, 0, ""),
		`{"expect":"only typed by a test that means to"}`,
	}
	t.Setenv(fakeprovider.ScriptEnv+"_ENGINEERING", writeScript(t, "engineering", engineering))
	readyFactory(t, coordination)
	registerHandoffFixture(t)
	if _, stderr, code := runAsmai(t, "start"); code != 0 {
		t.Fatal(stderr)
	}
}

// handingOff is Coordination's leader taking the user's message and handing
// it off to Engineering, its input box empty again by the end of its turn.
var handingOff = []string{
	`{"hook":"SessionStart","payload":{"hook_event_name":"SessionStart"}}`,
	inputBox(""),
	`{"expect":"Please implement fixture\r"}`,
	`{"hook":"UserPromptSubmit","payload":{"hook_event_name":"UserPromptSubmit","prompt":"Please implement fixture"}}`,
	fakeRun(`asmai job open --message latest --repository fixture --reading 'Implement fixture' --criterion 'Tests pass'`, 0, "Opened job 1"),
	fakeRun(`asmai handoff send --job 1 --to engineering --outcome 'Implement fixture' --decisions none --evidence none --constraints none --permissions none --criterion 'Tests pass'`, 0, "dispatch 1"),
	inputBox(""),
	`{"hook":"Stop","payload":{"hook_event_name":"Stop"}}`,
}

// replyStates returns the states the journal holds for the reply dispatch,
// in order.
func replyStates(t *testing.T) []string {
	t.Helper()
	var states []string
	for _, e := range journaled(t, "dispatch.changed") {
		var d struct {
			ID    int64  `json:"id"`
			State string `json:"state"`
		}
		json.Unmarshal(e.Data, &d)
		if d.ID == 2 {
			states = append(states, d.State)
		}
	}
	return states
}

// stillWaiting waits long enough for a nudge to have been typed, and says
// whether the reply is still waiting for one.
func stillWaiting(t *testing.T) bool {
	t.Helper()
	time.Sleep(500 * time.Millisecond)
	return slices.Equal(replyStates(t), []string{"created"})
}

// A reply that reaches Coordination while the user has unsent text in its
// input box waits: it is not typed while the user is in the conversation,
// nor once they leave it with the text still unsent, and is typed once they
// come back and submit the text.
func TestANudgeIsNeverTypedOverTheUsersUnsentText(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ASMAI_TEST_DIR", dir)
	coordination := append(slices.Clone(handingOff),
		`{"expect":"draft"}`,
		inputBox("draft"),
		fakeRun(`touch "$ASMAI_TEST_DIR/drafted"`, 0, ""),
		// Anything else typed here, a nudge included, ends the fake.
		`{"expect":"\r"}`,
		`{"hook":"UserPromptSubmit","payload":{"hook_event_name":"UserPromptSubmit","prompt":"draft"}}`,
		inputBox(""),
		`{"hook":"Stop","payload":{"hook_event_name":"Stop"}}`,
		`{"expect":"asmai inbox --dispatch 2\r"}`,
		`{"hook":"UserPromptSubmit","payload":{"hook_event_name":"UserPromptSubmit","prompt":"asmai inbox --dispatch 2"}}`,
		fakeRun(`asmai inbox --dispatch 2`, 0, "accepted from leader@engineering"),
		fakeRun(`touch "$ASMAI_TEST_DIR/fetched"`, 0, ""),
		`{"hook":"Stop","payload":{"hook_event_name":"Stop"}}`,
		`{"expect":"only typed by a test that means to"}`,
	)
	replyingFactory(t, dir, coordination)

	a := inTerminal(t, 100, 25)
	waitFor(t, "Coordination's input box", a.shows("❯ Try"))
	a.ptmx.WriteString("Please implement fixture\r")
	waitFor(t, "the handoff to Engineering", touched(dir, "handed-off"))
	a.ptmx.WriteString("draft")
	waitFor(t, "the user's unsent text", touched(dir, "drafted"))
	os.WriteFile(filepath.Join(dir, "release"), nil, 0o600)
	waitFor(t, "Engineering's reply", touched(dir, "replied"))
	if !stillWaiting(t) {
		t.Fatalf("with the user's text unsent in the conversation, the reply went %v, want it waiting", replyStates(t))
	}

	// Leaving with the text unsent does not let the nudge be typed over it.
	a.ptmx.WriteString("\x1d")
	if code := a.exited(t); code != 0 {
		t.Fatalf("leaving the conversation, asmai exited %d, want 0", code)
	}
	if !stillWaiting(t) {
		t.Fatalf("with the user's text left unsent in Coordination's input box, the reply went %v, want it waiting", replyStates(t))
	}

	// Submitting it lets the nudge be typed, once Coordination's turn ends.
	b := inTerminal(t, 100, 25, "chat")
	waitFor(t, "the user's unsent text", b.shows("❯ draft"))
	b.ptmx.WriteString("\r")
	waitFor(t, "the reply's nudge", touched(dir, "fetched"))
	if got := strings.Join(replyStates(t), ","); got != "created,unknown,nudged,delivered,working" {
		t.Errorf("the reply went %s, want created,unknown,nudged,delivered,working", got)
	}
	var texts []string
	for _, e := range journaled(t, "message.witnessed") {
		var m struct {
			Text string `json:"text"`
		}
		json.Unmarshal(e.Data, &m)
		texts = append(texts, m.Text)
	}
	if want := []string{"Please implement fixture", "draft"}; !slices.Equal(texts, want) {
		t.Errorf("the witnessed messages are %q, want %q", texts, want)
	}
}

// A reply that reaches Coordination while the user holds its input waits
// until they leave the conversation, and is typed then. Esc and the other
// keys the user types in the middle of that automated turn reach
// Coordination.
func TestTheUsersKeysReachCoordinationInTheMiddleOfAnAutomatedTurn(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ASMAI_TEST_DIR", dir)
	coordination := append(slices.Clone(handingOff),
		// The user types a key and erases it.
		`{"expect":"x\u007f"}`,
		inputBox(""),
		fakeRun(`touch "$ASMAI_TEST_DIR/erased"`, 0, ""),
		`{"expect":"asmai inbox --dispatch 2\r"}`,
		`{"hook":"UserPromptSubmit","payload":{"hook_event_name":"UserPromptSubmit","prompt":"asmai inbox --dispatch 2"}}`,
		`{"screen":"\u001b[HWorking (esc to interrupt)"}`,
		fakeRun(`touch "$ASMAI_TEST_DIR/automated"`, 0, ""),
		`{"expect":"\u001b"}`,
		fakeRun(`touch "$ASMAI_TEST_DIR/escaped"`, 0, ""),
		`{"expect":"typed mid-turn\t\u007f\u0001"}`,
		fakeRun(`touch "$ASMAI_TEST_DIR/typed"`, 0, ""),
		fakeRun(`asmai inbox --dispatch 2`, 0, "accepted from leader@engineering"),
		`{"hook":"Stop","payload":{"hook_event_name":"Stop"}}`,
		`{"expect":"only typed by a test that means to"}`,
	)
	replyingFactory(t, dir, coordination)

	a := inTerminal(t, 100, 25)
	waitFor(t, "Coordination's input box", a.shows("❯ Try"))
	a.ptmx.WriteString("Please implement fixture\r")
	waitFor(t, "the handoff to Engineering", touched(dir, "handed-off"))
	a.ptmx.WriteString("x\x7f")
	waitFor(t, "the user's key erased", touched(dir, "erased"))
	os.WriteFile(filepath.Join(dir, "release"), nil, 0o600)
	waitFor(t, "Engineering's reply", touched(dir, "replied"))
	if !stillWaiting(t) {
		t.Fatalf("with the user holding Coordination's input, the reply went %v, want it waiting", replyStates(t))
	}

	// Leaving lets the nudge be typed.
	a.ptmx.WriteString("\x1d")
	if code := a.exited(t); code != 0 {
		t.Fatalf("leaving the conversation, asmai exited %d, want 0", code)
	}
	waitFor(t, "the reply's automated turn", touched(dir, "automated"))

	// Back in the conversation, the user's keys reach Coordination in the
	// middle of that turn, Esc included.
	b := inTerminal(t, 100, 25, "chat")
	waitFor(t, "Coordination's automated turn", b.shows("Working (esc to interrupt)"))
	b.ptmx.WriteString("\x1b")
	waitFor(t, "Esc to reach Coordination", touched(dir, "escaped"))
	b.ptmx.WriteString("typed mid-turn\t\x7f\x01")
	waitFor(t, "the other keys to reach Coordination", touched(dir, "typed"))
}
