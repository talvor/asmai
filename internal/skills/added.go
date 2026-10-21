// SPDX-License-Identifier: Apache-2.0

package skills

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Where a skill on a session's list comes from. Skills reach agents only
// from these three: the bundle, added skills and switched-on repository
// skills.
const (
	// Shipped is a skill of the bundle, which a release qualifies.
	Shipped = "shipped"
	// User is an added skill: a directory the configuration adds to a role's
	// leader or worker list. It is the user's, and never qualified.
	User = "user"
	// Repository is a skill in a repository's own .claude/skills, switched
	// on for a role by the repository's configuration. It is the
	// repository's, and never qualified.
	Repository = "repository"
)

// RepositoryDir is where a repository keeps its own skills, one directory
// each.
var RepositoryDir = filepath.Join(".claude", "skills")

// Given is a skill on a session's list, and where it comes from.
type Given struct {
	Name   string
	Source string
	// Dir is the directory an added or repository skill is copied from at
	// each start; it is empty for a shipped skill.
	Dir string
}

// Bundled returns the shipped skills called names as a session's list.
func Bundled(names []string) []Given {
	list := make([]Given, len(names))
	for i, name := range names {
		list[i] = Given{Name: name, Source: Shipped}
	}
	return list
}

// List returns the list made of the shipped skills called names and the
// added skills in dirs, each named by its directory's name. An added skill
// with the name of one of names replaces it in its place; any other comes
// after names, in the order of dirs. A directory starting with ~/ is in the
// user's home.
func List(names, dirs []string) ([]Given, error) {
	list := Bundled(names)
	for _, dir := range dirs {
		added, err := addedSkill(dir)
		if err != nil {
			return nil, err
		}
		i := slices.IndexFunc(list, func(g Given) bool { return g.Name == added.Name })
		switch {
		case i < 0:
			list = append(list, added)
		case list[i].Source == User:
			return nil, fmt.Errorf("the added skills %s and %s have the same name, %s", list[i].Dir, added.Dir, added.Name)
		default:
			list[i] = added
		}
	}
	return list, nil
}

func addedSkill(dir string) (Given, error) {
	if rest, ok := strings.CutPrefix(dir, "~"); ok && (rest == "" || strings.HasPrefix(rest, "/")) {
		home, err := os.UserHomeDir()
		if err != nil {
			return Given{}, fmt.Errorf("finding the home directory of the added skill %s: %w", dir, err)
		}
		dir = filepath.Join(home, rest)
	}
	dir = filepath.Clean(dir)
	data, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		return Given{}, fmt.Errorf("the added skill %s has no SKILL.md: %w", dir, err)
	}
	if err := nameMatches("added", dir, data); err != nil {
		return Given{}, err
	}
	return Given{Name: filepath.Base(dir), Source: User, Dir: dir}, nil
}

// nameMatches refuses the kind skill at dir, whose SKILL.md holds data, when
// its front matter names it other than its directory's name, which is what
// `asmai` names it by.
func nameMatches(kind, dir string, data []byte) error {
	name := frontMatter(strings.ReplaceAll(string(data), "\r\n", "\n"))["name"]
	if name != "" && name != filepath.Base(dir) {
		return fmt.Errorf("the %s skill %s is named %s by its SKILL.md, not %s, its directory's name", kind, dir, name, filepath.Base(dir))
	}
	return nil
}

// RepositorySkills returns the skills in the repository at root's
// .claude/skills, each a directory with a SKILL.md, by name. A repository
// without that directory, or whose .claude or .claude/skills is a symbolic
// link, has none. A skill whose SKILL.md names it other than its directory's
// name is refused.
func RepositorySkills(root string) ([]Given, error) {
	dir := filepath.Join(root, RepositoryDir)
	for _, p := range []string{filepath.Dir(dir), dir} {
		if info, err := os.Lstat(p); err == nil && info.Mode()&fs.ModeSymlink != 0 {
			return nil, nil
		}
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the repository's skills: %w", err)
	}
	var list []Given
	for _, e := range entries {
		skill := filepath.Join(dir, e.Name())
		if !e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(skill, "SKILL.md"))
		if err != nil {
			continue
		}
		if err := nameMatches("repository", skill, data); err != nil {
			return nil, err
		}
		list = append(list, Given{Name: e.Name(), Source: Repository, Dir: skill})
	}
	return list, nil
}

// NamesOf returns the names of the skills on list, in its order.
func NamesOf(list []Given) []string {
	names := make([]string, len(list))
	for i, g := range list {
		names[i] = g.Name
	}
	return names
}

// files returns the files of the skill g, rooted at its directory: the
// bundle's for a shipped skill, otherwise its directory's as they are now.
func (g Given) files() (fs.FS, error) {
	if g.Source == Shipped {
		return Files(g.Name)
	}
	return os.DirFS(g.Dir), nil
}

// Qualified says whether the skill is one a release qualifies: only the
// bundle's are.
func (g Given) Qualified() bool {
	return g.Source == Shipped
}

// Owner says whose an unqualified skill is, as `asmai` marks it.
func (g Given) Owner() string {
	switch g.Source {
	case User:
		return "the user's"
	case Repository:
		return "the repository's"
	}
	return "the bundle's"
}

// describe returns the description and whether it starts only on request
// of the skill g, from its SKILL.md's front matter for one that is not
// shipped.
func (g Given) describe() (description string, userInvoked bool) {
	if g.Source == Shipped {
		s := byName[g.Name]
		return s.Description, s.UserInvoked
	}
	data, err := os.ReadFile(filepath.Join(g.Dir, "SKILL.md"))
	if err != nil {
		return "", false
	}
	fields := frontMatter(strings.ReplaceAll(string(data), "\r\n", "\n"))
	return fields["description"], fields["disable-model-invocation"] == "true"
}

// UserInvoked says whether the skill g starts only on request: upstream
// marks it so, or for a skill that is not shipped its own SKILL.md does.
func (g Given) UserInvoked() bool {
	_, userInvoked := g.describe()
	return userInvoked
}
