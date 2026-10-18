// SPDX-License-Identifier: Apache-2.0

// Command lavish-notices holds everything compiled into a Lavish build to the
// license allow-list and writes the build's notices file. The Lavish build
// workflow runs it in the lavish directory after `deno install --frozen`,
// with the Deno runtime's LICENSE.md at the pinned version, and fails when any
// license is not on the allow-list or cannot be determined. The Rust crates
// compiled into the Deno runtime are not checked or listed:
//
//	go run ./internal/cmd/lavish-notices -dir lavish -deno-license deno/LICENSE.md \
//	    -out lavish/LAVISH_NOTICES
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/talvor/asmai/internal/lavishbuild"
)

func main() {
	dir := flag.String("dir", "lavish", "the Lavish build directory, with deno.json, deno.lock and the node_modules `deno install` made")
	axi := flag.String("axi", "", "the pinned lavish-axi version")
	deno := flag.String("deno", "", "the pinned Deno version")
	denoLicense := flag.String("deno-license", "", "the Deno runtime's LICENSE.md at the pinned version")
	out := flag.String("out", "", "where to write the notices file; with none, only the licenses are checked")
	flag.Parse()
	if err := run(*dir, *axi, *deno, *denoLicense, *out); err != nil {
		fmt.Fprintln(os.Stderr, "lavish-notices:", err)
		os.Exit(1)
	}
}

func run(dir, axi, deno, denoLicense, out string) error {
	if axi == "" || deno == "" || denoLicense == "" {
		return fmt.Errorf("-axi, -deno and -deno-license are needed")
	}
	npm, err := lavishbuild.NPMMaterial(dir)
	if err != nil {
		return err
	}
	axiNotices, err := os.ReadFile(filepath.Join(dir, "node_modules", "lavish-axi", "THIRD-PARTY-NOTICES.md"))
	if err != nil {
		return fmt.Errorf("reading lavish-axi's notices: %w", err)
	}
	vendored, err := lavishbuild.ParseVendored(axiNotices)
	if err != nil {
		return err
	}
	ofl, err := os.ReadFile(filepath.Join(dir, "licenses", "OFL-1.1.txt"))
	if err != nil {
		return err
	}
	license, err := os.ReadFile(denoLicense)
	if err != nil {
		return err
	}
	in := lavishbuild.Inputs{Axi: axi, Deno: deno, NPM: npm, AxiNotices: axiNotices, Vendored: vendored, OFL: ofl, DenoLicense: license}
	fmt.Printf("%d npm packages, %d vendored assets and fonts and the Deno runtime checked against the license allow-list\n", len(npm), len(vendored.Entries))
	if problems := in.Problems(); len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintln(os.Stderr, "FAIL", p)
		}
		return fmt.Errorf("%d licenses are not on the allow-list or cannot be determined; widening the allow-list is the maintainer's decision", len(problems))
	}
	if out == "" {
		return nil
	}
	return os.WriteFile(out, in.Notices(), 0o644)
}
