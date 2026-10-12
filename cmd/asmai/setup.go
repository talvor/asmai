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

	"golang.org/x/term"

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
	stepFailed      = "failed"
	stepStopped     = "stopped"
)

// setupStages is how many stages `asmai setup` runs: config, folders,
// Claude Code and trust.
const setupStages = 4

// trustPrompt starts the line on which setup asks the user to go on to
// Claude Code's question about trusting a folder; the folder follows it.
const trustPrompt = "Press Enter to open Claude Code in "

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
// (ADR 0011). Each of its stages is headed and says what it is about to do;
// each step is skipped when it is already done, and reports one line, done,
// already done, or failed before the error. Before opening Claude Code in a
// folder, setup says how to answer its question and waits for the user to
// agree, on a terminal: it opens none without one. An existing
// configuration file is never changed: its problems are only reported.
// Claude Code records the trust itself; setup only reads its record, and
// never writes ~/.claude.json or ~/.claude.
func setup(o *output, paths statedir.Paths, stdin io.Reader) int {
	// With --json, stdout carries only the result, and Claude Code draws on
	// stderr.
	w := o.stdout
	if o.json {
		w = o.stderr
	}
	styled := isTerminal(w)
	var steps []setupStep
	// current is the step under way, which a failure reports as failed.
	current := ""
	report := func(step, state, detail string) {
		steps = append(steps, setupStep{Step: step, State: state, Detail: detail})
		fmt.Fprintf(w, "%s: %s. %s\n", step, state, detail)
		current = ""
	}
	stage := func(n int, title, about string) {
		heading := fmt.Sprintf("== %d/%d %s ", n, setupStages, title)
		heading += strings.Repeat("=", max(0, 60-len(heading)))
		if styled {
			heading = "\x1b[1m" + heading + "\x1b[22m"
		}
		fmt.Fprintf(w, "\n%s\n%s\n", heading, about)
	}
	fail := func(err error) int {
		if current != "" {
			steps = append(steps, setupStep{Step: current, State: stepFailed, Detail: err.Error()})
			fmt.Fprintf(w, "%s: %s. The error below says why, and what to do next.\n", current, stepFailed)
		}
		if o.json {
			o.printJSON(map[string]any{"error": err.Error(), "steps": steps})
			return 1
		}
		return o.fail(err)
	}
	fmt.Fprintf(w, "asmai setup readies this account to run the factory, in %d stages: config, folders, Claude Code and trust. A stage already done is skipped.\n", setupStages)

	current = "config"
	home, err := os.UserHomeDir()
	if err != nil {
		return fail(fmt.Errorf("finding the configuration file: %w", err))
	}
	configFile, err := config.ActivePath(home)
	if err != nil {
		return fail(err)
	}
	stage(1, "Config", fmt.Sprintf("Setup writes the configuration file, %s, if it is missing. An existing one is never changed.", configFile))
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

	current = "folders"
	stage(2, "Folders", fmt.Sprintf("Setup makes the folders the factory keeps its state in, in %s.", paths.Dir))
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
	current = plan.Title
	stage(3, plan.Title, fmt.Sprintf("Setup installs AsmAI's own copy of the pinned %s %s, once you confirm, unless it is installed already.", plan.Title, plan.Version))
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

	stage(4, "Trust", fmt.Sprintf("%s asks whether to trust a folder the first time it starts there, and no agent can answer that question. You answer it once here, for agents/ and workspaces/, and the trust covers every folder below them.", plan.Title))
	record := providers.ClaudeCodeRecord(home, os.Getenv)
	for _, dir := range []string{paths.Agents, paths.Workspaces} {
		step := "trust " + filepath.Base(dir) + "/"
		current = step
		trusted, err := providers.Trusted(record, dir)
		if err != nil {
			return fail(err)
		}
		if trusted {
			report(step, stepAlreadyDone, fmt.Sprintf("%s trusts %s", plan.Title, dir))
			continue
		}
		if in, ok := stdin.(*os.File); !ok || !term.IsTerminal(int(in.Fd())) {
			return fail(fmt.Errorf("trusting %s needs you at a terminal, to answer %s's question there; setup did not open %s. Run `asmai setup` in a terminal", dir, plan.Title, plan.Title))
		}
		explainTrust(w, plan, dir)
		if !agreeToTrust(w, stdin, dir) {
			report(step, stepStopped, fmt.Sprintf("You stopped setup before %s opened, so %s is still untrusted", plan.Title, dir))
			return fail(fmt.Errorf("%s does not trust %s, so no agent can start there; run `asmai setup` again when you are ready to answer its question", plan.Title, dir))
		}
		if err := openForTrust(plan.Path, dir, stdin, w, o.stderr); err != nil {
			return fail(err)
		}
		// What setup says next starts below what Claude Code left.
		fmt.Fprintln(w)
		if trusted, err = providers.Trusted(record, dir); err != nil {
			return fail(err)
		}
		if !trusted {
			return fail(fmt.Errorf("%s does not trust %s, so no agent can start there; run `asmai setup` again to answer its question", plan.Title, dir))
		}
		report(step, stepDone, fmt.Sprintf("%s trusts %s", plan.Title, dir))
	}

	fmt.Fprintln(w)
	if len(problems) > 0 {
		return fail(fmt.Errorf("%s has %s; fix %s, then run `asmai start`", configFile, plural(len(problems), "a problem", "problems"), plural(len(problems), "it", "them")))
	}
	if o.json {
		return o.printJSON(map[string]any{"steps": steps})
	}
	fmt.Fprintln(w, "Setup is done: `asmai start` starts the factory.")
	return 0
}

// explainTrust tells the user, before setup opens the pinned Claude Code in
// dir, why dir is to be trusted, what Claude Code will ask, which answer to
// choose, and what the other answer does.
func explainTrust(w io.Writer, plan providers.Plan, dir string) {
	why := "every leader's directory is made in it"
	if filepath.Base(dir) == "workspaces" {
		why = "every assignment's workspace is made in it"
	}
	rule := "  +" + strings.Repeat("-", 72)
	lines := []string{
		fmt.Sprintf("You are needed: trust %s", dir),
		fmt.Sprintf("Agents start in this folder: %s.", why),
		"",
		fmt.Sprintf("Next, setup opens AsmAI's pinned %s %s there. It asks whether", plan.Title, plan.Version),
		"you trust this folder, with two answers:",
		"",
		`    "Yes, I trust this folder"   <- choose this one:`,
		"                                    select it with the arrow keys, then press Enter",
		`    "No, exit"`,
		"",
		fmt.Sprintf("Once %s is ready for a prompt, leave it: type /exit and press Enter.", plan.Title),
		"Setup then goes on.",
		"",
		`If you choose "No, exit", setup stops with exit status 1, naming this folder`,
		"as untrusted. Running `asmai setup` again asks only about the folders still",
		"untrusted.",
	}
	fmt.Fprintf(w, "\n%s\n", rule)
	for _, line := range lines {
		fmt.Fprintf(w, "  | %s\n", line)
	}
	fmt.Fprintf(w, "%s\n", rule)
}

// agreeToTrust asks on w whether to open Claude Code in dir, and reports
// whether the user agreed on stdin: Enter, or y or yes, agrees, and n, no or
// the end of stdin declines. Anything else asks again.
func agreeToTrust(w io.Writer, stdin io.Reader, dir string) bool {
	for {
		fmt.Fprintf(w, "%s%s, or type n and press Enter to stop setup: ", trustPrompt, dir)
		answer := readLine(stdin)
		switch strings.ToLower(strings.TrimSpace(answer)) {
		case "", "y", "yes":
			if strings.HasSuffix(answer, "\n") {
				return true
			}
			fmt.Fprintln(w)
			return false
		case "n", "no":
			return false
		}
	}
}

// isTerminal reports whether w is a terminal.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
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
