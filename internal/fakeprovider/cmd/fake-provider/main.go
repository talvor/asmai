// SPDX-License-Identifier: Apache-2.0

// Command fake-provider is the scripted fake provider CLI for AsmAI's
// development tests. It never counts toward qualification and is never built
// into the asmai executable.
//
//	fake-provider --script FILE [--hook EVENT=COMMAND]...
//
// Run in a pseudo-terminal, it names itself a fake, then plays the script:
// it draws each screen, waits for each expected input, and delivers each hook
// payload to the command configured for its event. A hook command runs
// through /bin/sh with the payload on its stdin, the way Claude Code and Codex
// run their hook commands; an event with no command is not delivered. The
// fake exits 0 after the last step, 1 when the input differs from the script
// or the terminal fails, and 2 when it is used wrongly.
package main

import (
	"os"

	"github.com/talvor/asmai/internal/fakeprovider"
)

func main() {
	os.Exit(fakeprovider.Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
