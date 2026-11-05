// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/vt"
)

// An agent's credential cannot take: the command is the user's.
func TestAnAgentCannotRunAsmaiTake(t *testing.T) {
	t.Setenv(daemon.SessionCredential, "an-agent-session")
	var stdout, stderr bytes.Buffer

	code := run([]string{"take", "worker1@engineering"}, strings.NewReader(""), &stdout, &stderr)

	if code != 1 || !strings.Contains(stderr.String(), "an agent cannot run this command; ask the user to run `asmai take worker1@engineering`") {
		t.Errorf("an agent running asmai take exited %d printing %q, want it refused", code, stderr.String())
	}
}

// Take needs the agent named, and a terminal to type in.
func TestTakeNeedsAnAgentAndATerminal(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"take"}, strings.NewReader(""), &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "name the agent") {
		t.Errorf("asmai take without an agent exited %d printing %q, want a usage error", code, stderr.String())
	}
	stderr.Reset()
	if code := run([]string{"take", "worker1@engineering", "--interrupt"}, strings.NewReader(""), &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "needs a terminal") {
		t.Errorf("asmai take --interrupt outside a terminal exited %d printing %q, want it to need a terminal", code, stderr.String())
	}
}

// The status line of every attached terminal shows the input the user holds,
// from the daemon's frames, and a frame that says there is none clears it; the
// terminal that took the input shows that it types, and the terminal it was
// moved from observes.
func TestTheStatusLineShowsTheInputTheUserHolds(t *testing.T) {
	state := vt.New(300, 5).State()
	held, none := []string{"you hold worker1@engineering's input"}, []string{}
	c := &attachClient{agent: "worker1@engineering", taking: true, columns: 300, rows: 6, dirty: make(chan struct{}, 1), end: make(chan string, 1)}
	r, w := io.Pipe()
	go c.readFrames(r)
	enc := json.NewEncoder(w)
	line := func(f daemon.Frame) string {
		t.Helper()
		if err := enc.Encode(f); err != nil {
			t.Fatal(err)
		}
		<-c.dirty
		c.mu.Lock()
		defer c.mu.Unlock()
		return statusText(c.statusLine())
	}
	typing := " worker1@engineering · typing · Ctrl-] detaches · ↑/↓ history"
	if got, want := line(daemon.Frame{State: &state, Held: &held}), " you hold worker1@engineering's input ·"+typing; got != want {
		t.Errorf("the terminal that took the input shows %q, want %q", got, want)
	}
	if got, want := line(daemon.Frame{Held: &none}), typing; got != want {
		t.Errorf("once the hold is gone the status line is %q, want %q", got, want)
	}
	if got, want := line(daemon.Frame{TakenOver: "/dev/pts/9", Held: &held}), " you hold worker1@engineering's input · worker1@engineering · observing · your hold on its input moved to /dev/pts/9 · Ctrl-] detaches · ↑/↓ history"; got != want {
		t.Errorf("the older terminal shows %q, want %q", got, want)
	}
	if c.observing() == false {
		t.Errorf("the older terminal does not only observe")
	}
	w.Close()
	<-c.end

	// A terminal that only observes shows the held input too.
	o := &attachClient{agent: "leader@coordination", columns: 80, rows: 6, held: held, term: vt.New(80, 5)}
	if got := statusText(o.statusLine()); !strings.HasPrefix(got, " you hold worker1@engineering's input · leader@coordination") {
		t.Errorf("an observing terminal at 80 columns shows %q, want the held agent first", got)
	}
	multiple := []string{
		"you hold worker1@engineering's input",
		"you hold worker2@engineering's input",
		"you hold worker3@engineering's input",
	}
	for _, mode := range []struct {
		name         string
		conversation bool
		taking       bool
		takenOver    string
	}{
		{name: "observing"},
		{name: "conversation", conversation: true},
		{name: "typing", taking: true},
		{name: "displaced", taking: true, takenOver: "/dev/pts/9"},
	} {
		t.Run(mode.name, func(t *testing.T) {
			a := &attachClient{agent: "leader@coordination", columns: 80, rows: 6, held: multiple, term: vt.New(80, 5), conversation: mode.conversation, taking: mode.taking, takenOver: mode.takenOver}
			got := statusText(a.statusLine())
			for _, agent := range []string{"worker1@engineering", "worker2@engineering", "worker3@engineering"} {
				if !strings.Contains(got, agent) {
					t.Errorf("the 80-column status %q omits %s", got, agent)
				}
			}
		})
	}
}
