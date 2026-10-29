// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/talvor/asmai/internal/certify"
	"github.com/talvor/asmai/internal/config"
	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/providers"
	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/skills"
	"github.com/talvor/asmai/internal/statedir"
	"github.com/talvor/asmai/internal/store"
)

// latestSkillRelease asks upstream for the tag of its latest skill release.
// Tests replace it with a fake: the lookup needs the network.
var latestSkillRelease = func(ctx context.Context) (string, error) {
	address, err := skills.LatestReleaseURL(skills.Source())
	if err != nil {
		return "", err
	}
	return skills.LatestRelease(ctx, &http.Client{Timeout: 10 * time.Second}, address)
}

// upstreamLookupWait is how long doctor waits for upstream to name its
// latest release.
const upstreamLookupWait = 15 * time.Second

// unqualifiedSkill is an added or repository skill, as doctor lists it.
type unqualifiedSkill struct {
	Name       string   `json:"name"`
	Source     string   `json:"source"`
	Owner      string   `json:"owner"`
	Dir        string   `json:"dir"`
	Roles      []string `json:"roles"`
	Repository string   `json:"repository,omitempty"`
	Qualified  bool     `json:"qualified"`
}

// upstreamReport is what doctor says about upstream's skill releases.
type upstreamReport struct {
	Source  string `json:"source"`
	Bundled string `json:"bundled"`
	Latest  string `json:"latest,omitempty"`
	Newer   bool   `json:"newer"`
	Error   string `json:"error,omitempty"`
}

// hostChecks runs the checks the start of the factory, `asmai init` and
// `asmai doctor` all run, without changing anything: that this platform is
// certified, that the configuration file staffs the roles and holds no
// problem, that the pinned Claude Code is installed, reports the pinned
// version, uses a Claude subscription sign-in and lets AsmAI's permission
// rules take effect, that the pinned Lavish is installed, and that Claude
// Code trusts the directories agents start in. It returns them with the
// configuration file's path and the configuration, when it could be read.
func hostChecks(paths statedir.Paths, home string, registered []store.Repository) (checks []daemon.Check, configFile string, cfg config.Config, err error) {
	p, err := providers.ParsePins(pins)
	if err != nil {
		return nil, "", cfg, err
	}
	checks = append(checks, daemon.Check(certify.Platform(runtime.GOOS, runtime.GOARCH, p)))

	configFile, err = config.ActivePath(home)
	if err != nil {
		return nil, "", cfg, err
	}
	var problems []config.Problem
	cfg, problems, err = fileProblems(configFile)
	switch {
	case errors.Is(err, config.ErrMissing):
		checks = append(checks, daemon.Check{Name: "staffing", Problem: fmt.Sprintf("%s does not exist, so no role is staffed", configFile), Fix: "run `asmai init` to write the configuration file"})
	case err != nil:
		checks = append(checks, daemon.Check{Name: "staffing", Problem: err.Error(), Fix: "make the file readable"})
	case len(problems) == 0:
		checks = append(checks, daemon.Check{Name: "staffing", OK: true})
	}
	for _, problem := range problems {
		where := configFile
		if problem.Line > 0 {
			where = fmt.Sprintf("%s, line %d", configFile, problem.Line)
		}
		checks = append(checks, daemon.Check{Name: "staffing", Problem: where + ": " + problem.Problem, Fix: problem.Fix})
	}

	record := providers.ClaudeCodeRecord(home, os.Getenv)
	dirs := []string{paths.Agents, paths.Workspaces}
	for _, r := range registered {
		if _, err := os.Stat(r.Clone); err == nil {
			dirs = append(dirs, r.Clone)
		}
	}
	platform, ok := providers.Platform(runtime.GOOS, runtime.GOARCH)
	if ok {
		plan, planErr := p.ClaudeCodePlan(platform, paths.Providers)
		checks = append(checks, daemon.ClaudeCodeChecks(p.ClaudeCode.Version, plan.Path, planErr,
			func(path string) daemon.Check {
				return daemon.Check(certify.Version(context.Background(), path, p.ClaudeCode.Version, providers.ClaudeCodeVersion))
			}, daemon.SignInCheck,
			func(path string) daemon.Check { return daemon.PermissionRulesCheck(path, paths.Dir, paths.Socket) })...)
	}
	// A Lavish pinned but not yet published for this platform leaves nothing to install.
	if lavish, _, err := pinnedLavishPlan(paths); err != nil {
		checks = append(checks, daemon.Check{Name: "Lavish", Problem: err.Error(), Fix: "run an asmai built with its pins file"})
	} else if lavish != nil {
		state, err := lavish.Standing(lavish.Version)
		name := "Lavish " + lavish.Version
		switch {
		case err != nil:
			checks = append(checks, daemon.Check{Name: name, Problem: err.Error(), Fix: "run `asmai providers install`"})
		case state == providers.StateMissing:
			checks = append(checks, daemon.Check{Name: name, Problem: fmt.Sprintf("the pinned Lavish %s is not installed at %s, so the factory cannot show you decision requests", lavish.Version, lavish.Path), Fix: "run `asmai providers install`"})
		case state == providers.StateMismatch:
			checks = append(checks, daemon.Check{Name: name, Problem: fmt.Sprintf("the Lavish at %s is not the pinned build %s or is not executable, so the factory cannot show you decision requests", lavish.Path, lavish.Version), Fix: "run `asmai providers install`"})
		default:
			checks = append(checks, daemon.Check{Name: name, OK: true})
		}
	}
	checks = append(checks, daemon.TrustCheck(record, dirs))
	return checks, configFile, cfg, nil
}

// branchPrefixOf returns the prefix selected by cfg and where it comes from,
// without making the install's branch ID: the configuration's branch_prefix,
// else the ID kept in the state directory. An install the daemon has not yet
// given an ID has no prefix, and the source says when it gets one.
func branchPrefixOf(cfg config.Config, paths statedir.Paths) (prefix, source string) {
	if cfg.BranchPrefix != "" {
		return cfg.BranchPrefix, "from branch_prefix in the configuration file"
	}
	id, err := paths.ReadBranchID()
	switch {
	case err != nil:
		return "", err.Error()
	case id == "":
		return "", "the daemon makes an ID in the state directory when the next job needs a branch"
	}
	return id, "from the ID kept in the state directory"
}

func effectiveBranchPrefix(cfg config.Config, configFile string, paths statedir.Paths) (prefix, source string, pending, known bool) {
	data, err := os.ReadFile(configFile)
	if err == nil {
		resp, callErr := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandConfigApply, Configuration: string(data)})
		if errors.Is(callErr, daemon.ErrNotRunning) {
			prefix, source = branchPrefixOf(cfg, paths)
			return prefix, source, false, true
		}
		if callErr != nil || resp.ConfigApply == nil {
			return "", "applied prefix could not be confirmed", false, false
		}
		for _, change := range resp.ConfigApply.Changes {
			if change.Setting == "branches.branch_prefix" {
				applied := ""
				if change.Before != "" {
					applied, err = strconv.Unquote(change.Before)
					if err != nil {
						return "", "applied prefix could not be confirmed", false, false
					}
				}
				pending = applied != cfg.BranchPrefix
				cfg.BranchPrefix = applied
				break
			}
		}
		prefix, source = branchPrefixOf(cfg, paths)
		return prefix, source, pending, true
	}
	if _, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandStatus}); !errors.Is(err, daemon.ErrNotRunning) {
		return "", "applied prefix could not be confirmed", false, false
	}
	prefix, source = branchPrefixOf(cfg, paths)
	return prefix, source, false, true
}

// registeredRepositories reads the registered repositories from the store
// without changing it; a factory that has not been initialised has none.
func registeredRepositories(paths statedir.Paths) ([]store.Repository, error) {
	if _, err := os.Stat(paths.Store); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("reading the registered repositories: %w", err)
	}
	st, err := store.OpenReadOnly(paths.Store)
	if err != nil {
		return nil, fmt.Errorf("reading the registered repositories: %w", err)
	}
	defer st.Close()
	registered, err := st.Repositories()
	if err != nil {
		return nil, fmt.Errorf("reading the registered repositories: %w", err)
	}
	return registered, nil
}

// unqualifiedSkills lists the skills that reach agents without being in the
// bundle, which no release qualifies: the added skills of each role's
// leader and workers, and the skills of each registered repository that
// its configuration switches on for a role. A skill that cannot be read is
// returned as a failed check.
func unqualifiedSkills(cfg config.Config, registered []store.Repository) ([]unqualifiedSkill, []daemon.Check) {
	var found []unqualifiedSkill
	var failed []daemon.Check
	add := func(list []skills.Given, role, repository string) {
		for _, g := range list {
			if g.Qualified() {
				continue
			}
			i := slices.IndexFunc(found, func(s unqualifiedSkill) bool {
				return s.Name == g.Name && s.Dir == g.Dir && s.Repository == repository
			})
			if i < 0 {
				found = append(found, unqualifiedSkill{Name: g.Name, Source: g.Source, Owner: g.Owner(), Dir: g.Dir, Repository: repository})
				i = len(found) - 1
			}
			if !slices.Contains(found[i].Roles, role) {
				found[i].Roles = append(found[i].Roles, role)
			}
		}
	}
	for _, role := range roles.All {
		s := cfg.EffectiveSkills(role)
		if len(s.LeaderAdded) > 0 {
			list, err := skills.List(s.LeaderSkills, s.LeaderAdded)
			if err != nil {
				failed = append(failed, daemon.Check{Name: "added skills", Problem: fmt.Sprintf("%s's leader's skills: %v", role, err), Fix: "fix or remove the added skill in the configuration file, then run `asmai config apply`"})
			}
			add(list, role, "")
		}
		if len(s.WorkerAdded) > 0 {
			list, err := skills.List(s.WorkerSkills, s.WorkerAdded)
			if err != nil {
				failed = append(failed, daemon.Check{Name: "added skills", Problem: fmt.Sprintf("%s's workers' skills: %v", role, err), Fix: "fix or remove the added skill in the configuration file, then run `asmai config apply`"})
			}
			add(list, role, "")
		}
	}
	for _, r := range registered {
		options, ok := cfg.Options[r.Name]
		if !ok || len(options.RepositorySkills) == 0 {
			continue
		}
		own, err := skills.RepositorySkills(r.Clone)
		if err != nil {
			failed = append(failed, daemon.Check{Name: "repository skills", Problem: fmt.Sprintf("repository %s: %v", r.Name, err), Fix: "fix the skill in the repository, or remove its roles from the repository's `repository_skills` setting in the configuration file and run `asmai config apply`"})
			continue
		}
		for _, role := range options.RepositorySkills {
			add(own, role, r.Name)
		}
	}
	return found, failed
}

// upstreamStatus asks upstream for its latest skill release and compares
// it with the bundle's.
func upstreamStatus() upstreamReport {
	report := upstreamReport{Source: skills.Source(), Bundled: skills.Version()}
	ctx, cancel := context.WithTimeout(context.Background(), upstreamLookupWait)
	defer cancel()
	latest, err := latestSkillRelease(ctx)
	if err != nil {
		report.Error = err.Error()
		return report
	}
	report.Latest = latest
	report.Newer = skills.Newer(report.Bundled, latest)
	return report
}

// doctor is `asmai doctor`: it runs the checks the start of the factory
// runs, and changes nothing. It also reports what the pinned Claude Code
// cannot hide from an agent, marks added and repository skills as
// unqualified, and says when upstream has a newer skill release. It exits 1
// when a check fails; the rest it only reports.
func doctor(o *output, paths statedir.Paths) int {
	home, err := os.UserHomeDir()
	if err != nil {
		return o.fail(fmt.Errorf("finding the configuration file: %w", err))
	}
	registered, err := registeredRepositories(paths)
	if err != nil {
		return o.fail(err)
	}
	checks, configFile, cfg, err := hostChecks(paths, home, registered)
	if err != nil {
		return o.fail(err)
	}
	prefix, prefixSource, prefixPending, prefixKnown := effectiveBranchPrefix(cfg, configFile, paths)
	unqualified, skillChecks := unqualifiedSkills(cfg, registered)
	checks = append(checks, skillChecks...)
	gaps := providers.ClaudeCodeUnhidden()
	upstream := upstreamStatus()

	failures := 0
	for _, c := range checks {
		if !c.OK {
			failures++
		}
	}
	p, err := providers.ParsePins(pins)
	if err != nil {
		return o.fail(err)
	}
	if o.structured() {
		if unqualified == nil {
			unqualified = []unqualifiedSkill{}
		}
		branchPrefix := map[string]any{"prefix": prefix, "source": prefixSource, "pending_apply": prefixPending}
		if !prefixKnown {
			branchPrefix["configured_unconfirmed"] = cfg.BranchPrefix
		}
		code := o.printData(map[string]any{
			"ok":                 failures == 0,
			"platform":           runtime.GOOS + "/" + runtime.GOARCH,
			"provider":           map[string]string{"name": providers.ClaudeCode, "pinned": p.ClaudeCode.Version},
			"config":             configFile,
			"state_dir":          paths.Dir,
			"branch_prefix":      branchPrefix,
			"checks":             checks,
			"hiding_gaps":        gaps,
			"unqualified_skills": unqualified,
			"upstream":           upstream,
		})
		if code == 0 && failures > 0 {
			code = 1
		}
		return code
	}

	w := o.stdout
	fmt.Fprintf(w, "Platform       %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(w, "Provider pin   Claude Code %s\n", p.ClaudeCode.Version)
	fmt.Fprintf(w, "Configuration  %s\n", configFile)
	fmt.Fprintf(w, "State          %s\n", paths.Dir)
	if !prefixKnown {
		fmt.Fprintln(w, "Branch prefix  applied prefix could not be confirmed")
		if cfg.BranchPrefix != "" {
			fmt.Fprintf(w, "               configured branch_prefix %s (unconfirmed)\n", cfg.BranchPrefix)
		}
	} else if prefix == "" {
		fmt.Fprintf(w, "Branch prefix  none yet; %s\n", prefixSource)
	} else {
		fmt.Fprintf(w, "Branch prefix  %s (%s)\n", prefix, prefixSource)
	}
	if prefixPending {
		fmt.Fprintln(w, "               pending apply: the configuration file's branch_prefix differs; run `asmai config apply`")
	}
	fmt.Fprintln(w, "\nChecks")
	for _, c := range checks {
		if c.OK {
			fmt.Fprintf(w, "  ok      %s\n", c.Name)
			if c.Warning != "" {
				fmt.Fprintf(w, "          warning: %s\n", c.Warning)
			}
			continue
		}
		fmt.Fprintf(w, "  failed  %s: %s\n", c.Name, c.Problem)
		fmt.Fprintf(w, "          fix: %s\n", strings.ReplaceAll(c.Fix, "\n", "\n            "))
	}

	fmt.Fprintln(w, "\nWhat skill hiding cannot cover")
	for _, g := range gaps {
		fmt.Fprintf(w, "  %s\n      %s\n", g.What, g.Why)
	}

	fmt.Fprintln(w, "\nAdded and repository skills (unqualified: no release qualifies them)")
	if len(unqualified) == 0 {
		fmt.Fprintln(w, "  none")
	}
	for _, s := range unqualified {
		where := "added for " + strings.Join(s.Roles, ", ")
		if s.Repository != "" {
			where = fmt.Sprintf("repository %s, switched on for %s", s.Repository, strings.Join(s.Roles, ", "))
		}
		fmt.Fprintf(w, "  %s  unqualified, %s skill, %s\n      %s\n", s.Name, s.Owner, where, s.Dir)
	}

	fmt.Fprintln(w, "\nUpstream skills")
	switch {
	case upstream.Error != "":
		fmt.Fprintf(w, "  Could not look up upstream's latest release (%s): %s. The bundle is %s.\n", upstream.Source, upstream.Error, upstream.Bundled)
	case upstream.Newer:
		fmt.Fprintf(w, "  Upstream has a newer skill release: %s, and the bundle is %s (%s). A newer asmai release brings it once qualified.\n", upstream.Latest, upstream.Bundled, upstream.Source)
	case upstream.Latest == upstream.Bundled:
		fmt.Fprintf(w, "  The bundle, %s, is upstream's latest skill release (%s).\n", upstream.Bundled, upstream.Source)
	default:
		fmt.Fprintf(w, "  No newer upstream skill release was found; upstream reports %s and the bundle is %s (%s).\n", upstream.Latest, upstream.Bundled, upstream.Source)
	}

	if failures > 0 {
		fmt.Fprintf(o.stderr, "asmai: %s. Fix what failed, then run `asmai doctor` again.\n", plural(failures, "a check failed", "checks failed"))
		return 1
	}
	fmt.Fprintln(w, "\nEvery check passed.")
	return 0
}
