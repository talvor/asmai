// SPDX-License-Identifier: Apache-2.0

package config

import (
	"slices"
	"strings"

	"github.com/talvor/asmai/internal/roles"
)

// Change is one difference between two configurations, as `asmai config
// apply` shows it.
type Change struct {
	// Setting is the setting's dotted name, such as
	// roles.engineering.worker_model, or the table's, such as
	// repositories.otman, for a table that was added or removed.
	Setting string `json:"setting"`
	// Before and After are the values as the file writes them; empty for a
	// setting or a table that was not there.
	Before string `json:"before"`
	After  string `json:"after"`
	// Table is the table that was added or removed, with Added or Removed
	// saying which.
	Added   bool `json:"added,omitempty"`
	Removed bool `json:"removed,omitempty"`
	// Role and Side say which agents a setting of a role is for: its leader
	// ("leader"), or its workers ("worker"). Both are empty for a setting
	// that is no role's.
	Role string `json:"role,omitempty"`
	Side string `json:"side,omitempty"`
	// Skills says the setting is a skill list, shipped or added, and Gained
	// and Lost are the skills it now has and no longer has.
	Skills bool     `json:"skills,omitempty"`
	Gained []string `json:"gained,omitempty"`
	Lost   []string `json:"lost,omitempty"`
	// Repository and Roles identify the repository and roles whose workers
	// are affected by this setting.
	Repository string   `json:"repository,omitempty"`
	Roles      []string `json:"roles,omitempty"`
}

// Diff returns what changes from old to new, in the order the file is
// documented: a role or repository added or removed is one change, and a
// setting of a table both have is one change when its value differs.
func Diff(old, new Config) []Change {
	before, after := Entries(old), Entries(new)
	oldBy, newBy := map[string]Entry{}, map[string]Entry{}
	for _, e := range before {
		oldBy[e.Table+"."+e.Name] = e
	}
	for _, e := range after {
		newBy[e.Table+"."+e.Name] = e
	}
	oldTables, newTables := tableSet(before), tableSet(after)
	// A role or a repository is a table of its own, which is added or
	// removed whole.
	entity := func(table string) bool {
		return strings.HasPrefix(table, "roles.") || strings.HasPrefix(table, "repositories.")
	}

	var changes []Change
	reported := map[string]bool{}
	tableChange := func(table string, added bool) Change {
		c := Change{Setting: table, Added: added, Removed: !added}
		c.Role = strings.TrimPrefix(table, "roles.")
		if c.Role == table {
			c.Role = ""
		}
		return c
	}
	for _, e := range after {
		if entity(e.Table) && !oldTables[e.Table] {
			if !reported[e.Table] {
				reported[e.Table] = true
				changes = append(changes, tableChange(e.Table, true))
			}
			if e.IsList && len(e.List) > 0 && (strings.HasPrefix(e.Table, "roles.") || e.Name == "repository_skills") {
				changes = append(changes, change(e.Table, e.Name, Entry{}, e))
			}
			continue
		}
		o, ok := oldBy[e.Table+"."+e.Name]
		if ok && o.Text == e.Text {
			continue
		}
		changes = append(changes, change(e.Table, e.Name, o, e))
	}
	for _, e := range before {
		if entity(e.Table) && !newTables[e.Table] {
			if !reported[e.Table] {
				reported[e.Table] = true
				changes = append(changes, tableChange(e.Table, false))
			}
			if e.IsList && len(e.List) > 0 && (strings.HasPrefix(e.Table, "roles.") || e.Name == "repository_skills") {
				changes = append(changes, change(e.Table, e.Name, e, Entry{}))
			}
			continue
		}
		if _, ok := newBy[e.Table+"."+e.Name]; !ok {
			changes = append(changes, change(e.Table, e.Name, e, Entry{}))
		}
	}
	return changes
}

func tableSet(entries []Entry) map[string]bool {
	m := map[string]bool{}
	for _, e := range entries {
		m[e.Table] = true
	}
	return m
}

func change(table, name string, from, to Entry) Change {
	c := Change{Setting: table + "." + name, Before: from.Text, After: to.Text}
	switch {
	case strings.HasPrefix(table, "roles."):
		c.Role = strings.TrimPrefix(table, "roles.")
		c.Side = "worker"
		if strings.HasPrefix(name, "leader_") {
			c.Side = "leader"
		}
		if strings.HasSuffix(name, "skills") {
			c.Skills = true
			c.Gained, c.Lost = listDifference(from.List, to.List)
		}
	case strings.HasPrefix(table, "repositories.") && (name == "notes" || name == "commit_trailers"):
		c.Repository = strings.TrimPrefix(table, "repositories.")
		c.Roles = slices.Clone(roles.All)
	case strings.HasPrefix(table, "repositories.") && name == "repository_skills":
		c.Repository = strings.TrimPrefix(table, "repositories.")
		gained, lost := listDifference(from.List, to.List)
		c.Roles = slices.Concat(gained, lost)
		c.Gained, c.Lost = gained, lost
		slices.SortFunc(c.Roles, func(a, b string) int { return slices.Index(roles.All, a) - slices.Index(roles.All, b) })
	}
	return c
}

// listDifference returns the items of after that before lacks, and the
// items of before that after lacks.
func listDifference(before, after []string) (gained, lost []string) {
	for _, item := range after {
		if !slices.Contains(before, item) {
			gained = append(gained, item)
		}
	}
	for _, item := range before {
		if !slices.Contains(after, item) {
			lost = append(lost, item)
		}
	}
	return gained, lost
}
