// SPDX-License-Identifier: Apache-2.0

// Command check-licenses lists every Go module compiled into asmai for each
// target platform with its license, and fails when a license is not on the
// allow-list in package licenses or cannot be determined. CI runs it from the
// repository root with:
//
//	go run ./internal/cmd/check-licenses
package main

import (
	"fmt"
	"os"

	"github.com/talvor/asmai/internal/licenses"
	"github.com/talvor/asmai/internal/skills"
)

func main() {
	ok, err := licenses.Check(os.Stdout, ".", licenses.Packages, licenses.Targets)
	if err != nil {
		fmt.Fprintln(os.Stderr, "check-licenses:", err)
		os.Exit(1)
	}
	if !licenses.CheckEmbedded(os.Stdout, []licenses.Embedded{{Name: skills.LicenseName(), License: skills.License()}}) {
		ok = false
	}
	if !ok {
		fmt.Fprintln(os.Stderr, "check-licenses: a compiled-in module's or embedded skill bundle's license is not on the allow-list; widening it is the maintainer's decision")
		os.Exit(1)
	}
}
