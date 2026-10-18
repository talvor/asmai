// SPDX-License-Identifier: Apache-2.0

package config

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
	"github.com/pelletier/go-toml/v2/unstable"

	"github.com/talvor/asmai/internal/roles"
)

// ErrRepositoryEntryExists is returned by AddRepository for a name the file
// already has an entry for.
var ErrRepositoryEntryExists = errors.New("the configuration file already has an entry for that repository")

// ErrRepositoryEntryNotATable is returned by RemoveRepository for an entry
// that is not written as a plain [repositories.<name>] table, which only the
// user can remove without disturbing the rest of the file.
var ErrRepositoryEntryNotATable = errors.New("the configuration file has the entry, but not as a [repositories.<name>] table")

// ProblemsError is returned for a file that has problems, which are fixed
// before a command edits it.
type ProblemsError struct {
	Path     string
	Problems []Problem
}

func (e *ProblemsError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "the configuration file %s has problems, which must be fixed first:", e.Path)
	for _, p := range e.Problems {
		fmt.Fprintf(&b, "\n  %s; fix: %s", p, p.Fix)
	}
	return b.String()
}

// repositoryEntry is a [repositories.<name>] table as AddRepository writes
// it: the fields `asmai repo add` records, and each setting that is not at
// its default.
type repositoryEntry struct {
	Location         string   `toml:"location"`
	Origin           string   `toml:"origin"`
	DefaultBranch    string   `toml:"default_branch"`
	Notes            string   `toml:"notes,omitempty"`
	ChecksOneAtATime bool     `toml:"checks_one_at_a_time,omitempty"`
	NoCI             bool     `toml:"no_ci,omitempty"`
	CommitTrailers   *bool    `toml:"commit_trailers,omitempty"`
	RepositorySkills []string `toml:"repository_skills,omitempty"`
}

// AddRepository writes the entry [repositories.<name>] for r at the end of
// the configuration file at path, creating the file if it is missing. The
// rest of the file, comments and all, stays as it is. It refuses a file with
// problems, and a name the file already has an entry for.
func AddRepository(path, name string, r Repository) error {
	return AddRepositoryWithOptions(path, name, r, DefaultRepositoryOptions())
}

// AddRepositoryWithOptions is AddRepository for an entry that also sets o:
// each setting that is not at its default is written beside the fields the
// entry records. It refuses options the file would not honour.
func AddRepositoryWithOptions(path, name string, r Repository, o RepositoryOptions) error {
	if err := CheckRepositoryName(name); err != nil {
		return err
	}
	data, mode, err := read(path)
	if err != nil {
		return err
	}
	cfg, problems := Parse(data)
	if len(problems) > 0 {
		return &ProblemsError{Path: path, Problems: problems}
	}
	if _, ok := cfg.Repositories[name]; ok {
		return ErrRepositoryEntryExists
	}
	entry := repositoryEntry{
		Location: r.Location, Origin: r.Origin, DefaultBranch: r.DefaultBranch,
		Notes: o.Notes, ChecksOneAtATime: o.ChecksOneAtATime, NoCI: o.NoCI, RepositorySkills: o.RepositorySkills,
	}
	if !o.CommitTrailers {
		entry.CommitTrailers = &o.CommitTrailers
	}
	encoded, err := toml.Marshal(entry)
	if err != nil {
		return err
	}
	var b bytes.Buffer
	b.Write(data)
	if len(data) > 0 {
		if !bytes.HasSuffix(data, []byte("\n")) {
			b.WriteByte('\n')
		}
		if !bytes.HasSuffix(data, []byte("\n\n")) {
			b.WriteByte('\n')
		}
	}
	fmt.Fprintf(&b, "[repositories.%s]\n", name)
	b.Write(encoded)
	// Each writable folder is a table of its own, after the entry's fields.
	for _, w := range o.Writable {
		fmt.Fprintf(&b, "\n[[repositories.%s.writable]]\npath = %s\n", name, quote(w.Path))
		if w.Env != "" {
			fmt.Fprintf(&b, "env = %s\n", quote(w.Env))
		}
	}
	// What is written is what was meant.
	written, problems := Parse(b.Bytes())
	if len(problems) > 0 {
		return &EntryProblemsError{Problems: problems}
	}
	if written.Repositories[name] != r || !sameOptions(written.Options[name], o) {
		return fmt.Errorf("writing the entry for %s would not read back as it was given (%+v %+v)", name, written.Repositories[name], written.Options[name])
	}
	return write(path, b.Bytes(), mode)
}

// EntryProblemsError is returned by AddRepositoryWithOptions for an entry
// the file would not honour, such as a repository skill that names no role.
type EntryProblemsError struct {
	Problems []Problem
}

func (e *EntryProblemsError) Error() string {
	var b strings.Builder
	b.WriteString("the repository's settings are not ones this asmai honours:")
	for _, p := range e.Problems {
		fmt.Fprintf(&b, "\n  %s; fix: %s", p.Problem, p.Fix)
	}
	return b.String()
}

func sameOptions(a, b RepositoryOptions) bool {
	return a.Notes == b.Notes && a.ChecksOneAtATime == b.ChecksOneAtATime && a.NoCI == b.NoCI &&
		a.CommitTrailers == b.CommitTrailers && slices.Equal(a.RepositorySkills, b.RepositorySkills) &&
		slices.Equal(a.Writable, b.Writable)
}

// CheckRepositoryOptions returns why the file would not honour o as a
// repository's settings, or nil. It checks the form of its writable folders,
// not the places they open, which only CheckWritable on the host knows.
func CheckRepositoryOptions(o RepositoryOptions) error {
	var problems []Problem
	for _, s := range optionSettings(&RepositoryOptions{}) {
		if s.name != "repository_skills" {
			continue
		}
		if why, fix := s.check("", o.RepositorySkills); why != "" {
			problems = append(problems, Problem{Problem: "repository_skills " + why, Fix: fix})
		}
	}
	problems = append(problems, writableProblems("the repository's", o.Writable, nil)...)
	if len(problems) > 0 {
		return &EntryProblemsError{Problems: problems}
	}
	return nil
}

// ErrRoleNotATable is returned by SetStaffing for a role that is not
// written as a plain [roles.<role>] table, which only the user can change
// without disturbing the rest of the file.
var ErrRoleNotATable = errors.New("the configuration file has the role, but not as a [roles.<role>] table")

// SetStaffing sets the staffing fields of role in the configuration file at
// path, each by its name (leader_provider, leader_model, worker_provider or
// worker_model): a field the role's table has is changed in place, a field
// it lacks is added right under the table's header, and a role with no table
// gets one at the end of the file. The rest of the file, comments and all,
// stays as it is. It refuses a file with problems, a provider this asmai
// does not run and an empty model, and a role not written as a table.
func SetStaffing(path, role string, fields map[string]string) error {
	if !roles.Known(role) {
		return fmt.Errorf("%q is no role; the roles are %s", role, strings.Join(roles.All, ", "))
	}
	var probe Staffing
	for _, name := range slices.Sorted(maps.Keys(fields)) {
		if probe.field(name) == nil {
			return fmt.Errorf("%q is not a staffing field", name)
		}
		value := fields[name]
		if strings.HasSuffix(name, "_provider") && value != Claude {
			if value == "codex" {
				return fmt.Errorf("%s cannot be %q: Codex arrives in a later asmai; this asmai runs only %q", name, value, Claude)
			}
			return fmt.Errorf("%s cannot be %q: this asmai runs only %q", name, value, Claude)
		}
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s cannot be empty", name)
		}
	}
	data, mode, err := read(path)
	if err != nil {
		return err
	}
	cfg, problems := Parse(data)
	if len(problems) > 0 {
		return &ProblemsError{Path: path, Problems: problems}
	}
	var out []byte
	if _, ok := cfg.Roles[role]; !ok {
		var b bytes.Buffer
		b.Write(data)
		if len(data) > 0 {
			if !bytes.HasSuffix(data, []byte("\n")) {
				b.WriteByte('\n')
			}
			if !bytes.HasSuffix(data, []byte("\n\n")) {
				b.WriteByte('\n')
			}
		}
		fmt.Fprintf(&b, "[roles.%s]\n", role)
		for _, name := range staffingFields {
			if value, ok := fields[name]; ok {
				fmt.Fprintf(&b, "%s = %s\n", name, quote(value))
			}
		}
		out = b.Bytes()
	} else if out, err = setInTable(data, role, fields); err != nil {
		return err
	}
	written, problems := Parse(out)
	if len(problems) > 0 {
		return fmt.Errorf("the change would leave the file with problems (%v); nothing was written", problems[0])
	}
	got := written.Roles[role]
	for name, value := range fields {
		if *got.field(name) != value {
			return fmt.Errorf("writing %s for %s would not read back as it was given", name, role)
		}
	}
	return write(path, out, mode)
}

// setInTable returns data with fields set in the [roles.<role>] table.
func setInTable(data []byte, role string, fields map[string]string) ([]byte, error) {
	type change struct {
		offset, length int
		text           string
	}
	var changes []change
	replaced := map[string]bool{}
	header := 0
	inTable := false
	p := &unstable.Parser{}
	p.Reset(data)
	for p.NextExpression() {
		e := p.Expression()
		switch e.Kind {
		case unstable.Table, unstable.ArrayTable:
			inTable = e.Kind == unstable.Table && slices.Equal(keyPath(e.Key()), []string{"roles", role})
			if inTable {
				header = keyLine(p, e.Key())
			}
		case unstable.KeyValue:
			if !inTable {
				continue
			}
			path := keyPath(e.Key())
			if len(path) != 1 {
				return nil, ErrRoleNotATable
			}
			value, ok := fields[path[0]]
			if !ok {
				continue
			}
			node := e.Value()
			if node.Kind != unstable.String || node.Raw.Length == 0 {
				return nil, ErrRoleNotATable
			}
			changes = append(changes, change{int(node.Raw.Offset), int(node.Raw.Length), quote(value)})
			replaced[path[0]] = true
		}
	}
	if header == 0 {
		return nil, ErrRoleNotATable
	}
	// The fields the table lacks go right under its header.
	var added strings.Builder
	for _, name := range staffingFields {
		if value, ok := fields[name]; ok && !replaced[name] {
			fmt.Fprintf(&added, "%s = %s\n", name, quote(value))
		}
	}
	if added.Len() > 0 {
		at := len(data)
		for line, off := 1, 0; off < len(data); off++ {
			if data[off] != '\n' {
				continue
			}
			if line == header {
				at = off + 1
				break
			}
			line++
		}
		text := added.String()
		if at == len(data) && !bytes.HasSuffix(data, []byte("\n")) {
			text = "\n" + text
		}
		changes = append(changes, change{at, 0, text})
	}
	// Edit from the end, so that earlier offsets stay right.
	slices.SortFunc(changes, func(a, b change) int { return b.offset - a.offset })
	out := slices.Clone(data)
	for _, c := range changes {
		out = slices.Concat(out[:c.offset], []byte(c.text), out[c.offset+c.length:])
	}
	return out, nil
}

// RemoveRepository removes the entry [repositories.<name>] from the
// configuration file at path, with its [[repositories.<name>.writable]]
// tables, and reports whether the file had it. The rest of the file stays as
// it is. It refuses a file with problems, and an entry not written as a
// [repositories.<name>] table.
func RemoveRepository(path, name string) (removed bool, err error) {
	data, mode, err := read(path)
	if err != nil {
		return false, err
	}
	cfg, problems := Parse(data)
	if len(problems) > 0 {
		return false, &ProblemsError{Path: path, Problems: problems}
	}
	if _, ok := cfg.Repositories[name]; !ok {
		return false, nil
	}
	// The entry is its table and its writable folders' tables, which can lie
	// apart: take them out one at a time, each from the file as it then is.
	text, found := string(data), false
	for {
		lines := strings.SplitAfter(text, "\n")
		start, next := entryRegion(data, lines, name)
		if start < 0 {
			break
		}
		found = true
		text = strings.Join(removeRegion(lines, start, next), "")
		data = []byte(text)
	}
	if !found {
		return false, ErrRepositoryEntryNotATable
	}
	if after, _ := Parse(data); len(after.Repositories) != len(cfg.Repositories)-1 {
		return false, ErrRepositoryEntryNotATable
	}
	return true, write(path, data, mode)
}

// entryRegion returns the first line of the first table of the entry name in
// data, and the line of the table after it, or the number of lines; with no
// such table, a start of -1. Lines count from 0. lines is data split after
// each line.
func entryRegion(data []byte, lines []string, name string) (start, next int) {
	start, next = -1, len(lines)
	p := &unstable.Parser{}
	p.Reset(data)
	for p.NextExpression() {
		e := p.Expression()
		if e.Kind != unstable.Table && e.Kind != unstable.ArrayTable {
			continue
		}
		line := keyLine(p, e.Key()) - 1
		if start >= 0 {
			return start, line
		}
		key := keyPath(e.Key())
		if e.Kind == unstable.Table && slices.Equal(key, []string{"repositories", name}) ||
			e.Kind == unstable.ArrayTable && slices.Equal(key, []string{"repositories", name, "writable"}) {
			start = line
		}
	}
	return start, len(lines)
}

// removeRegion returns lines without the table that starts on line start and
// runs to line next, with the comment lines right above it.
func removeRegion(lines []string, start, next int) []string {
	// The comment lines right above the table go with it.
	for start > 0 && strings.HasPrefix(strings.TrimSpace(lines[start-1]), "#") {
		start--
	}
	// The comments and blank lines just above the next table are the next
	// table's, not this one's.
	end := next
	for end > start+1 && isBlankOrComment(lines[end-1]) {
		end--
	}
	if next == len(lines) {
		end = len(lines)
	}
	kept := slices.Concat(lines[:start], lines[end:])
	// Leave one blank line, not two or none, where the entry was.
	if start > 0 && start < len(kept) && isBlank(kept[start-1]) && isBlank(kept[start]) {
		kept = slices.Delete(kept, start, start+1)
	}
	if end == len(lines) {
		for len(kept) > 0 && isBlank(kept[len(kept)-1]) {
			kept = kept[:len(kept)-1]
		}
	}
	if start == 0 {
		for len(kept) > 0 && isBlank(kept[0]) {
			kept = kept[1:]
		}
	}
	return kept
}

// Replace replaces the configuration file at path with data, as a file
// renamed over it, keeping its mode, so that a crash leaves the old file or
// the new. It checks nothing: callers check data first.
func Replace(path string, data []byte) error {
	_, mode, err := read(path)
	if err != nil {
		return err
	}
	if mode == 0o644 {
		if _, statErr := os.Stat(path); statErr != nil {
			mode = 0o600
		}
	}
	return write(path, data, mode)
}

func isBlank(line string) bool { return strings.TrimSpace(line) == "" }

func isBlankOrComment(line string) bool {
	trimmed := strings.TrimSpace(line)
	return trimmed == "" || strings.HasPrefix(trimmed, "#")
}

// read returns the file at path, empty when it is missing, and the mode a
// rewrite keeps.
func read(path string) (data []byte, mode os.FileMode, err error) {
	data, err = os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0o644, nil
	}
	if err != nil {
		return nil, 0, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, 0, err
	}
	return data, info.Mode().Perm(), nil
}

// write replaces the file at path with data, through a file renamed over it
// so that a crash leaves the old file or the new. A file that is a symbolic
// link, as dotfiles managers make it, stays one: its target is replaced.
func write(path string, data []byte, mode os.FileMode) error {
	if target, err := filepath.EvalSymlinks(path); err == nil {
		path = target
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".config-*.toml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
