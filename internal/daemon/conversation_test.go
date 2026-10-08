// SPDX-License-Identifier: Apache-2.0

package daemon

import "testing"

func TestOnlyAnEnterThatCanSubmitIsCounted(t *testing.T) {
	for _, tc := range []struct {
		name string
		keys []string
		want int
	}{
		{"a message", []string{"hello\r"}, 1},
		{"two messages", []string{"one\r", "two\r"}, 2},
		{"text never submitted", []string{"draft", "\x1b"}, 0},
		{"Esc, then Enter", []string{"\x1b", "\r"}, 1},
		{"Alt-Enter starts a new line", []string{"one\x1b\rtwo\r"}, 1},
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
	}
}
