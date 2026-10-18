// SPDX-License-Identifier: Apache-2.0

// Command lavish-notices holds everything compiled into a Lavish build to the
// license allow-list and writes the build's notices file. The Lavish build
// workflow runs it in the lavish directory after `deno install --frozen`,
// with the Deno runtime's LICENSE.md and the `cargo metadata` of Deno's
// source at the pinned version, and fails when any license is not on the
// allow-list or cannot be determined:
//
//	go run ./internal/cmd/lavish-notices -dir lavish -deno-license deno/LICENSE.md \
//	    -cargo-metadata linux.json -cargo-metadata macos.json -out lavish/LAVISH_NOTICES
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/talvor/asmai/internal/lavishbuild"
)

type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

func main() {
	dir := flag.String("dir", "lavish", "the Lavish build directory, with deno.json, deno.lock and the node_modules `deno install` made")
	axi := flag.String("axi", "", "the pinned lavish-axi version")
	deno := flag.String("deno", "", "the pinned Deno version")
	denoLicense := flag.String("deno-license", "", "the Deno runtime's LICENSE.md at the pinned version")
	root := flag.String("cargo-root", "denort", "the package of Deno's source whose dependencies are compiled into the runtime")
	var metadata listFlag
	flag.Var(&metadata, "cargo-metadata", "`cargo metadata` of Deno's source for a certified platform; repeat for each")
	out := flag.String("out", "", "where to write the notices file; with none, only the licenses are checked")
	flag.Parse()
	if err := run(*dir, *axi, *deno, *denoLicense, *root, metadata, *out); err != nil {
		fmt.Fprintln(os.Stderr, "lavish-notices:", err)
		os.Exit(1)
	}
}

func run(dir, axi, deno, denoLicense, root string, metadata []string, out string) error {
	if axi == "" || deno == "" || denoLicense == "" || len(metadata) == 0 {
		return fmt.Errorf("-axi, -deno, -deno-license and -cargo-metadata are needed")
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
	crates, err := lavishbuild.RuntimeCrates(root, metadata...)
	if err != nil {
		return err
	}
	crateMaterial, err := lavishbuild.CrateMaterial(crates)
	if err != nil {
		return err
	}
	in := lavishbuild.Inputs{Axi: axi, Deno: deno, NPM: npm, AxiNotices: axiNotices, Vendored: vendored, OFL: ofl, DenoLicense: license, Crates: crateMaterial}
	fmt.Printf("%d npm packages, %d vendored assets and fonts, the Deno runtime and %d of its crates checked against the license allow-list\n", len(npm), len(vendored.Entries), len(crateMaterial))
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
