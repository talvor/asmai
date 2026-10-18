// SPDX-License-Identifier: Apache-2.0

package lavishbuild

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/talvor/asmai/internal/licenses"
)

// Inputs are what the notices of one Lavish build are made from.
type Inputs struct {
	// Axi and Deno are the pinned lavish-axi and Deno versions.
	Axi, Deno string
	// NPM are the locked npm packages with their licenses.
	NPM []Material
	// AxiNotices is lavish-axi's own THIRD-PARTY-NOTICES.md, which lists
	// what it vendors, and Vendored its parsed tables.
	AxiNotices []byte
	Vendored   Vendored
	// OFL is the text of the SIL Open Font License 1.1, which the fonts
	// vendored under it carry.
	OFL []byte
	// DenoLicense is the Deno runtime's own LICENSE.md, and Crates the Rust
	// crates compiled into it.
	DenoLicense []byte
	Crates      []Material
}

// Problems returns everything compiled into the build whose license is not
// acceptable, each with what it is: the build fails on any (10 rule 5).
func (in Inputs) Problems() []string {
	var problems []string
	for _, m := range in.NPM {
		if m.Problem != "" {
			problems = append(problems, fmt.Sprintf("npm package %s: %s", m.Name, m.Problem))
		}
	}
	for _, p := range in.Vendored.Problems() {
		problems = append(problems, "vendored by lavish-axi: "+p)
	}
	if in.Vendored.UsesOFL() && len(in.OFL) > 0 && !slicesContains(licenses.Detect([]licenses.LicenseFile{{Name: "OFL-1.1.txt", Text: in.OFL}}), OFL) {
		problems = append(problems, "the SIL OFL 1.1 text is not the SIL Open Font License 1.1")
	}
	if len(in.DenoLicense) == 0 {
		problems = append(problems, "the Deno runtime's LICENSE.md is missing")
	} else {
		got := licenses.Detect([]licenses.LicenseFile{{Name: "LICENSE.md", Text: in.DenoLicense}})
		if problem := (licenses.Module{Licenses: got}).Problem(); problem != "" {
			problems = append(problems, fmt.Sprintf("the Deno runtime's own license: %s", problem))
		}
	}
	for _, m := range in.Crates {
		if m.Problem != "" {
			problems = append(problems, fmt.Sprintf("Deno runtime crate %s: %s", m.Name, m.Problem))
		}
	}
	return problems
}

func slicesContains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// Notices writes the notices file of a Lavish build: lavish-axi with what it
// vendors, then its locked npm dependencies, then the Deno runtime and the
// crates compiled into it, each followed by its license texts. The same
// inputs always give the same bytes.
func (in Inputs) Notices() []byte {
	rule := strings.Repeat("=", 80) + "\n"
	var b bytes.Buffer
	fmt.Fprintf(&b, "Third-party notices of the Lavish build of lavish-axi %s compiled with Deno %s\n\n", in.Axi, in.Deno)
	b.WriteString("The Lavish build is AsmAI's lavish-axi, compiled with `deno compile` into one\n")
	b.WriteString("executable. It contains lavish-axi, its locked npm dependencies, the assets and\n")
	b.WriteString("fonts lavish-axi vendors, and the Deno runtime with the Rust crates compiled\n")
	b.WriteString("into it. Each is followed by its license texts.\n")
	section := func(title string, files []licenses.LicenseFile) {
		fmt.Fprintf(&b, "\n%s%s\n%s", rule, title, rule)
		for _, f := range files {
			fmt.Fprintf(&b, "\n-- %s --\n\n", f.Name)
			b.Write(f.Text)
			if !bytes.HasSuffix(f.Text, []byte("\n")) {
				b.WriteString("\n")
			}
		}
	}
	section("What lavish-axi vendors", []licenses.LicenseFile{{Name: "THIRD-PARTY-NOTICES.md of lavish-axi", Text: in.AxiNotices}})
	if in.Vendored.UsesOFL() {
		section("SIL Open Font License 1.1, for the fonts lavish-axi vendors under it", []licenses.LicenseFile{{Name: "OFL-1.1", Text: in.OFL}})
	}
	for _, m := range in.NPM {
		section("npm: "+m.Name, m.Files)
	}
	section("Deno "+in.Deno+" (runtime)", []licenses.LicenseFile{{Name: "LICENSE.md", Text: in.DenoLicense}})
	for _, m := range in.Crates {
		section("Deno runtime crate: "+m.Name, m.Files)
	}
	return b.Bytes()
}
