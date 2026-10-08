// SPDX-License-Identifier: Apache-2.0

// Package vt is AsmAI's own terminal emulation. The daemon feeds each agent's
// terminal output to a Terminal, which keeps the screen the provider drew as
// an xterm-compatible terminal would, and answers the provider's queries
// about the terminal. The attach client keeps a Terminal of its own from the
// daemon's State and output, and draws it on the user's terminal with a
// Renderer.
package vt

import (
	"fmt"
	"strings"
)

// Color is a cell's foreground or background color: the terminal's default,
// one of the 256 palette colors, or a 24-bit color.
type Color uint32

const (
	// DefaultColor is the terminal's own foreground or background color.
	DefaultColor Color = 0
	paletteColor Color = 1 << 24
	rgbColor     Color = 2 << 24
)

// Palette returns the palette color i: 0 to 7 are the ANSI colors, 8 to 15
// their bright forms, and 16 to 255 xterm's color cube and gray ramp.
func Palette(i uint8) Color { return paletteColor | Color(i) }

// RGB returns the 24-bit color r, g, b.
func RGB(r, g, b uint8) Color {
	return rgbColor | Color(r)<<16 | Color(g)<<8 | Color(b)
}

// sgr returns the SGR parameters that select c as the foreground color, or
// with background as the background color.
func (c Color) sgr(background bool) string {
	base := 30
	if background {
		base = 40
	}
	switch c &^ 0xffffff {
	case paletteColor:
		i := int(c & 0xff)
		switch {
		case i < 8:
			return fmt.Sprint(base + i)
		case i < 16:
			return fmt.Sprint(base + 60 + i - 8)
		default:
			return fmt.Sprintf("%d;5;%d", base+8, i)
		}
	case rgbColor:
		return fmt.Sprintf("%d;2;%d;%d;%d", base+8, c>>16&0xff, c>>8&0xff, c&0xff)
	default:
		return fmt.Sprint(base + 9)
	}
}

// Flags are a cell's renditions other than its colors.
type Flags uint16

const (
	Bold Flags = 1 << iota
	Faint
	Italic
	Underline
	Blink
	Inverse
	Invisible
	Strike
)

// Attr is how a cell is drawn.
type Attr struct {
	Fg    Color `json:"f,omitempty"`
	Bg    Color `json:"b,omitempty"`
	Flags Flags `json:"a,omitempty"`
}

// sgr returns the SGR sequence that selects a from any rendition.
func (a Attr) sgr() string {
	params := []string{"0"}
	for _, f := range []struct {
		flag  Flags
		param string
	}{{Bold, "1"}, {Faint, "2"}, {Italic, "3"}, {Underline, "4"}, {Blink, "5"}, {Inverse, "7"}, {Invisible, "8"}, {Strike, "9"}} {
		if a.Flags&f.flag != 0 {
			params = append(params, f.param)
		}
	}
	if a.Fg != DefaultColor {
		params = append(params, a.Fg.sgr(false))
	}
	if a.Bg != DefaultColor {
		params = append(params, a.Bg.sgr(true))
	}
	return "\x1b[" + strings.Join(params, ";") + "m"
}

// Cell is one cell of the screen. A character two cells wide is held by its
// first cell, marked Wide, and followed by a cell marked Cont.
type Cell struct {
	// Text is the character in the cell with any combining marks after it,
	// or "" for a blank cell.
	Text string `json:"t,omitempty"`
	Wide bool   `json:"w,omitempty"`
	Cont bool   `json:"c,omitempty"`
	Attr
}

// Cursor is the cursor and the state DECSC saves with it.
type Cursor struct {
	X int `json:"x"`
	Y int `json:"y"`
	// Attr is the rendition new characters are drawn with.
	Attr Attr `json:"attr"`
	// PendingWrap is set once a character is drawn in the last column: the
	// next character wraps to a new line first.
	PendingWrap bool `json:"pending_wrap,omitempty"`
	// Origin is DECOM, which makes the cursor's rows relative to the scroll
	// region; DECSC saves it with the cursor.
	Origin bool `json:"origin,omitempty"`
	// Charsets are the G0 and G1 character sets ('B' for ASCII, '0' for DEC
	// line drawing), and Shift selects G1 for drawing.
	Charsets [2]byte `json:"charsets"`
	Shift    bool    `json:"shift,omitempty"`
}

// Screen is one of the terminal's two screens, the main one and the
// alternate one full-screen programs use.
type Screen struct {
	Lines [][]Cell `json:"lines"`
	// Saved is the cursor DECSC saved on this screen.
	Saved *Cursor `json:"saved,omitempty"`
}

// Modes are the terminal modes the emulation keeps.
type Modes struct {
	// NoAutowrap is DECAWM reset: drawing in the last column does not wrap.
	NoAutowrap bool `json:"no_autowrap,omitempty"`
	// Insert is IRM: drawn characters shift the rest of the line right.
	Insert bool `json:"insert,omitempty"`
	// HiddenCursor is DECTCEM reset.
	HiddenCursor bool `json:"hidden_cursor,omitempty"`
}

// Parser is where the escape-sequence parser is between two writes, so that
// a sequence split across writes, or across a State and the output after it,
// is read whole.
type Parser struct {
	State   int    `json:"state,omitempty"`
	Pending []byte `json:"pending,omitempty"`
	// UTF8 holds the bytes of a character not yet complete.
	UTF8 []byte `json:"utf8,omitempty"`
}

// State is everything a Terminal holds, so that a Terminal made from it
// carries on exactly where the one it was taken from was.
type State struct {
	Columns   int    `json:"columns"`
	Rows      int    `json:"rows"`
	Main      Screen `json:"main"`
	Alt       Screen `json:"alt"`
	AltActive bool   `json:"alt_active,omitempty"`
	Cursor    Cursor `json:"cursor"`
	// Top and Bottom are the scroll region's first and last rows.
	Top    int   `json:"top"`
	Bottom int   `json:"bottom"`
	Modes  Modes `json:"modes"`
	// Tabs are the columns with a tab stop.
	Tabs   []int  `json:"tabs"`
	Title  string `json:"title,omitempty"`
	Parser Parser `json:"parser"`
}

// Terminal emulates a terminal of a fixed size, which Resize changes. It is
// not safe for concurrent use.
type Terminal struct {
	s         State
	tabs      []bool
	responses []byte
}

// New returns a terminal of columns by rows, cleared, with the cursor at the
// top left.
func New(columns, rows int) *Terminal {
	columns, rows = max(columns, 1), max(rows, 1)
	t := &Terminal{s: State{Columns: columns, Rows: rows}}
	t.reset()
	return t
}

// FromState returns a terminal holding s, as the terminal s was taken from
// held it.
func FromState(s State) (*Terminal, error) {
	if s.Columns < 1 || s.Rows < 1 || len(s.Main.Lines) != s.Rows || len(s.Alt.Lines) != s.Rows {
		return nil, fmt.Errorf("a terminal state of %d by %d holds %d and %d lines", s.Columns, s.Rows, len(s.Main.Lines), len(s.Alt.Lines))
	}
	for _, screen := range []Screen{s.Main, s.Alt} {
		for _, line := range screen.Lines {
			if len(line) != s.Columns {
				return nil, fmt.Errorf("a terminal state %d columns wide holds a line of %d", s.Columns, len(line))
			}
		}
	}
	t := &Terminal{s: s.clone()}
	t.s.Cursor.X = clamp(t.s.Cursor.X, 0, s.Columns-1)
	t.s.Cursor.Y = clamp(t.s.Cursor.Y, 0, s.Rows-1)
	if t.s.Top < 0 || t.s.Bottom >= s.Rows || t.s.Top >= t.s.Bottom {
		t.s.Top, t.s.Bottom = 0, s.Rows-1
	}
	t.tabs = make([]bool, s.Columns)
	for _, x := range s.Tabs {
		if x >= 0 && x < s.Columns {
			t.tabs[x] = true
		}
	}
	return t, nil
}

// State returns a copy of everything the terminal holds.
func (t *Terminal) State() State {
	s := t.s.clone()
	s.Tabs = []int{}
	for x, stop := range t.tabs {
		if stop {
			s.Tabs = append(s.Tabs, x)
		}
	}
	return s
}

func (s State) clone() State {
	c := s
	c.Main = s.Main.clone()
	c.Alt = s.Alt.clone()
	c.Tabs = append([]int(nil), s.Tabs...)
	c.Parser.Pending = append([]byte(nil), s.Parser.Pending...)
	c.Parser.UTF8 = append([]byte(nil), s.Parser.UTF8...)
	return c
}

func (s Screen) clone() Screen {
	c := Screen{Lines: make([][]Cell, len(s.Lines))}
	for i, line := range s.Lines {
		c.Lines[i] = append([]Cell(nil), line...)
	}
	if s.Saved != nil {
		saved := *s.Saved
		c.Saved = &saved
	}
	return c
}

// Size returns the terminal's columns and rows.
func (t *Terminal) Size() (columns, rows int) { return t.s.Columns, t.s.Rows }

// Cell returns the cell at column x and row y of the screen shown, or a blank
// cell outside it.
func (t *Terminal) Cell(x, y int) Cell {
	if x < 0 || y < 0 || x >= t.s.Columns || y >= t.s.Rows {
		return Cell{}
	}
	return t.screen().Lines[y][x]
}

// Cursor returns the cursor's column and row, and whether it is shown.
func (t *Terminal) Cursor() (x, y int, visible bool) {
	return t.s.Cursor.X, t.s.Cursor.Y, !t.s.Modes.HiddenCursor
}

// Title is the window title the program last set.
func (t *Terminal) Title() string { return t.s.Title }

// Lines returns the text of each row of the screen shown, without trailing
// blanks.
func (t *Terminal) Lines() []string {
	lines := make([]string, t.s.Rows)
	for y, line := range t.screen().Lines {
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
		lines[y] = strings.TrimRight(b.String(), " ")
	}
	return lines
}

// String returns the screen's text, one line per row.
func (t *Terminal) String() string {
	return strings.Join(t.Lines(), "\n")
}

// TakeResponses returns what the terminal answers the queries it was
// written since the last call, such as a cursor position report, for the
// caller to send back to the program as the terminal's input.
func (t *Terminal) TakeResponses() []byte {
	r := t.responses
	t.responses = nil
	return r
}

// Resize makes the terminal columns by rows. The screen keeps its top left;
// when the cursor would fall off the bottom, the lines above it move up.
func (t *Terminal) Resize(columns, rows int) {
	columns, rows = max(columns, 1), max(rows, 1)
	if columns == t.s.Columns && rows == t.s.Rows {
		return
	}
	shift := max(t.s.Cursor.Y-rows+1, 0)
	resizeScreen := func(s *Screen, shift int) {
		lines := s.Lines[shift:]
		resized := make([][]Cell, rows)
		for y := range resized {
			line := make([]Cell, columns)
			if y < len(lines) {
				copy(line, lines[y])
				if columns < len(lines[y]) && line[columns-1].Wide {
					line[columns-1] = Cell{Attr: line[columns-1].Attr}
				}
			}
			resized[y] = line
		}
		s.Lines = resized
		if s.Saved != nil {
			s.Saved.X = clamp(s.Saved.X, 0, columns-1)
			s.Saved.Y = clamp(s.Saved.Y-shift, 0, rows-1)
		}
	}
	if t.s.AltActive {
		resizeScreen(&t.s.Alt, shift)
		resizeScreen(&t.s.Main, 0)
	} else {
		resizeScreen(&t.s.Main, shift)
		resizeScreen(&t.s.Alt, 0)
	}
	tabs := make([]bool, columns)
	copy(tabs, t.tabs)
	for x := len(t.tabs); x < columns; x++ {
		tabs[x] = x%8 == 0 && x > 0
	}
	t.tabs = tabs
	t.s.Columns, t.s.Rows = columns, rows
	t.s.Cursor.X = clamp(t.s.Cursor.X, 0, columns-1)
	t.s.Cursor.Y = clamp(t.s.Cursor.Y-shift, 0, rows-1)
	t.s.Cursor.PendingWrap = false
	t.s.Top, t.s.Bottom = 0, rows-1
}

// reset is RIS: the terminal as New makes it, keeping its size.
func (t *Terminal) reset() {
	columns, rows := t.s.Columns, t.s.Rows
	t.s = State{Columns: columns, Rows: rows, Top: 0, Bottom: rows - 1}
	t.s.Main.Lines = blankLines(columns, rows)
	t.s.Alt.Lines = blankLines(columns, rows)
	t.s.Cursor.Charsets = [2]byte{'B', 'B'}
	t.tabs = make([]bool, columns)
	for x := 8; x < columns; x += 8 {
		t.tabs[x] = true
	}
}

func blankLines(columns, rows int) [][]Cell {
	lines := make([][]Cell, rows)
	for y := range lines {
		lines[y] = make([]Cell, columns)
	}
	return lines
}

func (t *Terminal) screen() *Screen {
	if t.s.AltActive {
		return &t.s.Alt
	}
	return &t.s.Main
}

func clamp(v, lo, hi int) int {
	return min(max(v, lo), hi)
}
