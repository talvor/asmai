// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/lavish"
)

// nothingChanged is the catch-up when nothing changed since the user last
// left and nothing waits on them (04 rule 17).
const nothingChanged = "Nothing has changed since you left, and nothing waits on you."

// eventLabels are how `asmai status` and the catch-up name each kind of
// event.
var eventLabels = map[string]string{
	daemon.EventWaiting:  "waiting on you",
	daemon.EventFinished: "job finished",
	daemon.EventFailed:   "job failed",
	daemon.EventPaused:   "job paused",
	daemon.EventHold:     "hold",
	daemon.EventTerminal: "native prompt",
	daemon.EventInput:    "your input",
	daemon.EventSignIn:   "sign-in",
}

// eventDetails are the lines under an event that say where to go from it: the
// Lavish page that answers a decision request, and the command that signs in
// again, which is the provider's own.
func eventDetails(e daemon.Event) []string {
	var details []string
	if e.Link != "" {
		details = append(details, "answer it in Lavish: "+e.Link)
	}
	if e.Command != "" {
		details = append(details, "sign in again with: "+e.Command)
	}
	return details
}

// eventRows are the rows of `asmai status` for events: one for each, with the
// details of those that have any under it.
func eventRows(events []daemon.Event) [][]string {
	var rows [][]string
	for _, e := range events {
		rows = append(rows, []string{eventLabels[e.Kind], e.Text})
		for _, d := range eventDetails(e) {
			rows = append(rows, []string{"", "  " + d})
		}
	}
	return rows
}

// printCatchUp prints the catch-up a returning user starts the conversation
// with: one line when nothing changed, otherwise each event, the command that
// forwards the Lavish port when a link needs it, and the text the user left
// unsent, which it only shows: it is never typed again (04 rules 16 to 18).
func printCatchUp(w io.Writer, c *daemon.CatchUp, forward func(port int) string) {
	if c == nil || (len(c.Events) == 0 && len(c.Unsent) == 0) {
		fmt.Fprintln(w, nothingChanged)
		return
	}
	if len(c.Events) > 0 {
		fmt.Fprintln(w, "Since you left:")
		for _, e := range c.Events {
			fmt.Fprintf(w, "  %s: %s\n", eventLabels[e.Kind], e.Text)
			for _, d := range eventDetails(e) {
				fmt.Fprintf(w, "    %s\n", d)
			}
		}
	}
	if c.LavishPort != 0 {
		fmt.Fprintf(w, "Lavish listens on this host only (%s). From your own machine, forward its port first:\n  %s\n", lavish.Host+fmt.Sprintf(":%d", c.LavishPort), forward(c.LavishPort))
	}
	for _, u := range c.Unsent {
		fmt.Fprintf(w, "You left this unsent in %s (saved, not retyped; copy it back if you want it):\n", u.Agent)
		for line := range strings.SplitSeq(u.Text, "\n") {
			fmt.Fprintf(w, "  | %s\n", line)
		}
	}
}
