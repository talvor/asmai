// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/daemon"
)

// Shift-Enter, in each encoding a terminal sends it, and the fallbacks,
// Alt-Enter and backslash-Enter, start a new line in the agent's prompt;
// plain Enter still submits it, and every other key reaches the agent as a
// terminal without the kitty keyboard protocol or modifyOtherKeys sends it.
func TestShiftEnterAndItsFallbacksStartANewLine(t *testing.T) {
	for _, tc := range []struct {
		name string
		keys []string
		want string
	}{
		{"Enter submits", []string{"hello\r"}, "hello\r"},
		{"Shift-Enter, kitty keyboard protocol", []string{"one\x1b[13;2utwo\r"}, "one\x1b\rtwo\r"},
		{"Shift-Enter, kitty with lock keys and event type", []string{"one\x1b[13;66:1utwo\r"}, "one\x1b\rtwo\r"},
		{"Shift-Enter, modifyOtherKeys", []string{"one\x1b[27;2;13~two\r"}, "one\x1b\rtwo\r"},
		{"Shift-Enter split across reads", []string{"one\x1b[1", "3;2", "utwo\r"}, "one\x1b\rtwo\r"},
		{"Alt-Enter, legacy", []string{"one\x1b\rtwo\r"}, "one\x1b\rtwo\r"},
		{"Alt-Enter, kitty keyboard protocol", []string{"one\x1b[13;3utwo\r"}, "one\x1b\rtwo\r"},
		{"Alt-Enter, modifyOtherKeys", []string{"one\x1b[27;3;13~two\r"}, "one\x1b\rtwo\r"},
		{"backslash-Enter", []string{"one\\\rtwo\r"}, "one\\\x7f\x1b\rtwo\r"},
		{"backslash, then Enter in the next read", []string{"one\\", "\r", "two\r"}, "one\\\x7f\x1b\rtwo\r"},
		{"a backslash not last before Enter", []string{"a\\b\r"}, "a\\b\r"},
		{"two lines, then backspace across the break", []string{"one\x1b[13;2u", "\x7f", "\x7f", "!\r"}, "one\x1b\r\x7f\x7f!\r"},
		{"Ctrl-Enter submits", []string{"x\x1b[13;5u"}, "x\r"},
		{"Esc, legacy", []string{"\x1b"}, "\x1b"},
		{"Esc, kitty keyboard protocol", []string{"\x1b[27u"}, "\x1b"},
		{"Ctrl-C, kitty keyboard protocol", []string{"\x1b[99;5u"}, "\x03"},
		{"Ctrl-], kitty keyboard protocol", []string{"\x1b[93;5u"}, "\x1d"},
		{"Alt-b, kitty keyboard protocol", []string{"\x1b[98;3u"}, "\x1bb"},
		{"Shift-Tab, kitty keyboard protocol", []string{"\x1b[9;2u"}, "\x1b[Z"},
		{"Ctrl-Backspace, kitty keyboard protocol", []string{"\x1b[127;5u"}, "\x08"},
		{"backspace, kitty keyboard protocol", []string{"\x1b[127u"}, "\x7f"},
		{"arrows and other sequences pass", []string{"\x1b[A\x1b[1;5D\x1b[3~\x1bOB"}, "\x1b[A\x1b[1;5D\x1b[3~\x1bOB"},
		{"a key with no legacy bytes passes", []string{"\x1b[97;9u\x1b[57399u"}, "\x1b[97;9u\x1b[57399u"},
		{"pasted text passes as it is", []string{"\x1b[200~a\\\rb\x1b[13;2u\x1b[201~\r"}, "\x1b[200~a\\\rb\x1b[13;2u\x1b[201~\r"},
		{"a paste split across reads", []string{"\x1b[20", "0~a\\", "\r\x1b[2", "01~\\", "\r"}, "\x1b[200~a\\\r\x1b[201~\\\x7f\x1b\r"},
		{"text and UTF-8 pass", []string{"héllo 世界"}, "héllo 世界"},
	} {
		var k keyReader
		var got strings.Builder
		for _, keys := range tc.keys {
			got.Write(k.read([]byte(keys)))
		}
		if got.String() != tc.want {
			t.Errorf("%s: %q reads as %q, want %q", tc.name, tc.keys, got.String(), tc.want)
		}
	}
}

// In the conversation, Shift-Enter reaches the agent as a new line in its
// prompt and not as Enter, and Ctrl-] in the kitty keyboard protocol still
// leaves.
func TestInTheConversationShiftEnterIsSentAsANewLineAndCtrlBracketLeaves(t *testing.T) {
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

	for _, tc := range []struct{ keys, want string }{
		{"first line\x1b[13;2u", "first line\x1b\r"},
		{"second\x1b[27;2;13~", "second\x1b\r"},
		{"third\r", "third\r"},
	} {
		typeKeys(t, c, w, tc.keys)
		select {
		case m := <-sent:
			if string(m.Keys) != tc.want {
				t.Errorf("typing %q sent the agent %q, want %q", tc.keys, m.Keys, tc.want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("typing %q sent the agent nothing", tc.keys)
		}
	}

	w.Write([]byte("\x1b[93;5u"))
	select {
	case m := <-sent:
		if !m.Leave {
			t.Errorf("Ctrl-] in the kitty keyboard protocol sent %+v, want the conversation left", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Ctrl-] in the kitty keyboard protocol did not leave the conversation")
	}
}
