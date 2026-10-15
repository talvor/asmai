// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/talvor/asmai/internal/config"
	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/store"
)

// ConfigApply is what `config.apply` answers: the file's change from the
// configuration the daemon has applied, and, once applied, that it was.
type ConfigApply struct {
	// File is the configuration file this daemon reads, and SHA256 the
	// digest of the content the request carried.
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
	// Changes are what the file changes; none means it is as applied.
	Changes []ConfigChange `json:"changes"`
	// Applied says the change was applied and journaled.
	Applied bool `json:"applied"`
}

// ConfigChange is one change and its effect on known agents.
type ConfigChange struct {
	config.Change
	// Agents are the known agents affected by the change.
	Agents []string `json:"agents"`
	Note   string   `json:"note,omitempty"`
}

// readApplied reads the configuration file the factory starts on, for
// apply to compare the next file with; a file that is missing or has
// problems is nothing applied yet.
func readApplied(path string) config.Config {
	if path != "" {
		if cfg, problems, err := config.Load(path); err == nil && len(problems) == 0 {
			return cfg
		}
	}
	cfg, _ := config.Parse(nil)
	return cfg
}

func withRegisteredRepositories(cfg config.Config, registered []store.Repository) config.Config {
	repositories := make(map[string]config.Repository, len(registered))
	options := make(map[string]config.RepositoryOptions, len(registered))
	for _, r := range registered {
		repositories[r.Name] = config.Repository{Location: r.Location, Origin: r.Origin, DefaultBranch: r.DefaultBranch}
		if value, ok := cfg.Options[r.Name]; ok {
			options[r.Name] = value
		} else {
			options[r.Name] = config.DefaultRepositoryOptions()
		}
	}
	cfg.Repositories = repositories
	cfg.Options = options
	return cfg
}

// noteRepository brings the configuration agents run on up to date with
// the entry the configuration file has for name, which `repo add` and
// `repo remove` write themselves, so that apply does not report it again.
func (d *daemon) noteRepository(name string) {
	if d.cfg.ConfigFile == "" {
		return
	}
	cfg, problems, err := config.Load(d.cfg.ConfigFile)
	if err != nil || len(problems) > 0 {
		return
	}
	d.configMu.Lock()
	defer d.configMu.Unlock()
	d.applied.Repositories = cloneMap(d.applied.Repositories)
	d.applied.Options = cloneMap(d.applied.Options)
	if r, ok := cfg.Repositories[name]; ok {
		d.applied.Repositories[name] = r
		d.applied.Options[name] = cfg.Options[name]
	} else {
		delete(d.applied.Repositories, name)
		delete(d.applied.Options, name)
	}
}

func cloneMap[V any](m map[string]V) map[string]V {
	out := make(map[string]V, len(m)+1)
	maps.Copy(out, m)
	return out
}

// configApplyCommand serves config.apply.
func (d *daemon) configApplyCommand(conn *net.UnixConn, req Request) {
	result, err := d.configApply(req)
	var resp Response
	if err != nil {
		resp.Error = err.Error()
		resp.ConfigApply = result
	} else {
		resp.ConfigApply = result
	}
	reply(conn, resp)
}

// configApply validates the file the request carries, works out what it
// changes and their effect on running agents, and with Confirm applies
// it and journals the applied configuration. Nothing is applied when anything is wrong.
func (d *daemon) configApply(req Request) (*ConfigApply, error) {
	d.repoMu.Lock()
	defer d.repoMu.Unlock()
	if d.cfg.ConfigFile == "" {
		return nil, refuse("this daemon reads no configuration file, so there is none to apply; start the factory with `asmai start`")
	}
	content := []byte(req.Configuration)
	digest := sha256.Sum256(content)
	result := &ConfigApply{File: d.cfg.ConfigFile, SHA256: hex.EncodeToString(digest[:]), Changes: []ConfigChange{}}

	cfg, problems := config.Parse(content)
	if len(problems) == 0 {
		problems = cfg.MissingStaffing(roles.Staffed)
	}
	if len(problems) > 0 {
		return nil, refuse("%v", &config.ProblemsError{Path: d.cfg.ConfigFile, Problems: problems})
	}
	disk, err := os.ReadFile(d.cfg.ConfigFile)
	if err != nil {
		return nil, fmt.Errorf("reading the configuration file: %w", err)
	}
	if !bytes.Equal(disk, content) {
		return nil, refuse("the file this daemon reads, %s, is not the file you applied: it has changed since, or the daemon reads another; run `asmai config apply` again", d.cfg.ConfigFile)
	}

	registered, err := d.store.Repositories()
	if err != nil {
		return nil, fmt.Errorf("reading the registered repositories: %w", err)
	}
	if mismatches := RepositoryProblems(cfg, registered); len(mismatches) > 0 {
		return nil, refuse("%s; %s", mismatches[0].Problem, mismatches[0].Fix)
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	d.configMu.Lock()
	base := d.applied
	d.configMu.Unlock()
	agents, err := d.store.Agents()
	if err != nil {
		return nil, fmt.Errorf("reading the agents: %w", err)
	}
	for _, c := range config.Diff(base, cfg) {
		picked, err := d.pickedUpBy(c, agents)
		if err != nil {
			return nil, err
		}
		change := ConfigChange{Change: c, Agents: picked}
		if c.Removed && c.Role != "" {
			change.Note = "running agents keep running; this role cannot start after removal"
		} else if c.Skills && c.Role != "" && c.After == "" {
			change.Note = "this role cannot start after removal"
		} else if pendingSkillDelivery(c) {
			change.Note = "skills reach affected agents' sessions at their next start once per-session skill delivery (asmai #89) lands"
		}
		result.Changes = append(result.Changes, change)
	}
	if req.Confirm && (req.Preview == nil || !reflect.DeepEqual(*req.Preview, *result)) {
		return result, refuse("the configuration or affected agents changed since the preview; run `asmai config apply` again")
	}
	if !req.Confirm || len(result.Changes) == 0 {
		return result, nil
	}

	applied := store.AppliedConfiguration{File: d.cfg.ConfigFile, SHA256: result.SHA256, Configuration: string(content)}
	for _, c := range result.Changes {
		applied.Changes = append(applied.Changes, store.AppliedChange{Setting: c.Setting, Before: c.Before, After: c.After, Agents: c.Agents, Note: c.Note})
	}
	if err := d.store.ConfigurationApplied(applied, time.Now()); err != nil {
		return result, fmt.Errorf("journaling the applied configuration: %w", err)
	}
	d.configMu.Lock()
	d.applied = cfg
	d.configMu.Unlock()
	result.Applied = true
	d.log.Info("configuration applied", "file", d.cfg.ConfigFile, "sha256", result.SHA256, "changes", len(result.Changes))
	return result, nil
}

func RepositoryProblems(cfg config.Config, registered []store.Repository) []config.Problem {
	var problems []config.Problem
	known := make(map[string]bool, len(registered))
	for _, r := range registered {
		known[r.Name] = true
		entry, ok := cfg.Repositories[r.Name]
		if !ok {
			problems = append(problems, config.Problem{Problem: fmt.Sprintf("[repositories.%s] is missing from the file but is registered", r.Name), Fix: fmt.Sprintf("restore its table, or run `asmai repo remove %s` to remove the registered repository", r.Name)})
			continue
		}
		for _, field := range []struct{ name, file, record string }{
			{"location", entry.Location, r.Location},
			{"origin", entry.Origin, r.Origin},
			{"default_branch", entry.DefaultBranch, r.DefaultBranch},
		} {
			if field.file != field.record {
				problems = append(problems, config.Problem{Problem: fmt.Sprintf("[repositories.%s].%s differs from the registered repository", r.Name, field.name), Fix: "restore the registered value in the file; config apply does not change the repository record"})
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.Repositories)) {
		if !known[name] {
			problems = append(problems, config.Problem{Problem: fmt.Sprintf("[repositories.%s] is in the file but is not registered", name), Fix: fmt.Sprintf("remove the table and register it with `asmai repo add <path-or-url> --name %s`, which writes it", name)})
		}
	}
	return problems
}

// pickedUpBy returns the known agents affected by change c, by address.
func (d *daemon) pickedUpBy(c config.Change, agents []store.Agent) ([]string, error) {
	picked := []string{}
	if c.Removed && c.Role != "" || c.Skills && c.Role != "" && c.After == "" {
		return picked, nil
	}
	for _, a := range agents {
		if a.State != store.AgentRunning && !(pendingSkillDelivery(c) && a.State == store.AgentStopped) {
			continue
		}
		leader := strings.HasPrefix(a.Agent, roles.Leader+"@")
		var affected bool
		switch {
		case c.Repository != "":
			if !slices.Contains(c.Roles, a.Role) || leader {
				continue
			}
			assignment, err := d.store.LiveAssignmentOf(a.Agent)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("reading %s's assignment: %w", a.Agent, err)
			}
			job, err := d.store.Job(assignment.Job)
			if err != nil {
				return nil, fmt.Errorf("reading job %d: %w", assignment.Job, err)
			}
			affected = job.Repository == c.Repository
		case c.Role == "" || a.Role != c.Role:
		case c.Side == "leader" && !leader, c.Side == "worker" && leader:
		case strings.HasSuffix(c.Setting, "_model"):
			model, err := strconv.Unquote(c.After)
			affected = err != nil || a.Model != model
		default:
			affected = true
		}
		if affected {
			picked = append(picked, a.Agent)
		}
	}
	return picked, nil
}

func pendingSkillDelivery(c config.Change) bool {
	return c.Skills || strings.HasSuffix(c.Setting, ".repository_skills")
}
