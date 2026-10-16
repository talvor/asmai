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
// called names, as shipped in the bundle, whatever dir held before.
func WritePlugin(dir string, names []string) error {
	for _, name := range names {
		if _, ok := byName[name]; !ok {
			return fmt.Errorf("%q is not a skill in the bundle", name)
		}
	}
	_, err := WriteSessionPlugin(dir, Bundled(names))
	return err
}

// WriteSessionPlugin makes dir a Claude Code plugin that holds exactly the
// skills on list, whatever dir held before, and returns the Delivery a
// dispatch records of it. A shipped skill is written from the bundle, and an
// added or repository skill copied from its directory as it is now, with its
// fingerprint taken of the copy. A session is started with --plugin-dir dir,
// so the skills it has are the ones written here and nothing they are later
// edited to hold: dir is made again at each start.
func WriteSessionPlugin(dir string, list []Given) (Delivery, error) {
	if err := os.RemoveAll(dir); err != nil {
		return Delivery{}, err
	}
	manifest, err := json.Marshal(map[string]string{
		"name":        PluginName,
		"version":     Version(),
		"description": "The skills AsmAI gives this agent, from its skill bundle.",
	})
	if err != nil {
		return Delivery{}, err
	}
	if err := writeFile(filepath.Join(dir, ".claude-plugin", "plugin.json"), manifest); err != nil {
		return Delivery{}, err
	}
	d := Delivery{Bundle: Version(), Skills: map[string]string{}}
	for _, g := range list {
		if _, ok := d.Skills[g.Name]; ok {
			return Delivery{}, fmt.Errorf("the skill list names %s twice", g.Name)
		}
		files, err := g.files()
		if err != nil {
			return Delivery{}, err
		}
		root := filepath.Join(dir, Dir, g.Name)
		err = fs.WalkDir(files, ".", func(p string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.Type()&fs.ModeSymlink != 0 {
				return fmt.Errorf("%s is a symbolic link, which a skill that is copied may not hold", p)
			}
			target := filepath.Join(root, filepath.FromSlash(p))
			if entry.IsDir() {
				return os.MkdirAll(target, 0o700)
			}
			data, err := fs.ReadFile(files, p)
			if err != nil {
				return err
			}
			return writeFile(target, data)
		})
		if err != nil {
			return Delivery{}, fmt.Errorf("writing skill %s: %w", g.Name, err)
		}
		if g.Qualified() {
			d.Skills[g.Name], _ = Fingerprint(g.Name)
			continue
		}
		if d.Skills[g.Name], err = FingerprintFS(os.DirFS(root)); err != nil {
			return Delivery{}, fmt.Errorf("fingerprinting skill %s: %w", g.Name, err)
		}
		if d.Unqualified == nil {
			d.Unqualified = map[string]string{}
		}
		d.Unqualified[g.Name] = g.Source
	}
	return d, nil
}

func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// Delivery is what a dispatch records of the skills its session was given:
// the bundle's version, the fingerprint of each skill of the session's list,
// and where each skill that is not qualified comes from, User or Repository.
type Delivery struct {
	Bundle      string            `json:"bundle"`
	Skills      map[string]string `json:"skills"`
	Unqualified map[string]string `json:"unqualified,omitempty"`
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

// WorkerSkills returns the skills a role's worker is given from the role's
// default list: see Narrow.
func WorkerSkills(role string, narrowed []string, skill string) ([]string, error) {
	return Narrow(role, workers[role], narrowed, skill)
}

// Narrow returns the skills a role's worker is given from the role's list:
// list when narrowed is empty, otherwise narrowed, which must be skills of
// list. skill, when it is not empty, is the skill the assignment names,
// which must be on the list the worker is given.
func Narrow(role string, list, narrowed []string, skill string) ([]string, error) {
	given := slices.Clone(list)
	if len(narrowed) > 0 {
		given = nil
		for _, name := range narrowed {
			if !slices.Contains(list, name) {
				return nil, fmt.Errorf("%q is not on %s's workers' skill list, which is %v: a list is only narrowed, never widened", name, role, list)
			}
			if !slices.Contains(given, name) {
				given = append(given, name)
			}
		}
	}
	if skill != "" && !slices.Contains(given, skill) {
		if len(given) == 0 {
			return nil, errors.New("the worker has no skill list to name a skill from")
		}
		return nil, fmt.Errorf("%q is not on the worker's skill list, which is %v", skill, given)
	}
	return given, nil
}
