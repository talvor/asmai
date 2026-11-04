// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"strings"

	"github.com/talvor/asmai/internal/vt"
)

// claudeCodePrompt is the mark Claude Code draws at the start of its input
// box, before the text typed into it.
const claudeCodePrompt = "❯"

// ClaudeCodeUnsentText reads the text typed into Claude Code's input box and
// not yet submitted from screen, the terminal Claude Code draws, and reports
// whether screen shows the box at all. Claude Code draws the box as the rows
// between two horizontal rules, with the cursor in it and its prompt mark at
// its start, and draws its placeholder suggestion faint: the box's text is
// what is drawn there but the mark and the faint cells. Any other mark at
// the start of the box, such as the one bash mode draws, is the user's.
func ClaudeCodeUnsentText(screen vt.State) (text string, readable bool) {
	lines := screen.Main.Lines
	if screen.AltActive {
		lines = screen.Alt.Lines
	}
	y := screen.Cursor.Y
	if y < 0 || y >= len(lines) || isRule(lines[y]) {
		return "", false
	}
	top, bottom := y-1, y+1
	for top >= 0 && !isRule(lines[top]) {
		top--
	}
	for bottom < len(lines) && !isRule(lines[bottom]) {
		bottom++
	}
	if top < 0 || bottom >= len(lines) {
		return "", false
	}
	var rows []string
	for i, line := range lines[top+1 : bottom] {
		var b strings.Builder
		for x, c := range line {
			switch {
			case c.Cont:
			case i == 0 && x == 0 && c.Text == claudeCodePrompt:
			case c.Text == "" || c.Flags&vt.Faint != 0:
				b.WriteByte(' ')
			default:
				b.WriteString(c.Text)
			}
		}
		rows = append(rows, strings.TrimSpace(b.String()))
	}
	return strings.TrimSpace(strings.Join(rows, "\n")), true
}

// isRule reports whether line is one of the horizontal rules Claude Code
// draws above and below its input box: a line that starts with the rule and
// is mostly rule.
func isRule(line []vt.Cell) bool {
	if len(line) == 0 || line[0].Text != "─" {
		return false
	}
	n := 0
	for _, c := range line {
		if c.Text == "─" {
			n++
		}
	}
	return 2*n >= len(line)
}
