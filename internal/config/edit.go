// SPDX-License-Identifier: Apache-2.0

package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
	"github.com/pelletier/go-toml/v2/unstable"
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

// AddRepository writes the entry [repositories.<name>] for r at the end of
// the configuration file at path, creating the file if it is missing. The
// rest of the file, comments and all, stays as it is. It refuses a file with
// problems, and a name the file already has an entry for.
func AddRepository(path, name string, r Repository) error {
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
	entry, err := toml.Marshal(r)
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
	b.Write(entry)
	// What is written is what was meant.
	if written, problems := Parse(b.Bytes()); len(problems) > 0 || written.Repositories[name] != r {
		return fmt.Errorf("writing the entry for %s would not read back as it was given (%+v, %v)", name, written.Repositories[name], problems)
	}
	return write(path, b.Bytes(), mode)
}

// RemoveRepository removes the entry [repositories.<name>] from the
// configuration file at path, and reports whether the file had it. The rest
// of the file stays as it is. It refuses a file with problems, and an entry
// not written as a [repositories.<name>] table.
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
	lines := strings.SplitAfter(string(data), "\n")
	start, next := -1, len(lines)
	p := &unstable.Parser{}
	p.Reset(data)
	for p.NextExpression() {
		e := p.Expression()
		if e.Kind != unstable.Table && e.Kind != unstable.ArrayTable {
			continue
		}
		line := keyLine(p, e.Key()) - 1
		if start >= 0 {
			next = line
			break
		}
		if e.Kind == unstable.Table && slices.Equal(keyPath(e.Key()), []string{"repositories", name}) {
			start = line
		}
	}
	if start < 0 {
		return false, ErrRepositoryEntryNotATable
	}
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
	return true, write(path, []byte(strings.Join(kept, "")), mode)
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
