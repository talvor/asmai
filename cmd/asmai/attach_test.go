// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/vt"
)

// The status line shows the native prompts agents wait at that nobody can
// answer, from the frame that tells the client of them until one says
// there are none.
func TestTheStatusLineShowsANativePromptNobodyCanAnswerUntilItIsGone(t *testing.T) {
	waiting := []string{"worker1@engineering waits at a native prompt nobody can answer: Bash `cat > x`"}
	none := []string{}
	state := vt.New(200, 5).State()
	frames := []daemon.Frame{
		{State: &state, Prompts: &waiting},
		{Output: []byte("more output")},
		{Prompts: &none},
	}
	c := &attachClient{agent: "leader@coordination", columns: 200, rows: 6, dirty: make(chan struct{}, 1), end: make(chan string, 1)}
	r, w := io.Pipe()
	go c.readFrames(r)
	enc := json.NewEncoder(w)
	for i, f := range frames {
		if err := enc.Encode(f); err != nil {
			t.Fatal(err)
		}
		// The client has applied a frame once it reads the next; the last
		// is applied when the stream closes.
		if i == len(frames)-1 {
			w.Close()
			<-c.end
		} else {
			<-c.dirty
		}
		c.mu.Lock()
		line := c.statusLine()
		c.mu.Unlock()
		shows := strings.Contains(line, "leader@coordination · observing · Ctrl-] detaches · ↑/↓ history · "+waiting[0])
		if want := i < 2; shows != want {
			t.Errorf("after frame %d the status line is %q, want the prompt shown: %t", i, line, want)
		}
	}
}

// historyClient is an attach client of agent showing a terminal of 10 by 3
// in its own of 100 by 4, whose history holds lines 1 to 4 under the
// screen's 5 to 7, with conn as its connection to the daemon.
func historyClient(t *testing.T, conversation bool, conn net.Conn) *attachClient {
	t.Helper()
	term := vt.New(10, 3)
	term.Write([]byte("1\r\n2\r\n3\r\n4\r\n5\r\n6\r\n7"))
	return &attachClient{agent: "leader@coordination", conversation: conversation, conn: conn, term: term,
		columns: 100, rows: 4, dirty: make(chan struct{}, 1), end: make(chan string, 1)}
}

// shown is what the client draws on a terminal of its size: the agent's
// screen, scrolled back as it is, over the status line.
func shown(c *attachClient) []string {
	c.mu.Lock()
	c.renderer.Invalidate()
	c.mu.Unlock()
	screen := vt.New(c.columns, c.rows)
	screen.Write(c.frame())
	return screen.Lines()
}

// typeKeys types keys on the client's terminal, and waits for the client to
// have read them.
func typeKeys(t *testing.T, c *attachClient, w io.Writer, keys string) {
	t.Helper()
	if _, err := io.WriteString(w, keys); err != nil {
		t.Fatal(err)
	}
	select {
	case <-c.dirty:
	case <-time.After(5 * time.Second):
		t.Fatalf("the client did not read %q", keys)
	}
}

func checkShown(t *testing.T, c *attachClient, after string, want ...string) {
	t.Helper()
	if got := shown(c)[:len(want)]; !slices.Equal(got, want) {
		t.Errorf("after %s the client shows %q, want %q", after, got, want)
	}
}

// Observing, the up and down arrows scroll the view through the history,
// stopping at its oldest line and at the live screen, and the status line
// says how far back it is.
func TestTheArrowsScrollTheObservedHistory(t *testing.T) {
	c := historyClient(t, false, nil)
	r, w := io.Pipe()
	go c.readKeys(r)
	if line := shown(c)[3]; !strings.Contains(line, "↑/↓ history") || strings.Contains(line, "back") {
		t.Errorf("the status line of the live screen is %q, want the keys that scroll it named and no lines back", line)
	}

	typeKeys(t, c, w, "\x1b[A")
	checkShown(t, c, "one up", "4", "5", "6")
	if line := shown(c)[3]; !strings.Contains(line, "1 of 4 lines back") {
		t.Errorf("one line back, the status line is %q", line)
	}
	typeKeys(t, c, w, "\x1b[A\x1b[A\x1b[A\x1b[A\x1b[A")
	checkShown(t, c, "more ups than the history holds", "1", "2", "3")
	// Other keys do nothing while observing.
	typeKeys(t, c, w, "x\r")
	checkShown(t, c, "other keys", "1", "2", "3")
	typeKeys(t, c, w, "\x1b[B\x1bOB")
	checkShown(t, c, "two downs", "3", "4", "5")
	typeKeys(t, c, w, "\x1b[B\x1b[B\x1b[B")
	checkShown(t, c, "more downs than lines back", "5", "6", "7")
	if c.back != 0 {
		t.Errorf("at the bottom the view is %d lines back, want the live screen", c.back)
	}

	// Ctrl-] still detaches.
	w.Write([]byte{detachKey})
	if why := <-c.end; why != "Detached from leader@coordination." {
		t.Errorf("Ctrl-] ended the client with %q", why)
	}
}

// Scrolled back, the view stays on the lines it shows as the agent's output
// scrolls more into the history, and keeps within a bounded history.
func TestTheScrolledBackViewStaysPutAsOutputArrives(t *testing.T) {
	c := historyClient(t, false, nil)
	c.back = 2
	r, w := io.Pipe()
	defer w.Close()
	go c.readFrames(r)
	enc := json.NewEncoder(w)
	output := func(o string) {
		t.Helper()
		if err := enc.Encode(daemon.Frame{Output: []byte(o)}); err != nil {
			t.Fatal(err)
		}
		<-c.dirty
	}
	output("\r\n8\r\n9")
	checkShown(t, c, "two more lines", "3", "4", "5")

	// Lines 1 to 1009 were written: the screen shows the last three, and
	// the history keeps the ScrollbackLimit lines before them, from 7. The
	// view stops at the oldest.
	var more strings.Builder
	for i := range vt.ScrollbackLimit {
		fmt.Fprintf(&more, "\r\n%d", 10+i)
	}
	output(more.String())
	c.mu.Lock()
	back, history := c.back, c.term.History()
	c.mu.Unlock()
	if back != vt.ScrollbackLimit || history != vt.ScrollbackLimit {
		t.Errorf("the view is %d lines back in a history of %d, want both at %d", back, history, vt.ScrollbackLimit)
	}
	checkShown(t, c, "more lines than the history keeps", "7", "8", "9")
}

// In the conversation, the up and down arrows scroll the history and never
// reach the agent, while every other key is sent on as typed and returns the
// view to the live screen, and Ctrl-] still leaves.
func TestInTheConversationTheArrowsScrollAndAreNotSent(t *testing.T) {
	client, daemonEnd := net.Pipe()
	defer daemonEnd.Close()
	sent := make(chan daemon.ClientMessage, 10)
	go func() {
		dec := json.NewDecoder(daemonEnd)
		for {
			var m daemon.ClientMessage
			if dec.Decode(&m) != nil {
				return
			}
			sent <- m
		}
	}()
	c := historyClient(t, true, client)
	r, w := io.Pipe()
	go c.readKeys(r)

	typeKeys(t, c, w, "\x1b[A\x1b[A")
	checkShown(t, c, "two ups", "3", "4", "5")
	typeKeys(t, c, w, "\x1b[B\x1b[A\x1b[B")
	checkShown(t, c, "down, up and down", "4", "5", "6")
	select {
	case m := <-sent:
		t.Fatalf("the arrows were sent to the agent as %+v", m)
	case <-time.After(100 * time.Millisecond):
	}
	if line := shown(c)[3]; !strings.Contains(line, "conversation · Ctrl-] leaves · ↑/↓ history · 1 of 4 lines back") {
		t.Errorf("one line back in the conversation, the status line is %q", line)
	}

	// Other keys, Esc and modified arrows included, are sent as typed, with
	// the arrows among them left out, and return the view to the live screen.
	typeKeys(t, c, w, "ab\x1b[Acd\x1b\x1b[1;5A\r")
	if m := <-sent; string(m.Keys) != "abcd\x1b\x1b[1;5A\r" {
		t.Errorf("the agent was sent %q, want the keys typed without the arrow", m.Keys)
	}
	checkShown(t, c, "typing", "5", "6", "7")
	if c.back != 0 {
		t.Errorf("after typing the view is %d lines back, want the live screen", c.back)
	}

	w.Write([]byte{detachKey})
	if m := <-sent; !m.Leave {
		t.Errorf("Ctrl-] sent %+v, want the conversation left", m)
	}
}
