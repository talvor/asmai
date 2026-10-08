// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// conversationScript is Coordination's leader, played by the fake, in a
// conversation: it takes the user's message and confirms it submitted, is
// interrupted with Esc in the middle of its turn, and then receives text the
// user types but never submits.
var conversationScript = []string{
	`{"hook": "SessionStart", "payload": {"session_id": "fake-session", "hook_event_name": "SessionStart", "source": "startup"}}`,
	`{"screen": "\u001b[?2004h\u001b[2J\u001b[H> "}`,
	`{"expect": "hello coordination\r"}`,
	`{"screen": "hello coordination\r\n"}`,
	`{"hook": "UserPromptSubmit", "payload": {"session_id": "fake-session", "hook_event_name": "UserPromptSubmit", "prompt": "hello coordination"}}`,
	`{"screen": "Working (esc to interrupt)\r\n"}`,
	`{"expect": "\u001b"}`,
	`{"screen": "Interrupted\r\n> "}`,
	`{"run": "touch \"$ASMAI_TEST_DIR/interrupted\""}`,
	`{"expect": "draft never sent"}`,
	`{"run": "touch \"$ASMAI_TEST_DIR/drafted\""}`,
	`{"expect": "only typed by a test that means to"}`,
}

type journalEntry struct {
	ID   int64           `json:"id"`
	At   time.Time       `json:"at"`
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
}

// journaled returns the journal's entries of kind, with their IDs.
func journaled(t *testing.T, kind string) []journalEntry {
	t.Helper()
	var j struct {
		Journal []journalEntry `json:"journal"`
	}
	asmaiJSON(t, &j, "export")
	return slices.DeleteFunc(j.Journal, func(e journalEntry) bool { return e.Kind != kind })
}

// coordination returns Coordination's leader as asmai status shows it.
func coordination(t *testing.T) leaderStatusJSON {
	t.Helper()
	var status struct {
		Leaders []leaderStatusJSON `json:"leaders"`
	}
	asmaiJSON(t, &status, "status")
	if len(status.Leaders) == 0 || status.Leaders[0].Agent != "leader@coordination" {
		t.Fatalf("asmai status shows the leaders %+v, want leader@coordination first", status.Leaders)
	}
	return status.Leaders[0]
}

type leaderStatusJSON struct {
	leaderJSON
	Input        string `json:"input"`
	Conversation string `json:"conversation"`
}

func touched(dir, name string) func() bool {
	return func() bool {
		_, err := os.Stat(filepath.Join(dir, name))
		return err == nil
	}
}

func (a *attachedTerminal) shows(text string) func() bool {
	return func() bool {
		return slices.ContainsFunc(a.lines(), func(line string) bool { return strings.Contains(line, text) })
	}
}

func (a *attachedTerminal) bracketsPastes() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.term.BracketedPaste()
}

// exited waits for asmai to exit, and returns its exit code.
func (a *attachedTerminal) exited(t *testing.T) int {
	t.Helper()
	select {
	case <-a.done:
		return a.cmd.ProcessState.ExitCode()
	case <-time.After(10 * time.Second):
		t.Fatalf("asmai in %s did not exit", a.name)
		return 0
	}
}

func TestTheConversationAttachesTheUserToCoordinationAndJournalsWhatTheySubmit(t *testing.T) {
	testDir := t.TempDir()
	t.Setenv("ASMAI_TEST_DIR", testDir)
	readyFactory(t, conversationScript)
	var stopped struct{}
	asmaiJSON(t, &stopped, "stop")

	// A bare asmai starts the stopped factory, says so, and attaches this
	// terminal to Coordination's leader with input.
	a := inTerminal(t, 100, 25)
	waitFor(t, "the conversation", func() bool {
		lines := a.lines()
		return lines[0] == ">" && strings.Contains(lines[24], "leader@coordination · conversation · Ctrl-] leaves · started the factory")
	})
	waitFor(t, "this terminal to bracket pasted text, as Coordination's does", a.bracketsPastes)
	leader := coordination(t)
	if leader.State != "running" || leader.Generation != 1 || leader.Conversation != a.name || leader.Input != "automation" {
		t.Errorf("in the conversation, asmai status shows %+v, want generation 1 running, the conversation open in %s and automation owning the input", leader, a.name)
	}

	// The user's keys reach Coordination as typed, and the message they
	// submit is journaled word for word, with the agent, the terminal and
	// when, citing the provider's confirmation that it was submitted.
	a.ptmx.WriteString("hello coordination\r")
	waitFor(t, "the witnessed message", func() bool { return len(journaled(t, "message.witnessed")) == 1 })
	witnessed := journaled(t, "message.witnessed")[0]
	var message struct {
		Agent       string `json:"agent"`
		Role        string `json:"role"`
		Generation  int    `json:"generation"`
		Terminal    string `json:"terminal"`
		Text        string `json:"text"`
		Observation int64  `json:"observation"`
	}
	json.Unmarshal(witnessed.Data, &message)
	if message.Agent != "leader@coordination" || message.Role != "coordination" || message.Generation != 1 || message.Terminal != a.name || message.Text != "hello coordination" {
		t.Errorf("the witnessed message is journaled as %s, want the user's words typed in %s into generation 1 of leader@coordination", witnessed.Data, a.name)
	}
	if time.Since(witnessed.At) > time.Minute {
		t.Errorf("the witnessed message is journaled at %v, want when it was submitted", witnessed.At)
	}
	observations := journaled(t, "observation")
	submitted := slices.IndexFunc(observations, func(e journalEntry) bool {
		var o struct {
			Event string `json:"event"`
		}
		json.Unmarshal(e.Data, &o)
		return o.Event == "UserPromptSubmit"
	})
	if submitted < 0 || observations[submitted].ID != message.Observation {
		t.Errorf("the witnessed message cites observation %d, want the prompt submission's, in %+v", message.Observation, observations)
	}
	if leader := coordination(t); leader.Input != "automation" {
		t.Errorf("once the user's message was submitted, the leader's input is %q, want automation", leader.Input)
	}

	// Esc reaches Coordination in the middle of its turn.
	waitFor(t, "the leader's turn", a.shows("Working (esc to interrupt)"))
	a.ptmx.WriteString("\x1b")
	waitFor(t, "the leader to be interrupted", touched(testDir, "interrupted"))

	// Text the user types reaches Coordination, and while it is unsent the
	// user owns the leader's input.
	a.ptmx.WriteString("draft never sent")
	waitFor(t, "the user's draft", touched(testDir, "drafted"))
	if leader := coordination(t); leader.Input != "user" || leader.Conversation != a.name {
		t.Errorf("with the user's text unsent, asmai status shows %+v, want the user owning the input in the conversation in %s", leader, a.name)
	}

	// Ctrl-] leaves the conversation: by the time asmai exits, Coordination
	// is back under automation, and nothing is paused or ended.
	a.ptmx.WriteString("\x1d")
	if code := a.exited(t); code != 0 {
		t.Fatalf("leaving the conversation, asmai exited %d, want 0", code)
	}
	if leader := coordination(t); leader.State != "running" || leader.Generation != 1 || leader.Input != "automation" || leader.Conversation != "" {
		t.Errorf("after the user left the conversation, asmai status shows %+v, want generation 1 running under automation with no conversation", leader)
	}
	waitFor(t, "the user's terminal to stop bracketing pasted text", func() bool { return !a.bracketsPastes() })
	for _, want := range []string{"Started the factory: it was not running.", "Left the conversation. Nothing is paused: Coordination is back under automation."} {
		if !a.shows(want)() {
			t.Errorf("the terminal shows\n%s\nwant it to say %q", strings.Join(a.lines(), "\n"), want)
		}
	}
	var left struct {
		Agent      string `json:"agent"`
		Generation int    `json:"generation"`
		Terminal   string `json:"terminal"`
		How        string `json:"how"`
		Input      string `json:"input"`
	}
	if entered := journaled(t, "conversation.entered"); len(entered) != 1 {
		t.Errorf("the journal holds %d entries into the conversation, want 1", len(entered))
	}
	lefts := journaled(t, "conversation.left")
	if len(lefts) != 1 {
		t.Fatalf("the journal holds %d departures from the conversation, want 1", len(lefts))
	}
	json.Unmarshal(lefts[0].Data, &left)
	if left.Agent != "leader@coordination" || left.Generation != 1 || left.Terminal != a.name || left.How != "detached" || left.Input != "automation" {
		t.Errorf("leaving the conversation is journaled as %s, want the user detaching from %s and automation owning the input", lefts[0].Data, a.name)
	}
	// The text the user never submitted is no witnessed message.
	if witnessed := journaled(t, "message.witnessed"); len(witnessed) != 1 {
		t.Errorf("the journal holds %d witnessed messages, want only the one submitted", len(witnessed))
	}
	if ended := entries(t, "session.ended"); len(ended) != 0 {
		t.Errorf("the conversation ended the leader's session: %s", ended)
	}

	// With the factory running, asmai chat attaches without starting
	// anything, and while it is open the conversation is refused in another
	// terminal.
	b := inTerminal(t, 100, 25, "chat")
	waitFor(t, "the conversation again", func() bool {
		return strings.HasSuffix(strings.TrimRight(b.lines()[24], " "), "leader@coordination · conversation · Ctrl-] leaves")
	})
	second := inTerminal(t, 100, 25, "chat")
	if code := second.exited(t); code != 1 {
		t.Errorf("a second conversation exited %d, want 1", code)
	}
	if want := "the conversation is open in another terminal (" + b.name + ")"; !second.shows(want)() {
		t.Errorf("a second conversation printed\n%s\nwant it to say %q", strings.Join(second.lines(), "\n"), want)
	}
	var screen struct {
		Started bool       `json:"started"`
		Agent   leaderJSON `json:"agent"`
		Screen  struct {
			Lines []string `json:"lines"`
		} `json:"screen"`
	}
	asmaiJSON(t, &screen, "chat")
	if screen.Started || screen.Agent.Agent != "leader@coordination" || len(screen.Screen.Lines) != 24 {
		t.Errorf("asmai chat --json printed %+v, want Coordination's screen and nothing started", screen)
	}
	b.ptmx.WriteString("\x1d")
	if code := b.exited(t); code != 0 {
		t.Fatalf("leaving the conversation, asmai chat exited %d, want 0", code)
	}
	if b.shows("Started")() {
		t.Errorf("asmai chat with the factory running printed\n%s\nwant nothing started", strings.Join(b.lines(), "\n"))
	}
	if lefts := journaled(t, "conversation.left"); len(lefts) != 2 {
		t.Errorf("the journal holds %d departures from the conversation, want 2", len(lefts))
	}
}
