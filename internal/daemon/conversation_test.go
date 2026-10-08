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
