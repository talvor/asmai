// SPDX-License-Identifier: Apache-2.0

package vt

import (
	"bytes"
	"fmt"
)

// Renderer draws a Terminal's screen on a real terminal, the way tmux draws a
// pane: it keeps what it drew last and writes only the cells that changed.
// The screen is drawn from the real terminal's top left, clipped to the
// window it is given.
type Renderer struct {
	drawn [][]Cell
}

// Invalidate makes the next Render draw every cell, as after the real
// terminal was cleared.
func (r *Renderer) Invalidate() {
	r.drawn = nil
}

// Render returns what draws t's screen on a real terminal into the window of
// columns by rows at its top left, leaving the cursor where t's is.
func (r *Renderer) Render(t *Terminal, columns, rows int) []byte {
	var b bytes.Buffer
	// Synchronized output, where the terminal has it, shows the frame whole.
	b.WriteString("\x1b[?2026h\x1b[?25l")
	if len(r.drawn) != rows || (rows > 0 && len(r.drawn[0]) != columns) {
		b.WriteString("\x1b[0m\x1b[2J")
		r.drawn = blankLines(columns, rows)
	}
	var pen Attr
	penSet := false
	cx, cy := -1, -1
	for y := range rows {
		for x := 0; x < columns; x++ {
			c := t.Cell(x, y)
			if c.Cont {
				continue
			}
			changed := c != r.drawn[y][x]
			if c.Wide {
				// A wide character that does not fit the window is drawn as
				// a blank.
				if x+1 >= columns {
					c = Cell{Attr: c.Attr}
				} else {
					changed = changed || t.Cell(x+1, y) != r.drawn[y][x+1]
				}
			}
			if !changed {
				continue
			}
			if cx != x || cy != y {
				fmt.Fprintf(&b, "\x1b[%d;%dH", y+1, x+1)
			}
			if !penSet || pen != c.Attr {
				b.WriteString(c.Attr.sgr())
				pen, penSet = c.Attr, true
			}
			text := c.Text
			if text == "" {
				text = " "
			}
			b.WriteString(text)
			r.drawn[y][x] = c
			cx, cy = x+1, y
			if c.Wide {
				r.drawn[y][x+1] = t.Cell(x+1, y)
				cx++
				x++
			}
		}
	}
	b.WriteString("\x1b[0m")
	x, y, visible := t.Cursor()
	if x < columns && y < rows {
		fmt.Fprintf(&b, "\x1b[%d;%dH", y+1, x+1)
		if visible {
			b.WriteString("\x1b[?25h")
		}
	}
	b.WriteString("\x1b[?2026l")
	return b.Bytes()
}
