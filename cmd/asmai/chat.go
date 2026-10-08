// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/term"

	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/statedir"
	"github.com/talvor/asmai/internal/store"
)

// chat opens the conversation, as a bare `asmai` does: it starts the factory
// if it is stopped, and says so, then attaches this terminal to
// Coordination's leader with input. Every key typed here reaches it, Esc
// included, until Ctrl-] leaves the conversation, which pauses nothing. With
// --json it prints Coordination's screen as it is now instead.
func chat(o *output, paths statedir.Paths, stdin io.Reader) int {
	coordination := roles.LeaderOf(roles.Coordination).String()
	if o.json {
		started, code := o.openFactory(paths)
		if code != 0 {
			return code
		}
		screen, err := snapshot(paths, coordination)
		if err != nil {
			return o.fail(err)
		}
		screen["started"] = started != ""
		return o.printJSON(screen)
	}
	in, inOK := stdin.(*os.File)
	out, outOK := o.stdout.(*os.File)
	if !inOK || !outOK || !term.IsTerminal(int(in.Fd())) || !term.IsTerminal(int(out.Fd())) {
		return o.fail(errors.New("the conversation with Coordination needs a terminal: run asmai in one, or asmai chat --json for Coordination's screen as text"))
	}
	started, code := o.openFactory(paths)
	if code != 0 {
		return code
	}
	notice := ""
	if started != "" {
		fmt.Fprintf(o.stdout, "Started %s: it was not running.\n", started)
		notice = "started " + started
	}
	return show(o, paths, daemon.Request{Command: daemon.CommandConversation, Terminal: terminalName(in)}, in, out, notice)
}

// openFactory makes sure the factory runs with Coordination's leader: it
// starts a stopped factory, or the leader if it is not running, and returns
// what it started, "the factory" or "Coordination's leader", or "" when both
// were running. When the checks fail it
// says so, and returns the exit code.
func (o *output) openFactory(paths statedir.Paths) (started string, code int) {
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandStatus})
	running := err == nil
	if err != nil && !errors.Is(err, daemon.ErrNotRunning) {
		return "", o.fail(err)
	}
	if running {
		for _, l := range resp.Leaders {
			if l.Agent == roles.LeaderOf(roles.Coordination).String() && l.State == store.AgentRunning {
				return "", 0
			}
		}
	}
	executable, err := os.Executable()
	if err != nil {
		return "", o.fail(fmt.Errorf("finding this executable: %w", err))
	}
	st, already, err := daemon.Start(paths, executable)
	if err != nil {
		return "", o.fail(err)
	}
	resp, err = daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandStart})
	if err != nil {
		return "", o.fail(err)
	}
	for _, c := range resp.Checks {
		if !c.OK {
			return "", o.started(st, already, paths, resp)
		}
	}
	if !running {
		return "the factory", 0
	}
	return "Coordination's leader", 0
}

// terminalName returns the path of the terminal f is, such as /dev/pts/3 or
// /dev/ttys003, or "" when it cannot be found.
func terminalName(f *os.File) string {
	info, err := f.Stat()
	if err != nil {
		return ""
	}
	for _, pattern := range []string{"/dev/pts/*", "/dev/tty*"} {
		paths, _ := filepath.Glob(pattern)
		for _, path := range paths {
			if candidate, err := os.Stat(path); err == nil && os.SameFile(info, candidate) {
				return path
			}
		}
	}
	return ""
}
