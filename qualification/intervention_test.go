// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/talvor/asmai/internal/store"
)

// box is the screen of Claude Code at its input box with text typed in it:
// the box between two rules, with the cursor after the text.
func box(text string) string {
	rule := strings.Repeat("─", 40)
	screen, _ := json.Marshal("\x1b[2J\x1b[H" + rule + "\r\n❯ " + text + "\r\n" + rule + fmt.Sprintf("\x1b[2;%dH", 3+len([]rune(text))))
	return `{"screen":` + string(screen) + `}`
}

// boxedLeader is Coordination's leader that comes up at an empty box and
// then plays steps.
func boxedLeader(steps ...string) []string {
	return append([]string{hook("SessionStart", "", ""), `{"screen":"\u001b[?2004h"}`, box("")}, steps...)
}

func TestC10PassesWhenTheUnsentTextIsReadSavedAndTheBoxCleared(t *testing.T) {
	scripts := t.TempDir()
	h := dashboardHarness(t, scripts)
	const draft = "a half written thought"
	writeSessionScript(t, scripts, "coordination-1", boxedLeader(
		expectText(draft), box(draft),
		`{"expect":"\u0003"}`, box(""),
		`{"expect":"asmai inbox --dispatch 1\r"}`,
		hook("UserPromptSubmit", "setup", "asmai inbox --dispatch 1"),
		scriptRun(`asmai inbox --dispatch 1`, 0, "(?s)setup"),
		hook("Stop", "setup", ""),
		`{"expect":"only typed by a test that means to"}`)...)

	r := result(t, h, "C10")

	if r.Outcome != Passed {
		t.Fatalf("C10 %s: %s", r.Outcome, r.Failure)
	}
	requireEvidence(t, r,
		`the unsent text "a half written thought" was read exactly`,
		"leaving the conversation saved the unsent text in the journal",
		"the box was cleared first (Ctrl-C), and its nudge was submitted exactly",
	)
}

func TestC10FailsWhenTheBoxIsNotCleared(t *testing.T) {
	scripts := t.TempDir()
	h := dashboardHarness(t, scripts)
	const draft = "a half written thought"
	h.Wait = 5e9
	writeSessionScript(t, scripts, "coordination-1", boxedLeader(
		expectText(draft), box(draft),
		`{"expect":"\u0003"}`, box(draft),
		`{"expect":"only typed by a test that means to"}`)...)

	r := result(t, h, "C10")

	if r.Outcome != Failed || !strings.Contains(r.Failure, "the nudge for the automated dispatch was not the only text typed, or never was") {
		t.Errorf("C10 %s: %q, want it to fail because the box kept the text", r.Outcome, r.Failure)
	}
}

func TestC13PassesWhenTheOlderTerminalOnlyObservesAndItsTextIsSaved(t *testing.T) {
	scripts := t.TempDir()
	h := dashboardHarness(t, scripts)
	const draft = "typed in the older terminal"
	writeSessionScript(t, scripts, "coordination-1", boxedLeader(
		expectText(draft), box(draft),
		`{"expect":"x"}`, box(draft+"x"),
		`{"expect":"\u007f"}`, box(draft),
		`{"expect":"only typed by a test that means to"}`)...)

	r := result(t, h, "C13")

	if r.Outcome != Passed {
		t.Fatalf("C13 %s: %s", r.Outcome, r.Failure)
	}
	requireEvidence(t, r,
		`the older terminal was told that "the newer terminal" took the conversation over`,
		`its unsent text "typed in the older terminal" was saved`,
		"a key the older terminal typed afterwards did not reach the agent",
		"the newer terminal's keys reach the agent",
	)
}

func TestC9PassesWhenTheStatusLineKeepsTheLastRowAtEveryHeight(t *testing.T) {
	scripts := t.TempDir()
	h := dashboardHarness(t, scripts)
	writeSessionScript(t, scripts, "coordination-1", boxedLeader(`{"expect":"only typed by a test that means to"}`)...)

	r := result(t, h, "C9")

	if r.Outcome != Passed {
		t.Fatalf("C9 %s: %s", r.Outcome, r.Failure)
	}
	requireEvidence(t, r, "at 24 rows the agent's screen is drawn in the 23 rows above the status line", "at 8 rows the agent's screen is drawn in the 7 rows above the status line")
}

func TestEntriesAfterAndDispatchToReadTheJournalFromABaseline(t *testing.T) {
	entries := []store.Entry{
		journalEntry(t, 1, store.KindDispatchChanged, map[string]any{"id": 1, "agent": "leader@coordination", "state": store.DispatchCreated}),
		journalEntry(t, 5, store.KindDispatchChanged, map[string]any{"id": 2, "agent": "leader@engineering", "state": store.DispatchCreated}),
		journalEntry(t, 6, store.KindDispatchChanged, map[string]any{"id": 3, "agent": "leader@coordination", "state": store.DispatchCreated}),
		journalEntry(t, 7, store.KindInputHeld, map[string]any{"agent": "leader@coordination"}),
	}
	if got := entriesAfter(entries, 4, store.KindDispatchChanged); len(got) != 2 {
		t.Errorf("entriesAfter found %d dispatch entries after the baseline, want 2", len(got))
	}
	if id, ok := dispatchTo(entries, 4, "leader@coordination"); !ok || id != 3 {
		t.Errorf("dispatchTo = %d, %v, want the dispatch created after the baseline, 3", id, ok)
	}
	if _, ok := dispatchTo(entries, 6, "leader@coordination"); ok {
		t.Error("dispatchTo found a dispatch created at or before the baseline")
	}
}

func expectText(text string) string {
	encoded, _ := json.Marshal(map[string]string{"expect": text})
	return string(encoded)
}
