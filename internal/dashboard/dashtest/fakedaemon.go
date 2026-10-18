// SPDX-License-Identifier: Apache-2.0

// Package dashtest is the development test harness of the Dashboard: a fake
// daemon to read, a PTY to run the asmai executable in, and golden screens to
// compare what the terminal shows with. It is for tests only; no asmai
// executable is built with it.
package dashtest

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/statedir"
	"github.com/talvor/asmai/internal/vt"
)

// FakeDaemon answers the Dashboard's calls on a Unix socket in a state
// directory of its own, with whatever a test says, and can be stopped and
// started again to play a factory that goes down and comes back.
type FakeDaemon struct {
	t     *testing.T
	paths statedir.Paths

	mu       sync.Mutex
	ln       net.Listener
	handlers map[string]func(daemon.Request) daemon.Response
	calls    []daemon.Request
	// attachScreen, when set, is what an attach to any agent is shown.
	attachScreen *string
	// head is the journal ID the subscription last ticked with; watchers are
	// the subscriptions open.
	head     int64
	watchers map[net.Conn]bool
	snapshot daemon.DashboardSnapshot
}

// NewFakeDaemon starts a fake daemon that answers status and an empty
// snapshot as a running factory does, and stops it when the test ends. Add answers to the calls the
// Dashboard learns to make with Handle.
func NewFakeDaemon(t *testing.T) *FakeDaemon {
	t.Helper()
	// os.MkdirTemp keeps the socket's path short enough on macOS.
	dir, err := os.MkdirTemp("", "dash")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	f := &FakeDaemon{t: t, paths: statedir.At(dir), handlers: map[string]func(daemon.Request) daemon.Response{}, watchers: map[net.Conn]bool{}}
	f.Handle(daemon.CommandStatus, func(daemon.Request) daemon.Response {
		return daemon.Response{Status: &daemon.Status{Version: "fake", PID: 1}}
	})
	f.SetAgents()
	f.SetJobs()
	f.SetWaiting()
	f.Start()
	t.Cleanup(f.Stop)
	return f
}

// SetAgents sets the agents the snapshot lists, each with the status a test
// gives it: the fake daemon derives nothing, as the Dashboard does not.
func (f *FakeDaemon) SetAgents(agents ...daemon.DashboardAgent) {
	f.mu.Lock()
	f.snapshot.Agents = append([]daemon.DashboardAgent{}, agents...)
	f.mu.Unlock()
	f.serveSnapshot()
}

// SetJobs sets the jobs the snapshot lists, each with the stage a test gives
// it.
func (f *FakeDaemon) SetJobs(jobs ...daemon.DashboardJob) {
	f.mu.Lock()
	f.snapshot.Jobs = append([]daemon.DashboardJob{}, jobs...)
	f.mu.Unlock()
	f.serveSnapshot()
}

// SetWaiting sets the Waiting on you set the snapshot carries, in the order
// given: the fake daemon orders nothing.
func (f *FakeDaemon) SetWaiting(items ...daemon.WaitingItem) {
	f.mu.Lock()
	f.snapshot.Waiting = append([]daemon.WaitingItem{}, items...)
	f.mu.Unlock()
	f.serveSnapshot()
}

func (f *FakeDaemon) serveSnapshot() {
	f.Handle(daemon.CommandDashboardSnapshot, func(daemon.Request) daemon.Response {
		f.mu.Lock()
		defer f.mu.Unlock()
		snapshot := f.snapshot
		return daemon.Response{Dashboard: &snapshot}
	})
}

// ServeAttach makes the fake daemon let `asmai attach` follow any agent, as the
// real one does: it answers with the agent and streams a screen showing text,
// then holds the connection until the client hangs up.
func (f *FakeDaemon) ServeAttach(text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attachScreen = &text
}

// holdAttach answers an attach request and keeps the stream open; it reports
// whether it did.
func (f *FakeDaemon) holdAttach(conn net.Conn, req daemon.Request) bool {
	f.mu.Lock()
	screen := f.attachScreen
	f.mu.Unlock()
	if req.Command != daemon.CommandAttach || screen == nil {
		return false
	}
	term := vt.New(max(req.Columns, 1), max(req.Rows, 1))
	term.Write([]byte("\x1b[2J\x1b[H" + *screen))
	state := term.State()
	for _, v := range []any{daemon.Response{Attached: &daemon.Leader{Agent: req.Agent, State: "running", Generation: 1}}, daemon.Frame{State: &state}} {
		out, _ := json.Marshal(v)
		conn.Write(append(out, '\n'))
	}
	io.Copy(io.Discard, conn)
	return true
}

// Paths are the state directory's paths the fake daemon listens in.
func (f *FakeDaemon) Paths() statedir.Paths { return f.paths }

// StateDir is the state directory asmai finds the fake daemon in, as
// ASMAI_STATE_DIR.
func (f *FakeDaemon) StateDir() string { return f.paths.Dir }

// Handle sets how the fake daemon answers command.
func (f *FakeDaemon) Handle(command string, answer func(daemon.Request) daemon.Response) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[command] = answer
}

// Calls are the requests the fake daemon has had, oldest first. A read-only
// Dashboard sends no command that acts on the factory.
func (f *FakeDaemon) Calls() []daemon.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]daemon.Request(nil), f.calls...)
}

// Start listens again, as a factory that was started.
func (f *FakeDaemon) Start() {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ln != nil {
		return
	}
	ln, err := net.Listen("unix", f.paths.Socket)
	if err != nil {
		f.t.Fatal(err)
	}
	f.ln = ln
	go f.serve(ln)
}

// Stop closes the socket and removes it, as a factory that stopped.
func (f *FakeDaemon) Stop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ln == nil {
		return
	}
	f.ln.Close()
	os.Remove(filepath.Clean(f.paths.Socket))
	f.ln = nil
	for conn := range f.watchers {
		conn.Close()
	}
}

// Tick sends every open change subscription a tick, as the daemon does when
// the journal grows or a waiting prompt or an input owner changes.
func (f *FakeDaemon) Tick() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.head++
	for conn := range f.watchers {
		f.tick(conn)
	}
}

// Subscribers is how many change subscriptions are open.
func (f *FakeDaemon) Subscribers() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.watchers)
}

// tick writes one tick to conn. f.mu is held.
func (f *FakeDaemon) tick(conn net.Conn) {
	out, _ := json.Marshal(daemon.Response{Change: &daemon.DashboardChange{Journal: f.head}})
	conn.Write(append(out, '\n'))
}

// subscribe keeps conn open as a change subscription, with a tick at once,
// until either side closes it.
func (f *FakeDaemon) subscribe(conn net.Conn, r *bufio.Reader) {
	f.mu.Lock()
	f.watchers[conn] = true
	f.tick(conn)
	f.mu.Unlock()
	io.Copy(io.Discard, r)
	f.mu.Lock()
	delete(f.watchers, conn)
	f.mu.Unlock()
}

func (f *FakeDaemon) serve(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go f.answer(conn)
	}
}

func (f *FakeDaemon) answer(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	line, err := r.ReadBytes('\n')
	if err != nil {
		return
	}
	var req daemon.Request
	if err := json.Unmarshal(line, &req); err != nil {
		return
	}
	f.mu.Lock()
	f.calls = append(f.calls, req)
	if req.Command == daemon.CommandDashboardSubscribe && f.handlers[req.Command] == nil {
		f.mu.Unlock()
		f.subscribe(conn, r)
		return
	}
	handler := f.handlers[req.Command]
	f.mu.Unlock()
	if f.holdAttach(conn, req) {
		return
	}
	resp := daemon.Response{Error: "the fake daemon does not answer " + req.Command}
	if handler != nil {
		resp = handler(req)
	}
	out, _ := json.Marshal(resp)
	conn.Write(append(out, '\n'))
}
