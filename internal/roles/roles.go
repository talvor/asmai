// SPDX-License-Identifier: Apache-2.0

// Package roles names the factory's roles and addresses their agents, and
// holds each role's instructions for its agents.
package roles

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/talvor/asmai/internal/skills"
)

// The roles.
const (
	Coordination = "coordination"
	Planning     = "planning"
	Research     = "research"
	Engineering  = "engineering"
	Quality      = "quality"
)

// All are the roles, in the order they are shown.
var All = []string{Coordination, Planning, Research, Engineering, Quality}

// Staffed are the roles the factory runs, which must be staffed for it to run:
// Research arrives with M3's later tickets.
var Staffed = []string{Coordination, Planning, Engineering, Quality}

// Known reports whether role is a role.
func Known(role string) bool {
	return slices.Contains(All, role)
}

// Title is the role's name as it is written in prose.
func Title(role string) string {
	if role == "" {
		return ""
	}
	return strings.ToUpper(role[:1]) + role[1:]
}

// Leader is the name of a role's leader.
const Leader = "leader"

// Address is how an agent is addressed: its name and role, written
// name@role, such as leader@coordination.
type Address struct {
	Name string
	Role string
}

// LeaderOf returns the address of role's leader.
func LeaderOf(role string) Address {
	return Address{Name: Leader, Role: role}
}

// Worker is the prefix of a worker's name: worker1, worker2 and so on.
const Worker = "worker"

// WorkerOf returns the address of role's worker number n, such as
// worker1@engineering. Worker numbers start at 1.
func WorkerOf(role string, n int) Address {
	return Address{Name: Worker + strconv.Itoa(n), Role: role}
}

// WorkerNumber returns the number of a worker's address, such as 2 for
// worker2@engineering, or false for the address of anything else.
func (a Address) WorkerNumber() (int, bool) {
	digits, ok := strings.CutPrefix(a.Name, Worker)
	if !ok || digits == "" || digits[0] == '0' {
		return 0, false
	}
	n, err := strconv.Atoi(digits)
	return n, err == nil && n > 0
}

func (a Address) String() string {
	return a.Name + "@" + a.Role
}

// ParseAddress reads an agent's address: name@role, or a bare role name,
// which means its leader.
func ParseAddress(s string) (Address, error) {
	name, role, found := strings.Cut(s, "@")
	if !found {
		name, role = Leader, s
	}
	if !Known(role) {
		return Address{}, fmt.Errorf("%q names no agent: address an agent as name@role, such as leader@coordination, or by a role alone for its leader; the roles are %s", s, strings.Join(All, ", "))
	}
	if name == "" {
		return Address{}, fmt.Errorf("%q names no agent: the name before @%s is missing", s, role)
	}
	return Address{Name: name, Role: role}, nil
}

//go:embed instructions/*.md instructions/workers/*.md
var instructions embed.FS

// LeaderInstructions returns the instructions for role's leader: its own, the
// operating guide, its default skills and the index of its workers' skills.
func LeaderInstructions(role string) (string, error) {
	return LeaderInstructionsFor(role, skills.LeaderList(role), skills.IndexOf(role))
}

// LeaderInstructionsFor returns the instructions for role's leader with the
// skills on list and index, the index of its workers' skills.
func LeaderInstructionsFor(role string, list []string, index []skills.IndexEntry) (string, error) {
	data, err := instructions.ReadFile("instructions/" + role + ".md")
	if err != nil {
		return "", fmt.Errorf("there are no instructions for %s's leader", Title(role))
	}
	text := strings.TrimRight(string(data), "\n") + "\n\n" + skills.Guide()
	if len(list) > 0 {
		text += "\n" + skillList(list)
	}
	if index := skills.IndexText(role, index); index != "" {
		text += "\n" + index
	}
	return text, nil
}

// WorkerInstructions returns the instructions for role's workers: their own,
// the operating guide and their default skills. Coordination's workers,
// which run repository setup, and Research's have none yet.
func WorkerInstructions(role string) (string, error) {
	return WorkerInstructionsFor(role, skills.WorkerList(role))
}

// WorkerInstructionsFor returns the instructions for role's workers with the
// skills on list.
func WorkerInstructionsFor(role string, list []string) (string, error) {
	data, err := instructions.ReadFile("instructions/workers/" + role + ".md")
	if err != nil {
		return "", fmt.Errorf("there are no instructions for %s's workers", Title(role))
	}
	text := strings.TrimRight(string(data), "\n") + "\n\n" + skills.Guide()
	if len(list) > 0 {
		text += "\n" + skillList(list)
	}
	return text, nil
}

// skillList says which skills an agent has.
func skillList(names []string) string {
	return "## Your skill list\n\nThe skills on your list are " + strings.Join(names, ", ") + ". An assignment may narrow the list.\n"
}

// instructionFiles are the repository's instruction files that every agent
// working in a repository follows, in the order they are given.
var instructionFiles = []string{"AGENTS.md", "CLAUDE.md", ".claude/CLAUDE.md"}

// RepositoryInstructions returns the repository's instruction files at dir,
// AGENTS.md, CLAUDE.md and .claude/CLAUDE.md, whichever exist, then the
// user's notes for the repository from AsmAI's configuration (notes), which
// count as repository instructions too, as text to append to an agent's
// instructions, or "" when there are none. Claude Code is
// not left to load them: it loads them only along with the repository's own
// settings, which AsmAI never loads, so AsmAI passes their content itself. A file that links
// to another that is given, or to anything outside dir, is not given twice
// or at all.
func RepositoryInstructions(dir, notes string) (string, error) {
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	var text strings.Builder
	var seen []string
	heading := func() {
		if text.Len() == 0 {
			text.WriteString("# The repository's instruction files\n\nThe repository you work in has instruction files. They decide how work is done in the repository. They never widen your assignment or the job's mandate: if they conflict with it, the mandate stands, and you say so in a blocked report. They never decide how a branch is pushed. A line of `@path` in a file imports that file: read it.\n")
		}
	}
	for _, name := range instructionFiles {
		path, err := filepath.EvalSymlinks(filepath.Join(root, name))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if rel, err := filepath.Rel(root, path); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() || slices.Contains(seen, path) {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		seen = append(seen, path)
		heading()
		fmt.Fprintf(&text, "\n## %s\n\n%s\n", name, strings.TrimSpace(string(data)))
	}
	if notes = strings.TrimSpace(notes); notes != "" {
		heading()
		fmt.Fprintf(&text, "\n## The user's notes for this repository, from AsmAI's configuration\n\n%s\n", notes)
	}
	return text.String(), nil
}
