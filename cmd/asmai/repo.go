// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/statedir"
	"github.com/talvor/asmai/internal/store"
)

// repoAdd registers the repository source names, as name when the user chose
// one. The daemon makes AsmAI's clone, writes the configuration entry and
// records it. A source that is a path on this host is given to the daemon
// absolute, because the daemon does not run where this command does.
func repoAdd(o *output, paths statedir.Paths, source, name string) int {
	if _, err := os.Stat(source); err == nil {
		if source, err = filepath.Abs(source); err != nil {
			return o.fail(err)
		}
	}
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandRepoAdd, Source: source, Repository: name})
	if err != nil {
		return o.fail(err)
	}
	if resp.Repository == nil {
		return o.fail(fmt.Errorf("the daemon did not answer with the repository it registered"))
	}
	r := *resp.Repository
	if o.json {
		return o.printJSON(map[string]any{"registered": true, "repository": r})
	}
	fmt.Fprintf(o.stdout, "Registered %s, and made AsmAI's clone of it from its origin.\n", r.Name)
	o.table(repositoryRows(r))
	if r.Location != "" {
		fmt.Fprintf(o.stdout, "Your own checkout is recorded but never used for work.\n")
	}
	return 0
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
	if o.json {
		return o.printJSON(map[string]any{"repositories": list})
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
	if o.json {
		return o.printJSON(map[string]any{"repository": resp.Repository})
	}
	o.table(append(repositoryRows(*resp.Repository), []string{"registered", resp.Repository.AddedAt.Local().Format(time.RFC3339)}))
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
	if o.json {
		return o.printJSON(map[string]any{"removed": true, "repository": resp.Repository})
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
