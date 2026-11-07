// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"time"

	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/recording"
	"github.com/talvor/asmai/internal/statedir"
)

// replay plays the recording of agent's latest dispatch, or of dispatch, on
// this terminal. It says when there is no recording, or it has been deleted.
func replay(o *output, paths statedir.Paths, agent string, dispatch int64) int {
	if o.structured() {
		return o.fail(errors.New("asmai replay plays a recording on this terminal: it has no --json or --toon form"))
	}
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandReplay, Agent: agent, Dispatch: dispatch})
	if err != nil {
		return o.fail(err)
	}
	if resp.Recording == nil {
		return o.fail(errors.New("the daemon did not answer with a recording"))
	}
	fmt.Fprintf(o.stderr, "Replaying dispatch %d of %s, recorded %s.\n", resp.Recording.Dispatch, resp.Recording.Agent, resp.Recording.Started.Local().Format("2006-01-02 15:04"))
	err = recording.Play(o.stdout, resp.Recording.Path, time.Sleep)
	// A replay never leaves the terminal in a style the recording set.
	fmt.Fprint(o.stdout, "\x1b[0m\x1b[?25h\x1b[?7h\r\n")
	if err != nil {
		return o.fail(err)
	}
	fmt.Fprintln(o.stderr, "End of the recording.")
	return 0
}
