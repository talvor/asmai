// SPDX-License-Identifier: Apache-2.0

package config

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/pelletier/go-toml/v2/unstable"
)

// Writable is a writable folder a repository's entry declares (ADR 0016): a
// folder outside a workspace, usually a build cache, that the write guard
// lets the workers of the repository's jobs write, with the environment
// variable that names it for the repository's tools, when it has one.
type Writable struct {
	// Path is the folder as the file writes it: absolute, or starting with
	// "~/".
	Path string `json:"path"`
	// Env is the variable the workers' environment carries the folder's
	// absolute path in; empty for none.
	Env      string `json:"env,omitempty"`
	emptyEnv bool
}

// Folder is a writable folder resolved: Path as declared, Resolved the
// absolute path a worker may write.
type Folder struct {
	Path     string `json:"path"`
	Resolved string `json:"resolved"`
	Env      string `json:"env,omitempty"`
}

// RepositoryFolders are the folders one repository opens outside its
// workspaces.
type RepositoryFolders struct {
	Repository string   `json:"repository"`
	Folders    []Folder `json:"folders"`
}

// detection maps a marker file in a clone's root to the writable folders
// registering a repository with it suggests. A language that needs folders is
// one more line, added when a repository of it hits the problem (ADR 0016).
var detection = map[string][]Writable{
	"go.mod": {{Path: "~/.cache/go-build", Env: "GOCACHE"}},
}

// DetectWritable returns the writable folders suggested for the repository
// cloned in dir, from the marker files its root has.
func DetectWritable(dir string) []Writable {
	var found []Writable
	for _, marker := range slices.Sorted(maps.Keys(detection)) {
		if info, err := os.Stat(filepath.Join(dir, marker)); err != nil || info.IsDir() {
			continue
		}
		for _, w := range detection[marker] {
			if !slices.ContainsFunc(found, func(f Writable) bool { return f.Path == w.Path }) {
				found = append(found, w)
			}
		}
	}
	return found
}

// ParseWritable reads a folder as `asmai repo add --writable` writes it:
// <path>, or <path>=<ENV> to export the folder's path as ENV.
func ParseWritable(text string) (Writable, error) {
	if strings.TrimSpace(text) == "" {
		return Writable{}, errors.New("a writable folder is empty: write it as <path> or <path>=<ENV>, such as ~/.cache/go-build=GOCACHE")
	}
	i := strings.LastIndex(text, "=")
	if i < 0 {
		return Writable{Path: text}, nil
	}
	if i == len(text)-1 {
		return Writable{}, fmt.Errorf("%q ends in \"=\" with no variable name: write <path>, or <path>=<ENV> such as ~/.cache/go-build=GOCACHE", text)
	}
	return Writable{Path: text[:i], Env: text[i+1:]}, nil
}

// Resolve returns the absolute path of the folder path declares, with ~ taken
// as home: the path cleaned, so that "~/a/../b" is "<home>/b".
func Resolve(home, path string) (string, error) {
	switch {
	case path == "~":
		return filepath.Clean(home), nil
	case strings.HasPrefix(path, "~/"):
		return filepath.Join(home, path[2:]), nil
	case filepath.IsAbs(path):
		return filepath.Clean(path), nil
	}
	return "", fmt.Errorf("%q is not an absolute path", path)
}

// Folders returns the folders o declares, resolved with ~ taken as home. A
// folder that cannot be resolved is an error: validate the options first.
func (o RepositoryOptions) Folders(home string) ([]Folder, error) {
	var folders []Folder
	for _, w := range o.Writable {
		resolved, err := Resolve(home, w.Path)
		if err != nil {
			return nil, err
		}
		folders = append(folders, Folder{Path: w.Path, Resolved: resolved, Env: w.Env})
	}
	return folders, nil
}

// OpenFolders returns the folders each repository of c opens outside its
// workspaces, by repository name, leaving out a repository that opens none.
// Home is what ~ means.
func (c Config) OpenFolders(home string) []RepositoryFolders {
	var out []RepositoryFolders
	for _, name := range slices.Sorted(maps.Keys(c.Options)) {
		folders, err := c.Options[name].Folders(home)
		if err != nil || len(folders) == 0 {
			continue
		}
		out = append(out, RepositoryFolders{Repository: name, Folders: folders})
	}
	return out
}

var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// The variables a writable folder may not export: the worker's home, its
// search path and its temporary directory, which the daemon and the write
// guard rely on. Names starting with the prefixes are git's and AsmAI's own,
// ASMAI_SLOT among them.
var (
	reservedEnv      = []string{"HOME", "PATH", "TMPDIR"}
	reservedEnvStart = []string{"GIT_", "ASMAI_"}
)

const writableFix = `declare the folder that holds the cache, such as path = "~/.cache/go-build" with env = "GOCACHE"`

// checkWritable returns why w is not a writable folder this asmai honours,
// and the fix, or "". The reason begins with the folder as written.
func checkWritable(w Writable) (why, fix string) {
	if why, fix := checkWritablePath(w.Path); why != "" {
		return why, fix
	}
	return checkWritableEnv(w)
}

func checkWritablePath(path string) (why, fix string) {
	switch {
	case strings.TrimSpace(path) == "":
		return `"" is not a path`, `write the folder as path = "~/.cache/go-build"`
	case path != "~" && !strings.HasPrefix(path, "~/") && !filepath.IsAbs(path):
		return fmt.Sprintf("%q is not an absolute path", path), `write the folder as an absolute path, or one starting with ~/, such as "~/.cache/go-build"`
	}
	return "", ""
}

func checkWritableEnv(w Writable) (why, fix string) {
	switch env := w.Env; {
	case env == "" && !w.emptyEnv:
	case !envNamePattern.MatchString(env):
		return fmt.Sprintf("%q exports %q, which is not an environment variable name: letters, digits and \"_\", not starting with a digit", w.Path, env),
			`name the variable the folder's tools read, such as env = "GOCACHE", or remove env`
	case slices.Contains(reservedEnv, env):
		return fmt.Sprintf("%q exports %q, which a worker's session relies on as the daemon sets it; HOME, PATH and TMPDIR are not a folder's to set", w.Path, env),
			"name the variable the folder's tools read, or remove env"
	case slices.ContainsFunc(reservedEnvStart, func(prefix string) bool { return strings.HasPrefix(env, prefix) }):
		return fmt.Sprintf("%q exports %q, but names starting GIT_ or ASMAI_ are git's and AsmAI's own, such as GIT_DIR and ASMAI_SLOT", w.Path, env),
			"name the variable the folder's tools read, or remove env"
	}
	return "", ""
}

// checkWritableExports returns why list[i] exports a name an earlier folder
// of the list does, or "".
func checkWritableExports(list []Writable, i int) (why, fix string) {
	if list[i].Env == "" {
		return "", ""
	}
	for _, earlier := range list[:i] {
		if earlier.Env == list[i].Env {
			return fmt.Sprintf("%q exports %q, which %q already exports", list[i].Path, list[i].Env, earlier.Path),
				"give each folder its own variable, or remove the second folder"
		}
	}
	return "", ""
}

// Bounds are the places a writable folder may not open, which only the host
// can say: the home directory's ancestors, the user's keys and Claude Code's
// files, AsmAI's state and its clones (ADR 0016).
type Bounds struct {
	// Home is the user's home directory, and what ~ means.
	Home string
	// StateDir is AsmAI's state directory.
	StateDir string
	// Clones are AsmAI's clone of each repository, by name.
	Clones map[string]string
}

// NewBounds returns the bounds for a host whose home directory is home and
// whose state directory is stateDir, with repositories the directory of the
// clones, which are those of names.
func NewBounds(home, stateDir, repositories string, names []string) Bounds {
	clones := make(map[string]string, len(names))
	for _, name := range names {
		clones[name] = filepath.Join(repositories, name)
	}
	return Bounds{Home: home, StateDir: stateDir, Clones: clones}
}

// check returns why the folder w opens a place the bounds keep closed, and
// the fix, or "". A folder is compared as written and with its symbolic links
// followed, so that a link into a closed place is no way around it.
func (b Bounds) check(w Writable) (why, fix string) {
	folder, err := Resolve(b.Home, w.Path)
	if err != nil {
		return err.Error(), writableFix
	}
	folders := pathForms(folder)
	// Home is closed to its ancestors only: a folder inside it is what a
	// repository declares.
	home := pathForms(b.Home)
	if slices.ContainsFunc(folders, func(f string) bool {
		return slices.ContainsFunc(home, func(h string) bool { return within(f, h) })
	}) {
		return fmt.Sprintf("%q is your home directory or one of its ancestors, which a worker could then write whole", w.Path), writableFix
	}
	// holdable says a folder that holds the place is refused for that, not
	// for what else it holds.
	type closed struct {
		path, reason string
		holdable     bool
	}
	places := []closed{
		{filepath.Join(b.Home, ".ssh"), "~/.ssh, where your SSH keys are", true},
		{filepath.Join(b.Home, ".claude"), "~/.claude, where Claude Code keeps its settings", true},
		{filepath.Join(b.Home, ".claude.json"), "~/.claude.json, where Claude Code records your trust", true},
	}
	// A folder that holds a clone is inside the state directory or holds it
	// too, and is refused as that: a clone is named only for the folders that
	// are it or inside it.
	for _, name := range slices.Sorted(maps.Keys(b.Clones)) {
		places = append(places, closed{b.Clones[name], fmt.Sprintf("AsmAI's clone of %s, %s", name, b.Clones[name]), false})
	}
	places = append(places, closed{b.StateDir, fmt.Sprintf("AsmAI's state directory, %s, which holds its store and its socket", b.StateDir), true})
	for _, place := range places {
		if place.path == "" {
			continue
		}
		for _, p := range pathForms(place.path) {
			for _, f := range folders {
				switch {
				case f == p:
					return fmt.Sprintf("%q is %s", w.Path, place.reason), writableFix
				case within(p, f):
					return fmt.Sprintf("%q is inside %s", w.Path, place.reason), writableFix
				case within(f, p) && place.holdable:
					return fmt.Sprintf("%q holds %s", w.Path, place.reason), writableFix
				}
			}
		}
	}
	return "", ""
}

// within reports that path is dir or inside it.
func within(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// pathForms returns path cleaned and, when it or its nearest existing
// ancestor is reached through symbolic links, with those followed.
func pathForms(path string) []string {
	forms := []string{filepath.Clean(path)}
	rest := ""
	for dir := forms[0]; ; dir = filepath.Dir(dir) {
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			if followed := filepath.Join(real, rest); followed != forms[0] {
				forms = append(forms, followed)
			}
			break
		}
		if filepath.Dir(dir) == dir {
			break
		}
		rest = filepath.Join(filepath.Base(dir), rest)
	}
	return forms
}

// writableProblems returns the problems with list, the writable folders of
// the repository whose entry subject names, each problem naming the subject
// and the folder: the form of each, the variable names, and, with bounds, the
// places they open.
func writableProblems(subject string, list []Writable, bounds *Bounds) []Problem {
	var problems []Problem
	for i, w := range list {
		why, fix := checkWritable(w)
		if why == "" {
			why, fix = checkWritableExports(list, i)
		}
		if why == "" && bounds != nil {
			why, fix = bounds.check(w)
		}
		if why != "" {
			problems = append(problems, Problem{Problem: fmt.Sprintf("%s writable folder %s", subject, why), Fix: fix})
		}
	}
	return problems
}

// CheckWritable returns why the file would not honour list as the writable
// folders of the repository name, on the host bounds describes, or nil.
func CheckWritable(name string, list []Writable, bounds Bounds) error {
	if problems := writableProblems("["+tableOf(name)+"]", list, &bounds); len(problems) > 0 {
		return &EntryProblemsError{Problems: problems}
	}
	return nil
}

func tableOf(name string) string { return "repositories." + name }

// WritableProblems returns the problems with the writable folders of every
// repository in c on the host bounds describes: those a repository's entry
// has by itself, which Parse finds, are not repeated.
func (c Config) WritableProblems(bounds Bounds) []Problem {
	var problems []Problem
	for _, name := range slices.Sorted(maps.Keys(c.Options)) {
		for _, w := range c.Options[name].Writable {
			if why, _ := checkWritable(w); why != "" {
				continue
			}
			if why, fix := bounds.check(w); why != "" {
				problems = append(problems, Problem{Problem: fmt.Sprintf("[%s] writable folder %s", tableOf(name), why), Fix: fix})
			}
		}
	}
	return problems
}

// writableDraft is a [[repositories.<name>.writable]] table being read.
type writableDraft struct {
	repo           string
	line, pathLine int
	envLine        int
	w              Writable
	hasPath        bool
	// bad says a field of the table was refused, with its problem noted.
	bad bool
}

// writableFields are the fields of a [[repositories.<name>.writable]] table.
var writableFields = []string{"path", "env"}

// writableField sets the field name of the draft d to value, found on line,
// or notes why it cannot.
func (st *state) writableField(d *writableDraft, path []string, value *unstable.Node, line int) {
	header := "[[repositories." + d.repo + ".writable]]"
	name := strings.Join(path, ".")
	if !slices.Contains(writableFields, name) {
		st.problems = append(st.problems, Problem{Line: line,
			Problem: fmt.Sprintf("%s in %s is not a setting this asmai reads; the fields are %s", name, header, joinFieldNames(writableFields)),
			Fix:     fmt.Sprintf("remove %s, or correct its name", name)})
		return
	}
	if value.Kind != unstable.String {
		d.bad = true
		st.problems = append(st.problems, Problem{Line: line,
			Problem: fmt.Sprintf("%s in %s is %s, not a string", name, header, describe(value.Kind)),
			Fix:     fmt.Sprintf(`write %s as a string in double quotes, such as %s = "%s"`, name, name, map[string]string{"path": "~/.cache/go-build", "env": "GOCACHE"}[name])})
		return
	}
	switch name {
	case "path":
		d.w.Path, d.hasPath, d.pathLine = string(value.Data), true, line
	case "env":
		d.w.Env, d.envLine = string(value.Data), line
		d.w.emptyEnv = d.w.Env == ""
	}
}

// finishWritable checks the [[repositories.<name>.writable]] tables the file
// had, and adds the folders they declare to their repositories, in the
// order written.
func (st *state) finishWritable() {
	for _, d := range st.drafts {
		if _, ok := st.cfg.Repositories[d.repo]; !ok {
			st.problems = append(st.problems, Problem{Line: d.line,
				Problem: fmt.Sprintf("[[repositories.%s.writable]] has no [repositories.%s] entry", d.repo, d.repo),
				Fix:     fmt.Sprintf("add [repositories.%s] for this writable folder", d.repo)})
			continue
		}
		if d.bad {
			continue
		}
		table := "[" + tableOf(d.repo) + "]"
		if !d.hasPath {
			st.problems = append(st.problems, Problem{Line: d.line,
				Problem: fmt.Sprintf("[[repositories.%s.writable]] has no path", d.repo),
				Fix:     `add path = "~/.cache/go-build", the folder workers may write`})
			continue
		}
		options := st.cfg.Options[d.repo]
		problem := func(line int, why, fix string) {
			st.problems = append(st.problems, Problem{Line: line, Problem: fmt.Sprintf("%s writable folder %s", table, why), Fix: fix})
		}
		if why, fix := checkWritablePath(d.w.Path); why != "" {
			problem(d.pathLine, why, fix)
			continue
		}
		if why, fix := checkWritableEnv(d.w); why != "" {
			problem(d.envLine, why, fix)
			continue
		}
		options.Writable = append(options.Writable, d.w)
		if why, fix := checkWritableExports(options.Writable, len(options.Writable)-1); why != "" {
			problem(d.envLine, why, fix)
			continue
		}
		st.cfg.Options[d.repo] = options
	}
}
