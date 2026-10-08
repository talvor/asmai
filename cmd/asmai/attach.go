// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/statedir"
	"github.com/talvor/asmai/internal/vt"
)

// detachKey is the key that detaches an attach client: Ctrl-]. In M4 it
// opens a menu instead.
const detachKey = 0x1d

// frameInterval is the shortest time between two frames the attach client
// draws.
const frameInterval = 16 * time.Millisecond

// attach shows the agent's terminal on this one, drawing it from the
// daemon's emulation of it with a status line under it, and observes: no key
// typed here reaches the agent. Ctrl-] detaches. With --json it prints the
// agent's screen as it is now instead.
func attach(o *output, paths statedir.Paths, agent string, stdin io.Reader) int {
	if o.json {
		return screenJSON(o, paths, agent)
	}
	in, inOK := stdin.(*os.File)
	out, outOK := o.stdout.(*os.File)
	if !inOK || !outOK || !term.IsTerminal(int(in.Fd())) || !term.IsTerminal(int(out.Fd())) {
		// Say first what is wrong with the agent asked for, if anything.
		if _, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandAttach, Agent: agent, Snapshot: true}); err != nil {
			return o.fail(err)
		}
		return o.fail(errors.New("asmai attach draws the agent's screen on a terminal: run it in one, or with --json for the screen as text"))
	}
	columns, rows, err := term.GetSize(int(out.Fd()))
	if err != nil {
		return o.fail(fmt.Errorf("finding the terminal's size: %w", err))
	}
	// The agent's terminal is one row shorter than this one: the last row is
	// the status line.
	conn, r, resp, err := daemon.Open(paths.Socket, daemon.Request{Command: daemon.CommandAttach, Agent: agent, Columns: columns, Rows: max(rows-1, 1)})
	if conn != nil {
		defer conn.Close()
	}
	if err != nil {
		return o.fail(err)
	}
	restore, err := term.MakeRaw(int(in.Fd()))
	if err != nil {
		return o.fail(fmt.Errorf("making the terminal raw: %w", err))
	}

	c := &attachClient{
		out:     out,
		conn:    conn,
		agent:   resp.Attached.Agent,
		columns: columns,
		rows:    rows,
		dirty:   make(chan struct{}, 1),
		end:     make(chan string, 1),
	}
	// The alternate screen keeps the user's own screen to come back to, and
	// without autowrap the status line's last cell cannot scroll it.
	out.WriteString("\x1b[?1049h\x1b[?7l\x1b[H\x1b[2J")
	go c.readFrames(r)
	go c.readKeys(in)
	go c.followSize(int(out.Fd()))
	why := c.draw()

	out.WriteString("\x1b[0m\x1b[?7h\x1b[?25h\x1b[?1049l")
	term.Restore(int(in.Fd()), restore)
	fmt.Fprintln(o.stderr, why)
	return 0
}

// attachClient draws an agent's terminal, which it emulates from the
// daemon's frames, on the user's terminal.
type attachClient struct {
	out   *os.File
	conn  net.Conn
	agent string

	mu            sync.Mutex
	term          *vt.Terminal
	columns, rows int
	renderer      vt.Renderer
	dirty         chan struct{}
	end           chan string
}

func (c *attachClient) markDirty() {
	select {
	case c.dirty <- struct{}{}:
	default:
	}
}

func (c *attachClient) finish(why string) {
	select {
	case c.end <- why:
	default:
	}
}

// readFrames applies each of the daemon's frames to the client's terminal.
func (c *attachClient) readFrames(r io.Reader) {
	dec := json.NewDecoder(r)
	for {
		var f daemon.Frame
		if err := dec.Decode(&f); err != nil {
			c.finish(fmt.Sprintf("Detached from %s: the factory stopped or could not be reached.", c.agent))
			return
		}
		c.mu.Lock()
		switch {
		case f.State != nil:
			t, err := vt.FromState(*f.State)
			if err != nil {
				c.mu.Unlock()
				c.finish(fmt.Sprintf("Detached from %s: %v.", c.agent, err))
				return
			}
			c.term = t
			c.renderer.Invalidate()
		case f.Output != nil && c.term != nil:
			c.term.Write(f.Output)
			// The daemon's terminal answers the agent's queries; this one
			// only draws.
			c.term.TakeResponses()
		}
		c.mu.Unlock()
		if f.Ended != "" {
			c.finish(fmt.Sprintf("%s's session ended (%s).", c.agent, f.Ended))
			return
		}
		c.markDirty()
	}
}

// readKeys reads the user's keys. None reaches the agent; Ctrl-] detaches.
func (c *attachClient) readKeys(in *os.File) {
	buf := make([]byte, 256)
	for {
		n, err := in.Read(buf)
		if strings.IndexByte(string(buf[:n]), detachKey) >= 0 {
			c.finish(fmt.Sprintf("Detached from %s.", c.agent))
			return
		}
		if err != nil {
			c.finish(fmt.Sprintf("Detached from %s: the terminal closed.", c.agent))
			return
		}
	}
}

// followSize gives the agent's terminal this terminal's size, less the
// status line, whenever it changes.
func (c *attachClient) followSize(fd int) {
	changed := make(chan os.Signal, 1)
	signal.Notify(changed, syscall.SIGWINCH)
	for range changed {
		columns, rows, err := term.GetSize(fd)
		if err != nil {
			continue
		}
		c.mu.Lock()
		resized := columns != c.columns || rows != c.rows
		c.columns, c.rows = columns, rows
		c.renderer.Invalidate()
		c.mu.Unlock()
		if resized {
			line, _ := json.Marshal(daemon.Resize{Columns: columns, Rows: max(rows-1, 1)})
			c.conn.Write(append(line, '\n'))
		}
		c.markDirty()
	}
}

// draw draws a frame whenever the screen changed, at most one per
// frameInterval, until the client ends, and returns why it ended.
func (c *attachClient) draw() string {
	for {
		select {
		case why := <-c.end:
			return why
		case <-c.dirty:
		}
		c.mu.Lock()
		var frame []byte
		if c.term != nil {
			frame = c.renderer.Render(c.term, c.columns, max(c.rows-1, 0))
			frame = append(frame, c.statusLine()...)
		}
		c.mu.Unlock()
		if _, err := c.out.Write(frame); err != nil {
			return fmt.Sprintf("Detached from %s: %v.", c.agent, err)
		}
		select {
		case why := <-c.end:
			return why
		case <-time.After(frameInterval):
		}
	}
}

// statusLine draws the status line on the terminal's last row, and puts the
// cursor back where the agent's is.
func (c *attachClient) statusLine() string {
	text := fmt.Sprintf(" %s · observing · Ctrl-] detaches", c.agent)
	if title := c.term.Title(); title != "" {
		text += " · " + title
	}
	runes := []rune(text)
	if len(runes) > c.columns {
		runes = runes[:c.columns]
	}
	line := string(runes) + strings.Repeat(" ", c.columns-len(runes))
	var b strings.Builder
	fmt.Fprintf(&b, "\x1b[?2026h\x1b7\x1b[%d;1H\x1b[0;7m%s\x1b[0m\x1b8\x1b[?2026l", c.rows, line)
	return b.String()
}

// screenJSON prints the agent's screen as it is now.
func screenJSON(o *output, paths statedir.Paths, agent string) int {
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandAttach, Agent: agent, Snapshot: true})
	if err != nil {
		return o.fail(err)
	}
	if resp.Screen == nil || resp.Attached == nil {
		return o.fail(errors.New("the daemon did not answer with the agent's screen"))
	}
	t, err := vt.FromState(*resp.Screen)
	if err != nil {
		return o.fail(err)
	}
	columns, rows := t.Size()
	x, y, visible := t.Cursor()
	return o.printJSON(map[string]any{
		"agent": resp.Attached,
		"screen": map[string]any{
			"columns": columns,
			"rows":    rows,
			"lines":   t.Lines(),
			"cursor":  map[string]any{"x": x, "y": y, "visible": visible},
			"title":   t.Title(),
		},
	})
}
