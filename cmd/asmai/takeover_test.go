// SPDX-License-Identifier: Apache-2.0

package main

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// The newest terminal to attach takes the conversation. The older one is
// told in a line, only observes from then on, and the text it left unsent is
// saved as if the user had left. Each leaves in its own way.
func TestASecondAttachTakesTheConversationAndSavesTheOlderTerminalsUnsentText(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ASMAI_TEST_DIR", dir)
	readyFactory(t, []string{
		`{"hook":"SessionStart","payload":{"hook_event_name":"SessionStart"}}`,
		inputBox(""),
		`{"expect":"draft"}`,
		inputBox("draft"),
		fakeRun(`touch "$ASMAI_TEST_DIR/drafted"`, 0, ""),
		// Anything else typed here, by the older terminal included, ends the
		// fake.
		`{"expect":"only typed by a test that means to"}`,
	})

	a := inTerminal(t, 100, 25, "chat")
	waitFor(t, "Coordination's input box", a.shows("❯ Try"))
	a.ptmx.WriteString("draft")
	waitFor(t, "the user's unsent text", touched(dir, "drafted"))

	b := inTerminal(t, 100, 25, "chat")
	notice := "observing · the conversation moved to " + b.name + " · Ctrl-] detaches"
	waitFor(t, "the older terminal's notice", a.shows(notice))
	waitFor(t, "the newer terminal's conversation", b.shows("conversation · Ctrl-] leaves"))
	if leader := coordination(t); leader.Conversation != b.name {
		t.Errorf("after the second attach, asmai status shows the conversation in %q, want %s", leader.Conversation, b.name)
	}
	if line := strings.TrimRight(a.lines()[24], " "); strings.Contains(line, "conversation · Ctrl-] leaves") {
		t.Errorf("the older terminal's status line is %q, want it observing", line)
	}

	// The text is saved as if the user had left, before the newer terminal
	// entered.
	texts, hows := savedUnsent(t)
	if !slices.Equal(texts, []string{"draft"}) || !slices.Equal(hows, []string{"taken over"}) {
		t.Errorf("the journal saved the unsent texts %q, departures %q, want \"draft\" saved as the older terminal was taken over", texts, hows)
	}
	both := append(journaled(t, "conversation.entered"), journaled(t, "conversation.left")...)
	slices.SortFunc(both, func(x, y journalEntry) int { return int(x.ID - y.ID) })
	var seen []string
	for _, e := range both {
		seen = append(seen, e.Kind)
	}
	if want := []string{"conversation.entered", "conversation.left", "conversation.entered"}; !slices.Equal(seen, want) {
		t.Errorf("the journal holds %v, want %v", seen, want)
	}

	// What the older terminal types reaches nothing, and the daemon goes on
	// streaming the agent to it.
	a.ptmx.WriteString("more")
	time.Sleep(300 * time.Millisecond)
	if ended := entries(t, "session.ended"); len(ended) != 0 {
		t.Errorf("keys typed in the older terminal reached Coordination, ending its session: %s", ended)
	}

	// Detaching the older terminal leaves the conversation where it is.
	a.ptmx.WriteString("\x1d")
	if code := a.exited(t); code != 0 {
		t.Fatalf("detaching the older terminal, asmai exited %d, want 0", code)
	}
	if !a.shows("Detached from leader@coordination.")() {
		t.Errorf("the older terminal shows\n%s\nwant it to say it detached", strings.Join(a.lines(), "\n"))
	}
	if leader := coordination(t); leader.Conversation != b.name {
		t.Errorf("after the older terminal detached, asmai status shows the conversation in %q, want %s", leader.Conversation, b.name)
	}
	if lefts := journaled(t, "conversation.left"); len(lefts) != 1 {
		t.Errorf("the journal holds %d departures, want only the one the takeover caused", len(lefts))
	}

	b.ptmx.WriteString("\x1d")
	if code := b.exited(t); code != 0 {
		t.Fatalf("leaving the conversation, asmai exited %d, want 0", code)
	}
	if lefts := journaled(t, "conversation.left"); len(lefts) != 2 {
		t.Errorf("the journal holds %d departures, want 2", len(lefts))
	}
}
