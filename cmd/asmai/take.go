// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io"
	"os"

	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/statedir"
)

// take makes the user the input owner of the agent: the daemon answers once
// the handover happened, at the agent's turn boundary or, with interrupt,
// after interrupting its turn. The hold stays until the user releases it.
func take(o *output, paths statedir.Paths, agent string, interrupt bool, stdin io.Reader) int {
	terminal := ""
	if f, ok := stdin.(*os.File); ok {
		terminal = terminalName(f)
	}
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandTake, Agent: agent, Interrupt: interrupt, Terminal: terminal})
	if err != nil {
		return o.fail(err)
	}
	if resp.Taken == nil {
		return o.fail(fmt.Errorf("the daemon did not answer with the take"))
	}
	if o.structured() {
		return o.printData(resp.Taken)
	}
	t := resp.Taken
	switch t.How {
	case daemon.TookByInterrupt:
		fmt.Fprintf(o.stdout, "You hold %s's input: its turn was interrupted.\n", t.Agent)
	case daemon.TookByMoving:
		fmt.Fprintf(o.stdout, "You hold %s's input: the hold moved here from %s.\n", t.Agent, t.MovedFrom)
	default:
		fmt.Fprintf(o.stdout, "You hold %s's input: its turn finished.\n", t.Agent)
	}
	if t.Unsent != "" {
		fmt.Fprintf(o.stdout, "Its unsent draft was saved to the journal and cleared: %q\n", t.Unsent)
	}
	fmt.Fprintf(o.stdout, "Automated messages for it wait in its inbox. `asmai attach %s` shows its terminal.\n", t.Agent)
	return 0
}
