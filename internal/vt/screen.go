// SPDX-License-Identifier: Apache-2.0

package vt

// decGraphics maps the characters of the DEC line-drawing set to Unicode.
var decGraphics = map[rune]rune{
	'`': '◆', 'a': '▒', 'b': '␉', 'c': '␌', 'd': '␍', 'e': '␊', 'f': '°', 'g': '±',
	'h': '␤', 'i': '␋', 'j': '┘', 'k': '┐', 'l': '┌', 'm': '└', 'n': '┼', 'o': '⎺',
	'p': '⎻', 'q': '─', 'r': '⎼', 's': '⎽', 't': '├', 'u': '┤', 'v': '┴', 'w': '┬',
	'x': '│', 'y': '≤', 'z': '≥', '{': 'π', '|': '≠', '}': '£', '~': '·',
}

// print draws r at the cursor and moves the cursor past it.
func (t *Terminal) print(r rune) {
	c := &t.s.Cursor
	set := c.Charsets[0]
	if c.Shift {
		set = c.Charsets[1]
	}
	if set == '0' {
		if g, ok := decGraphics[r]; ok {
			r = g
		}
	}
	width := runeWidth(r)
	lines := t.screen().Lines
	if width == 0 {
		// A combining mark joins the character before the cursor.
		x, y := c.X-1, c.Y
		if c.PendingWrap {
			x = c.X
		}
		if x >= 0 && lines[y][x].Cont && x > 0 {
			x--
		}
		if x >= 0 && lines[y][x].Text != "" {
			lines[y][x].Text += string(r)
		}
		return
	}
	if c.PendingWrap && !t.s.Modes.NoAutowrap {
		t.index()
		c.X = 0
	}
	c.PendingWrap = false
	if width == 2 && c.X == t.s.Columns-1 {
		if t.s.Modes.NoAutowrap || t.s.Columns < 2 {
			return
		}
		t.clearCell(c.X, c.Y)
		lines[c.Y][c.X] = Cell{Attr: c.Attr}
		t.index()
		c.X = 0
	}
	line := lines[c.Y]
	if t.s.Modes.Insert {
		t.insertCells(width)
	}
	t.clearCell(c.X, c.Y)
	if width == 2 {
		t.clearCell(c.X+1, c.Y)
		line[c.X] = Cell{Text: string(r), Wide: true, Attr: c.Attr}
		line[c.X+1] = Cell{Cont: true, Attr: c.Attr}
	} else {
		line[c.X] = Cell{Text: string(r), Attr: c.Attr}
	}
	if c.X+width >= t.s.Columns {
		c.X = t.s.Columns - 1
		c.PendingWrap = !t.s.Modes.NoAutowrap
	} else {
		c.X += width
	}
}

// clearCell blanks the other half of a wide character at x, y, ready for x
// to be drawn over.
func (t *Terminal) clearCell(x, y int) {
	line := t.screen().Lines[y]
	if x < 0 || x >= len(line) {
		return
	}
	switch {
	case line[x].Wide && x+1 < len(line):
		line[x+1] = Cell{Attr: line[x+1].Attr}
	case line[x].Cont && x > 0:
		line[x-1] = Cell{Attr: line[x-1].Attr}
	}
}

// repeat draws the last character drawn n more times.
func (t *Terminal) repeat(n int) {
	c := t.s.Cursor
	x := c.X - 1
	if c.PendingWrap {
		x = c.X
	}
	if x < 0 {
		return
	}
	line := t.screen().Lines[c.Y]
	if line[x].Cont && x > 0 {
		x--
	}
	r := []rune(line[x].Text)
	if len(r) == 0 {
		return
	}
	for range min(n, t.s.Columns*t.s.Rows) {
		t.print(r[0])
	}
}

// blank is an erased cell: it keeps the background color being drawn with.
func (t *Terminal) blank() Cell {
	return Cell{Attr: Attr{Bg: t.s.Cursor.Attr.Bg}}
}

// index moves the cursor down a row, scrolling the region up at its bottom.
func (t *Terminal) index() {
	c := &t.s.Cursor
	c.PendingWrap = false
	switch {
	case c.Y == t.s.Bottom:
		t.scrollUp(t.s.Top, t.s.Bottom, 1)
	case c.Y < t.s.Rows-1:
		c.Y++
	}
}

// reverseIndex moves the cursor up a row, scrolling the region down at its
// top.
func (t *Terminal) reverseIndex() {
	c := &t.s.Cursor
	c.PendingWrap = false
	switch {
	case c.Y == t.s.Top:
		t.scrollDown(t.s.Top, t.s.Bottom, 1)
	case c.Y > 0:
		c.Y--
	}
}

// scrollUp moves rows top to bottom up by n, blanking the rows uncovered.
func (t *Terminal) scrollUp(top, bottom, n int) {
	lines := t.screen().Lines
	n = min(n, bottom-top+1)
	copy(lines[top:bottom+1], lines[top+n:bottom+1])
	for y := bottom - n + 1; y <= bottom; y++ {
		lines[y] = t.blankLine()
	}
}

// scrollDown moves rows top to bottom down by n, blanking the rows uncovered.
func (t *Terminal) scrollDown(top, bottom, n int) {
	lines := t.screen().Lines
	n = min(n, bottom-top+1)
	copy(lines[top+n:bottom+1], lines[top:bottom+1-n])
	for y := top; y < top+n; y++ {
		lines[y] = t.blankLine()
	}
}

func (t *Terminal) blankLine() []Cell {
	line := make([]Cell, t.s.Columns)
	b := t.blank()
	for x := range line {
		line[x] = b
	}
	return line
}

// moveTo moves the cursor to column x and row y of the screen, within it.
func (t *Terminal) moveTo(x, y int) {
	c := &t.s.Cursor
	c.X = clamp(x, 0, t.s.Columns-1)
	c.Y = clamp(y, 0, t.s.Rows-1)
	c.PendingWrap = false
}

// cursorPosition is CUP: row y and column x, relative to the scroll region in
// origin mode.
func (t *Terminal) cursorPosition(y, x int) {
	if t.s.Cursor.Origin {
		t.moveTo(x, clamp(y+t.s.Top, t.s.Top, t.s.Bottom))
		return
	}
	t.moveTo(x, y)
}

// cursorUp moves the cursor up n rows, stopping at the scroll region's top
// when it starts inside the region.
func (t *Terminal) cursorUp(n int) {
	c := &t.s.Cursor
	limit := 0
	if c.Y >= t.s.Top {
		limit = t.s.Top
	}
	t.moveTo(c.X, max(c.Y-n, limit))
}

// cursorDown moves the cursor down n rows, stopping at the scroll region's
// bottom when it starts inside the region.
func (t *Terminal) cursorDown(n int) {
	c := &t.s.Cursor
	limit := t.s.Rows - 1
	if c.Y <= t.s.Bottom {
		limit = t.s.Bottom
	}
	t.moveTo(c.X, min(c.Y+n, limit))
}

// tab moves the cursor to the n-th next tab stop, or with a negative n back.
func (t *Terminal) tab(n int) {
	c := &t.s.Cursor
	c.PendingWrap = false
	for ; n > 0 && c.X < t.s.Columns-1; n-- {
		c.X++
		for c.X < t.s.Columns-1 && !t.tabs[c.X] {
			c.X++
		}
	}
	for ; n < 0 && c.X > 0; n++ {
		c.X--
		for c.X > 0 && !t.tabs[c.X] {
			c.X--
		}
	}
}

// eraseCells blanks n cells from column x of row y.
func (t *Terminal) eraseCells(x, y, n int) {
	line := t.screen().Lines[y]
	end := min(x+n, len(line))
	if end <= x {
		return
	}
	t.clearCell(x, y)
	t.clearCell(end-1, y)
	b := t.blank()
	for i := x; i < end; i++ {
		line[i] = b
	}
	t.s.Cursor.PendingWrap = false
}

// eraseLine is EL: 0 erases from the cursor to the end of the line, 1 from
// its start to the cursor, and 2 the whole line.
func (t *Terminal) eraseLine(mode int) {
	c := t.s.Cursor
	switch mode {
	case 0:
		t.eraseCells(c.X, c.Y, t.s.Columns-c.X)
	case 1:
		t.eraseCells(0, c.Y, c.X+1)
	case 2:
		t.eraseCells(0, c.Y, t.s.Columns)
	}
}

// eraseDisplay is ED: 0 erases from the cursor to the end of the screen, 1
// from its start to the cursor, and 2 the whole screen. There is no
// scrollback, so 3 erases nothing.
func (t *Terminal) eraseDisplay(mode int) {
	c := t.s.Cursor
	switch mode {
	case 0:
		t.eraseLine(0)
		for y := c.Y + 1; y < t.s.Rows; y++ {
			t.eraseCells(0, y, t.s.Columns)
		}
	case 1:
		for y := 0; y < c.Y; y++ {
			t.eraseCells(0, y, t.s.Columns)
		}
		t.eraseLine(1)
	case 2:
		for y := 0; y < t.s.Rows; y++ {
			t.eraseCells(0, y, t.s.Columns)
		}
	}
}

// insertLines is IL: n blank lines at the cursor's row, inside the scroll
// region.
func (t *Terminal) insertLines(n int) {
	c := &t.s.Cursor
	if c.Y < t.s.Top || c.Y > t.s.Bottom {
		return
	}
	t.scrollDown(c.Y, t.s.Bottom, n)
	c.X, c.PendingWrap = 0, false
}

// deleteLines is DL: n lines removed at the cursor's row, inside the scroll
// region.
func (t *Terminal) deleteLines(n int) {
	c := &t.s.Cursor
	if c.Y < t.s.Top || c.Y > t.s.Bottom {
		return
	}
	t.scrollUp(c.Y, t.s.Bottom, n)
	c.X, c.PendingWrap = 0, false
}

// insertCells is ICH: n blank cells at the cursor, shifting the rest of the
// line right.
func (t *Terminal) insertCells(n int) {
	c := &t.s.Cursor
	line := t.screen().Lines[c.Y]
	n = min(n, t.s.Columns-c.X)
	t.clearCell(c.X, c.Y)
	copy(line[c.X+n:], line[c.X:])
	b := t.blank()
	for x := c.X; x < c.X+n; x++ {
		line[x] = b
	}
	if last := len(line) - 1; line[last].Wide {
		line[last] = Cell{Attr: line[last].Attr}
	}
	c.PendingWrap = false
}

// deleteCells is DCH: n cells removed at the cursor, shifting the rest of the
// line left.
func (t *Terminal) deleteCells(n int) {
	c := &t.s.Cursor
	line := t.screen().Lines[c.Y]
	n = min(n, t.s.Columns-c.X)
	t.clearCell(c.X, c.Y)
	t.clearCell(c.X+n-1, c.Y)
	copy(line[c.X:], line[c.X+n:])
	b := t.blank()
	for x := len(line) - n; x < len(line); x++ {
		line[x] = b
	}
	if line[c.X].Cont {
		line[c.X] = Cell{Attr: line[c.X].Attr}
	}
	c.PendingWrap = false
}

// saveCursor is DECSC.
func (t *Terminal) saveCursor() {
	saved := t.s.Cursor
	t.screen().Saved = &saved
}

// restoreCursor is DECRC; with nothing saved it homes the cursor.
func (t *Terminal) restoreCursor() {
	saved := t.screen().Saved
	if saved == nil {
		saved = &Cursor{Charsets: [2]byte{'B', 'B'}}
	}
	t.s.Cursor = *saved
	t.s.Cursor.X = clamp(saved.X, 0, t.s.Columns-1)
	t.s.Cursor.Y = clamp(saved.Y, 0, t.s.Rows-1)
}
