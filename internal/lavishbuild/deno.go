// SPDX-License-Identifier: Apache-2.0

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

// Satisfied reports whether the SPDX license expression expr is satisfied
// by licenses on the allow-list: an OR needs one side, an AND both, and a
// WITH exception does not change the license it qualifies. The old "/" is an
// OR. An expression it cannot parse is not satisfied.
func Satisfied(expr string, allowed []string) bool {
	p := &exprParser{tokens: tokenize(expr), allowed: allowed}
	ok := p.or()
	return ok && p.pos == len(p.tokens) && !p.bad
}

func tokenize(expr string) []string {
	expr = strings.NewReplacer("(", " ( ", ")", " ) ", "/", " OR ").Replace(expr)
	return strings.Fields(expr)
}

type exprParser struct {
	tokens  []string
	pos     int
	allowed []string
	bad     bool
}

func (p *exprParser) peek() string {
	if p.pos < len(p.tokens) {
		return p.tokens[p.pos]
	}
	return ""
}

func (p *exprParser) or() bool {
	ok := p.and()
	for strings.EqualFold(p.peek(), "OR") {
		p.pos++
		// Both sides are parsed, so a malformed right side is seen.
		right := p.and()
		ok = ok || right
	}
	return ok
}

func (p *exprParser) and() bool {
	ok := p.term()
	for strings.EqualFold(p.peek(), "AND") {
		p.pos++
		right := p.term()
		ok = ok && right
	}
	return ok
}

func (p *exprParser) term() bool {
	tok := p.peek()
	switch tok {
	case "":
		p.bad = true
		return false
	case "(":
		p.pos++
		ok := p.or()
		if p.peek() != ")" {
			p.bad = true
			return false
		}
		p.pos++
		return ok
	case ")", "AND", "OR", "WITH":
		p.bad = true
		return false
	}
	p.pos++
	id := strings.TrimSuffix(tok, "+")
	if strings.EqualFold(p.peek(), "WITH") {
		p.pos += 2
	}
	return slices.Contains(p.allowed, id)
}

// Crate is a Rust crate compiled into the Deno runtime, from `cargo
// metadata`.
type Crate struct {
	Name, Version, License string
	// Dir holds the crate's source, and so its license files.
	Dir string
}

func (c Crate) String() string { return c.Name + " " + c.Version }

// metadata is the part of `cargo metadata --format-version 1` that is read.
type metadata struct {
	Packages []struct {
		ID           string `json:"id"`
		Name         string `json:"name"`
		Version      string `json:"version"`
		License      string `json:"license"`
		LicenseFile  string `json:"license_file"`
		ManifestPath string `json:"manifest_path"`
	} `json:"packages"`
	Resolve struct {
		Nodes []struct {
			ID   string `json:"id"`
			Deps []struct {
				Pkg      string `json:"pkg"`
				DepKinds []struct {
					Kind *string `json:"kind"`
				} `json:"dep_kinds"`
			} `json:"deps"`
		} `json:"nodes"`
	} `json:"resolve"`
}

// RuntimeCrates returns the crates compiled into the package named root, by
// name and version: those reachable from it through normal dependencies, not
// through dev or build dependencies, in the `cargo metadata` output of each
// of files. Several files are the platforms the runtime is compiled for.
func RuntimeCrates(root string, files ...string) ([]Crate, error) {
	byKey := map[string]Crate{}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		var md metadata
		if err := json.Unmarshal(data, &md); err != nil {
			return nil, fmt.Errorf("%s is not cargo metadata: %w", file, err)
		}
		pkgs := map[string]int{}
		rootID := ""
		for i, p := range md.Packages {
			pkgs[p.ID] = i
			if p.Name == root {
				rootID = p.ID
			}
		}
		if rootID == "" {
			return nil, fmt.Errorf("%s holds no package named %s", file, root)
		}
		deps := map[string][]string{}
		for _, n := range md.Resolve.Nodes {
			for _, d := range n.Deps {
				if slices.ContainsFunc(d.DepKinds, func(k struct {
					Kind *string `json:"kind"`
				}) bool {
					return k.Kind == nil
				}) {
					deps[n.ID] = append(deps[n.ID], d.Pkg)
				}
			}
		}
		seen := map[string]bool{rootID: true}
		queue := []string{rootID}
		for len(queue) > 0 {
			id := queue[0]
			queue = queue[1:]
			p := md.Packages[pkgs[id]]
			c := Crate{Name: p.Name, Version: p.Version, License: p.License, Dir: filepath.Dir(p.ManifestPath)}
			if p.License == "" && p.LicenseFile != "" {
				c.License = "SEE LICENSE IN " + p.LicenseFile
			}
			byKey[p.Name+" "+p.Version] = c
			for _, d := range deps[id] {
				if !seen[d] {
					seen[d] = true
					queue = append(queue, d)
				}
			}
		}
	}
	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	crates := make([]Crate, 0, len(keys))
	for _, k := range keys {
		crates = append(crates, byKey[k])
	}
	return crates, nil
}

// CrateMaterial holds each crate's license to the allow-list: the license it
// declares must be satisfied by licenses on the list. Its license files are
// read from its source for the notices; a crate under a choice of licenses
// carries the files of all of them.
func CrateMaterial(crates []Crate) ([]Material, error) {
	var out []Material
	for _, c := range crates {
		files, err := licenses.ReadLicenseFiles(c.Dir)
		if err != nil {
			return nil, fmt.Errorf("reading %s's license files from %s (run `cargo fetch --locked` first): %w", c, c.Dir, err)
		}
		m := Material{Name: c.String(), Files: files, Licenses: []string{c.License}}
		switch {
		case c.License == "":
			m.Problem = "no license is declared"
		case !Satisfied(c.License, licenses.Allowed):
			m.Problem = fmt.Sprintf("license %s is not satisfied by the allow-list", c.License)
		case len(files) == 0:
			m.Problem = "ships no license file to carry in the notices"
		}
		out = append(out, m)
	}
	return out, nil
}
