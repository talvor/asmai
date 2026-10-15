// SPDX-License-Identifier: Apache-2.0

package skills

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
)

// PluginName is the name of the per-session plugin an agent's skills are
// delivered in. Claude Code names a plugin's skills with the plugin's name
// ahead of the skill's, so the skill tdd of a session is asmai:tdd, while
// upstream's skills, and the operating guide, call each other by bare name.
const PluginName = "asmai"

// Namespaced returns the name Claude Code gives the skill called name in
// the plugin: PluginName and a colon ahead of its own name.
func Namespaced(name string) string {
	return PluginName + ":" + name
}

// WritePlugin makes dir a Claude Code plugin that holds exactly the skills
// called names, as upstream has them, whatever dir held before. A session is
// started with --plugin-dir dir, so the skills it has are the ones written
// here and nothing it is later edited to hold: dir is made again at each
// start.
func WritePlugin(dir string, names []string) error {
	for _, name := range names {
		if _, ok := byName[name]; !ok {
			return fmt.Errorf("%q is not a skill in the bundle", name)
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	manifest, err := json.Marshal(map[string]string{
		"name":        PluginName,
		"version":     Version(),
		"description": "The skills AsmAI gives this agent, from its skill bundle.",
	})
	if err != nil {
		return err
	}
	if err := writeFile(filepath.Join(dir, ".claude-plugin", "plugin.json"), manifest); err != nil {
		return err
	}
	for _, name := range names {
		files, err := Files(name)
		if err != nil {
			return err
		}
		err = fs.WalkDir(files, ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			target := filepath.Join(dir, Dir, name, filepath.FromSlash(p))
			if d.IsDir() {
				return os.MkdirAll(target, 0o700)
			}
			data, err := fs.ReadFile(files, p)
			if err != nil {
				return err
			}
			return writeFile(target, data)
		})
		if err != nil {
			return fmt.Errorf("writing skill %s: %w", name, err)
		}
	}
	return nil
}

func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// Delivery is what a dispatch records of the skills its session was given:
// the bundle's version, and the fingerprint of each skill of the session's
// list.
type Delivery struct {
	Bundle string            `json:"bundle"`
	Skills map[string]string `json:"skills"`
}

// DeliveryOf returns the Delivery for the skills called names, or an error
// for one the bundle does not ship.
func DeliveryOf(names []string) (Delivery, error) {
	d := Delivery{Bundle: Version(), Skills: map[string]string{}}
	for _, name := range names {
		fp, ok := Fingerprint(name)
		if !ok {
			return Delivery{}, fmt.Errorf("%q is not a skill in the bundle", name)
		}
		d.Skills[name] = fp
	}
	return d, nil
}

// WorkerSkills returns the skills a role's worker is given: the role's
// default list when narrowed is empty, otherwise narrowed, which must be
// skills of that list. skill, when it is not empty, is the skill the
// assignment names, which must be on the list the worker is given.
func WorkerSkills(role string, narrowed []string, skill string) ([]string, error) {
	defaults := workers[role]
	list := slices.Clone(defaults)
	if len(narrowed) > 0 {
		list = nil
		for _, name := range narrowed {
			if !slices.Contains(defaults, name) {
				return nil, fmt.Errorf("%q is not on %s's workers' skill list, which is %v: a list is only narrowed, never widened", name, role, defaults)
			}
			if !slices.Contains(list, name) {
				list = append(list, name)
			}
		}
	}
	if skill != "" && !slices.Contains(list, skill) {
		if len(list) == 0 {
			return nil, errors.New("the worker has no skill list to name a skill from")
		}
		return nil, fmt.Errorf("%q is not on the worker's skill list, which is %v", skill, list)
	}
	return list, nil
}
