// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"testing"

	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/session"
)

func TestOnlyAnEnterThatCanSubmitIsCounted(t *testing.T) {
	for _, tc := range []struct {
		name string
		// keys are the messages the user's terminal sent, in order.
		keys []string
		want int
	}{
		{"a message", []string{"hello\r"}, 1},
		{"two messages", []string{"one\r", "two\r"}, 2},
		{"text never submitted", []string{"draft", "\x1b"}, 0},
		{"Alt-Enter starts a new line", []string{"\x1b\r"}, 0},
		{"Alt-Enter inside a message", []string{"one\x1b\rtwo\r"}, 1},
		// The client sends Alt-Enter as one message, so an Esc ending one
		// message is the Esc key, and the Enter after it submits.
		{"Esc, then Enter", []string{"\x1b", "\r"}, 1},
		{"text and Esc, then Enter", []string{"one\x1b", "\rtwo\r"}, 2},
		{"Esc, then text, then Enter", []string{"\x1b", "a", "\r"}, 1},
		{"pasted lines", []string{"\x1b[200~one\rtwo\r\x1b[201~\r"}, 1},
		{"a paste split across reads", []string{"\x1b[2", "00~one\r", "two\x1b[20", "1~", "\r"}, 1},
	} {
		c := &conversation{}
		got := 0
		for _, keys := range tc.keys {
			got += c.enters([]byte(keys))
		}
		if got != tc.want {
			t.Errorf("%s: %q counts %d Enter keys, want %d", tc.name, tc.keys, got, tc.want)
		}
	}
}

func TestTheUsersSubmissionsAreToldApartByTheEnterKeysTheyTyped(t *testing.T) {
	type step struct {
		do   string // "enter", "submit" or "stop"
		want bool   // for "submit": whether it is the user's message
	}
	for _, tc := range []struct {
		name  string
		steps []step
	}{
		{"a message", []step{{"enter", false}, {"submit", true}, {"stop", false}, {"submit", false}}},
		{"no key typed", []step{{"submit", false}}},
		{"a message queued during the turn", []step{{"enter", false}, {"submit", true}, {"enter", false}, {"stop", false}, {"submit", true}}},
		{"two messages typed before the first is confirmed", []step{{"enter", false}, {"enter", false}, {"submit", true}, {"stop", false}, {"submit", true}}},
		{"an Enter on a menu, then a message", []step{{"enter", false}, {"enter", false}, {"submit", true}, {"stop", false}, {"stop", false}, {"submit", false}}},
		{"an Enter on a menu in one turn, then a message", []step{{"enter", false}, {"stop", false}, {"enter", false}, {"submit", true}, {"stop", false}, {"submit", false}}},
		{"two messages queued during the turn, submitted together", []step{{"enter", false}, {"submit", true}, {"enter", false}, {"enter", false}, {"stop", false}, {"submit", true}, {"stop", false}, {"submit", false}}},
		{"an Enter on a menu in a turn that queues nothing", []step{{"enter", false}, {"stop", false}, {"stop", false}, {"submit", false}}},
	} {
		l := &leader{}
		for i, s := range tc.steps {
			switch s.do {
			case "enter":
				l.userInput = true
				l.submits++
			case "stop":
				l.finished()
			case "submit":
				if _, ok := l.submitted(); ok != s.want {
					t.Errorf("%s: submission at step %d taken for the user's: %v, want %v", tc.name, i, ok, s.want)
				}
				if l.userInput {
					t.Errorf("%s: after the submission at step %d the user still owns the input", tc.name, i)
				}
			}
		}
		if last := tc.steps[len(tc.steps)-1]; last.do == "submit" && !last.want && (l.submits != 0 || l.earlier != 0 || l.userInput) {
			t.Errorf("%s: %d Enter keys since the last finished turn and %d before it are left over, and the user owns the input: %v; want none left over", tc.name, l.submits, l.earlier, l.userInput)
		}
	}
}

func TestEscAndCtrlCAreTheKeysThatMayInterruptATurn(t *testing.T) {
	for _, tc := range []struct {
		name string
		keys []string
		want bool
	}{
		{"Esc", []string{"\x1b"}, true},
		{"text, then Esc", []string{"draft\x1b"}, true},
		{"Ctrl-C", []string{"\x03"}, true},
		{"text", []string{"hello\r"}, false},
		{"Alt-Enter", []string{"\x1b\r"}, false},
		{"an arrow key", []string{"\x1b[A"}, false},
		{"Ctrl-C inside a paste", []string{"\x1b[200~one\x03"}, false},
		{"a paste ended", []string{"\x1b[200~one\x1b[201~"}, false},
	} {
		c := &conversation{}
		got := false
		for _, keys := range tc.keys {
			c.enters([]byte(keys))
			got = c.interrupts([]byte(keys))
		}
		if got != tc.want {
			t.Errorf("%s: %q may interrupt a turn: %v, want %v", tc.name, tc.keys, got, tc.want)
		}
	}
}

func TestNothingIsTypedWhileAnInterruptedTurnsStateIsUnknownAndItsOwnStopEndsIt(t *testing.T) {
	d := &daemon{leaders: map[string]*leader{}}
	l := &leader{session: &session.Session{}, generation: 1, ready: true, currentDispatch: 7, currentPromptID: "turn", interrupted: 7}
	d.leaders["leader@coordination"] = l
	// The nudge is held: it would reach the store, which this daemon has
	// none of, if it were not.
	d.nudge("leader@coordination")
	if l.renudge != 0 || l.pendingDispatch != 0 {
		t.Fatalf("with the turn's state unknown, the leader was nudged: %+v", l)
	}
	ref := sessionRef{address: roles.LeaderOf(roles.Coordination), generation: 1}
	if settled, err := d.interruptedTurn(l, ref, "turn", ""); settled || err != nil {
		t.Fatalf("the turn's own Stop settled it as interrupted (%v, %v), want it to end as any turn does", settled, err)
	}
	if l.interrupted != 0 || l.renudge != 0 || l.currentDispatch != 7 {
		t.Errorf("after the turn's own Stop, interrupted is %d, renudge %d and the current dispatch %d, want 0, 0 and 7 for the Stop to end", l.interrupted, l.renudge, l.currentDispatch)
	}
}
