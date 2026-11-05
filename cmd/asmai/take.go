// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/term"

	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/statedir"
)

// take makes the user the input owner of agent and shows its terminal on this
// one with the user's keys typed in, as the conversation does Coordination's.
// A running turn is waited for, or with interrupt interrupted; the daemon
// clears any unsent automated draft before the terminal is drawn. Ctrl-]
// detaches and leaves the input with the user.
func take(o *output, paths statedir.Paths, agent string, interrupt bool, stdin io.Reader) int {
	in, inOK := stdin.(*os.File)
	out, outOK := o.stdout.(*os.File)
	if o.structured() || !inOK || !outOK || !term.IsTerminal(int(in.Fd())) || !term.IsTerminal(int(out.Fd())) {
		return o.fail(errors.New("asmai take types into the agent's terminal, which needs a terminal: run it in one"))
	}
	if !interrupt {
		fmt.Fprintf(o.stderr, "Taking %s: waiting for its running turn to end, if there is one. Ctrl-C stops waiting; --interrupt interrupts the turn instead.\n", agent)
	}
	return show(o, paths, daemon.Request{Command: daemon.CommandTake, Agent: agent, Interrupt: interrupt, Terminal: terminalName(in)}, in, out, "")
}
