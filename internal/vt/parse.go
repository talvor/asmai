// SPDX-License-Identifier: Apache-2.0

package vt

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The parser's states.
const (
	stateGround = iota
	stateEscape
	stateEscapeIntermediate
	stateCSI
	stateCSIIgnore
	stateOSC
	stateOSCEscape
	stateString
	stateStringEscape
)

const (
	// maxCSI and maxOSC bound what the parser keeps of one sequence.
	maxCSI = 256
	maxOSC = 4096
)

// Write feeds p, the program's output, to the terminal. It never fails.
func (t *Terminal) Write(p []byte) (int, error) {
	for _, b := range p {
		t.feed(b)
	}
	return len(p), nil
}

func (t *Terminal) feed(b byte) {
	ps := &t.s.Parser
	switch ps.State {
	case stateGround:
		t.ground(b)
	case stateEscape:
		switch {
		case b == 0x1b:
		case b == 0x18 || b == 0x1a:
			ps.State = stateGround
		case b < 0x20:
			t.control(b)
		case b == '[':
			ps.State, ps.Pending = stateCSI, nil
		case b == ']':
			ps.State, ps.Pending = stateOSC, nil
		case b == 'P' || b == 'X' || b == '^' || b == '_':
			ps.State = stateString
		case b >= 0x20 && b <= 0x2f:
			ps.State, ps.Pending = stateEscapeIntermediate, []byte{b}
		default:
			ps.State = stateGround
			t.escape(nil, b)
		}
	case stateEscapeIntermediate:
		switch {
		case b == 0x1b:
			ps.State = stateEscape
		case b == 0x18 || b == 0x1a:
			ps.State = stateGround
		case b < 0x20:
			t.control(b)
		case b <= 0x2f:
			ps.Pending = append(ps.Pending, b)
		default:
			ps.State = stateGround
			t.escape(ps.Pending, b)
		}
	case stateCSI, stateCSIIgnore:
		switch {
		case b == 0x1b:
			ps.State = stateEscape
		case b == 0x18 || b == 0x1a:
			ps.State = stateGround
		case b < 0x20:
			t.control(b)
		case b <= 0x3f:
			if len(ps.Pending) >= maxCSI {
				ps.State = stateCSIIgnore
			} else {
				ps.Pending = append(ps.Pending, b)
			}
		case b <= 0x7e:
			ignore := ps.State == stateCSIIgnore
			ps.State = stateGround
			if !ignore {
				t.csi(ps.Pending, b)
			}
		default:
			ps.State = stateGround
		}
	case stateOSC:
		switch b {
		case 0x07:
			ps.State = stateGround
			t.osc(ps.Pending)
		case 0x1b:
			ps.State = stateOSCEscape
		case 0x18, 0x1a:
			ps.State = stateGround
		default:
			if len(ps.Pending) < maxOSC {
				ps.Pending = append(ps.Pending, b)
			}
		}
	case stateOSCEscape:
		ps.State = stateGround
		t.osc(ps.Pending)
		if b != '\\' {
			ps.State = stateEscape
			t.feed(b)
		}
	case stateString:
		switch b {
		case 0x1b:
			ps.State = stateStringEscape
		case 0x18, 0x1a:
			ps.State = stateGround
		}
	case stateStringEscape:
		ps.State = stateGround
		if b != '\\' {
			ps.State = stateEscape
			t.feed(b)
		}
	default:
		ps.State = stateGround
	}
}

// ground handles a byte outside any escape sequence: a control, or part of a
// character to draw.
func (t *Terminal) ground(b byte) {
	ps := &t.s.Parser
	if len(ps.UTF8) > 0 {
		if b >= 0x80 && b <= 0xbf {
			ps.UTF8 = append(ps.UTF8, b)
			if utf8.FullRune(ps.UTF8) {
				r, _ := utf8.DecodeRune(ps.UTF8)
				ps.UTF8 = nil
				t.print(r)
			}
			return
		}
		ps.UTF8 = nil
		t.print(utf8.RuneError)
	}
	switch {
	case b < 0x20:
		t.control(b)
	case b == 0x7f:
	case b < 0x80:
		t.print(rune(b))
	case b >= 0xc2 && b <= 0xf4:
		ps.UTF8 = []byte{b}
	default:
		t.print(utf8.RuneError)
	}
}

// control executes a C0 control.
func (t *Terminal) control(b byte) {
	c := &t.s.Cursor
	switch b {
	case 0x08:
		if c.X > 0 {
			c.X--
		}
		c.PendingWrap = false
	case 0x09:
		t.tab(1)
	case 0x0a, 0x0b, 0x0c:
		t.index()
	case 0x0d:
		c.X, c.PendingWrap = 0, false
	case 0x0e:
		c.Shift = true
	case 0x0f:
		c.Shift = false
	case 0x1b:
		t.s.Parser.State = stateEscape
	}
}

// escape executes an escape sequence that is not a CSI, OSC or string.
func (t *Terminal) escape(intermediates []byte, final byte) {
	c := &t.s.Cursor
	switch string(intermediates) {
	case "":
		switch final {
		case '7':
			t.saveCursor()
		case '8':
			t.restoreCursor()
		case 'D':
			t.index()
		case 'E':
			t.index()
			c.X = 0
		case 'H':
			t.tabs[c.X] = true
		case 'M':
			t.reverseIndex()
		case 'c':
			t.reset()
		}
	case "(", ")":
		set := final
		if set != '0' {
			set = 'B'
		}
		c.Charsets[intermediates[0]-'('] = set
	case "#":
		if final == '8' {
			for _, line := range t.screen().Lines {
				for x := range line {
					line[x] = Cell{Text: "E"}
				}
			}
			t.s.Top, t.s.Bottom = 0, t.s.Rows-1
			t.moveTo(0, 0)
		}
	}
}

// osc executes an operating system command. Only the window title is kept.
func (t *Terminal) osc(data []byte) {
	command, text, _ := strings.Cut(string(data), ";")
	if command == "0" || command == "2" {
		t.s.Title = text
	}
}

// params are a CSI's parameters, each with its colon-separated
// sub-parameters. A parameter left out is -1.
type params [][]int

func parseParams(s string) params {
	if s == "" {
		return nil
	}
	var ps params
	for _, field := range strings.Split(s, ";") {
		var p []int
		for _, sub := range strings.Split(field, ":") {
			n, err := strconv.Atoi(sub)
			if err != nil || n < 0 {
				n = -1
			}
			p = append(p, min(n, 1<<16))
		}
		ps = append(ps, p)
	}
	return ps
}

// get returns parameter i, or def when it is left out or 0.
func (ps params) get(i, def int) int {
	if i >= len(ps) || ps[i][0] <= 0 {
		return def
	}
	return ps[i][0]
}

// csi executes a control sequence: buf is what came between CSI and final.
func (t *Terminal) csi(buf []byte, final byte) {
	var private byte
	if len(buf) > 0 && buf[0] >= '<' && buf[0] <= '?' {
		private, buf = buf[0], buf[1:]
	}
	end := len(buf)
	for end > 0 && buf[end-1] >= 0x20 && buf[end-1] <= 0x2f {
		end--
	}
	intermediates := string(buf[end:])
	body := string(buf[:end])
	if strings.ContainsAny(body, "<=>?") {
		return
	}
	ps := parseParams(body)
	c := &t.s.Cursor
	n := ps.get(0, 1)

	if private == '?' {
		switch {
		case final == 'h' && intermediates == "":
			for _, p := range ps {
				t.privateMode(p[0], true)
			}
		case final == 'l' && intermediates == "":
			for _, p := range ps {
				t.privateMode(p[0], false)
			}
		case final == 'n' && ps.get(0, 0) == 6:
			t.respond(fmt.Sprintf("\x1b[?%d;%dR", t.reportedRow(), c.X+1))
		case final == 'J' || final == 'K':
			// DECSED and DECSEL erase as ED and EL do; there is no protected
			// rendition to keep.
			t.csi([]byte(body), final)
		}
		return
	}
	if private == '>' {
		if final == 'c' && intermediates == "" && ps.get(0, 0) == 0 {
			t.respond("\x1b[>0;10;1c")
		}
		return
	}
	if private != 0 {
		return
	}
	if intermediates != "" {
		return
	}

	switch final {
	case '@':
		t.insertCells(n)
	case 'A':
		t.cursorUp(n)
	case 'B', 'e':
		t.cursorDown(n)
	case 'C', 'a':
		t.moveTo(c.X+n, c.Y)
	case 'D':
		t.moveTo(c.X-n, c.Y)
	case 'E':
		t.cursorDown(n)
		c.X = 0
	case 'F':
		t.cursorUp(n)
		c.X = 0
	case 'G', '`':
		t.moveTo(n-1, c.Y)
	case 'H', 'f':
		t.cursorPosition(ps.get(0, 1)-1, ps.get(1, 1)-1)
	case 'I':
		t.tab(n)
	case 'J':
		t.eraseDisplay(ps.get(0, 0))
	case 'K':
		t.eraseLine(ps.get(0, 0))
	case 'L':
		t.insertLines(n)
	case 'M':
		t.deleteLines(n)
	case 'P':
		t.deleteCells(n)
	case 'S':
		t.scrollUp(t.s.Top, t.s.Bottom, n)
	case 'T':
		if len(ps) <= 1 {
			t.scrollDown(t.s.Top, t.s.Bottom, n)
		}
	case 'X':
		t.eraseCells(c.X, c.Y, n)
	case 'Z':
		t.tab(-n)
	case 'b':
		t.repeat(n)
	case 'c':
		if ps.get(0, 0) == 0 {
			t.respond("\x1b[?1;2c")
		}
	case 'd':
		t.cursorPosition(n-1, c.X)
	case 'g':
		switch ps.get(0, 0) {
		case 0:
			t.tabs[c.X] = false
		case 3:
			clear(t.tabs)
		}
	case 'h', 'l':
		if ps.get(0, 0) == 4 {
			t.s.Modes.Insert = final == 'h'
		}
	case 'm':
		t.sgr(ps)
	case 'n':
		switch ps.get(0, 0) {
		case 5:
			t.respond("\x1b[0n")
		case 6:
			t.respond(fmt.Sprintf("\x1b[%d;%dR", t.reportedRow(), c.X+1))
		}
	case 'r':
		top, bottom := ps.get(0, 1)-1, ps.get(1, t.s.Rows)-1
		bottom = min(bottom, t.s.Rows-1)
		if top < bottom {
			t.s.Top, t.s.Bottom = top, bottom
			t.cursorPosition(0, 0)
		}
	case 's':
		if len(ps) == 0 {
			t.saveCursor()
		}
	case 'u':
		if len(ps) == 0 {
			t.restoreCursor()
		}
	}
}

// privateMode sets or resets a DEC private mode.
func (t *Terminal) privateMode(mode int, set bool) {
	switch mode {
	case 6:
		t.s.Cursor.Origin = set
		t.cursorPosition(0, 0)
	case 7:
		t.s.Modes.NoAutowrap = !set
		if !set {
			t.s.Cursor.PendingWrap = false
		}
	case 25:
		t.s.Modes.HiddenCursor = !set
	case 2004:
		t.s.Modes.BracketedPaste = set
	case 47, 1047:
		if !set && mode == 1047 && t.s.AltActive {
			t.s.Alt.Lines = blankLines(t.s.Columns, t.s.Rows)
		}
		t.s.AltActive = set
	case 1048:
		if set {
			t.saveCursor()
		} else {
			t.restoreCursor()
		}
	case 1049:
		if set {
			if t.s.AltActive {
				return
			}
			t.saveCursor()
			t.s.AltActive = true
			t.s.Alt.Lines = blankLines(t.s.Columns, t.s.Rows)
		} else {
			if !t.s.AltActive {
				return
			}
			t.s.AltActive = false
			t.restoreCursor()
		}
	}
}

func (t *Terminal) respond(s string) {
	t.responses = append(t.responses, s...)
}

// reportedRow is the cursor's row as a position report gives it: relative to
// the scroll region in origin mode, and counted from 1.
func (t *Terminal) reportedRow() int {
	if t.s.Cursor.Origin {
		return t.s.Cursor.Y - t.s.Top + 1
	}
	return t.s.Cursor.Y + 1
}

// sgr sets the rendition new characters are drawn with.
func (t *Terminal) sgr(ps params) {
	a := &t.s.Cursor.Attr
	if len(ps) == 0 {
		*a = Attr{}
		return
	}
	for i := 0; i < len(ps); i++ {
		p := ps[i]
		switch v := p[0]; {
		case v <= 0:
			*a = Attr{}
		case v == 1:
			a.Flags |= Bold
		case v == 2:
			a.Flags |= Faint
		case v == 3:
			a.Flags |= Italic
		case v == 4:
			if len(p) > 1 && p[1] == 0 {
				a.Flags &^= Underline
			} else {
				a.Flags |= Underline
			}
		case v == 5 || v == 6:
			a.Flags |= Blink
		case v == 7:
			a.Flags |= Inverse
		case v == 8:
			a.Flags |= Invisible
		case v == 9:
			a.Flags |= Strike
		case v == 21:
			a.Flags |= Underline
		case v == 22:
			a.Flags &^= Bold | Faint
		case v == 23:
			a.Flags &^= Italic
		case v == 24:
			a.Flags &^= Underline
		case v == 25:
			a.Flags &^= Blink
		case v == 27:
			a.Flags &^= Inverse
		case v == 28:
			a.Flags &^= Invisible
		case v == 29:
			a.Flags &^= Strike
		case v >= 30 && v <= 37:
			a.Fg = Palette(uint8(v - 30))
		case v == 38 || v == 48 || v == 58:
			var color Color
			var ok bool
			if len(p) > 1 {
				color, ok = extendedColor(p[1:])
			} else {
				var used int
				color, ok, used = extendedColorParams(ps[i+1:])
				i += used
			}
			if ok && v == 38 {
				a.Fg = color
			} else if ok && v == 48 {
				a.Bg = color
			}
		case v == 39:
			a.Fg = DefaultColor
		case v >= 40 && v <= 47:
			a.Bg = Palette(uint8(v - 40))
		case v == 49:
			a.Bg = DefaultColor
		case v >= 90 && v <= 97:
			a.Fg = Palette(uint8(v - 90 + 8))
		case v >= 100 && v <= 107:
			a.Bg = Palette(uint8(v - 100 + 8))
		}
	}
}

// extendedColor reads an extended color given as sub-parameters: 5:N, 2:R:G:B
// or 2:CS:R:G:B.
func extendedColor(sub []int) (Color, bool) {
	switch {
	case sub[0] == 5 && len(sub) >= 2 && sub[1] >= 0 && sub[1] <= 255:
		return Palette(uint8(sub[1])), true
	case sub[0] == 2 && len(sub) >= 4:
		rgb := sub[len(sub)-3:]
		for _, v := range rgb {
			if v < 0 || v > 255 {
				return 0, false
			}
		}
		return RGB(uint8(rgb[0]), uint8(rgb[1]), uint8(rgb[2])), true
	}
	return 0, false
}

// extendedColorParams reads an extended color given as the parameters after
// 38, 48 or 58: 5;N or 2;R;G;B. It returns how many parameters it used.
func extendedColorParams(rest params) (Color, bool, int) {
	if len(rest) == 0 {
		return 0, false, 0
	}
	switch rest[0][0] {
	case 5:
		if len(rest) < 2 {
			return 0, false, len(rest)
		}
		v := rest[1][0]
		return Palette(uint8(max(min(v, 255), 0))), v >= 0 && v <= 255, 2
	case 2:
		if len(rest) < 4 {
			return 0, false, len(rest)
		}
		r, g, b := rest[1][0], rest[2][0], rest[3][0]
		ok := r >= 0 && r <= 255 && g >= 0 && g <= 255 && b >= 0 && b <= 255
		return RGB(uint8(max(r, 0)), uint8(max(g, 0)), uint8(max(b, 0))), ok, 4
	}
	return 0, false, 1
}
