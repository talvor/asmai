// SPDX-License-Identifier: Apache-2.0

package lavishbuild

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/talvor/asmai"
	"github.com/talvor/asmai/internal/providers"
)

const mit = `MIT License

Copyright (c) 2020 Someone

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
`

// gpl is enough of the GNU GPL v3's text to be recognised as it.
func gpl(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("/usr/share/common-licenses/GPL-3")
	if err != nil {
		t.Skip("no GPL text on this host")
	}
	return string(b)
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// buildDir makes a Lavish build directory that locks the packages, each
// installed with its files, as `deno install` lays them out.
func buildDir(t *testing.T, packages map[string]map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	lock := `{"version": "5", "specifiers": {}, "npm": {`
	first := true
	for key, files := range packages {
		if !first {
			lock += ","
		}
		first = false
		lock += `"` + key + `": {"integrity": "sha512-x"}`
		name := key[:strings.LastIndex(strings.SplitN(key, "_", 2)[0], "@")]
		for file, content := range files {
			write(t, filepath.Join(dir, "node_modules", ".deno", strings.ReplaceAll(key, "/", "+"), "node_modules", filepath.FromSlash(name), file), content)
		}
	}
	write(t, filepath.Join(dir, "deno.lock"), lock+"}}")
	return dir
}

func problems(ms []Material) map[string]string {
	out := map[string]string{}
	for _, m := range ms {
		out[m.Name] = m.Problem
	}
	return out
}

func TestEveryLockedNpmPackageIsHeldToTheAllowListWithItsLicenseTextForTheNotices(t *testing.T) {
	dir := buildDir(t, map[string]map[string]string{
		"express@5.2.1":             {"LICENSE": mit},
		"@scope/pkg@1.0.0":          {"LICENSE.md": mit},
		"peered@2.0.0_react@18.0.0": {"LICENSE": mit},
		"declared@1.0.0":            {"package.json": `{"license": "ISC"}`},
		"oddly-worded@3.0.0":        {"LICENSE": "Take it, it is yours, no warranty.\n", "package.json": `{"license": "BSD-2-Clause"}`},
		"copyleft@1.0.0":            {"LICENSE": gpl(t), "package.json": `{"license": "GPL-3.0"}`},
		"unlicensed@1.0.0":          {"package.json": `{"license": "UNLICENSED"}`},
		"unlisted@1.0.0":            {"package.json": `{"license": "MPL-2.0"}`},
	})

	got, err := NPMMaterial(dir)

	if err != nil {
		t.Fatal(err)
	}
	p := problems(got)
	for _, ok := range []string{"express 5.2.1", "@scope/pkg 1.0.0", "peered 2.0.0", "declared 1.0.0", "oddly-worded 3.0.0"} {
		if problem, found := p[ok]; !found || problem != "" {
			t.Errorf("%s: problem %q (found %v), want it accepted", ok, problem, found)
		}
	}
	for _, bad := range []string{"copyleft 1.0.0", "unlicensed 1.0.0", "unlisted 1.0.0"} {
		if p[bad] == "" {
			t.Errorf("%s was accepted, want it refused", bad)
		}
	}
	for _, m := range got {
		if m.Name == "oddly-worded 3.0.0" && (len(m.Files) != 2 || !strings.Contains(string(m.Files[0].Text), "declares the license BSD-2-Clause")) {
			t.Errorf("a package whose license text is not recognised carries %v, want its declared license noted before its file", m.Files)
		}
	}
}

func TestAPackageMissingFromNodeModulesIsAnErrorNotAPass(t *testing.T) {
	dir := buildDir(t, map[string]map[string]string{"express@5.2.1": {"LICENSE": mit}})
	os.RemoveAll(filepath.Join(dir, "node_modules"))

	if _, err := NPMMaterial(dir); err == nil || !strings.Contains(err.Error(), "deno install") {
		t.Errorf("a lock with nothing installed gave %v, want an error saying to run deno install", err)
	}
}

func TestAFileThatIsNotALockOfVersion5IsRefused(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "deno.lock"), `{"version": "4", "npm": {}}`)
	if _, err := LockedPackages(filepath.Join(dir, "deno.lock")); err == nil {
		t.Error("a version 4 lock was accepted")
	}
}

const axiNotices = `# Third-party notices

## Bundled into dist/whiteboard

| Package | License | Copyright |
| ------- | ------- | --------- |
| ` + "`react`" + ` | MIT | Copyright (c) Meta |

## Fonts vendored into dist/whiteboard/fonts/

| Family | License |
| ------ | ------- |
| Excalifont | MIT (created for Excalidraw) |
| Nunito | SIL Open Font License 1.1 |

## Bundled into dist/design/

| Asset | License |
| ----- | ------- |
| daisyUI | MIT |
`

func TestTheFontsLavishVendorsMayBeUnderTheSILOFLAndNothingElseMayBe(t *testing.T) {
	v, err := ParseVendored([]byte(axiNotices))
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Entries) != 4 || !v.UsesOFL() {
		t.Fatalf("the entries are %+v, want the four rows and the use of the OFL", v.Entries)
	}
	if p := v.Problems(); len(p) != 0 {
		t.Errorf("the vendored fonts and assets have problems %v, want none", p)
	}

	// The same license on something that is not a font is not accepted.
	notFont := strings.Replace(axiNotices, "| daisyUI | MIT |", "| daisyUI | SIL Open Font License 1.1 |", 1)
	v, _ = ParseVendored([]byte(notFont))
	if p := v.Problems(); len(p) != 1 || !strings.Contains(p[0], "daisyUI") || !strings.Contains(p[0], OFL) {
		t.Errorf("the OFL on a design asset gave problems %v, want it refused", p)
	}
	unknown := strings.Replace(axiNotices, "| Nunito | SIL Open Font License 1.1 |", "| Nunito | CC-BY-NC-4.0 |", 1)
	v, _ = ParseVendored([]byte(unknown))
	if p := v.Problems(); len(p) != 1 || !strings.Contains(p[0], "not one that is understood") {
		t.Errorf("an unknown license gave problems %v, want it refused", p)
	}
	if _, err := ParseVendored([]byte("# nothing\n")); err == nil {
		t.Error("a notices file with no tables was accepted")
	}
}

func TestTheNoticesCarryEverythingAndTheSameInputsGiveTheSameBytes(t *testing.T) {
	dir := buildDir(t, map[string]map[string]string{"express@5.2.1": {"LICENSE": mit}})
	npm, err := NPMMaterial(dir)
	if err != nil {
		t.Fatal(err)
	}
	vendored, _ := ParseVendored([]byte(axiNotices))
	ofl, err := os.ReadFile("../../lavish/licenses/OFL-1.1.txt")
	if err != nil {
		t.Fatal(err)
	}
	in := Inputs{Axi: "0.1.89", Deno: "2.9.6", NPM: npm, AxiNotices: []byte(axiNotices), Vendored: vendored, OFL: ofl, DenoLicense: []byte(mit)}

	if p := in.Problems(); len(p) != 0 {
		t.Fatalf("the inputs have problems %v", p)
	}
	notices := in.Notices()
	for _, want := range []string{"lavish-axi 0.1.89 compiled with Deno 2.9.6", "What lavish-axi vendors", "SIL Open Font License", "npm: express 5.2.1", "Deno 2.9.6 (runtime)"} {
		if !strings.Contains(string(notices), want) {
			t.Errorf("the notices lack %q", want)
		}
	}
	if string(in.Notices()) != string(notices) {
		t.Error("the same inputs gave different notices")
	}

	in.DenoLicense = []byte("All rights reserved.")
	if p := in.Problems(); len(p) != 1 || !strings.Contains(p[0], "Deno runtime's own license") {
		t.Errorf("a Deno license that is not on the allow-list gave problems %v", p)
	}
}

// The files in the repository that name the pinned versions agree with the
// pins file, so a Lavish build is never made from a different lavish-axi than
// the one pinned.
func TestTheLavishBuildDirectoryBuildsThePinnedLavishAxi(t *testing.T) {
	pins, err := providers.ParsePins(asmai.Pins)
	if err != nil {
		t.Fatal(err)
	}
	axi := pins.Lavish.Version
	deno, err := os.ReadFile("../../lavish/deno.json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(deno), `"npm:/lavish-axi@`+axi+`/"`) {
		t.Errorf("lavish/deno.json does not import lavish-axi %s:\n%s", axi, deno)
	}
	lock, err := os.ReadFile("../../lavish/deno.lock")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(lock), `"npm:lavish-axi@`+axi+`": "`+axi+`"`) {
		t.Errorf("lavish/deno.lock does not lock lavish-axi %s", axi)
	}
	pkgs, err := LockedPackages("../../lavish/deno.lock")
	if err != nil || len(pkgs) < 10 {
		t.Errorf("lavish/deno.lock locks %d packages (%v), want lavish-axi and its dependencies", len(pkgs), err)
	}
	main, err := os.ReadFile("../../lavish/main.ts")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--notices", "server.mjs", `import("lavish-axi/dist/server.mjs")`, `import("lavish-axi/dist/cli.mjs")`} {
		if !strings.Contains(string(main), want) {
			t.Errorf("lavish/main.ts lacks %q, which the build and the daemon rely on", want)
		}
	}
}

// The workflow builds for every platform the pins file certifies, publishes
// the build as a release that is never the latest, with notices, checksums
// and attestations, and runs when the pins move.
func TestTheWorkflowPublishesTheBuildWithItsNoticesAndNeverAsTheLatestRelease(t *testing.T) {
	wf, err := os.ReadFile("../../.github/workflows/lavish-build.yml")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"      - pins.json", "workflow_dispatch:",
		"lavish-v$axi-deno$deno",
		"go run ./internal/cmd/lavish-notices", "--include LAVISH_NOTICES",
		"--latest=false", "actions/attest-build-provenance", "lavish-checksums.txt",
		"go run ./internal/cmd/pin-lavish",
		"x86_64-unknown-linux-gnu", "aarch64-apple-darwin",
	} {
		if !strings.Contains(string(wf), want) {
			t.Errorf("the Lavish build workflow lacks %q", want)
		}
	}
	// The tag the workflow publishes is the one the install fetches from.
	pins, _ := providers.ParsePins(asmai.Pins)
	if want := "lavish-v" + pins.Lavish.Version + "-deno" + pins.Lavish.Deno; pins.Lavish.Tag() != want {
		t.Errorf("the pinned build's tag is %s, want %s", pins.Lavish.Tag(), want)
	}
}
