// SPDX-License-Identifier: Apache-2.0

// Command gen-notices generates THIRD_PARTY_NOTICES, which asmai embeds, from
// every Go module compiled into asmai for each target platform. With -check it
// changes nothing and fails when THIRD_PARTY_NOTICES differs from what
// generation produces. Run it from the repository root:
//
//	go run ./internal/cmd/gen-notices
//	go run ./internal/cmd/gen-notices -check
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"

	"github.com/talvor/asmai/internal/licenses"
)

const file = "THIRD_PARTY_NOTICES"

func main() {
	check := flag.Bool("check", false, "fail when "+file+" is not what generation produces, instead of writing it")
	flag.Parse()

	notices, err := licenses.Notices(".", "./cmd/asmai", licenses.Targets)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gen-notices:", err)
		os.Exit(1)
	}
	if !*check {
		if err := os.WriteFile(file, notices, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "gen-notices:", err)
			os.Exit(1)
		}
		return
	}
	embedded, err := os.ReadFile(file)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gen-notices:", err)
		os.Exit(1)
	}
	if !bytes.Equal(embedded, notices) {
		fmt.Fprintf(os.Stderr, "gen-notices: %s does not match the Go modules compiled into asmai; run `make notices` and commit it\n", file)
		os.Exit(1)
	}
	fmt.Printf("%s matches the Go modules compiled into asmai\n", file)
}
