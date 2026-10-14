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
