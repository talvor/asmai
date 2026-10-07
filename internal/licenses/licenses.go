// SPDX-License-Identifier: Apache-2.0

// Package licenses checks the license of every Go module compiled into a
// package against AsmAI's license allow-list.
package licenses

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/google/licensecheck"
)

// Allowed is the license allow-list: the SPDX identifiers a Go module compiled
// into asmai may carry. It is fixed by the release, not a setting. Widening it
// is the maintainer's decision, made before any module under another license
// is compiled in.
var Allowed = []string{
	"Apache-2.0",
	"BSD-2-Clause",
	"BSD-3-Clause",
	"ISC",
	"MIT",
}

// minCoverage is the share of a license file, in percent, that must match
// known license texts for its licenses to count as determined.
const minCoverage = 75

// Platform is a target operating system and CPU architecture.
type Platform struct {
	GOOS, GOARCH string
}

func (p Platform) String() string { return p.GOOS + "/" + p.GOARCH }

// Module is a Go module compiled into a package, with the licenses detected in
// its license files. No licenses means they cannot be determined.
type Module struct {
	Path, Version string
	Main          bool
	Licenses      []string
}

func (m Module) String() string {
	if m.Main {
		return m.Path + " (main module)"
	}
	return m.Path + " " + m.Version
}

// Problem says why m's licenses are not acceptable, or returns "" when every
// one of them is on the allow-list.
func (m Module) Problem() string {
	if len(m.Licenses) == 0 {
		return "license cannot be determined"
	}
	var off []string
	for _, id := range m.Licenses {
		if !slices.Contains(Allowed, id) {
			off = append(off, id)
		}
	}
	switch len(off) {
	case 0:
		return ""
	case 1:
		return "license " + off[0] + " is not on the allow-list"
	default:
		return "licenses " + strings.Join(off, ", ") + " are not on the allow-list"
	}
}

// Check lists to w the modules compiled into pkg, a package pattern resolved
// in the main module at dir, for each platform with their licenses, then names
// each module whose licenses are not acceptable. It reports whether there were
// none. Modules used only by tests are not compiled in, so are not checked.
func Check(w io.Writer, dir, pkg string, platforms []Platform) (bool, error) {
	var failures []string
	for _, p := range platforms {
		mods, err := CompiledIn(dir, pkg, p)
		if err != nil {
			return false, err
		}
		fmt.Fprintf(w, "Go modules compiled in for %s:\n", p)
		for _, m := range mods {
			ids := "unknown"
			if len(m.Licenses) > 0 {
				ids = strings.Join(m.Licenses, ", ")
			}
			fmt.Fprintf(w, "  %s  %s\n", m, ids)
			if problem := m.Problem(); problem != "" {
				failures = append(failures, fmt.Sprintf("FAIL %s: %s: %s", p, m, problem))
			}
		}
	}
	for _, f := range failures {
		fmt.Fprintln(w, f)
	}
	return len(failures) == 0, nil
}

// CompiledIn returns the modules compiled into pkg for platform p, with CGo
// disabled, the main module first and the rest by path.
func CompiledIn(dir, pkg string, p Platform) ([]Module, error) {
	cmd := exec.Command("go", "list", "-deps", "-json=Module", pkg)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOOS="+p.GOOS, "GOARCH="+p.GOARCH, "CGO_ENABLED=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list for %s: %v\n%s", p, err, stderr.Bytes())
	}

	type listedModule struct {
		Path, Version, Dir string
		Main               bool
		Replace            *listedModule
	}
	seen := map[string]bool{}
	var mods []Module
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var listed struct{ Module *listedModule }
		if err := dec.Decode(&listed); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("go list for %s: %v", p, err)
		}
		lm := listed.Module
		if lm == nil || seen[lm.Path] {
			continue // the standard library, or a module already listed
		}
		seen[lm.Path] = true
		src := lm.Dir
		if lm.Replace != nil {
			src = lm.Replace.Dir
		}
		ids, err := detect(src)
		if err != nil {
			return nil, err
		}
		mods = append(mods, Module{Path: lm.Path, Version: lm.Version, Main: lm.Main, Licenses: ids})
	}
	slices.SortFunc(mods, func(a, b Module) int {
		if a.Main != b.Main {
			if a.Main {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Path, b.Path)
	})
	return mods, nil
}

// detect returns the sorted licenses found in the license files at the root
// of a module's directory, or none when there is no license file or a license
// file is not mostly known license text.
func detect(dir string) ([]string, error) {
	if dir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() || !isLicenseFile(e.Name()) {
			continue
		}
		text, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		cov := licensecheck.Scan(text)
		if cov.Percent < minCoverage {
			return nil, nil
		}
		for _, m := range cov.Match {
			if !slices.Contains(ids, m.ID) {
				ids = append(ids, m.ID)
			}
		}
	}
	slices.Sort(ids)
	return ids, nil
}

// isLicenseFile reports whether name is a license file's name, such as
// LICENSE, LICENSE.txt, LICENSE-MIT, LICENCE.md, COPYING or MIT-LICENSE.
func isLicenseFile(name string) bool {
	upper := strings.ToUpper(name)
	for _, ext := range []string{".TXT", ".MD", ".MARKDOWN"} {
		upper = strings.TrimSuffix(upper, ext)
	}
	for _, base := range []string{"LICENSE", "LICENCE", "COPYING"} {
		if upper == base || strings.HasPrefix(upper, base+"-") || strings.HasPrefix(upper, base+".") {
			return true
		}
	}
	return strings.HasPrefix(upper, "MIT-LICENSE") || strings.HasPrefix(upper, "MIT-LICENCE")
}
