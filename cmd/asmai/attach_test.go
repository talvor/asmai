// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

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
		shows := strings.Contains(line, "leader@coordination · observing · Ctrl-] detaches · "+waiting[0])
		if want := i < 2; shows != want {
			t.Errorf("after frame %d the status line is %q, want the prompt shown: %t", i, line, want)
		}
	}
}

// The status line shows the model the agent runs, and its effort once the
// daemon knows it, in an observed session and in the conversation alike,
// after the existing hints; a frame tells the client when either changes.
func TestTheStatusLineShowsTheAgentsModelAndEffortOnceKnown(t *testing.T) {
	state := vt.New(200, 5).State()
	for _, conversation := range []bool{false, true} {
		hints := "leader@coordination · observing · Ctrl-] detaches"
		if conversation {
			hints = "leader@coordination · conversation · Ctrl-] leaves"
		}
		c := &attachClient{agent: "leader@coordination", conversation: conversation, model: daemon.Model{Name: "opus"}, columns: 200, rows: 6, dirty: make(chan struct{}, 1), end: make(chan string, 1)}
		r, w := io.Pipe()
		go c.readFrames(r)
		enc := json.NewEncoder(w)
		line := func(after daemon.Frame) string {
			t.Helper()
			if err := enc.Encode(after); err != nil {
				t.Fatal(err)
			}
			<-c.dirty
			c.mu.Lock()
			defer c.mu.Unlock()
			return statusText(c.statusLine())
		}
		if got, want := line(daemon.Frame{State: &state}), " "+hints+" · opus"; got != want {
			t.Errorf("in the conversation %t, before the effort is known the status line is %q, want %q", conversation, got, want)
		}
		if got, want := line(daemon.Frame{Model: &daemon.Model{Name: "claude-opus-5-5", Effort: "medium"}}), " "+hints+" · claude-opus-5-5 · medium effort"; got != want {
			t.Errorf("in the conversation %t, once the effort is known the status line is %q, want %q", conversation, got, want)
		}
		w.Close()
		<-c.end
	}
}

// A model and effort that do not fit cut the status line short at the
// terminal's width rather than wrapping it.
func TestTheModelTruncatesTheStatusLineOnANarrowTerminal(t *testing.T) {
	c := &attachClient{agent: "worker1@engineering", model: daemon.Model{Name: "claude-opus-5-5", Effort: "high"}, columns: 40, rows: 6, term: vt.New(40, 5)}
	got := statusText(c.statusLine())
	want := " worker1@engineering · observing · Ctrl-"
	if got != want {
		t.Errorf("on a 40-column terminal the status line is %q, want it cut to %q", got, want)
	}
}

// statusText returns the text a status line draws, without the spaces that
// fill the row after it.
func statusText(line string) string {
	_, text, _ := strings.Cut(line, "\x1b[0;7m")
	text, _, _ = strings.Cut(text, "\x1b[0m")
	return strings.TrimRight(text, " ")
}
