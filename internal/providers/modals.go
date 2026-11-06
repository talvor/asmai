// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"fmt"
	"strings"

	"github.com/talvor/asmai/internal/vt"
)

// ModalAnswer is a safe default answer to a modal: one of the two keys that
// close a modal without choosing anything beyond what it already offers.
// Neither can change the provider, the model, the sign-in or the
// permissions, so an allow-list entry cannot be written to do so (03 rule
// 24).
type ModalAnswer string

const (
	// AnswerEnter takes the modal's default choice.
	AnswerEnter ModalAnswer = "\r"
	// AnswerEscape dismisses the modal.
	AnswerEscape ModalAnswer = "\x1b"
)

// Modal is one qualified modal of a provider version: a dialog the version
// draws over its input box that qualification showed is safe to answer
// without the user, and the answer to give.
type Modal struct {
	// Name says what the modal is, for the journal.
	Name string
	// Shows is the text the modal draws that no other input state does.
	Shows string
	// Answer is the safe default answer.
	Answer ModalAnswer
}

// Valid reports whether m can be on an allow-list: it is named, recognised
// by the text it draws, and answered by a safe answer.
func (m Modal) Valid() error {
	switch {
	case strings.TrimSpace(m.Name) == "":
		return fmt.Errorf("a qualified modal has a name")
	case strings.TrimSpace(m.Shows) == "":
		return fmt.Errorf("qualified modal %q has no text it is recognised by", m.Name)
	case m.Answer != AnswerEnter && m.Answer != AnswerEscape:
		return fmt.Errorf("qualified modal %q has no safe default answer: it is answered with Enter or Escape", m.Name)
	}
	return nil
}

// qualifiedModals is the allow-list of each qualified provider version, by
// provider and version. A version's list starts empty: a modal joins it only
// when qualification of that version shows it is safe to answer.
var qualifiedModals = map[string]map[string][]Modal{
	ClaudeCode: {},
}

// QualifiedModals returns the allow-list of the qualified version of
// provider: empty for a version with no qualified modal, and for one that is
// not qualified at all.
func QualifiedModals(provider, version string) []Modal {
	var modals []Modal
	for _, m := range qualifiedModals[provider][version] {
		if m.Valid() == nil {
			modals = append(modals, m)
		}
	}
	return modals
}

// ClaudeCodeAtInput reports whether screen shows Claude Code at an input
// state automation may type into: its input box, the explicit input modes
// the factory already waits for before it types anything (bracketed paste
// asked for and the cursor shown), a plain ">" prompt, or nothing drawn yet. Any other screen
// shows something else over the input, such as a dialog.
func ClaudeCodeAtInput(screen vt.State) bool {
	if _, readable := ClaudeCodeUnsentText(screen); readable {
		return true
	}
	if !screen.AltActive && screen.Modes.BracketedPaste && !screen.Modes.HiddenCursor {
		return true
	}
	if screen.AltActive {
		return false
	}
	// A plain ">" prompt is a prompt only a terminal that is not Claude Code
	// draws, which draws a dialog's choices with its own mark.
	text := ScreenText(screen)
	last := text[strings.LastIndex(text, "\n")+1:]
	return text == "" || last == ">" || strings.HasPrefix(last, "> ")
}

// MatchModal returns the modal of list that screen shows, if any.
func MatchModal(list []Modal, screen vt.State) (Modal, bool) {
	text := ScreenText(screen)
	for _, m := range list {
		if m.Valid() == nil && strings.Contains(text, m.Shows) {
			return m, true
		}
	}
	return Modal{}, false
}

// ScreenText returns the text screen shows, row by row.
func ScreenText(screen vt.State) string {
	lines := screen.Main.Lines
	if screen.AltActive {
		lines = screen.Alt.Lines
	}
	rows := make([]string, 0, len(lines))
	for _, line := range lines {
		var b strings.Builder
		for _, c := range line {
			switch {
			case c.Cont:
			case c.Text == "":
				b.WriteByte(' ')
			default:
				b.WriteString(c.Text)
			}
		}
		rows = append(rows, strings.TrimRight(b.String(), " "))
	}
	return strings.TrimSpace(strings.Join(rows, "\n"))
}
