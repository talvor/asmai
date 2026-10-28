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
	"github.com/talvor/asmai/internal/store"
)

// The model an agent runs is the one it was launched with until its
// provider reports the one it resolved, and its effort is unknown until the
// provider reports it: Claude Code's SessionStart names the model, and its
// Stop the effort level. A hook of an earlier session changes neither, and
// a payload that names them in another shape is still observed.
func TestTheModelAndEffortAreTheOnesTheProviderLastReported(t *testing.T) {
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
	d := &daemon{
		cfg: Config{Paths: paths}, work: context.Background(), store: s,
		leaders:  map[string]*leader{address.String(): {address: address, generation: 2, model: "opus"}},
		sessions: map[string]sessionRef{"credential": {address: address, generation: 2}, "earlier": {address: address, generation: 1}},
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	agent := address.String()
	expect := func(when string, want Model) {
		t.Helper()
		if got := d.modelOf(agent, 2); got != want {
			t.Errorf("%s, the model is %+v, want %+v", when, got, want)
		}
	}
	hook := func(credential, payload string) {
		t.Helper()
		if err := d.observe(credential, json.RawMessage(payload)); err != nil {
			t.Fatalf("the hook %s was refused: %v", payload, err)
		}
	}

	expect("before the provider reports anything", Model{Name: "opus"})
	hook("credential", `{"hook_event_name": "SessionStart", "source": "startup", "model": "claude-opus-5-5"}`)
	expect("after SessionStart", Model{Name: "claude-opus-5-5"})
	hook("credential", `{"hook_event_name": "Stop", "effort": {"level": "medium"}}`)
	expect("after Stop", Model{Name: "claude-opus-5-5", Effort: "medium"})
	hook("credential", `{"hook_event_name": "UserPromptSubmit", "prompt": "hi"}`)
	expect("after a hook that names neither", Model{Name: "claude-opus-5-5", Effort: "medium"})
	hook("earlier", `{"hook_event_name": "Stop", "model": "claude-sonnet-5-5", "effort": {"level": "low"}}`)
	expect("after a hook of the earlier session", Model{Name: "claude-opus-5-5", Effort: "medium"})
	hook("credential", `{"hook_event_name": "Stop", "effort": "high", "model": 5}`)
	expect("after a hook naming the effort as text", Model{Name: "claude-opus-5-5", Effort: "high"})
	if got := d.modelOf(agent, 1); got != (Model{}) {
		t.Errorf("the earlier session's model is %+v, want none", got)
	}
}
