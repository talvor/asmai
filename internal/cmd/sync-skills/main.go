// SPDX-License-Identifier: Apache-2.0

// Command sync-skills takes AsmAI's skill bundle from a checkout of upstream's
// repository at a release: the skills of its engineering and productivity
// folders, unchanged, with its LICENSE, and the manifest that pins the
// release and fingerprints each skill. It replaces the bundle, so any patch to
// a skill has to be made again and listed. Run it from the repository root:
//
//	go run ./internal/cmd/sync-skills <upstream checkout> <release> <commit>
package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/talvor/asmai/internal/skills"
)

const (
	source = "https://github.com/mattpocock/skills"
	dir    = "internal/skills"
)

// promoted are upstream's folders the bundle takes its skills from.
var promoted = []string{"engineering", "productivity"}

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: sync-skills <upstream checkout> <release> <commit>")
		os.Exit(2)
	}
	if err := sync(os.Args[1], os.Args[2], os.Args[3]); err != nil {
		fmt.Fprintln(os.Stderr, "sync-skills:", err)
		os.Exit(1)
	}
}

func sync(upstream, release, commit string) error {
	bundle := filepath.Join(dir, "bundle")
	if err := os.RemoveAll(bundle); err != nil {
		return err
	}
	license, err := os.ReadFile(filepath.Join(upstream, "LICENSE"))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(bundle, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(bundle, "LICENSE"), license, 0o644); err != nil {
		return err
	}
	m := skills.Manifest{Source: source, Release: release, Commit: commit, License: "MIT", Skills: map[string]skills.Entry{}, Patches: []skills.Patch{}}
	for _, group := range promoted {
		entries, err := os.ReadDir(filepath.Join(upstream, "skills", group))
		if err != nil {
			return err
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			from := filepath.Join(upstream, "skills", group, e.Name())
			to := filepath.Join(bundle, skills.Dir, e.Name())
			if err := copyTree(from, to); err != nil {
				return err
			}
			fp, err := skills.FingerprintFS(os.DirFS(to))
			if err != nil {
				return err
			}
			data, err := os.ReadFile(filepath.Join(to, "SKILL.md"))
			if err != nil {
				return err
			}
			front, _ := strings.CutPrefix(string(data), "---\n")
			front, _, _ = strings.Cut(front, "\n---\n")
			m.Skills[e.Name()] = skills.Entry{Group: group, Fingerprint: fp, UserInvoked: strings.Contains("\n"+front+"\n", "\ndisable-model-invocation: true\n")}
		}
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "bundle.json"), append(data, '\n'), 0o644)
}

func copyTree(from, to string) error {
	return filepath.WalkDir(from, func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, name)
		target := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
}
