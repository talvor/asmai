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
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/talvor/asmai/internal/config"
	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/store"
)

// ConfigApply is what `config.apply` answers: the file's change from the
// configuration the agents run on, and, once it was applied, that it was.
type ConfigApply struct {
	// File is the configuration file this daemon reads, and SHA256 the
	// digest of the content the request carried.
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
	// Changes are what the file changes; none means it is as applied.
	Changes []ConfigChange `json:"changes"`
	// Applied says the change was applied and journaled.
	Applied bool `json:"applied"`
	// Removed are the repositories the file no longer has, whose records
	// and clones the apply removed.
	Removed []string `json:"removed,omitempty"`
}

// ConfigChange is one change, with the running agents that pick it up.
type ConfigChange struct {
	config.Change
	// Agents are the running agents that pick the change up at their next
	// start; with none it applies at once.
	Agents []string `json:"agents"`
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
// changes and which running agents pick that up, and with Confirm applies
// it: the repositories it no longer has are removed, and the applied
// configuration is journaled. Nothing is applied when anything is wrong.
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

	// A repository the file has that is not registered has no clone, and
	// only `asmai repo add` makes one; one the file no longer has is
	// removed, which an open job refuses.
	registered, err := d.store.Repositories()
	if err != nil {
		return nil, fmt.Errorf("reading the registered repositories: %w", err)
	}
	var removed []string
	isRegistered := map[string]bool{}
	for _, r := range registered {
		isRegistered[r.Name] = true
		entry, ok := cfg.Repositories[r.Name]
		if !ok {
			removed = append(removed, r.Name)
			continue
		}
		for _, field := range []struct{ name, file, record string }{
			{"location", entry.Location, r.Location},
			{"origin", entry.Origin, r.Origin},
			{"default_branch", entry.DefaultBranch, r.DefaultBranch},
		} {
			if field.file != field.record {
				return nil, refuse("[repositories.%s].%s differs from the registered repository; config apply does not change the repository record", r.Name, field.name)
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.Repositories)) {
		if !isRegistered[name] {
			return nil, refuse("[repositories.%s] is in the file but is not registered, so AsmAI has no clone of it; remove the table and register it with `asmai repo add <path-or-url> --name %s`, which writes it", name, name)
		}
	}
	for _, name := range removed {
		open, err := d.store.OpenJobs(name)
		if err != nil {
			return nil, fmt.Errorf("reading the jobs of %s: %w", name, err)
		}
		if len(open) > 0 {
			jobs := make([]string, len(open))
			for i, n := range open {
				jobs[i] = strconv.FormatInt(n, 10)
			}
			return nil, refuse("the file no longer has [repositories.%s], which has open %s %s; nothing is applied. Put the table back, or apply once %s ended", name, plural(len(open), "job", "jobs"), strings.Join(jobs, ", "), plural(len(open), "it has", "they have"))
		}
	}

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
		result.Changes = append(result.Changes, ConfigChange{Change: c, Agents: picked})
	}
	if !req.Confirm || len(result.Changes) == 0 {
		return result, nil
	}

	for _, name := range removed {
		if _, err := d.removeRepository(name); err != nil {
			return result, err
		}
		result.Removed = append(result.Removed, name)
	}
	applied := store.AppliedConfiguration{File: d.cfg.ConfigFile, SHA256: result.SHA256, Configuration: string(content)}
	for _, c := range result.Changes {
		applied.Changes = append(applied.Changes, store.AppliedChange{Setting: c.Setting, Before: c.Before, After: c.After, Agents: c.Agents})
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

// pickedUpBy returns the running agents that get change c at their next
// start, by address: the agents of the role it is for, on the side it is
// for, that do not already run what it sets.
func (d *daemon) pickedUpBy(c config.Change, agents []store.Agent) ([]string, error) {
	picked := []string{}
	for _, a := range agents {
		if a.State != store.AgentRunning {
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
		case c.Removed:
			// A role removed from the file keeps its running agents, which
			// cannot start again.
			affected = true
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
