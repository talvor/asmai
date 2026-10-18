// SPDX-License-Identifier: Apache-2.0

// Package lavishbuild checks and documents what is compiled into a Lavish
// build: lavish-axi, its locked npm dependencies, the fonts and assets it
// vendors, and the Deno runtime. The license allow-list of package licenses
// covers all of it (10 rule 5), and the build carries one notices file for it
// (10 rule 4). The workflow that makes a Lavish build runs `lavish-notices`,
// which uses this package.
package lavishbuild

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/talvor/asmai/internal/licenses"
)

// Material is one thing compiled into a Lavish build, with its license files
// and the licenses found in them.
type Material struct {
	// Name says what it is, such as "express 5.2.1".
	Name     string
	Licenses []string
	Files    []licenses.LicenseFile
	// Problem says why its licenses are not acceptable, or is empty.
	Problem string
}

// Package is an npm package the deno.lock locks.
type Package struct {
	Name, Version string
	// Key is the lock's key for it, which names its directory in
	// node_modules/.deno.
	Key string
}

func (p Package) String() string { return p.Name + " " + p.Version }

// LockedPackages returns the npm packages deno.lock at path locks, by name
// and version.
func LockedPackages(path string) ([]Package, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var lock struct {
		Version string                     `json:"version"`
		NPM     map[string]json.RawMessage `json:"npm"`
	}
	if err := json.Unmarshal(data, &lock); err != nil {
		return nil, fmt.Errorf("%s is not a Deno lock file: %w", path, err)
	}
	if lock.Version != "5" {
		return nil, fmt.Errorf("%s is lock file version %q, but only version 5 is understood", path, lock.Version)
	}
	var pkgs []Package
	for key := range lock.NPM {
		// A key is name@version, with a trailing _peer@version when the
		// package is resolved against a peer.
		at := strings.LastIndex(strings.SplitN(key, "_", 2)[0], "@")
		if at <= 0 {
			return nil, fmt.Errorf("%s locks %q, which is not name@version", path, key)
		}
		base := strings.SplitN(key, "_", 2)[0]
		pkgs = append(pkgs, Package{Name: base[:at], Version: base[at+1:], Key: key})
	}
	slices.SortFunc(pkgs, func(a, b Package) int { return strings.Compare(a.Key, b.Key) })
	return pkgs, nil
}

// packageDir is where `deno install` puts p under the build directory dir.
func packageDir(dir string, p Package) string {
	return filepath.Join(dir, "node_modules", ".deno", strings.ReplaceAll(p.Key, "/", "+"), "node_modules", filepath.FromSlash(p.Name))
}

// NPMMaterial reads the license files of every package the build directory
// dir's deno.lock locks, from the node_modules `deno install` made in dir,
// and holds each to the allow-list. A package with no license file is
// accepted only on a license on the allow-list that its package.json declares.
func NPMMaterial(dir string) ([]Material, error) {
	pkgs, err := LockedPackages(filepath.Join(dir, "deno.lock"))
	if err != nil {
		return nil, err
	}
	var out []Material
	for _, p := range pkgs {
		pdir := packageDir(dir, p)
		files, err := licenses.ReadLicenseFiles(pdir)
		if err != nil {
			return nil, fmt.Errorf("reading %s's licenses from %s (run `deno install --frozen` in %s first): %w", p, pdir, dir, err)
		}
		m := Material{Name: p.String(), Files: files, Licenses: licenses.Detect(files)}
		if len(m.Licenses) == 0 {
			// No license file, or one whose text is not recognised as a
			// license, as when a package words a BSD license its own way: the
			// license the package declares counts when it is on the allow-list.
			declared, err := declaredLicense(pdir)
			if err != nil {
				return nil, err
			}
			if slices.Contains(licenses.Allowed, declared) {
				m.Licenses = []string{declared}
				note := fmt.Sprintf("%s declares the license %s in its package.json and ships no license file.\n", p, declared)
				if len(files) > 0 {
					note = fmt.Sprintf("%s declares the license %s in its package.json; the license file below is not text that is recognised as that license.\n", p, declared)
				}
				m.Files = append([]licenses.LicenseFile{{Name: "package.json", Text: []byte(note)}}, files...)
			}
		}
		m.Problem = licenses.Module{Licenses: m.Licenses}.Problem()
		out = append(out, m)
	}
	return out, nil
}

func declaredLicense(pdir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(pdir, "package.json"))
	if err != nil {
		return "", err
	}
	var pkg struct {
		License any `json:"license"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return "", fmt.Errorf("%s/package.json: %w", pdir, err)
	}
	if s, ok := pkg.License.(string); ok {
		return s, nil
	}
	return "", nil
}
