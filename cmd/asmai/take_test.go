// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
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
	if got, want := line(daemon.Frame{State: &state, Held: &held}), " held 1: worker1@engineering ·"+typing; got != want {
		t.Errorf("the terminal that took the input shows %q, want %q", got, want)
	}
	if got, want := line(daemon.Frame{Held: &none}), typing; got != want {
		t.Errorf("once the hold is gone the status line is %q, want %q", got, want)
	}
	if got, want := line(daemon.Frame{TakenOver: "/dev/pts/9", Held: &held}), " held 1: worker1@engineering · worker1@engineering · observing · your hold on its input moved to /dev/pts/9 · Ctrl-] detaches · ↑/↓ history"; got != want {
		t.Errorf("the older terminal shows %q, want %q", got, want)
	}
	if c.observing() == false {
		t.Errorf("the older terminal does not only observe")
	}
	w.Close()
	<-c.end

	// A terminal that only observes shows the held input too.
	o := &attachClient{agent: "leader@coordination", columns: 80, rows: 6, held: held, term: vt.New(80, 5)}
	if got := statusText(o.statusLine()); !strings.HasPrefix(got, " held 1: worker1@engineering · leader@coordination") {
		t.Errorf("an observing terminal at 80 columns shows %q, want the held agent first", got)
	}
	modes := []struct {
		name         string
		conversation bool
		taking       bool
		takenOver    string
	}{
		{name: "observing"},
		{name: "conversation", conversation: true},
		{name: "typing", taking: true},
		{name: "displaced", taking: true, takenOver: "/dev/pts/9"},
	}
	for _, size := range []struct {
		count int
		want  string
	}{
		{1, " held 1: worker1@engineering"},
		{3, " held 3: worker1@engineering, worker2@engineering, worker3@engineering"},
		{7, " held 7: worker1@engineering, worker2@engineering, worker3@engineering +4 more"},
	} {
		var holds []string
		for n := 1; n <= size.count; n++ {
			holds = append(holds, fmt.Sprintf("you hold worker%d@engineering's input", n))
		}
		for _, mode := range modes {
			t.Run(fmt.Sprintf("%d holds %s", size.count, mode.name), func(t *testing.T) {
				a := &attachClient{agent: "leader@coordination", columns: 80, rows: 6, held: holds, term: vt.New(80, 5), conversation: mode.conversation, taking: mode.taking, takenOver: mode.takenOver}
				got := statusText(a.statusLine())
				if !strings.HasPrefix(got, size.want) {
					t.Errorf("the 80-column status %q, want prefix %q", got, size.want)
				}
			})
		}
	}
}

// An agent's credential cannot release: only the user returns input.
func TestAnAgentCannotRunAsmaiRelease(t *testing.T) {
	t.Setenv(daemon.SessionCredential, "an-agent-session")
	var stdout, stderr bytes.Buffer

	code := run([]string{"release", "worker1@engineering"}, strings.NewReader(""), &stdout, &stderr)

	if code != 1 || !strings.Contains(stderr.String(), "an agent cannot run this command; ask the user to run `asmai release worker1@engineering`") {
		t.Errorf("an agent running asmai release exited %d printing %q, want it refused", code, stderr.String())
	}
}

func TestReleaseNeedsTheAgentNamed(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"release"}, strings.NewReader(""), &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "name the agent you hold") {
		t.Errorf("asmai release without an agent exited %d printing %q, want a usage error", code, stderr.String())
	}
}

// A terminal that still streams the agent is told when its input is
// released, and ends.
func TestAStreamingTerminalEndsWhenTheInputIsReleased(t *testing.T) {
	c := &attachClient{agent: "worker1@engineering", taking: true, columns: 80, rows: 6, dirty: make(chan struct{}, 1), end: make(chan string, 1)}
	r, w := io.Pipe()
	go c.readFrames(r)
	if err := json.NewEncoder(w).Encode(daemon.Frame{Released: true}); err != nil {
		t.Fatal(err)
	}
	if got := <-c.end; !strings.Contains(got, "Released worker1@engineering") {
		t.Errorf("the terminal ended with %q, want it to say the input was released", got)
	}
}

// An agent's credential cannot replay, and replay needs the agent named.
func TestReplayIsTheUsersAndNeedsAnAgent(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"replay"}, strings.NewReader(""), &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "name the agent") {
		t.Errorf("asmai replay without an agent exited %d printing %q, want a usage error", code, stderr.String())
	}
	t.Setenv(daemon.SessionCredential, "an-agent-session")
	stderr.Reset()
	if code := run([]string{"replay", "worker1@engineering"}, strings.NewReader(""), &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "an agent cannot run this command") {
		t.Errorf("an agent running asmai replay exited %d printing %q, want it refused", code, stderr.String())
	}
}
