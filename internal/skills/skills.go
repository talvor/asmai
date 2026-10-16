// SPDX-License-Identifier: Apache-2.0

// Package skills holds the skill bundle AsmAI embeds: upstream's promoted
// skills at the pinned release, with listed patches and their MIT notice; the
// operating guide that maps their single-agent phrasing onto the factory;
// each role's default skill lists; and the index each leader gets of its
// workers' skills.
package skills

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strconv"
	"strings"
)

//go:embed bundle
var embedded embed.FS

//go:embed bundle.json
var manifestJSON []byte

//go:embed guide.md
var guide string

// Dir is where each skill's directory is under Files.
const Dir = "skills"

// Manifest is bundle.json: where the bundle comes from, the fingerprint each
// skill had when it was taken from upstream, and the patches made to it.
type Manifest struct {
	// Source is upstream's repository, Release the upstream release the
	// bundle is, and Commit the commit that release names.
	Source  string `json:"source"`
	Release string `json:"release"`
	Commit  string `json:"commit"`
	// License is the SPDX identifier of upstream's license, whose text is
	// bundle/LICENSE.
	License string `json:"license"`
	// Skills holds each shipped skill, by name.
	Skills map[string]Entry `json:"skills"`
	// Patches are the changes made to upstream's text, each with its reason.
	// A skill is patched only where the operating guide cannot work.
	Patches []Patch `json:"patches"`
}

// Entry is one shipped skill's record in the manifest.
type Entry struct {
	// Group is the upstream folder the skill is promoted from.
	Group string `json:"group"`
	// Fingerprint is the skill's fingerprint in upstream's release.
	Fingerprint string `json:"fingerprint"`
	UserInvoked bool   `json:"user_invoked,omitempty"`
}

// Patch is a change made to an upstream skill, with its reason.
type Patch struct {
	Skill  string `json:"skill"`
	File   string `json:"file"`
	Reason string `json:"reason"`
}

var manifest = func() Manifest {
	var m Manifest
	if err := json.Unmarshal(manifestJSON, &m); err != nil {
		panic("skills: bundle.json: " + err.Error())
	}
	return m
}()

// Skill is a shipped skill.
type Skill struct {
	// Name is the skill's name, which is also the name of its directory.
	Name string
	// Description is upstream's description of the skill.
	Description string
	// UserInvoked is whether upstream marks the skill as one that starts
	// only on request.
	UserInvoked bool
	// Group is the upstream folder the skill comes from.
	Group string
	// Fingerprint identifies the skill's text: see FingerprintFS.
	Fingerprint string
}

// Purpose is what the skill is for: upstream's description without the part
// that says when to use it. It is empty when upstream's description is only
// the when-clause.
func (s Skill) Purpose() string {
	if strings.HasPrefix(s.Description, "Use when") {
		return ""
	}
	purpose, _, _ := strings.Cut(s.Description, " Use when")
	return strings.TrimSpace(purpose)
}

var byName = func() map[string]Skill {
	skills := map[string]Skill{}
	for name, e := range manifest.Skills {
		data, err := embedded.ReadFile(path.Join("bundle", Dir, name, "SKILL.md"))
		if err != nil {
			panic("skills: " + err.Error())
		}
		fields := frontMatter(string(data))
		files, err := fs.Sub(embedded, path.Join("bundle", Dir, name))
		if err != nil {
			panic("skills: " + err.Error())
		}
		fp, err := FingerprintFS(files)
		if err != nil {
			panic("skills: " + err.Error())
		}
		skills[name] = Skill{
			Name:        name,
			Description: fields["description"],
			UserInvoked: e.UserInvoked,
			Group:       e.Group,
			Fingerprint: fp,
		}
	}
	return skills
}()

// frontMatter returns the top-level `key: value` lines of a file's front
// matter, with double or single quotes taken off the values.
func frontMatter(text string) map[string]string {
	fields := map[string]string{}
	rest, ok := strings.CutPrefix(text, "---\n")
	if !ok {
		return fields
	}
	block, _, _ := strings.Cut(rest, "\n---\n")
	for line := range strings.SplitSeq(block, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok || key == "" || strings.ContainsAny(key[:1], " \t") {
			continue
		}
		value = strings.TrimSpace(value)
		if unquoted, err := strconv.Unquote(value); err == nil {
			value = unquoted
		} else if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
			value = value[1 : len(value)-1]
		}
		fields[key] = value
	}
	return fields
}

// FingerprintFS returns the fingerprint of the skill whose files are fsys: the
// SHA-256, in hex, of one line for each file by path, the path, a NUL and the
// hex SHA-256 of the file's content. It changes when any file of the skill
// changes, is added or is removed.
func FingerprintFS(fsys fs.FS) (string, error) {
	var lines bytes.Buffer
	err := fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		fmt.Fprintf(&lines, "%s\x00%s\n", name, hex.EncodeToString(sum[:]))
		return nil
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(lines.Bytes())
	return hex.EncodeToString(sum[:]), nil
}

// Names are the shipped skills' names, in order.
func Names() []string {
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// Get returns the shipped skill called name.
func Get(name string) (Skill, bool) {
	s, ok := byName[name]
	return s, ok
}

// Files returns the files of the shipped skill called name, rooted at its
// directory.
func Files(name string) (fs.FS, error) {
	if _, ok := byName[name]; !ok {
		return nil, fmt.Errorf("%q is not a skill in the bundle", name)
	}
	return fs.Sub(embedded, path.Join("bundle", Dir, name))
}

// Version is the bundle's version: the upstream release it is. A dispatch
// records it.
func Version() string {
	return manifest.Release
}

// Source is the upstream repository the bundle is taken from.
func Source() string {
	return manifest.Source
}

// Commit is the upstream commit Version names.
func Commit() string {
	return manifest.Commit
}

// Fingerprint returns the fingerprint of the shipped skill called name.
func Fingerprint(name string) (string, bool) {
	s, ok := byName[name]
	return s.Fingerprint, ok
}

// Fingerprints returns the fingerprint of every shipped skill, by name.
func Fingerprints() map[string]string {
	out := make(map[string]string, len(byName))
	for name, s := range byName {
		out[name] = s.Fingerprint
	}
	return out
}

// Patches are the changes made to upstream's skills, each with its reason.
func Patches() []Patch {
	return slices.Clone(manifest.Patches)
}

// Upstream is the fingerprint each skill had in upstream's release, which
// the shipped skill has too unless it is patched.
func Upstream() map[string]string {
	out := make(map[string]string, len(manifest.Skills))
	for name, e := range manifest.Skills {
		out[name] = e.Fingerprint
	}
	return out
}

// License is the text of upstream's license, which the bundle is shipped
// with.
func License() []byte {
	data, err := embedded.ReadFile("bundle/LICENSE")
	if err != nil {
		panic("skills: " + err.Error())
	}
	return data
}

// LicenseName says what the license covers, as the third-party notices and
// the license check name it.
func LicenseName() string {
	return "Matt Pocock's skills " + manifest.Release + " (the skill bundle, " + manifest.Source + ")"
}

// LicenseID is the SPDX identifier of the license.
func LicenseID() string {
	return manifest.License
}

// Guide is the operating guide, which every agent's instructions carry.
func Guide() string {
	return guide
}
