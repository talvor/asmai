// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/talvor/asmai/internal/vt"
)

// Played on AsmAI's terminal emulation, the screens the pinned Claude Code
// drew in a recorded session show no input box at its trust prompt, then
// its input box empty under its faint placeholder, holding the text typed
// into it until Enter submits it, and empty again during the turn and after
// it.
func TestTheUnsentTextIsReadFromTheInputBoxClaudeCodeDraws(t *testing.T) {
	f, err := os.Open("../fakeprovider/testdata/recorded/claude-code-2.1.292-reply-ok.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	term := vt.New(80, 24)
	var read []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(nil, 1<<22)
	for scanner.Scan() {
		var step struct {
			Screen string `json:"screen"`
			Expect string `json:"expect"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &step); err != nil {
			t.Fatal(err)
		}
		term.Write([]byte(step.Screen))
		term.TakeResponses()
		// The keys the user typed, not the terminal's answers to Claude
		// Code's queries.
		if step.Expect != "" && !strings.HasPrefix(step.Expect, "\x1b") {
			text, readable := ClaudeCodeUnsentText(term.State())
			if !readable {
				text = "no input box"
			}
			read = append(read, text)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	// The trust prompt's Enter, the message and its Enter, then /exit and
	// its Enter.
	if want := []string{"no input box", "", "Reply with the single word: ok", "", "/exit"}; strings.Join(read, "|") != strings.Join(want, "|") {
		t.Errorf("the unsent text read before each of the user's keys is %q, want %q", read, want)
	}
}

func TestTheUnsentTextIsTheInputBoxsTextButItsPromptMarkAndPlaceholder(t *testing.T) {
	const rule = "\x1b[38;5;244m────────────────────────────────────────\x1b[0m"
	for _, tc := range []struct {
		name, screen string
		text         string
		readable     bool
	}{
		{"the placeholder", rule + "\r\n❯ \x1b[2mTry \"fix lint errors\"\x1b[22m\r\n" + rule + "\x1b[2;3H", "", true},
		{"typed text", rule + "\r\n❯ draft\r\n" + rule + "\x1b[2;8H", "draft", true},
		{"two lines", rule + "\r\n❯ first\r\n  second\r\n" + rule + "\x1b[3;9H", "first\nsecond", true},
		{"bash mode", rule + "\r\n! \x1b[2mRun a command\x1b[22m\r\n" + rule + "\x1b[2;3H", "!", true},
		{"no box", "> draft", "", false},
		{"a rule only above", rule + "\r\n❯ draft", "", false},
		{"the cursor outside the box", rule + "\r\n❯ draft\r\n" + rule + "\r\nbelow", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			term := vt.New(40, 10)
			term.Write([]byte(tc.screen))
			if text, readable := ClaudeCodeUnsentText(term.State()); text != tc.text || readable != tc.readable {
				t.Errorf("on\n%s\nthe unsent text is %q, readable %v, want %q, %v", term, text, readable, tc.text, tc.readable)
			}
		})
	}
}
