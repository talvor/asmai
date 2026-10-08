// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
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
	"unicode"

	"golang.org/x/term"

	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/statedir"
	"github.com/talvor/asmai/internal/vt"
)

// detachKey is the key that detaches an attach client, and leaves the
// conversation: Ctrl-]. In M4 it opens a menu instead.
const detachKey = 0x1d

// leaveWait is how long the conversation waits for the daemon to say the
// user has left before it detaches anyway.
const leaveWait = 5 * time.Second

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
	return show(o, paths, daemon.Request{Command: daemon.CommandAttach, Agent: agent}, in, out, "")
}

// show sends the daemon req, attach or the conversation, and draws the
// agent's terminal it streams on out, which is in's terminal, until the user
// detaches or the session ends. In the conversation, the keys typed on in
// reach the agent; otherwise none does. notice, if any, shows on the status
// line until the user's first key.
func show(o *output, paths statedir.Paths, req daemon.Request, in, out *os.File, notice string) int {
	columns, rows, err := term.GetSize(int(out.Fd()))
	if err != nil {
		return o.fail(fmt.Errorf("finding the terminal's size: %w", err))
	}
	// The agent's terminal is one row shorter than this one: the last row is
	// the status line.
	req.Columns, req.Rows = columns, max(rows-1, 1)
	conn, r, resp, err := daemon.Open(paths.Socket, req)
	if conn != nil {
		defer conn.Close()
	}
	if err != nil {
		return o.fail(err)
	}
	if resp.Attached == nil {
		return o.fail(errors.New("the daemon did not answer with the agent it attached"))
	}
	restore, err := term.MakeRaw(int(in.Fd()))
	if err != nil {
		return o.fail(fmt.Errorf("making the terminal raw: %w", err))
	}

	c := &attachClient{
		out:          out,
		conn:         conn,
		agent:        resp.Attached.Agent,
		conversation: req.Command == daemon.CommandConversation,
		notice:       notice,
		columns:      columns,
		rows:         rows,
		dirty:        make(chan struct{}, 1),
		end:          make(chan string, 1),
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
// daemon's frames, on the user's terminal. In the conversation it also sends
// the daemon the user's keys.
type attachClient struct {
	out          *os.File
	conn         net.Conn
	agent        string
	conversation bool
	// sending keeps each line sent to the daemon whole.
	sending sync.Mutex

	mu            sync.Mutex
	notice        string
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
		if f.Left {
			c.finish(leftConversation)
			return
		}
		c.markDirty()
	}
}

// leftConversation is what the conversation says once the user has left it.
const leftConversation = "Left the conversation. Nothing is paused: Coordination is back under automation."

// readKeys reads the user's keys until Ctrl-]. Outside the conversation none
// reaches the agent, and Ctrl-] detaches. In the conversation every other
// key is sent on as typed, Esc included, and Ctrl-] leaves it.
func (c *attachClient) readKeys(in *os.File) {
	buf := make([]byte, 256)
	for {
		n, err := in.Read(buf)
		keys := buf[:n]
		detach := bytes.IndexByte(keys, detachKey)
		if detach >= 0 {
			keys = keys[:detach]
		}
		if c.conversation && len(keys) > 0 {
			c.mu.Lock()
			c.notice = ""
			c.mu.Unlock()
			c.markDirty()
			c.send(daemon.ClientMessage{Keys: keys})
		}
		if detach >= 0 {
			c.leave()
			return
		}
		if err != nil {
			c.finish(fmt.Sprintf("Detached from %s: the terminal closed.", c.agent))
			return
		}
	}
}

// leave detaches: from the conversation, once the daemon says the user has
// left it.
func (c *attachClient) leave() {
	if !c.conversation {
		c.finish(fmt.Sprintf("Detached from %s.", c.agent))
		return
	}
	if !c.send(daemon.ClientMessage{Leave: true}) {
		c.finish(leftConversation)
		return
	}
	time.AfterFunc(leaveWait, func() { c.finish(leftConversation) })
}

// send sends the daemon one line.
func (c *attachClient) send(m daemon.ClientMessage) bool {
	line, _ := json.Marshal(m)
	c.sending.Lock()
	defer c.sending.Unlock()
	_, err := c.conn.Write(append(line, '\n'))
	return err == nil
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
			c.send(daemon.ClientMessage{Columns: columns, Rows: max(rows-1, 1)})
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
	if c.conversation {
		text = fmt.Sprintf(" %s · conversation · Ctrl-] leaves", c.agent)
		if c.notice != "" {
			text += " · " + c.notice
		}
	}
	if title := strings.Map(printable, c.term.Title()); title != "" {
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

func printable(r rune) rune {
	if unicode.IsControl(r) {
		return -1
	}
	return r
}

// screenJSON prints the agent's screen as it is now.
func screenJSON(o *output, paths statedir.Paths, agent string) int {
	screen, err := snapshot(paths, agent)
	if err != nil {
		return o.fail(err)
	}
	return o.printJSON(screen)
}

// snapshot returns the agent and its screen as it is now, as --json prints
// them.
func snapshot(paths statedir.Paths, agent string) (map[string]any, error) {
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandAttach, Agent: agent, Snapshot: true})
	if err != nil {
		return nil, err
	}
	if resp.Screen == nil || resp.Attached == nil {
		return nil, errors.New("the daemon did not answer with the agent's screen")
	}
	t, err := vt.FromState(*resp.Screen)
	if err != nil {
		return nil, err
	}
	columns, rows := t.Size()
	x, y, visible := t.Cursor()
	return map[string]any{
		"agent": resp.Attached,
		"screen": map[string]any{
			"columns": columns,
			"rows":    rows,
			"lines":   t.Lines(),
			"cursor":  map[string]any{"x": x, "y": y, "visible": visible},
			"title":   t.Title(),
		},
	}, nil
}
