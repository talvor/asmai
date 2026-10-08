// SPDX-License-Identifier: Apache-2.0

package vt

import (
	"encoding/json"
	"strings"
	"testing"
)

func screen(t *testing.T, columns, rows int, output ...string) *Terminal {
	t.Helper()
	term := New(columns, rows)
	for _, o := range output {
		term.Write([]byte(o))
	}
	return term
}

func checkLines(t *testing.T, term *Terminal, want ...string) {
	t.Helper()
	got := term.Lines()
	for len(want) < len(got) {
		want = append(want, "")
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("the screen shows\n%s\nwant\n%s", strings.Join(got, "|\n"), strings.Join(want, "|\n"))
	}
}

func checkCursor(t *testing.T, term *Terminal, x, y int) {
	t.Helper()
	if gx, gy, _ := term.Cursor(); gx != x || gy != y {
		t.Errorf("the cursor is at %d,%d, want %d,%d", gx, gy, x, y)
	}
}

func TestTextWrapsAndScrolls(t *testing.T) {
	term := screen(t, 5, 3, "abcdefg\r\nhi\r\njk\r\nlm")
	checkLines(t, term, "hi", "jk", "lm")
	checkCursor(t, term, 2, 2)

	// The cursor stays in the last column until the next character wraps.
	term = screen(t, 5, 3, "abcde")
	checkCursor(t, term, 4, 0)
	term.Write([]byte("f"))
	checkLines(t, term, "abcde", "f")

	// Without autowrap, the last column is drawn over.
	term = screen(t, 5, 3, "\x1b[?7labcdefg")
	checkLines(t, term, "abcdg")
}

func TestCursorMovementAndErasing(t *testing.T) {
	term := screen(t, 10, 4,
		"0123456789\r\n0123456789\r\n0123456789\r\n0123456789",
		"\x1b[2;3H\x1b[K",        // erase to the end of row 2
		"\x1b[3;5H\x1b[1K",       // erase row 3 up to column 5
		"\x1b[1;8H\x1b[2X",       // erase 2 cells
		"\x1b[4;2H\x1b[3P",       // delete 3 cells
		"\x1b[4;1H\x1b[2@AB",     // insert 2 cells and draw over them
		"\x1b[10;10H\x1b[A\x1bM", // the cursor stops at the bottom, then moves up twice
	)
	checkLines(t, term, "0123456  9", "01", "     56789", "AB0456789")
	checkCursor(t, term, 9, 1)
}

func TestScrollRegionInsertAndDeleteLines(t *testing.T) {
	term := screen(t, 3, 5, "a\r\nb\r\nc\r\nd\r\ne",
		"\x1b[2;4r",       // rows 2 to 4 scroll
		"\x1b[4;1H\n",     // a line feed at the region's bottom scrolls only the region
		"\x1b[2;1H\x1b[L", // insert a line at row 2
	)
	checkLines(t, term, "a", "", "c", "d", "e")
	term.Write([]byte("\x1b[3;1H\x1b[2M")) // delete two lines at row 3
	checkLines(t, term, "a", "", "", "", "e")
}

func TestColorsAndRenditions(t *testing.T) {
	term := screen(t, 20, 2, "\x1b[1;38;5;220mA\x1b[22;38;2;1;2;3;48:5:17mB\x1b[0;4:3;7;94mC\x1b[mD\x1b[>4;2mE")
	want := []Attr{
		{Fg: Palette(220), Flags: Bold},
		{Fg: RGB(1, 2, 3), Bg: Palette(17)},
		{Fg: Palette(12), Flags: Underline | Inverse},
		{},
		// CSI > 4;2 m sets a keyboard mode, not a rendition.
		{},
	}
	for x, attr := range want {
		if got := term.Cell(x, 0).Attr; got != attr {
			t.Errorf("cell %d is drawn with %+v, want %+v", x, got, attr)
		}
	}
	checkLines(t, term, "ABCDE")
}

func TestWideAndCombiningCharacters(t *testing.T) {
	term := screen(t, 6, 2, "a世e\u0301b")
	checkLines(t, term, "a世e\u0301b")
	if c := term.Cell(1, 0); !c.Wide || c.Text != "世" || !term.Cell(2, 0).Cont {
		t.Errorf("the wide character is held as %+v, %+v", c, term.Cell(2, 0))
	}
	checkCursor(t, term, 5, 0)

	// A wide character that does not fit wraps whole.
	term = screen(t, 3, 2, "ab世")
	checkLines(t, term, "ab", "世")

	// Drawing over half a wide character blanks the other half.
	term = screen(t, 4, 1, "世世\x1b[1;2Hx")
	checkLines(t, term, " x世")
}

func TestTheAlternateScreenKeepsTheMainOne(t *testing.T) {
	term := screen(t, 10, 3, "main\x1b[?1049h\x1b[Halt")
	checkLines(t, term, "alt")
	term.Write([]byte("\x1b[?1049l"))
	checkLines(t, term, "main")
	checkCursor(t, term, 4, 0)
}

func TestQueriesAreAnswered(t *testing.T) {
	term := screen(t, 10, 5, "\x1b[3;4H\x1b[6n\x1b[c\x1b[5n\x1b[>0q\x1b[?u\x1b[?2026$p")
	// Cursor position, primary device attributes and status are answered;
	// XTVERSION, kitty keyboard and mode queries are not, as on a terminal
	// without them.
	if got, want := string(term.TakeResponses()), "\x1b[3;4R\x1b[?1;2c\x1b[0n"; got != want {
		t.Errorf("the terminal answered %q, want %q", got, want)
	}
	if got := term.TakeResponses(); len(got) != 0 {
		t.Errorf("the answers were taken twice: %q", got)
	}
}

func TestSequencesSplitAcrossWritesAndStringsAreRead(t *testing.T) {
	term := New(20, 2)
	for _, b := range []byte("\x1b]0;my title\x07\x1b[1;3Hx\x1bP+q544e\x1b\\é") {
		term.Write([]byte{b})
	}
	checkLines(t, term, "  xé")
	if got := term.Title(); got != "my title" {
		t.Errorf("the title is %q", got)
	}
}

func TestDECLineDrawing(t *testing.T) {
	term := screen(t, 5, 1, "\x1b(0lqk\x1b(Bq")
	checkLines(t, term, "┌─┐q")
}

func TestAStateCarriesOnMidSequence(t *testing.T) {
	term := screen(t, 10, 3, "ab\x1b[31mc\x1b7\x1b[2;5r\x1b[3")
	data, err := json.Marshal(term.State())
	if err != nil {
		t.Fatal(err)
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	copied, err := FromState(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []*Terminal{term, copied} {
		tt.Write([]byte("Gd\x1b8e"))
	}
	if got, want := copied.String(), term.String(); got != want {
		t.Errorf("the terminal made from the state shows %q, want %q", got, want)
	}
	// CSI 3 G moved to the third column, and DECRC back to after "c".
	checkLines(t, copied, "abde")
	if a := copied.Cell(3, 0).Attr; a.Fg != Palette(1) {
		t.Errorf("the restored cursor draws with %+v, want red", a)
	}
}

func TestResizeKeepsTheTopLeftAndTheCursorOnScreen(t *testing.T) {
	term := screen(t, 6, 4, "1\r\n2\r\n3\r\n4567")
	term.Resize(3, 2)
	checkLines(t, term, "3", "456")
	checkCursor(t, term, 2, 1)
	term.Resize(8, 3)
	checkLines(t, term, "3", "456")
	if c, r := term.Size(); c != 8 || r != 3 {
		t.Errorf("the terminal is %dx%d", c, r)
	}
}

func TestTheRendererDrawsTheScreenAndThenOnlyWhatChanged(t *testing.T) {
	term := screen(t, 6, 2, "\x1b[1mab\x1b[m\r\n世d")
	var r Renderer
	first := r.Render(term, 6, 2)
	shown := New(6, 2)
	shown.Write(first)
	checkLines(t, shown, "ab", "世d")
	if shown.Cell(0, 0).Flags != Bold || shown.Cell(2, 0).Flags != 0 {
		t.Errorf("the rendered screen lost its renditions")
	}

	term.Write([]byte("\x1b[1;6Hz"))
	second := r.Render(term, 6, 2)
	shown.Write(second)
	checkLines(t, shown, "ab   z", "世d")
	if strings.Contains(string(second), "ab") || strings.Contains(string(second), "世") {
		t.Errorf("the second frame %q redraws cells that did not change", second)
	}
	// A window smaller than the screen shows its top left.
	r.Invalidate()
	small := New(3, 1)
	small.Write(r.Render(term, 3, 1))
	checkLines(t, small, "ab")
}
