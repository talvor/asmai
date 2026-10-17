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
	f := &FakeDaemon{t: t, paths: statedir.At(dir), handlers: map[string]func(daemon.Request) daemon.Response{}}
	f.Handle(daemon.CommandStatus, func(daemon.Request) daemon.Response {
		return daemon.Response{Status: &daemon.Status{Version: "fake", PID: 1}}
	})
	f.SetAgents()
	f.Start()
	t.Cleanup(f.Stop)
	return f
}

// SetAgents sets the agents the snapshot lists, each with the status a test
// gives it: the fake daemon derives nothing, as the Dashboard does not.
func (f *FakeDaemon) SetAgents(agents ...daemon.DashboardAgent) {
	snapshot := daemon.DashboardSnapshot{Agents: append([]daemon.DashboardAgent{}, agents...)}
	f.Handle(daemon.CommandDashboardSnapshot, func(daemon.Request) daemon.Response {
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
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return
	}
	var req daemon.Request
	if err := json.Unmarshal(line, &req); err != nil {
		return
	}
	f.mu.Lock()
	f.calls = append(f.calls, req)
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
