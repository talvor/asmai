// SPDX-License-Identifier: Apache-2.0

package dashtest

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/talvor/asmai/internal/vt"
)

// settle is how long the terminal must stay quiet for a screen to count as
// drawn.
const settle = 150 * time.Millisecond

// waitLimit is how long WaitFor waits for a screen.
const waitLimit = 10 * time.Second

// Terminal is a program running in a PTY of a given size, as the controlling
// terminal of a session of its own, with what it draws held in a terminal
// emulator to read back as text.
type Terminal struct {
	t    *testing.T
	cmd  *exec.Cmd
	ptmx *os.File

	mu       sync.Mutex
	term     *vt.Terminal
	lastRead time.Time
	done     chan struct{} // closed once the program exits
	exitErr  error
}

// Config is how Start runs the program.
type Config struct {
	// Columns and Rows are the size of the terminal.
	Columns, Rows int
	// Env is the program's whole environment, as KEY=VALUE; Start adds
	// nothing, so that a test names every variable that matters. TERM
	// defaults to xterm-256color.
	Env []string
}

// Start runs the executable bin with args in a new terminal, and kills it
// when the test ends.
func Start(t *testing.T, cfg Config, bin string, args ...string) *Terminal {
	t.Helper()
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := pty.Setsize(ptmx, &pty.Winsize{Cols: uint16(cfg.Columns), Rows: uint16(cfg.Rows)}); err != nil {
		t.Fatal(err)
	}
	x := &Terminal{t: t, ptmx: ptmx, term: vt.New(cfg.Columns, cfg.Rows), done: make(chan struct{})}
	x.cmd = exec.Command(bin, args...)
	x.cmd.Env = cfg.Env
	if !hasVar(cfg.Env, "TERM") {
		x.cmd.Env = append(x.cmd.Env, "TERM=xterm-256color")
	}
	x.cmd.Stdin, x.cmd.Stdout, x.cmd.Stderr = tty, tty, tty
	x.cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	err = x.cmd.Start()
	tty.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		x.cmd.Process.Kill()
		<-x.done
		ptmx.Close()
	})
	go x.read()
	go func() {
		err := x.cmd.Wait()
		x.mu.Lock()
		x.exitErr = err
		x.mu.Unlock()
		close(x.done)
	}()
	return x
}

func hasVar(env []string, name string) bool {
	for _, kv := range env {
		if strings.HasPrefix(kv, name+"=") {
			return true
		}
	}
	return false
}

// read feeds what the program draws to the emulator, and sends back what the
// emulator answers to the program's questions about the terminal.
func (x *Terminal) read() {
	buf := make([]byte, 4096)
	for {
		n, err := x.ptmx.Read(buf)
		if n > 0 {
			x.mu.Lock()
			x.term.Write(buf[:n])
			x.lastRead = time.Now()
			reply := x.term.TakeResponses()
			x.mu.Unlock()
			if len(reply) > 0 {
				x.ptmx.Write(reply)
			}
		}
		if err != nil {
			return
		}
	}
}

// Send types s.
func (x *Terminal) Send(s string) {
	x.t.Helper()
	if _, err := x.ptmx.WriteString(s); err != nil {
		x.t.Fatal(err)
	}
}

// Resize changes the terminal's size, which tells the program.
func (x *Terminal) Resize(columns, rows int) {
	x.t.Helper()
	x.mu.Lock()
	x.term.Resize(columns, rows)
	x.mu.Unlock()
	if err := pty.Setsize(x.ptmx, &pty.Winsize{Cols: uint16(columns), Rows: uint16(rows)}); err != nil {
		x.t.Fatal(err)
	}
}

// Exited reports whether the program has exited, and how.
func (x *Terminal) Exited() (bool, error) {
	select {
	case <-x.done:
		x.mu.Lock()
		defer x.mu.Unlock()
		return true, x.exitErr
	default:
		return false, nil
	}
}

// WaitExit waits for the program to exit.
func (x *Terminal) WaitExit() error {
	x.t.Helper()
	select {
	case <-x.done:
		x.mu.Lock()
		defer x.mu.Unlock()
		return x.exitErr
	case <-time.After(waitLimit):
		x.t.Fatalf("the program did not exit; the screen is\n%s", x.Screen())
		return nil
	}
}

// WaitFor waits until the screen satisfies ok and the program has been quiet
// for a moment, and returns the screen, or fails the test with the screen it
// was left with.
func (x *Terminal) WaitFor(what string, ok func(screen string) bool) string {
	x.t.Helper()
	deadline := time.Now().Add(waitLimit)
	for time.Now().Before(deadline) {
		x.mu.Lock()
		quiet := time.Since(x.lastRead) >= settle
		x.mu.Unlock()
		if screen := x.Screen(); quiet && ok(screen) {
			return screen
		}
		time.Sleep(10 * time.Millisecond)
	}
	x.t.Fatalf("the screen never showed %s; it is\n%s", what, x.Screen())
	return ""
}

// WaitForText waits for the screen to show text.
func (x *Terminal) WaitForText(text string) string {
	x.t.Helper()
	return x.WaitFor(fmt.Sprintf("%q", text), func(screen string) bool { return strings.Contains(screen, text) })
}

// Screen is what the terminal shows, a line to a row with its trailing
// spaces trimmed.
func (x *Terminal) Screen() string {
	x.mu.Lock()
	defer x.mu.Unlock()
	lines := x.term.Lines()
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ")
	}
	return strings.Join(lines, "\n") + "\n"
}

// Attributes describes how the screen is drawn where it is not plain: a line
// for each run of cells with the same rendition, as "row col-col: bold,
// reverse, fg=2". A golden screen holds them, so that reverse video and
// NO_COLOR can be told from the text.
func (x *Terminal) Attributes() string {
	x.mu.Lock()
	defer x.mu.Unlock()
	columns, rows := x.term.Size()
	var b strings.Builder
	for y := range rows {
		start := 0
		for x0 := 1; x0 <= columns; x0++ {
			if x0 < columns && x.term.Cell(x0, y).Attr == x.term.Cell(start, y).Attr {
				continue
			}
			if d := describe(x.term.Cell(start, y).Attr); d != "" {
				fmt.Fprintf(&b, "%d %d-%d: %s\n", y+1, start+1, x0, d)
			}
			start = x0
		}
	}
	return b.String()
}

// Snapshot is the screen and its attributes, the text a golden screen holds.
func (x *Terminal) Snapshot() string {
	return x.Screen() + "--- attributes\n" + x.Attributes()
}

func describe(a vt.Attr) string {
	var parts []string
	for _, f := range []struct {
		flag vt.Flags
		name string
	}{{vt.Bold, "bold"}, {vt.Faint, "faint"}, {vt.Italic, "italic"}, {vt.Underline, "underline"}, {vt.Blink, "blink"}, {vt.Inverse, "reverse"}, {vt.Invisible, "invisible"}, {vt.Strike, "strike"}} {
		if a.Flags&f.flag != 0 {
			parts = append(parts, f.name)
		}
	}
	if a.Fg != vt.DefaultColor {
		parts = append(parts, fmt.Sprintf("fg=%#x", uint32(a.Fg)))
	}
	if a.Bg != vt.DefaultColor {
		parts = append(parts, fmt.Sprintf("bg=%#x", uint32(a.Bg)))
	}
	return strings.Join(parts, ", ")
}
