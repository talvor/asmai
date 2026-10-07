// SPDX-License-Identifier: Apache-2.0

package spdx_test

import (
	"os"
	"slices"
	"testing"
	"testing/fstest"

	"github.com/talvor/asmai/internal/spdx"
)

func TestMissingNamesSourceFilesWithoutTheLine(t *testing.T) {
	fsys := fstest.MapFS{
		"cmd/asmai/main.go":          {Data: []byte("// SPDX-License-Identifier: Apache-2.0\n\npackage main\n")},
		"cmd/asmai/bare.go":          {Data: []byte("package main\n")},
		"internal/late.go":           {Data: []byte("package internal\n\n// SPDX-License-Identifier: Apache-2.0\n")},
		"scripts/ok.sh":              {Data: []byte("#!/bin/sh\n# SPDX-License-Identifier: Apache-2.0\necho hi\n")},
		"scripts/bad.sh":             {Data: []byte("#!/bin/sh\necho hi\n")},
		".github/workflows/ci.yml":   {Data: []byte("# SPDX-License-Identifier: Apache-2.0\nname: CI\n")},
		".github/workflows/bad.yaml": {Data: []byte("name: Bad\n")},
		"wrong.go":                   {Data: []byte("// SPDX-License-Identifier: MIT\n\npackage wrong\n")},
		"README.md":                  {Data: []byte("# not source\n")},
		"LICENSE":                    {Data: []byte("Apache License\n")},
	}

	got, err := spdx.Missing(fsys)
	if err != nil {
		t.Fatal(err)
	}

	want := []string{
		".github/workflows/bad.yaml",
		"cmd/asmai/bare.go",
		"internal/late.go",
		"scripts/bad.sh",
		"wrong.go",
	}
	if !slices.Equal(got, want) {
		t.Errorf("Missing() = %q, want %q", got, want)
	}
}

func TestMissingSkipsOnlyRootGitAndBuildOutput(t *testing.T) {
	fsys := fstest.MapFS{
		".git/hooks/pre-commit.sh": {Data: []byte("#!/bin/sh\n")},
		"dist/linux-amd64/x.go":    {Data: []byte("package x\n")},
		"internal/dist/x.go":       {Data: []byte("package dist\n")},
		"internal/.git/x.sh":       {Data: []byte("#!/bin/sh\n")},
	}

	got, err := spdx.Missing(fsys)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"internal/.git/x.sh", "internal/dist/x.go"}
	if !slices.Equal(got, want) {
		t.Errorf("Missing() = %q, want %q", got, want)
	}
}

// Every source file in this repository starts with the SPDX line, so CI fails
// on a pull request that adds one without it.
func TestEverySourceFileInTheRepositoryCarriesTheLine(t *testing.T) {
	got, err := spdx.Missing(os.DirFS("../.."))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range got {
		t.Errorf("%s does not start with %q", path, spdx.Line)
	}
}
