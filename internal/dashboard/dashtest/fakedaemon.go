// SPDX-License-Identifier: Apache-2.0

// Package dashtest is the development test harness of the Dashboard: a fake
// daemon to read, a PTY to run the asmai executable in, and golden screens to
// compare what the terminal shows with. It is for tests only; no asmai
// executable is built with it.
package dashtest

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/statedir"
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
}

// NewFakeDaemon starts a fake daemon that answers status as a running
// factory does, and stops it when the test ends. Add answers to the calls the
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
	f.Start()
	t.Cleanup(f.Stop)
	return f
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
	resp := daemon.Response{Error: "the fake daemon does not answer " + req.Command}
	if handler != nil {
		resp = handler(req)
	}
	out, _ := json.Marshal(resp)
	conn.Write(append(out, '\n'))
}
