// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/talvor/asmai/internal/fakeprovider"
)

// printedBeforeTheConversation returns what asmai printed on the terminal
// before it took the screen for the live conversation, as lines.
func (a *attachedTerminal) printedBeforeTheConversation() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	before, _, _ := bytes.Cut(a.written, []byte("\x1b[?1049h"))
	var lines []string
	for line := range strings.SplitSeq(string(before), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// leaveFrom has the user leave the conversation in a, and waits for asmai to
// exit.
func leaveFrom(t *testing.T, a *attachedTerminal) {
	t.Helper()
	a.ptmx.WriteString("\x1d")
	if code := a.exited(t); code != 0 {
		t.Fatalf("leaving the conversation, asmai exited %d, want 0", code)
	}
}

// When nothing changed since the user left, and nothing waits on them, the
// catch-up is one line, printed before the live conversation; the first
// conversation says as much, and so does a return.
func TestTheCatchUpIsOneLineWhenNothingChanged(t *testing.T) {
	readyFactory(t, []string{
		`{"hook":"SessionStart","payload":{"hook_event_name":"SessionStart"}}`,
		inputBox(""),
		`{"expect":"only typed by a test that means to"}`,
	})
	if _, stderr, code := runAsmai(t, "start"); code != 0 {
		t.Fatal(stderr)
	}
	for _, visit := range []string{"first", "return"} {
		a := inTerminal(t, 100, 25)
		waitFor(t, "Coordination's input box", a.shows("❯ Try"))
		if got := a.printedBeforeTheConversation(); len(got) != 1 || got[0] != nothingChanged {
			t.Errorf("the %s visit printed %q before the conversation, want only %q", visit, got, nothingChanged)
		}
		leaveFrom(t, a)
	}
}

// The catch-up on a return offers back the text the user left unsent, without
// retyping it into Coordination's input box, and a lost sign-in shows
// Claude Code's own sign-in command, in the catch-up as in `asmai status`.
func TestTheCatchUpOffersTheUnsentTextBackAndShowsALostSignIn(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ASMAI_TEST_DIR", dir)
	signedOut := filepath.Join(dir, "signed-out")
	t.Setenv(fakeprovider.SignedOutFileEnv, signedOut)
	readyFactory(t, []string{
		`{"hook":"SessionStart","payload":{"hook_event_name":"SessionStart"}}`,
		inputBox(""),
		`{"expect":"draft"}`,
		inputBox("draft"),
		fakeRun(`touch "$ASMAI_TEST_DIR/drafted"`, 0, ""),
		// The saved text is cleared at the boundary, and never typed again.
		`{"expect":"\u000c"}`,
		inputBox(""),
		`{"expect":"only typed by a test that means to"}`,
	})
	if _, stderr, code := runAsmai(t, "start"); code != 0 {
		t.Fatal(stderr)
	}

	a := inTerminal(t, 100, 25)
	waitFor(t, "Coordination's input box", a.shows("❯ Try"))
	a.ptmx.WriteString("draft")
	waitFor(t, "the user's unsent text", touched(dir, "drafted"))
	leaveFrom(t, a)
	waitFor(t, "the text to be saved", func() bool { return len(journaled(t, "unsent.saved")) == 1 })

	if err := os.WriteFile(signedOut, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := runAsmai(t, "status")
	if code != 0 || !strings.Contains(stdout, "sign-in") || !strings.Contains(stdout, "sign in again with: ") || !strings.Contains(stdout, " auth login") {
		t.Errorf("asmai status printed %s%s (exit %d), want the lost sign-in with its native command", stdout, stderr, code)
	}
	var status struct {
		Events []struct {
			Kind    string `json:"kind"`
			Command string `json:"command"`
		} `json:"events"`
	}
	asmaiJSON(t, &status, "status")
	if len(status.Events) != 1 || status.Events[0].Kind != "sign-in" || !strings.HasSuffix(status.Events[0].Command, " auth login") {
		t.Errorf("asmai status --json lists the events %+v, want only the lost sign-in", status.Events)
	}

	b := inTerminal(t, 100, 25)
	waitFor(t, "Coordination's input box", b.shows("❯ Try"))
	printed := strings.Join(b.printedBeforeTheConversation(), "\n")
	for _, want := range []string{"Since you left:", "sign-in: Claude Code's sign-in is lost", "sign in again with: ", " auth login", "| draft"} {
		if !strings.Contains(printed, want) {
			t.Errorf("the catch-up printed before the conversation is\n%s\nwant it to hold %q", printed, want)
		}
	}
	if b.shows("draft")() {
		t.Errorf("the saved text was typed into the conversation again: %q", b.lines())
	}
	leaveFrom(t, b)

	// What was offered is not offered again.
	if texts, _ := savedUnsent(t); len(texts) != 1 {
		t.Errorf("the journal saved the unsent texts %q, want only the one", texts)
	}
	c := inTerminal(t, 100, 25)
	waitFor(t, "Coordination's input box", c.shows("❯ Try"))
	if printed := strings.Join(c.printedBeforeTheConversation(), "\n"); strings.Contains(printed, "draft") {
		t.Errorf("the next return printed %s, want the unsent text offered once", printed)
	}
}
