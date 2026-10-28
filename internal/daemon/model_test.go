// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/session"
	"github.com/talvor/asmai/internal/store"
)

// The model an agent runs is the one it was launched with until its
// provider reports the one it resolved, and its effort is unknown until the
// provider reports it: Claude Code's SessionStart names the model, and its
// Stop the effort level. A hook of an earlier session changes neither, and
// a payload that names them in another shape is still observed. The
// session is a placeholder that runs nothing, so SessionStart, whose
// observing waits on the session's screen, goes to modelReported alone.
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
		leaders:  map[string]*leader{address.String(): {address: address, generation: 2, session: &session.Session{}, model: "opus"}},
		sessions: map[string]sessionRef{"credential": {address: address, generation: 2}, "earlier": {address: address, generation: 1}},
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	agent := address.String()
	ref := sessionRef{address: address, generation: 2}
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
	d.modelReported(ref, json.RawMessage(`{"hook_event_name":"SessionStart","source":"startup","model":"claude-opus-5-5"}`))
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
	// A leader whose session has ended keeps the model it last ran.
	d.leaders[agent].session = nil
	d.modelReported(ref, json.RawMessage(`{"hook_event_name":"Stop","model":"claude-sonnet-5-5","effort":{"level":"low"}}`))
	expect("after a hook once the session ended", Model{Name: "claude-opus-5-5", Effort: "high"})
}

// An attach stream sends the model and effort with its answer, and a frame
// of its own once a hook of the attached session reports either changed:
// the hooks' payloads go to modelReported, as observe passes them.
func TestAnAttachStreamSendsTheModelOnceAHookReportsItChanged(t *testing.T) {
	s, err := session.Start(session.Spec{Path: "/bin/sleep", Args: []string{"30"}, Columns: 20, Rows: 5})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Stop(time.Second) })
	address := roles.LeaderOf(roles.Coordination)
	ref := sessionRef{address: address, generation: 2}
	d := &daemon{
		leaders: map[string]*leader{address.String(): {address: address, generation: 2, session: s, model: "opus"}},
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	server, client := connectedPair(t)
	model := d.modelOf(address.String(), 2)
	attached := &Leader{Agent: address.String(), State: store.AgentRunning, Generation: 2, PID: s.PID(), Model: model.Name, Effort: model.Effort}
	go d.stream(server, bufio.NewReader(server), Request{}, s, attached, nil)

	r := bufio.NewReader(client)
	line, err := r.ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Attached == nil || resp.Attached.Model != "opus" || resp.Attached.Effort != "" {
		t.Errorf("the stream answered %+v, want the agent running opus at an effort not yet known", resp.Attached)
	}
	if first := readFrame(t, r); first.State == nil || first.Model != nil {
		t.Errorf("the stream's first frame is %+v, want the screen and no model", first)
	}
	d.modelReported(ref, json.RawMessage(`{"hook_event_name":"SessionStart","source":"startup","model":"claude-opus-5-5"}`))
	client.SetReadDeadline(time.Now().Add(5 * promptCheck))
	want := func(when string, model Model) {
		t.Helper()
		for {
			f := readFrame(t, r)
			if f.Model == nil {
				continue
			}
			if *f.Model != model {
				t.Errorf("%s, the stream sent the model %+v, want %+v", when, *f.Model, model)
			}
			return
		}
	}
	want("after SessionStart", Model{Name: "claude-opus-5-5"})
	d.modelReported(ref, json.RawMessage(`{"hook_event_name":"Stop","effort":{"level":"high"}}`))
	client.SetReadDeadline(time.Now().Add(5 * promptCheck))
	want("after Stop", Model{Name: "claude-opus-5-5", Effort: "high"})
}

// connectedPair returns the two ends of a connected Unix socket pair, which
// needs no socket path.
func connectedPair(t *testing.T) (*net.UnixConn, *net.UnixConn) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	var conns [2]*net.UnixConn
	for i, fd := range fds {
		f := os.NewFile(uintptr(fd), "socketpair")
		c, err := net.FileConn(f)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		conns[i] = c.(*net.UnixConn)
		t.Cleanup(func() { c.Close() })
	}
	return conns[0], conns[1]
}
