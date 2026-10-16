// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"fmt"
	"slices"
	"strings"

	"github.com/talvor/asmai/internal/config"
	"github.com/talvor/asmai/internal/skills"
	"github.com/talvor/asmai/internal/store"
)

// leaderSkills returns the skills role's leader is given: its list in the
// configuration, with its added skills.
func leaderSkills(cfg config.Config, role string) ([]skills.Given, error) {
	s := cfg.EffectiveSkills(role)
	list, err := skills.List(s.LeaderSkills, s.LeaderAdded)
	if err != nil {
		return nil, fmt.Errorf("%s's leader's skills: %w", role, err)
	}
	return list, nil
}

// workerSkills returns the list of role's workers before an assignment
// narrows it: its list in the configuration, with its added skills.
func workerSkills(cfg config.Config, role string) ([]skills.Given, error) {
	s := cfg.EffectiveSkills(role)
	list, err := skills.List(s.WorkerSkills, s.WorkerAdded)
	if err != nil {
		return nil, fmt.Errorf("%s's workers' skills: %w", role, err)
	}
	return list, nil
}

// assignmentSkills returns the skills the worker carrying a is given, as
// sessionSkills makes them from the settings of a's job's repository.
func (d *daemon) assignmentSkills(cfg config.Config, a store.Assignment) ([]skills.Given, error) {
	_, role, _ := strings.Cut(a.Worker, "@")
	list, left, err := sessionSkills(cfg, role, a.Skills, d.repositoryOptions(cfg, a.Job), a.Workspace.Path)
	if err != nil {
		return nil, fmt.Errorf("the skills of assignment %d: %w", a.ID, err)
	}
	for _, g := range left {
		d.log.Warn("a repository skill is left out: a skill on the role's list has its name", "assignment", a.ID, "skill", g.Name, "dir", g.Dir)
	}
	return list, nil
}

// sessionSkills returns the skills a worker of role is given for an
// assignment in a job of the repository whose settings are options, in the
// workspace at workspace: its role's workers' list, narrowed to narrowed when
// that is not empty, then the repository's own skills in the workspace when
// options switch them on for role. Only an assignment's worker works in one
// repository's job, so only it is given that repository's skills. A
// repository skill named like one on the role's list, narrowed out or not,
// is left out, and returned in left.
func sessionSkills(cfg config.Config, role string, narrowed []string, options config.RepositoryOptions, workspace string) (list, left []skills.Given, err error) {
	if list, err = workerSkills(cfg, role); err != nil {
		return nil, nil, err
	}
	roleList := list
	if len(narrowed) > 0 {
		var given []skills.Given
		for _, name := range narrowed {
			i := slices.IndexFunc(list, func(g skills.Given) bool { return g.Name == name })
			if i < 0 {
				return nil, nil, fmt.Errorf("the list is narrowed to %s, which is no longer on %s's workers' skill list", name, role)
			}
			given = append(given, list[i])
		}
		list = given
	}
	if !slices.Contains(options.RepositorySkills, role) {
		return list, nil, nil
	}
	own, err := skills.RepositorySkills(workspace)
	if err != nil {
		return nil, nil, err
	}
	for _, g := range own {
		if slices.ContainsFunc(roleList, func(on skills.Given) bool { return on.Name == g.Name }) {
			left = append(left, g)
			continue
		}
		list = append(list, g)
	}
	return list, left, nil
}

// listSkills sets a.List to the skill list its worker is given, as
// assignmentSkills makes it from the applied configuration. When that fails
// the list is left out, and the worker's start reports why.
func (d *daemon) listSkills(a *store.Assignment) {
	d.configMu.Lock()
	cfg := d.applied
	d.configMu.Unlock()
	if list, err := d.assignmentSkills(cfg, *a); err == nil {
		a.List = skills.NamesOf(list)
	}
}
