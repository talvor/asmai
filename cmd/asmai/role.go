// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/talvor/asmai/internal/config"
	"github.com/talvor/asmai/internal/roles"
)

// roleStaffing is a role's staffing and skill lists as `asmai role list
// --json` prints them.
type roleStaffing struct {
	Role           string   `json:"role"`
	Staffed        bool     `json:"staffed"`
	LeaderProvider string   `json:"leader_provider"`
	LeaderModel    string   `json:"leader_model"`
	WorkerProvider string   `json:"worker_provider"`
	WorkerModel    string   `json:"worker_model"`
	LeaderSkills   []string `json:"leader_skills"`
	WorkerSkills   []string `json:"worker_skills"`
	LeaderAdded    []string `json:"leader_added_skills"`
	WorkerAdded    []string `json:"worker_added_skills"`
}

// roleList is `asmai role list`: each role's provider and model for its
// leader and its workers, and its skill lists, from the configuration file.
func roleList(o *output) int {
	path, data, err := configFile()
	if err != nil {
		return o.fail(err)
	}
	cfg, problems := config.Parse(data)
	if len(problems) > 0 {
		printProblems(o.stderr, path, problems)
		return 1
	}
	var list []roleStaffing
	for _, role := range roles.All {
		staffing, staffed := cfg.Roles[role]
		skills := cfg.EffectiveSkills(role)
		list = append(list, roleStaffing{
			Role: role, Staffed: staffed && cfg.MissingStaffing([]string{role}) == nil,
			LeaderProvider: staffing.LeaderProvider, LeaderModel: staffing.LeaderModel,
			WorkerProvider: staffing.WorkerProvider, WorkerModel: staffing.WorkerModel,
			LeaderSkills: nonNil(skills.LeaderSkills), WorkerSkills: nonNil(skills.WorkerSkills),
			LeaderAdded: nonNil(skills.LeaderAdded), WorkerAdded: nonNil(skills.WorkerAdded),
		})
	}
	if o.json {
		return o.printJSON(map[string]any{"file": path, "roles": list})
	}
	rows := [][]string{{"ROLE", "LEADER", "WORKERS", "LEADER SKILLS", "WORKER SKILLS"}}
	for _, r := range list {
		rows = append(rows, []string{r.Role, pair(r.LeaderProvider, r.LeaderModel), pair(r.WorkerProvider, r.WorkerModel),
			skillText(r.LeaderSkills, r.LeaderAdded), skillText(r.WorkerSkills, r.WorkerAdded)})
	}
	o.table(rows)
	return 0
}

func nonNil(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}

func pair(provider, model string) string {
	if provider == "" && model == "" {
		return "-"
	}
	return orDash(provider) + "/" + orDash(model)
}

func orDash(text string) string {
	if text == "" {
		return "-"
	}
	return text
}

// skillText writes a role's shipped skills, then its added skill
// directories each marked with +.
func skillText(shipped, added []string) string {
	items := append([]string(nil), shipped...)
	for _, dir := range added {
		items = append(items, "+"+dir)
	}
	if len(items) == 0 {
		return "-"
	}
	return strings.Join(items, ",")
}

// roleChoices are the staffing a user gave `asmai role set`.
type roleChoices struct {
	leaderProvider, leaderModel string
	workerProvider, workerModel string
	present                     map[string]bool
}

func (c roleChoices) fields() map[string]string {
	fields := map[string]string{}
	for _, choice := range []struct{ flag, name, value string }{
		{"leader-provider", "leader_provider", c.leaderProvider},
		{"leader-model", "leader_model", c.leaderModel},
		{"worker-provider", "worker_provider", c.workerProvider},
		{"worker-model", "worker_model", c.workerModel},
	} {
		if c.present[choice.flag] {
			fields[choice.name] = choice.value
		}
	}
	return fields
}

// roleSet is `asmai role set <role>`: it sets the provider and model of a
// role's leader and workers in the configuration file, keeping the rest of
// the file as it is. It refuses Codex. Running agents keep what they run on
// until their next start, and `asmai config apply` says which they are.
func roleSet(o *output, role string, c roleChoices) int {
	if !roles.Known(role) {
		return o.fail(fmt.Errorf("%q is no role; the roles are %s", role, strings.Join(roles.All, ", ")))
	}
	fields := c.fields()
	if len(fields) == 0 {
		return o.fail(errors.New("say what to set: --leader-provider, --leader-model, --worker-provider or --worker-model"))
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return o.fail(fmt.Errorf("finding the configuration file: %w", err))
	}
	path, err := config.ActivePath(home)
	if err != nil {
		return o.fail(err)
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return o.fail(fmt.Errorf("%s does not exist; run `asmai setup`, which writes it with every role staffed", path))
	}
	if err := config.SetStaffing(path, role, fields); err != nil {
		return o.fail(err)
	}
	cfg, _, err := config.Load(path)
	if err != nil {
		return o.fail(err)
	}
	s := cfg.Roles[role]
	if o.json {
		return o.printJSON(map[string]any{"role": role, "file": path, "set": fields,
			"leader_provider": s.LeaderProvider, "leader_model": s.LeaderModel, "worker_provider": s.WorkerProvider, "worker_model": s.WorkerModel})
	}
	fmt.Fprintf(o.stdout, "Set %s in %s: leader %s, workers %s.\n", role, path, pair(s.LeaderProvider, s.LeaderModel), pair(s.WorkerProvider, s.WorkerModel))
	if missing := cfg.MissingStaffing([]string{role}); len(missing) > 0 {
		fmt.Fprintf(o.stdout, "%s is not fully staffed yet: %s\n", roles.Title(role), missing[0].Problem)
	}
	fmt.Fprintln(o.stdout, "Running agents keep what they run on until their next start; `asmai config apply` shows which pick the change up.")
	return 0
}
