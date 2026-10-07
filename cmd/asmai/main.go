// SPDX-License-Identifier: Apache-2.0

// Command asmai is AsmAI's single executable.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/talvor/asmai"
)

// version is set at build time with -ldflags "-X main.version=v1.2.3".
// Development builds keep "dev".
var version = "dev"

const usage = `usage: asmai <command>

commands:
  asmai version    print the version of this executable
  asmai notices    print AsmAI's license and the third-party notices
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "version":
		fmt.Fprintf(stdout, "asmai %s\n", version)
		return 0
	case "notices":
		stdout.Write(asmai.License)
		fmt.Fprintln(stdout)
		stdout.Write(asmai.ThirdPartyNotices)
		return 0
	default:
		fmt.Fprintf(stderr, "asmai: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}
