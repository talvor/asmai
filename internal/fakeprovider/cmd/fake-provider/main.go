// SPDX-License-Identifier: Apache-2.0

// Command fake-provider is the scripted fake provider CLI for AsmAI's
// development tests. It never counts toward qualification and is never built
// into the asmai executable.
//
//	ASMAI_FAKE_PROVIDER_SCRIPT=FILE fake-provider [CLAUDE CODE FLAGS]
//	fake-provider --script FILE [CLAUDE CODE FLAGS]
//
// Run in a pseudo-terminal, it names itself a fake, then plays the script:
// it draws each screen, waits for each expected input, delivers each hook
// payload to the commands configured for its event, and runs each command as
// an agent's tool call would, in its own environment and working directory.
// It reads its hook commands from Claude Code settings, given as Claude Code's
// --settings takes them, and runs them through /bin/sh with the payload on
// their stdin, the way Claude Code runs its hook commands; an event with no
// command is not delivered. With the script in its environment, it starts
// with the arguments the daemon starts Claude Code with: --settings,
// --setting-sources, --model and --append-system-prompt. The fake exits 0 after the
// last step, 1 when the input or a command differs from the script or the
// terminal fails, and 2 when it is used wrongly.
package main

import (
	"os"

	"github.com/talvor/asmai/internal/fakeprovider"
)

func main() {
	os.Exit(fakeprovider.Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
