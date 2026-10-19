// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/talvor/asmai/internal/config"
	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/providers"
	"github.com/talvor/asmai/internal/statedir"
	"github.com/talvor/asmai/internal/store"
)

// repoAdd registers the repository source names, as name when the user chose
// one. The daemon makes AsmAI's clone, writes the configuration entry and
// records it. Then, as `asmai init` does, repo add has the user trust the
// clone, in which the pinned Claude Code asks first (ADR 0011): workers
// start in worktrees of it. With no terminal, or no pinned Claude Code yet,
// it opens none, and says to run `asmai init` in a terminal; the
// repository is registered all the same. A source that is a path on this
// host is given to the daemon absolute, because the daemon does not run
// where this command does.
func repoAdd(o *output, paths statedir.Paths, source, name string, settings repoSettings, stdin io.Reader) int {
	if _, err := os.Stat(source); err == nil {
		if source, err = filepath.Abs(source); err != nil {
			return o.fail(err)
		}
	}
	options, err := settings.options()
	if err != nil {
		return o.fail(err)
	}
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandRepoAdd, Source: source, Repository: name, DefaultBranch: settings.defaultBranch, Options: &options})
	if err != nil {
		return o.fail(err)
	}
	if resp.Repository == nil {
		return o.fail(fmt.Errorf("the daemon did not answer with the repository it registered"))
	}
	r := *resp.Repository
	// With --json or --toon, stdout carries only the result, and Claude Code draws on
	// stderr.
	w := o.stdout
	if o.structured() {
		w = o.stderr
	} else {
		fmt.Fprintf(w, "Registered %s, and made AsmAI's clone of it from its origin.\n", r.Name)
		o.table(append(repositoryRows(r), writableRows(resp.Writable)...))
		if r.Location != "" {
			fmt.Fprintf(w, "Your own checkout is recorded but never used for work.\n")
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return o.fail(err)
	}
	record := providers.ClaudeCodeRecord(home, os.Getenv)
	plan, err := pinnedPlan(paths)
	if err != nil {
		return o.fail(err)
	}
	untrusted := fmt.Sprintf("%s does not trust the clone yet, so no worker can start in a workspace of %s", plan.Title, r.Name)
	if trusted, err := providers.Trusted(record, r.Clone); err != nil {
		return o.fail(err)
	} else if !trusted && !exists(plan.Path) {
		fmt.Fprintf(w, "%s, and the pinned %s is not installed yet: run `asmai init` in a terminal, which installs it and opens it in %s for you to trust it.\n", untrusted, plan.Title, r.Clone)
		return repoAdded(o, r, resp.Writable)
	}
	step := "trust " + trustStep(paths, r.Clone)
	state, detail, err := trustFolder(w, o.stderr, stdin, plan, record, r.Clone, true, "repo add", false)
	switch {
	case errors.Is(err, errNoTerminal):
		fmt.Fprintf(w, "%s, and trusting it needs you at a terminal: run `asmai init` in a terminal, which opens %s in %s for you to trust it.\n", untrusted, plan.Title, r.Clone)
		return repoAdded(o, r, resp.Writable)
	case state != "":
		fmt.Fprintf(w, "%s: %s. %s\n", step, state, detail)
	}
	if err != nil {
		return o.fail(err)
	}
	return repoAdded(o, r, resp.Writable)
}

type repoSettings struct {
	defaultBranch    string
	notes            string
	checksOneAtATime bool
	noCI             bool
	commitTrailers   bool
	skills           listFlags
	writable         listFlags
}

// options returns the entry's settings the flags give. With no --writable,
// the daemon suggests the folders for the repository's language.
func (s repoSettings) options() (config.RepositoryOptions, error) {
	o := config.RepositoryOptions{
		Notes: s.notes, ChecksOneAtATime: s.checksOneAtATime, NoCI: s.noCI,
		CommitTrailers: s.commitTrailers, RepositorySkills: []string(s.skills),
	}
	for _, text := range s.writable {
		w, err := config.ParseWritable(text)
		if err != nil {
			return config.RepositoryOptions{}, fmt.Errorf("--writable: %w", err)
		}
		o.Writable = append(o.Writable, w)
	}
	return o, nil
}

// repoAdded reports, with --json or --toon, that r is registered, and the folders its
// workers may write.
func repoAdded(o *output, r store.Repository, writable []config.Folder) int {
	if o.structured() {
		if writable == nil {
			writable = []config.Folder{}
		}
		return o.printData(map[string]any{"registered": true, "repository": r, "writable": writable})
	}
	return 0
}

// writableRows are the rows of the folders a repository's workers may write
// outside their workspaces.
func writableRows(folders []config.Folder) [][]string {
	var rows [][]string
	for _, f := range folders {
		text := f.Resolved
		if f.Env != "" {
			text += " (exported as " + f.Env + ")"
		}
		rows = append(rows, []string{"writable", text})
	}
	return rows
}

func repoList(o *output, paths statedir.Paths) int {
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandRepoList})
	if err != nil {
		return o.fail(err)
	}
	list := resp.Repositories
	if list == nil {
		list = []store.Repository{}
	}
	if o.structured() {
		return o.printData(map[string]any{"repositories": list})
	}
	if len(list) == 0 {
		fmt.Fprintln(o.stdout, "No repository is registered; register one with `asmai repo add <path-or-url>`.")
		return 0
	}
	rows := [][]string{{"NAME", "ORIGIN", "DEFAULT BRANCH", "LOCATION", "CLONE"}}
	for _, r := range list {
		rows = append(rows, []string{r.Name, r.Origin, r.DefaultBranch, locationText(r), r.Clone})
	}
	o.table(rows)
	return 0
}

func repoShow(o *output, paths statedir.Paths, name string) int {
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandRepoShow, Repository: name})
	if err != nil {
		return o.fail(err)
	}
	if resp.Repository == nil {
		return o.fail(fmt.Errorf("the daemon did not answer with the repository"))
	}
	if o.structured() {
		writable := resp.Writable
		if writable == nil {
			writable = []config.Folder{}
		}
		return o.printData(map[string]any{"repository": resp.Repository, "writable": writable})
	}
	rows := append(repositoryRows(*resp.Repository), writableRows(resp.Writable)...)
	o.table(append(rows, []string{"registered", resp.Repository.AddedAt.Local().Format(time.RFC3339)}))
	return 0
}

func repoRemove(o *output, paths statedir.Paths, name string) int {
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandRepoRemove, Repository: name})
	if err != nil {
		return o.fail(err)
	}
	if resp.Repository == nil {
		return o.fail(fmt.Errorf("the daemon did not answer with the repository it removed"))
	}
	if o.structured() {
		return o.printData(map[string]any{"removed": true, "repository": resp.Repository})
	}
	fmt.Fprintf(o.stdout, "Removed %s: its record, its entry in the configuration file and AsmAI's clone. Your own checkout was not touched.\n", resp.Repository.Name)
	return 0
}

func repositoryRows(r store.Repository) [][]string {
	return [][]string{
		{"name", r.Name},
		{"location", locationText(r)},
		{"origin", r.Origin},
		{"default branch", r.DefaultBranch},
		{"clone", r.Clone},
	}
}

// locationText is where the user's checkout is, or that there is none.
func locationText(r store.Repository) string {
	if r.Location == "" {
		return "-"
	}
	return r.Location
}
