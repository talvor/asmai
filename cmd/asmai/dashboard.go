// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"io"
	"os"

	"golang.org/x/term"

	"github.com/talvor/asmai/internal/dashboard"
	"github.com/talvor/asmai/internal/statedir"
)

// dashboardCommand shows the Dashboard on this terminal until the user
// leaves it. The Dashboard only reads: it needs the daemon's answer, not its
// consent, and starts nothing.
func dashboardCommand(o *output, paths statedir.Paths, stdin io.Reader) int {
	if o.toon {
		return o.fail(errors.New("asmai dashboard draws on a terminal and has no --toon form; `asmai status`, `agents`, `jobs` and `log` print TOON"))
	}
	if o.json {
		return o.fail(errors.New("asmai dashboard draws on a terminal and has no --json form; `asmai status`, `agents`, `jobs` and `log` print JSON"))
	}
	in, inOK := stdin.(*os.File)
	out, outOK := o.stdout.(*os.File)
	if !inOK || !outOK || !term.IsTerminal(int(in.Fd())) || !term.IsTerminal(int(out.Fd())) {
		return o.fail(errors.New("asmai dashboard draws on a terminal: run it in one"))
	}
	if err := dashboard.Run(paths.Socket, dashboard.Options{Input: in, Output: out, Environ: os.Environ()}); err != nil {
		return o.fail(err)
	}
	return 0
}
