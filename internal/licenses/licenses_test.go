// SPDX-License-Identifier: Apache-2.0

package licenses_test

import (
	"bytes"
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
