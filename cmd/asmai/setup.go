// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/talvor/asmai/internal/config"
	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/providers"
	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/statedir"
)

// What a setup step reports.
const (
	stepDone        = "done"
	stepAlreadyDone = "already done"
)

// setupStep is one step of `asmai setup`, as it reported it.
type setupStep struct {
	Step   string `json:"step"`
	State  string `json:"state"`
	Detail string `json:"detail"`
}

// setup is the first run: it writes a configuration file staffing every role
// when there is none, makes the state directory's folders, installs the
// pinned Claude Code once the user confirms, and opens it in the agents and
// workspaces folders in the user's own terminal, for the user to trust each
// (ADR 0011). Each step is skipped when it is already done, and reports one
// line, done or already done. An existing configuration file is never
// changed: its problems are only reported. Claude Code records the trust
// itself; setup only reads its record, and never writes ~/.claude.json or
// ~/.claude.
func setup(o *output, paths statedir.Paths, stdin io.Reader) int {
	// With --json, stdout carries only the result, and Claude Code draws on
	// stderr.
	w := o.stdout
	if o.json {
		w = o.stderr
	}
	var steps []setupStep
	report := func(step, state, detail string) {
		steps = append(steps, setupStep{Step: step, State: state, Detail: detail})
		fmt.Fprintf(w, "%s: %s. %s\n", step, state, detail)
	}
	fail := func(err error) int {
		if o.json {
			o.printJSON(map[string]any{"error": err.Error(), "steps": steps})
			return 1
		}
		return o.fail(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fail(fmt.Errorf("finding the configuration file: %w", err))
	}

	configFile, err := config.ActivePath(home)
	if err != nil {
		return fail(err)
	}
	created, err := config.CreateDefault(configFile)
	if err != nil {
		return fail(err)
	}
	var problems []config.Problem
	if created {
		report("config", stepDone, fmt.Sprintf("Wrote %s, staffing %s", configFile, staffedRoles()))
	} else {
		cfg, found, err := config.Load(configFile)
		if err != nil {
			return fail(fmt.Errorf("reading %s: %w", configFile, err))
		}
		if problems = found; len(problems) == 0 {
			problems = cfg.MissingStaffing(roles.Staffed)
		}
		report("config", stepAlreadyDone, fmt.Sprintf("%s exists, and setup leaves it as it is", configFile))
		for _, p := range problems {
			where := configFile
			if p.Line > 0 {
				where = fmt.Sprintf("%s, line %d", configFile, p.Line)
			}
			fmt.Fprintf(w, "  problem  %s: %s\n", where, p.Problem)
			fmt.Fprintf(w, "      fix  %s\n", strings.ReplaceAll(p.Fix, "\n", "\n           "))
		}
	}

	made, err := paths.MakeFolders()
	if err != nil {
		return fail(err)
	}
	if made {
		report("folders", stepDone, fmt.Sprintf("Made the factory's folders in %s", paths.Dir))
	} else {
		report("folders", stepAlreadyDone, fmt.Sprintf("%s holds the factory's folders", paths.Dir))
	}

	plan, err := pinnedPlan(paths)
	if err != nil {
		return fail(err)
	}
	// A running daemon records the install now, and a pin it records is
	// installed, as `asmai providers install` takes it; a stopped one finds
	// the pinned copy at its path when it next starts.
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandProviders})
	running := err == nil
	if err != nil && !errors.Is(err, daemon.ErrNotRunning) {
		return fail(err)
	}
	recorded := false
	for _, installed := range resp.Providers {
		recorded = recorded || installed.Name == plan.Name && installed.Version == plan.Version && installed.Path == plan.Path && exists(plan.Path)
	}
	present := recorded
	if !present {
		if present, err = providers.Present(plan); err != nil {
			return fail(err)
		}
	}
	if present && (recorded || !running) {
		report(plan.Title, stepAlreadyDone, fmt.Sprintf("The pinned %s %s is at %s", plan.Title, plan.Version, plan.Path))
	} else {
		if !present {
			if err := fetchPinned(w, stdin, plan); err != nil {
				return fail(err)
			}
		}
		if running {
			if _, err := recordInstall(paths, plan); err != nil {
				return fail(err)
			}
		}
		report(plan.Title, stepDone, fmt.Sprintf("Installed the pinned %s %s at %s", plan.Title, plan.Version, plan.Path))
	}

	record := providers.ClaudeCodeRecord(home, os.Getenv)
	for _, dir := range []string{paths.Agents, paths.Workspaces} {
		step := "trust " + filepath.Base(dir) + "/"
		trusted, err := providers.Trusted(record, dir)
		if err != nil {
			return fail(err)
		}
		if trusted {
			report(step, stepAlreadyDone, fmt.Sprintf("%s trusts %s", plan.Title, dir))
			continue
		}
		fmt.Fprintf(w, "Trusting %s, where agents start: the pinned %s opens there now. Choose \"Yes, I trust this folder\", then leave it with /exit.\n", dir, plan.Title)
		if err := openForTrust(plan.Path, dir, stdin, w, o.stderr); err != nil {
			return fail(err)
		}
		if trusted, err = providers.Trusted(record, dir); err != nil {
			return fail(err)
		}
		if !trusted {
			return fail(fmt.Errorf("%s does not trust %s, so no agent can start there; run `asmai setup` again to answer its question", plan.Title, dir))
		}
		report(step, stepDone, fmt.Sprintf("%s trusts %s", plan.Title, dir))
	}

	if len(problems) > 0 {
		return fail(fmt.Errorf("%s has %s; fix %s, then run `asmai start`", configFile, plural(len(problems), "a problem", "problems"), plural(len(problems), "it", "them")))
	}
	if o.json {
		return o.printJSON(map[string]any{"steps": steps})
	}
	fmt.Fprintln(w, "Setup is done: `asmai start` starts the factory.")
	return 0
}

// staffedRoles names the roles M1 runs, for people.
func staffedRoles() string {
	var titles []string
	for _, role := range roles.Staffed {
		titles = append(titles, roles.Title(role))
	}
	return strings.Join(titles[:len(titles)-1], ", ") + " and " + titles[len(titles)-1]
}

// openForTrust runs the pinned Claude Code at path in dir, on the user's own
// terminal, until the user leaves it: it asks them whether to trust dir, and
// records their answer itself. It starts with the user's environment, less
// any provider API-key variable and with Claude Code's auto-updater off, so
// it installs no Claude Code of the user's own, and loads no repository's
// settings or MCP server. An interrupt reaches Claude Code, and leaves setup running.
func openForTrust(path, dir string, stdin io.Reader, stdout, stderr io.Writer) error {
	var env []string
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); !providers.IsAPIKeyVariable(name) && name != "DISABLE_AUTOUPDATER" {
			env = append(env, kv)
		}
	}
	env = append(env, "DISABLE_AUTOUPDATER=1")
	cmd := exec.Command(path, "--setting-sources", "user", "--strict-mcp-config")
	cmd.Dir, cmd.Env = dir, env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt, syscall.SIGQUIT)
	defer signal.Stop(interrupts)
	err := cmd.Run()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		return fmt.Errorf("opening Claude Code in %s: %w", dir, err)
	}
	return nil
}
