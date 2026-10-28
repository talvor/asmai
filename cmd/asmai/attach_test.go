// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
	"unicode/utf8"

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
	want := " worker1@engineering · observing · Ctrl-] detaches · claude-op"
	columns := utf8.RuneCountInString(want)
	c := &attachClient{agent: "worker1@engineering", model: daemon.Model{Name: "claude-opus-5-5", Effort: "high"}, columns: columns, rows: 6, term: vt.New(columns, 5)}
	line := c.statusLine()
	if got := statusText(line); got != want {
		t.Errorf("on a %d-column terminal the status line is %q, want it cut inside the model to %q", columns, got, want)
	}
	if strings.Contains(line, "\n") || strings.Contains(line, "high effort") {
		t.Errorf("on a %d-column terminal the status line %q wraps or shows the effort", columns, line)
	}
}

// What waits on the user stays visible: a native prompt nobody can answer
// comes before the model and effort, so on a terminal it fitted before the
// model was shown it still shows in full, and the model is what is cut.
func TestANativePromptShowsInFullBesideTheModel(t *testing.T) {
	prompt := "worker1@engineering waits at a native prompt nobody can answer"
	before := " leader@coordination · observing · Ctrl-] detaches · " + prompt
	columns := utf8.RuneCountInString(before + " · cl")
	c := &attachClient{agent: "leader@coordination", prompts: []string{prompt}, model: daemon.Model{Name: "claude-opus-5-5", Effort: "medium"}, columns: columns, rows: 6, term: vt.New(columns, 5)}
	if got, want := statusText(c.statusLine()), before+" · cl"; got != want {
		t.Errorf("on a %d-column terminal the status line is %q, want the prompt in full and then the model cut: %q", columns, got, want)
	}
}

// asmai attach --json and --toon print the agent's model and effort with
// the rest of the agent once the daemon knows them, and leave out each it
// does not.
func TestTheAttachSnapshotCarriesTheModelAndEffortOnlyWhenKnown(t *testing.T) {
	screen := vt.New(20, 2).State()
	for _, tt := range []struct {
		model, effort string
		want, absent  []string
	}{
		{model: "claude-opus-5-5", effort: "medium", want: []string{"claude-opus-5-5", "medium"}},
		{model: "opus", want: []string{"opus"}, absent: []string{"effort"}},
		{absent: []string{"model", "effort"}},
	} {
		resp := daemon.Response{Attached: &daemon.Leader{Agent: "leader@coordination", State: "running", Generation: 1, PID: 7, Model: tt.model, Effort: tt.effort}, Screen: &screen}
		data, err := snapshotOf(resp)
		if err != nil {
			t.Fatal(err)
		}
		for _, toon := range []bool{false, true} {
			var stdout, stderr strings.Builder
			if code := (&output{stdout: &stdout, stderr: &stderr, json: !toon, toon: toon}).printData(data); code != 0 {
				t.Fatalf("printing the snapshot failed: %s", stderr.String())
			}
			if !toon {
				var printed struct {
					Agent struct {
						Agent  string  `json:"agent"`
						Model  *string `json:"model"`
						Effort *string `json:"effort"`
					} `json:"agent"`
				}
				if err := json.Unmarshal([]byte(stdout.String()), &printed); err != nil {
					t.Fatal(err)
				}
				if a := printed.Agent; a.Agent != "leader@coordination" || (a.Model == nil) != (tt.model == "") || a.Model != nil && *a.Model != tt.model || (a.Effort == nil) != (tt.effort == "") || a.Effort != nil && *a.Effort != tt.effort {
					t.Errorf("asmai attach --json printed %s, want the agent with model %q and effort %q", stdout.String(), tt.model, tt.effort)
				}
				continue
			}
			for _, w := range tt.want {
				if !strings.Contains(stdout.String(), w) {
					t.Errorf("asmai attach --toon printed %s, want %q in it", stdout.String(), w)
				}
			}
			for _, key := range tt.absent {
				if strings.Contains(stdout.String(), key) {
					t.Errorf("asmai attach --toon printed %s, want no %s", stdout.String(), key)
				}
			}
		}
	}
}

// statusText returns the text a status line draws, without the spaces that
// fill the row after it.
func statusText(line string) string {
	_, text, _ := strings.Cut(line, "\x1b[0;7m")
	text, _, _ = strings.Cut(text, "\x1b[0m")
	return strings.TrimRight(text, " ")
}
