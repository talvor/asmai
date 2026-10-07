// SPDX-License-Identifier: Apache-2.0

// Command record-session records a real Claude Code session as a script for
// the fake provider: what it drew, the input it was given and the hook
// payloads it sent, with the Claude Code version it came from. Personal data
// is scrubbed and a recording holding a credential is refused. It is for
// making the development tests' recordings and is never built into the asmai
// executable.
//
//	record-session --out FILE [--redact VALUE]... [-- CLAUDE [ARG]...]
//
// Run it in a terminal in the directory the session should work in, and use
// the session as usual. It records on an 80 by 24 terminal; the recording is written when Claude Code exits.
package main

import (
	"os"

	"github.com/talvor/asmai/internal/fakeprovider/record"
)

func main() {
	os.Exit(record.Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
