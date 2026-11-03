// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/talvor/asmai/internal/fakeprovider"
)

// The user interrupts Coordination's automated turn with Esc in the
// conversation. Claude Code reports no interrupt, so the turn's state is
// unknown: nothing is typed into Coordination, even after the user leaves,
// until Claude Code reports a finished turn. When the next Stop is another
// prompt's, the interrupted turn is recognised and journaled, and
// Coordination is nudged again with the same dispatch: it reads the same
// message again by its ID, which is not delivered twice, and is told to check
// its recorded asmai calls before acting again.
func TestAnInterruptedAutomatedTurnIsRecognisedAtTheNextStopAndNudgedAgain(t *testing.T) {
	gitIdentity(t)
	testDir := t.TempDir()
	t.Setenv("ASMAI_TEST_DIR", testDir)
	t.Setenv(fakeprovider.ScriptEnv+"_ENGINEERING", filepath.Join(testDir, "engineering.jsonl"))

	engineering := concat(agentSession,
		turn(1, "handoff",
			fakeRun(`asmai inbox --dispatch 1`, 0, "Add a greeting"),
			fakeRun(`asmai handoff accept --handoff 1`, 0, "reply dispatch 2"),
		),
		[]string{idleSession},
	)
	writeFiles(t, testDir, map[string][]string{"engineering.jsonl": engineering})
	leaderScript := []string{
		hookStep("SessionStart", "", ""),
		`{"screen":"\u001b[?2004h> "}`,
		`{"expect":"Please add a greeting\r"}`,
		hookStep("UserPromptSubmit", "", "Please add a greeting"),
		fakeRun(`asmai job open --message latest --repository fixture --reading 'Add a greeting' --criterion 'Greeting is printed'`, 0, "Opened job 1"),
		fakeRun(`asmai handoff send --job 1 --to engineering --outcome 'Add a greeting' --decisions none --evidence none --constraints none --permissions none --criterion 'Greeting is printed'`, 0, "dispatch 1"),
		hookStep("Stop", "", ""),
		// The automated turn reads the reply, and the user interrupts it:
		// Claude Code reports no Stop for it.
		`{"expect":"asmai inbox --dispatch 2\r"}`,
		hookStep("UserPromptSubmit", "reply", "asmai inbox --dispatch 2"),
		fakeRun(`asmai inbox --dispatch 2 > "$ASMAI_TEST_DIR/first-read"`, 0, ""),
		`{"screen":"Working (esc to interrupt)\r\n"}`,
		`{"expect":"\u001b"}`,
		`{"screen":"Interrupted\r\n> "}`,
		touchFile("interrupted"),
		// A nudge typed while the turn's state is unknown would arrive
		// here, in place of the user's message.
		`{"expect":"go on\r"}`,
		hookStep("UserPromptSubmit", "user", "go on"),
		hookStep("Stop", "user", ""),
		`{"expect":"asmai inbox --dispatch 2\r"}`,
		hookStep("UserPromptSubmit", "again", "asmai inbox --dispatch 2"),
		fakeRun(`asmai inbox --dispatch 2 > "$ASMAI_TEST_DIR/read-again"`, 0, ""),
		hookStep("Stop", "again", ""),
		touchFile("read-again-done"),
		idleSession,
	}
	readyFactory(t, leaderScript)
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

	a := inTerminal(t, 100, 25)
	waitFor(t, "Coordination's prompt", a.shows(">"))
	a.ptmx.WriteString("Please add a greeting\r")
	waitFor(t, "Coordination's automated turn", a.shows("Working (esc to interrupt)"))
	a.ptmx.WriteString("\x1b")
	waitFor(t, "Coordination to be interrupted", touched(testDir, "interrupted"))

	// With no signal of the interruption, the turn's state is unknown, and
	// leaving the conversation types nothing into Coordination.
	if leader := coordination(t); leader.Input != "unknown" {
		t.Errorf("after Esc in Coordination's automated turn, its input is %q, want unknown", leader.Input)
	}
	a.ptmx.WriteString("\x1d")
	if code := a.exited(t); code != 0 {
		t.Fatalf("leaving the conversation, asmai exited %d, want 0", code)
	}
	if leader := coordination(t); leader.Input != "unknown" || leader.State != "running" {
		t.Errorf("after the user left the interrupted conversation, asmai status shows %+v, want it running with its input unknown", leader)
	}
	if got := strings.Join(dispatchStates(t, 0)[2], ","); got != "created,unknown,nudged,delivered,working" {
		t.Errorf("before Claude Code reports a finished turn, dispatch 2 moved through %s, want it still working and not nudged again", got)
	}

	// The user's next message ends with a Stop of its own prompt, which
	// shows the interrupted turn is over.
	b := inTerminal(t, 100, 25, "chat")
	waitFor(t, "the conversation again", b.shows("leader@coordination · conversation"))
	b.ptmx.WriteString("go on\r")
	waitFor(t, "Coordination to read its messages again", touched(testDir, "read-again-done"))

	var interrupted []json.RawMessage
	for _, e := range entries(t, "dispatch.changed") {
		var c struct {
			Interrupted bool `json:"interrupted"`
		}
		if json.Unmarshal(e, &c) == nil && c.Interrupted {
			interrupted = append(interrupted, e)
		}
	}
	if len(interrupted) != 1 {
		t.Fatalf("the journal holds %d interrupted turns, want 1: %s", len(interrupted), interrupted)
	}
	var change struct {
		ID         int64  `json:"id"`
		Agent      string `json:"agent"`
		Generation int    `json:"generation"`
		From       string `json:"from"`
		State      string `json:"state"`
	}
	json.Unmarshal(interrupted[0], &change)
	if change.ID != 2 || change.Agent != "leader@coordination" || change.Generation != 1 || change.From != "working" || change.State != "nudged" {
		t.Errorf("the interrupted turn is journaled as %s, want dispatch 2 of generation 1 of leader@coordination moved from working back to nudged", interrupted[0])
	}
	if got, want := strings.Join(dispatchStates(t, 0)[2], ","), "created,unknown,nudged,delivered,working,nudged,unknown,nudged,delivered,working,stopped"; got != want {
		t.Errorf("dispatch 2 moved through %s, want %s: nudged again after the interruption and its turn finished", got, want)
	}

	// The same message is read again by its ID, and delivered once.
	first, err := os.ReadFile(filepath.Join(testDir, "first-read"))
	if err != nil {
		t.Fatal(err)
	}
	again, err := os.ReadFile(filepath.Join(testDir, "read-again"))
	if err != nil {
		t.Fatal(err)
	}
	header := regexp.MustCompile(`(?m)^message (\d+) \| dispatch 2 \|`)
	m1, m2 := header.FindSubmatch(first), header.FindSubmatch(again)
	if m1 == nil || m2 == nil || string(m1[1]) != string(m2[1]) {
		t.Fatalf("the first read printed\n%s\nand the read again\n%s\nwant the same message of dispatch 2 in both", first, again)
	}
	if strings.Contains(string(first), "was interrupted") {
		t.Errorf("the first read printed\n%s\nwant no word of an interruption", first)
	}
	for _, want := range []string{
		"Your turn for dispatch 2 was interrupted, and this is message " + string(m2[1]) + " read again by its ID.",
		"check what your recorded asmai calls already did",
		"never repeat one blindly",
	} {
		if !strings.Contains(string(again), want) {
			t.Errorf("the read again printed\n%s\nwant it to say %q", again, want)
		}
	}
	fetched := 0
	for _, e := range entries(t, "message.fetched") {
		var f struct {
			Dispatch int64 `json:"dispatch"`
		}
		if json.Unmarshal(e, &f) == nil && f.Dispatch == 2 {
			fetched++
		}
	}
	if fetched != 1 {
		t.Errorf("the message of dispatch 2 is journaled fetched %d times, want once", fetched)
	}
	if leader := coordination(t); leader.Input != "automation" {
		t.Errorf("once the interrupted turn was nudged again and finished, Coordination's input is %q, want automation", leader.Input)
	}
	if witnessed := journaled(t, "message.witnessed"); len(witnessed) != 2 {
		t.Errorf("the journal holds %d witnessed messages, want the user's two", len(witnessed))
	}
	b.ptmx.WriteString("\x1d")
	if code := b.exited(t); code != 0 {
		t.Fatalf("leaving the conversation, asmai chat exited %d, want 0", code)
	}
}
