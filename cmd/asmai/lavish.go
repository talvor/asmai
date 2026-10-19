// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"

	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/lavish"
	"github.com/talvor/asmai/internal/statedir"
)

// lavishCommand is `asmai lavish`: the review links waiting for the user in
// the factory's Lavish server, and the command that forwards the server's
// port over SSH to a user on another machine (04 rule 21). The server listens
// on this host only. With the factory stopped it still lists the pages, which
// the server keeps in its state directory, and says the server is not
// running.
func lavishCommand(o *output, paths statedir.Paths) int {
	var status *daemon.LavishStatus
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandLavish})
	switch {
	case err == nil:
		status = resp.Lavish
	case errors.Is(err, daemon.ErrNotRunning):
		pages, pagesErr := lavish.Pages(paths.Lavish)
		status = &daemon.LavishStatus{URL: lavish.URL(lavish.Port), Port: lavish.Port, StateDir: paths.Lavish, Problem: "the factory is stopped, so its Lavish server is not running; `asmai start` runs it", Pages: pages}
		if pagesErr != nil {
			status.PagesError = pagesErr.Error()
			status.Pages = []lavish.Page{}
		}
	default:
		return o.fail(err)
	}
	forward := lavish.ForwardCommand(sshDestination(), status.Port)
	if o.structured() {
		return o.printData(map[string]any{"lavish": status, "forward": forward})
	}
	w := o.stdout
	if status.Running {
		fmt.Fprintf(w, "Lavish server  running at %s (pid %d)\n", status.URL, status.PID)
	} else {
		fmt.Fprintf(w, "Lavish server  not running: %s\n", status.Problem)
	}
	if status.PagesError != "" {
		fmt.Fprintf(w, "Review links   could not be read: %s\n", status.PagesError)
	} else if len(status.Pages) == 0 {
		fmt.Fprintln(w, "Review links   none; no review page is waiting for you")
	} else {
		fmt.Fprintln(w, "Review links   waiting for you")
		rows := [][]string{}
		for _, p := range status.Pages {
			waiting := ""
			if p.PendingPrompts > 0 {
				waiting = plural(p.PendingPrompts, "1 message not yet collected", fmt.Sprintf("%d messages not yet collected", p.PendingPrompts))
			}
			rows = append(rows, []string{"  " + p.URL, p.File, waiting})
		}
		o.table(rows)
	}
	fmt.Fprintf(w, "\nLavish listens on this host only (%s). To open the links from your own machine, forward its port, then open them there:\n  %s\n", lavish.Host+":"+strconv.Itoa(status.Port), forward)
	return 0
}

// sshDestination is how the user reaches this host over SSH, as well as the
// host can tell: the user and the host's name. The user may reach it by
// another name, which the command's reader substitutes.
func sshDestination() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "this-host"
	}
	if user := os.Getenv("USER"); user != "" {
		return user + "@" + host
	}
	return host
}
