// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/session"
	"github.com/talvor/asmai/internal/store"
)

// A message of several lines, each started with Alt-Enter as the attach
// client sends Shift-Enter and its fallbacks, is one message: only the Enter
// that submits it counts, the user owns the input until it is submitted,
// and the witnessed message holds the user's words word for word with their
// line breaks.
func TestAMultiLineMessageIsOneWitnessedMessageWithItsLineBreaks(t *testing.T) {
	paths := stateDir(t)
	if err := os.MkdirAll(paths.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(paths.Store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	address := roles.LeaderOf(roles.Coordination)
	l := &leader{address: address, generation: 1, session: &session.Session{}}
	d := &daemon{
		cfg: Config{Paths: paths}, work: context.Background(), store: s,
		leaders:  map[string]*leader{address.String(): l},
		sessions: map[string]sessionRef{"credential": {address: address, generation: 1}},
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	c := &conversation{leader: l, session: l.session, generation: 1, terminal: "/dev/pts/test"}

	// The second line is started with backslash-Enter, which the client
	// sends as the backslash erased and Alt-Enter.
	for _, keys := range []string{"first line\x1b\r", "second line\\\x7f\x1b\r", "third line"} {
		d.typed(c, []byte(keys))
	}
	if l.submits != 0 || !l.userInput {
		t.Errorf("with the message's lines typed and not submitted, %d Enter keys are counted and the user owns the input: %v; want none counted and the user owning it", l.submits, l.userInput)
	}
	d.typed(c, []byte("\r"))
	if l.submits != 1 {
		t.Errorf("after Enter, %d Enter keys are counted, want 1", l.submits)
	}

	text := "first line\nsecond line\nthird line"
	payload, _ := json.Marshal(map[string]string{"hook_event_name": "UserPromptSubmit", "prompt": text})
	if err := d.observe("credential", payload); err != nil {
		t.Fatal(err)
	}
	entries, err := s.Journal()
	if err != nil {
		t.Fatal(err)
	}
	var witnessed []store.Witnessed
	for _, e := range entries {
		if e.Kind == store.KindMessageWitnessed {
			var w store.Witnessed
			json.Unmarshal(e.Data, &w)
			witnessed = append(witnessed, w)
		}
	}
	want := store.Witnessed{Agent: address.String(), Role: roles.Coordination, Generation: 1, Terminal: "/dev/pts/test", Text: text}
	if len(witnessed) != 1 || witnessed[0] != want {
		t.Errorf("the journal holds the witnessed messages %+v, want only %+v", witnessed, want)
	}
	if l.userInput || l.submits != 0 {
		t.Errorf("once the message was submitted, %d Enter keys are counted and the user owns the input: %v; want automation owning it", l.submits, l.userInput)
	}
}
