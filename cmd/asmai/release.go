// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io"
	"os"

	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/statedir"
)

// release gives the input of agent, which the user holds, back to automation,
// with an optional note for the intervention record. It says what became of
// the agent's dispatch.
func release(o *output, paths statedir.Paths, agent, note string, stdin io.Reader) int {
	req := daemon.Request{Command: daemon.CommandRelease, Agent: agent, Note: note}
	if in, ok := stdin.(*os.File); ok {
		req.Terminal = terminalName(in)
	}
	resp, err := daemon.Call(paths.Socket, req)
	if err != nil {
		return o.fail(err)
	}
	r := resp.Released
	if o.structured() {
		return o.printData(map[string]any{"released": r})
	}
	i := r.Intervention
	fmt.Fprintf(o.stdout, "Released %s: its input is back with automation.\n", agent)
	switch {
	case !i.Typed():
		fmt.Fprintf(o.stdout, "You typed no message, so its dispatch goes on (%d prompt answers recorded).\n", len(i.Answers))
	case r.Assignment != 0:
		fmt.Fprintf(o.stdout, "You typed %d message(s), so its dispatch %d is superseded: %s has the intervention record, and sends assignment %d again in a new dispatch.\n", len(i.Messages), r.Superseded, r.Owner, r.Assignment)
	default:
		fmt.Fprintf(o.stdout, "You typed %d message(s): %s has the intervention record.\n", len(i.Messages), r.Owner)
	}
	return 0
}
