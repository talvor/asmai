// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"os"
	"testing"

	"github.com/talvor/asmai/internal/vt"
)

// The pinned Claude Code carries an allow-list, and it starts empty.
func TestTheQualifiedModalsOfThePinnedClaudeCodeStartEmpty(t *testing.T) {
	data, err := os.ReadFile("../../pins.json")
	if err != nil {
		t.Fatal(err)
	}
	pins, err := ParsePins(data)
	if err != nil {
		t.Fatal(err)
	}
	if got := QualifiedModals(ClaudeCode, pins.ClaudeCode.Version); len(got) != 0 {
		t.Fatalf("the allow-list = %v, want empty", got)
	}
	if got := QualifiedModals(ClaudeCode, "0.0.0"); len(got) != 0 {
		t.Fatalf("an unqualified version has an allow-list: %v", got)
	}
}

// A modal may be answered only with a key that chooses nothing beyond what
// it offers.
func TestAModalWithoutASafeDefaultAnswerIsNotOnAnyAllowList(t *testing.T) {
	for name, m := range map[string]Modal{
		"no name":    {Shows: "x", Answer: AnswerEnter},
		"no text":    {Name: "n", Answer: AnswerEnter},
		"typed text": {Name: "n", Shows: "x", Answer: "/model opus\r"},
		"no answer":  {Name: "n", Shows: "x"},
	} {
		if m.Valid() == nil {
			t.Errorf("%s: a modal without a safe answer is valid", name)
		}
	}
	if err := (Modal{Name: "n", Shows: "x", Answer: AnswerEscape}).Valid(); err != nil {
		t.Fatal(err)
	}
}

func TestAModalIsRecognisedByTheTextItDraws(t *testing.T) {
	term := vt.New(40, 5)
	term.Write([]byte("\x1b[2J\x1b[HUpdate available\r\n[Enter] ok"))
	list := []Modal{{Name: "update", Shows: "Update available", Answer: AnswerEscape}}
	if m, ok := MatchModal(list, term.State()); !ok || m.Name != "update" {
		t.Fatalf("MatchModal = %v, %v", m, ok)
	}
	if _, ok := MatchModal(nil, term.State()); ok {
		t.Fatal("an empty allow-list matched a screen")
	}
	if ClaudeCodeAtInput(term.State()) {
		t.Fatal("a modal is not an input state")
	}
	ready := vt.New(40, 5)
	ready.Write([]byte("\x1b[?2004h> "))
	if !ClaudeCodeAtInput(ready.State()) {
		t.Fatal("the input modes are an input state")
	}
	if !ClaudeCodeAtInput(vt.New(40, 5).State()) {
		t.Fatal("a screen with nothing drawn is not a dialog")
	}
}
