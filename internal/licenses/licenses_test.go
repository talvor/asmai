// SPDX-License-Identifier: Apache-2.0

package licenses_test

import (
	"bytes"
	"os"
	"testing"

	"github.com/talvor/asmai/internal/licenses"
)

var bothPlatforms = []licenses.Platform{
	{GOOS: "linux", GOARCH: "amd64"},
	{GOOS: "darwin", GOARCH: "arm64"},
}

// testdata/app compiles in an MIT module on every platform, a BSD-3-Clause
// module only on Linux, an Unlicense module and a module whose license cannot
// be determined. Its tests alone use another Unlicense module. None of these
// modules is added to asmai.
func TestCheckListsCompiledInModulesAndRejectsLicensesOffTheAllowList(t *testing.T) {
	t.Setenv("GOWORK", "off")
	var out bytes.Buffer

	ok, err := licenses.Check(&out, "testdata/app", ".", bothPlatforms)
	if err != nil {
		t.Fatal(err)
	}

	if ok {
		t.Error("Check() passed, want it to fail")
	}
	want := `Go modules compiled in for linux/amd64:
  example.com/app (main module)  MIT
  example.com/bsdlib v0.0.0  BSD-3-Clause
  example.com/mitlib v0.0.0  MIT
  example.com/unknown v0.0.0  unknown
  example.com/unlicensed v0.0.0  Unlicense
Go modules compiled in for darwin/arm64:
  example.com/app (main module)  MIT
  example.com/mitlib v0.0.0  MIT
  example.com/unknown v0.0.0  unknown
  example.com/unlicensed v0.0.0  Unlicense
FAIL linux/amd64: example.com/unknown v0.0.0: license cannot be determined
FAIL linux/amd64: example.com/unlicensed v0.0.0: license Unlicense is not on the allow-list
FAIL darwin/arm64: example.com/unknown v0.0.0: license cannot be determined
FAIL darwin/arm64: example.com/unlicensed v0.0.0: license Unlicense is not on the allow-list
`
	if got := out.String(); got != want {
		t.Errorf("Check() printed\n%s\nwant\n%s", got, want)
	}
}

// The notices name every module compiled into testdata/app on any platform,
// except the main module, each followed by its license files. Licenses off the
// allow-list are the check's business, not the notices'.
func TestNoticesCarryEveryCompiledInModulesLicenseText(t *testing.T) {
	t.Setenv("GOWORK", "off")

	got, err := licenses.Notices("testdata/app", ".", bothPlatforms)
	if err != nil {
		t.Fatal(err)
	}

	want := "Third-party notices\n\n" +
		"The Go modules compiled in, each followed by its license files.\n" +
		"\n" + rule + "example.com/bsdlib v0.0.0\n" + rule +
		"\n-- LICENSE.txt --\n\n" + readFile(t, "testdata/bsdlib/LICENSE.txt") +
		"\n" + rule + "example.com/mitlib v0.0.0\n" + rule +
		"\n-- LICENSE --\n\n" + readFile(t, "testdata/mitlib/LICENSE") +
		"\n" + rule + "example.com/unknown v0.0.0\n" + rule +
		"\n-- COPYING --\n\n" + readFile(t, "testdata/unknown/COPYING") +
		"\n" + rule + "example.com/unlicensed v0.0.0\n" + rule +
		"\n-- LICENSE --\n\n" + readFile(t, "testdata/unlicensed/LICENSE")
	if string(got) != want {
		t.Errorf("Notices() =\n%s\nwant\n%s", got, want)
	}
}

func TestNoticesAreTheSameBytesEachTime(t *testing.T) {
	t.Setenv("GOWORK", "off")

	first, err := licenses.Notices("testdata/app", ".", bothPlatforms)
	if err != nil {
		t.Fatal(err)
	}
	reversed := []licenses.Platform{bothPlatforms[1], bothPlatforms[0]}
	second, err := licenses.Notices("testdata/app", ".", reversed)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(first, second) {
		t.Errorf("Notices() differs between runs:\n%s\nthen\n%s", first, second)
	}
}

const rule = "================================================================================\n"

func readFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestNoticesSayWhenNoModuleIsCompiledIn(t *testing.T) {
	t.Setenv("GOWORK", "off")

	got, err := licenses.Notices("testdata/mitlib", ".", bothPlatforms)
	if err != nil {
		t.Fatal(err)
	}

	want := "Third-party notices\n\n" +
		"The Go modules compiled in, each followed by its license files.\n" +
		"\nNone: only the standard library is compiled in.\n"
	if string(got) != want {
		t.Errorf("Notices() =\n%s\nwant\n%s", got, want)
	}
}
