// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"io"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/dashboard/dashtest"
)

// menuClient is a history client of agent with a fake daemon that knows the
// agents a Ctrl-] menu lists, typing through w.
func menuClient(t *testing.T, agent string, conversation bool) (*attachClient, io.Writer, *dashtest.FakeDaemon) {
	t.Helper()
	fake := dashtest.NewFakeDaemon(t)
	fake.SetAgents(
		daemon.DashboardAgent{Agent: "leader@coordination", Role: "coordination", Status: daemon.StatusIdle},
		daemon.DashboardAgent{Agent: "worker1@engineering", Role: "engineering", Status: daemon.StatusWorking, Doing: "assignment 3"},
		daemon.DashboardAgent{Agent: "worker2@engineering", Role: "engineering", Status: daemon.StatusWaitingOnYou, Doing: "native prompt: Bash"},
		daemon.DashboardAgent{Agent: "worker3@quality", Role: "quality", Status: daemon.StatusStopped},
	)
	fake.Start()
	client, server := net.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })
	go io.Copy(io.Discard, server)
	c := historyClient(t, conversation, client)
	c.agent = agent
	c.paths = fake.Paths()
	c.terminal = "/dev/pts/7"
	r, w := io.Pipe()
	go c.readKeys(r)
	return c, w, fake
}

func screenText(c *attachClient) string {
	c.mu.Lock()
	c.rows = 24
	c.mu.Unlock()
	return strings.Join(shown(c), "\n")
}

func waitText(t *testing.T, c *attachClient, want ...string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		text := screenText(c)
		ok := true
		for _, w := range want {
			ok = ok && strings.Contains(text, w)
		}
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the screen shows\n%s\nwant %q", text, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Ctrl-] opens the menu with detach, take, release and interrupt, and lists
// the conversation and every agent, those waiting on the user first.
func TestCtrlBracketOpensTheMenuListingTheConversationAndEveryAgentWaitingFirst(t *testing.T) {
	c, w, _ := menuClient(t, "worker1@engineering", false)
	io.WriteString(w, string(rune(detachKey)))
	waitText(t, c, "Detach", "Take", "Release", "Interrupt", "Conversation", "worker1@engineering", "worker2@engineering", "worker3@quality", "stopped")
	text := screenText(c)
	index := func(s string) int { return strings.Index(text, s) }
	if !(index("worker2@engineering ·") < index("Conversation ·") && index("Conversation ·") < index("worker1@engineering ·") && index("worker1@engineering ·") < index("worker3@quality ·")) {
		t.Errorf("the menu does not list the agent waiting on the user first:\n%s", text)
	}
	if index("worker2@engineering · Waiting on you") < 0 {
		t.Errorf("the menu does not say worker2 waits on the user:\n%s", text)
	}
	select {
	case why := <-c.end:
		t.Fatalf("opening the menu ended the client: %q", why)
	default:
	}
	// Esc closes the menu without doing anything.
	io.WriteString(w, "\x1b")
	deadline := time.Now().Add(5 * time.Second)
	for strings.Contains(screenText(c), "Move this terminal to") {
		if time.Now().After(deadline) {
			t.Fatal("Esc did not close the menu")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Choosing an agent moves the terminal there: the client ends and asks for
// the agent to be attached next.
func TestChoosingAnAgentInTheMenuMovesTheTerminalThere(t *testing.T) {
	c, w, _ := menuClient(t, "leader@coordination", false)
	io.WriteString(w, string(rune(detachKey)))
	waitText(t, c, "worker2@engineering")
	// Detach, Take, Release, Interrupt, then the first listed: worker2.
	io.WriteString(w, "\x1b[B\x1b[B\x1b[B\x1b[B\r")
	<-c.end
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.next == nil || c.next.Command != daemon.CommandAttach || c.next.Agent != "worker2@engineering" {
		t.Errorf("the client moves to %+v, want an attach of worker2@engineering", c.next)
	}
}

// Taking and interrupting an agent through the menu move the terminal to a
// take of it, and an interrupt takes it over the running turn.
func TestTheMenuTakesAndInterruptsTheAgentShown(t *testing.T) {
	for _, tt := range []struct {
		name      string
		down      string
		interrupt bool
	}{{"take", "\x1b[B", false}, {"interrupt", "\x1b[B\x1b[B\x1b[B", true}} {
		t.Run(tt.name, func(t *testing.T) {
			c, w, _ := menuClient(t, "worker1@engineering", false)
			io.WriteString(w, string(rune(detachKey)))
			waitText(t, c, "Take")
			io.WriteString(w, tt.down+"\r")
			<-c.end
			c.mu.Lock()
			defer c.mu.Unlock()
			if c.next == nil || c.next.Command != daemon.CommandTake || c.next.Agent != "worker1@engineering" || c.next.Interrupt != tt.interrupt || c.next.Terminal != "/dev/pts/7" {
				t.Errorf("the client moves to %+v", c.next)
			}
		})
	}
}

// What cannot be done where the terminal is says why instead of doing it.
func TestTheMenuSaysWhyAnActionCannotBeChosen(t *testing.T) {
	c, w, _ := menuClient(t, "worker1@engineering", false)
	io.WriteString(w, string(rune(detachKey)))
	waitText(t, c, "Release")
	io.WriteString(w, "\x1b[B\x1b[B\r")
	waitText(t, c, "! you do not hold worker1@engineering's input")
	select {
	case <-c.end:
		t.Fatal("an action that cannot be done ended the client")
	default:
	}
}

// Releasing through the menu releases the agent's input, and interrupting
// while typing sends Esc to the agent.
func TestTheMenuReleasesAndInterruptsWhileTyping(t *testing.T) {
	c, w, fake := menuClient(t, "worker1@engineering", false)
	c.taking = true
	fake.Handle(daemon.CommandRelease, func(req daemon.Request) daemon.Response {
		return daemon.Response{}
	})
	sent := make(chan daemon.ClientMessage, 4)
	client, server := net.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })
	c.conn = client
	go func() {
		dec := json.NewDecoder(server)
		for {
			var m daemon.ClientMessage
			if dec.Decode(&m) != nil {
				return
			}
			sent <- m
		}
	}()
	io.WriteString(w, string(rune(detachKey))+"\x1b[B\x1b[B\x1b[B\r")
	select {
	case m := <-sent:
		if string(m.Keys) != "\x1b" {
			t.Errorf("interrupting sent %+v, want Esc", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("interrupting sent nothing to the agent")
	}
	io.WriteString(w, string(rune(detachKey))+"\x1b[B\x1b[B\r")
	if why := <-c.end; !strings.HasPrefix(why, "Released worker1@engineering") {
		t.Errorf("releasing ended the client with %q", why)
	}
	calls := fake.Calls()
	if !slices.ContainsFunc(calls, func(r daemon.Request) bool {
		return r.Command == daemon.CommandRelease && r.Agent == "worker1@engineering" && r.Terminal == "/dev/pts/7"
	}) {
		t.Errorf("the menu sent %+v, want a release", calls)
	}
}

// Leaving the conversation through the menu leaves it: the daemon is told,
// which saves the unsent text, and moving elsewhere does the same.
func TestLeavingTheConversationThroughTheMenuLeavesIt(t *testing.T) {
	for _, tt := range []struct {
		name string
		keys string
		next string
	}{{"detach", "\r", ""}, {"move", "\x1b[B\x1b[B\x1b[B\x1b[B\x1b[B\x1b[B\r", daemon.CommandAttach}} {
		t.Run(tt.name, func(t *testing.T) {
			c, w, _ := menuClient(t, "leader@coordination", true)
			sent := make(chan daemon.ClientMessage, 4)
			client, server := net.Pipe()
			t.Cleanup(func() { client.Close(); server.Close() })
			c.conn = client
			go func() {
				dec := json.NewDecoder(server)
				for {
					var m daemon.ClientMessage
					if dec.Decode(&m) != nil {
						return
					}
					sent <- m
				}
			}()
			io.WriteString(w, string(rune(detachKey)))
			waitText(t, c, "Move this terminal to")
			waitText(t, c, "worker3@quality")
			io.WriteString(w, tt.keys)
			select {
			case m := <-sent:
				if !m.Leave {
					t.Errorf("the menu sent %+v, want the conversation left", m)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the menu did not leave the conversation")
			}
			c.mu.Lock()
			defer c.mu.Unlock()
			if (c.next == nil) != (tt.next == "") || (c.next != nil && c.next.Command != tt.next) {
				t.Errorf("the client moves to %+v, want %q", c.next, tt.next)
			}
		})
	}
}
