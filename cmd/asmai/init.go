// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"

	"golang.org/x/term"

	"github.com/talvor/asmai/internal/certify"
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

// setupStages is how many stages `asmai init` runs: platform, config,
// folders, Claude Code and trust.
const setupStages = 5

// trustPrompt starts the line on which setup or repo add asks the user to go
// on to Claude Code's question about trusting a folder; the folder follows
// it.
const trustPrompt = "Press Enter to open Claude Code in "

// setupStep is one step of `asmai init`, as it reported it.
type setupStep struct {
	Step   string `json:"step"`
	State  string `json:"state"`
	Detail string `json:"detail"`
}

// initFactory is `asmai init`, the first run: it checks that this platform
// is certified, writes a configuration file staffing every role
// when there is none, makes the state directory's folders, installs the
// pinned Claude Code once the user confirms, and opens it in the agents and
// workspaces folders and in each registered repository's clone, in the
// user's own terminal, for the user to trust each (ADR 0011). Each of its
// stages is headed and says what it is about to do; each step is skipped
// when it is already done, and reports one line, done, already done, or
// failed before the error. Before opening Claude Code in a
// folder, setup says how to answer its question and waits for the user to
// agree, on a terminal: it opens none without one. An existing
// configuration file is never changed: its problems are only reported.
// Claude Code records the trust itself; setup only reads its record, and
// never writes ~/.claude.json or ~/.claude.
func initFactory(o *output, paths statedir.Paths, stdin io.Reader, assumeYes bool, requestedModel string) int {
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
	fmt.Fprintf(w, "asmai init readies this account to run the factory, in %d stages: platform, config, folders, Claude Code and trust. A stage already done is skipped.\n", setupStages)

	current = "platform"
	pinList, err := providers.ParsePins(pins)
	if err != nil {
		return fail(err)
	}
	stage(1, "Platform", "Init checks that this platform is one this asmai is certified on, since AsmAI starts no agent on any other.")
	if check := certify.Platform(runtime.GOOS, runtime.GOARCH, pinList); check.OK {
		report("platform", stepAlreadyDone, fmt.Sprintf("%s/%s is certified", runtime.GOOS, runtime.GOARCH))
	} else {
		report("platform", stepFailed, check.Problem)
		return fail(fmt.Errorf("%s: %s; fix: %s", check.Name, check.Problem, check.Fix))
	}

	current = "config"
	home, err := os.UserHomeDir()
	if err != nil {
		return fail(fmt.Errorf("finding the configuration file: %w", err))
	}
	configFile, err := config.ActivePath(home)
	if err != nil {
		return fail(err)
	}
	stage(2, "Config", fmt.Sprintf("Setup writes the configuration file, %s, if it is missing. An existing one is never changed.", configFile))
	_, statErr := os.Stat(configFile)
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return fail(fmt.Errorf("checking %s: %w", configFile, statErr))
	}
	model := "opus"
	if requestedModel != "" || errors.Is(statErr, os.ErrNotExist) {
		model, err = chooseClaudeDefaultModel(w, stdin, assumeYes, requestedModel)
		if err != nil {
			return fail(err)
		}
	}
	created, err := config.CreateDefault(configFile, model)
	if err != nil {
		return fail(err)
	}
	cfg, problems, err := fileProblems(configFile)
	if err != nil {
		return fail(err)
	}
	if !created && requestedModel != "" && len(problems) == 0 && cfg.Providers[config.Claude].DefaultModel != model {
		return fail(fmt.Errorf("%s has Claude default model %q, but --claude-default-model requests %q; edit [providers.claude].default_model to change it", configFile, cfg.Providers[config.Claude].DefaultModel, model))
	}
	if created {
		report("config", stepDone, fmt.Sprintf("Wrote %s, staffing %s and setting Claude's default model to %s", configFile, staffedRoles(), model))
	} else {
		report("config", stepAlreadyDone, fmt.Sprintf("%s exists, and setup leaves it as it is", configFile))
	}
	for _, p := range problems {
		where := configFile
		if p.Line > 0 {
			where = fmt.Sprintf("%s, line %d", configFile, p.Line)
		}
		fmt.Fprintf(w, "  problem  %s: %s\n", where, p.Problem)
		fmt.Fprintf(w, "      fix  %s\n", strings.ReplaceAll(p.Fix, "\n", "\n           "))
	}

	current = "folders"
	stage(3, "Folders", fmt.Sprintf("Setup makes the folders the factory keeps its state in, in %s.", paths.Dir))
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
	stage(4, plan.Title, fmt.Sprintf("Setup installs AsmAI's own copy of the pinned %s %s, once you confirm, unless it is installed already, then checks its version, subscription sign-in and permission rules.", plan.Title, plan.Version))
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
			if err := fetchPinnedAs(w, stdin, plan, assumeYes); err != nil {
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

	// The pinned Claude Code is there now: check that it reports the pinned
	// version, sign-in and permission rules. A failed check stops before trust.
	current = "Claude Code checks"
	for _, check := range daemon.ClaudeCodeChecks(pinList.ClaudeCode.Version, plan.Path, nil,
		func(path string) daemon.Check {
			return daemon.Check(certify.Version(context.Background(), path, pinList.ClaudeCode.Version, providers.ClaudeCodeVersion))
		}, daemon.SignInCheck,
		func(path string) daemon.Check { return daemon.PermissionRulesCheck(path, paths.Dir) }) {
		if check.Name == "Claude Code "+plan.Version {
			continue // The install, reported above.
		}
		if check.OK {
			report(check.Name, stepAlreadyDone, fmt.Sprintf("%s passes", check.Name))
			continue
		}
		report(check.Name, stepFailed, check.Problem)
		fmt.Fprintf(w, "      fix  %s\n", check.Fix)
		return fail(fmt.Errorf("%s: %s; fix: %s", check.Name, check.Problem, check.Fix))
	}

	stage(5, "Trust", fmt.Sprintf("%s asks whether to trust a folder the first time it starts there, and no agent can answer that question. You answer it once here, for agents/ and workspaces/, whose trust covers every folder below them, and for each registered repository's clone in repositories/, whose trust covers every workspace checked out from it.", plan.Title))
	record := providers.ClaudeCodeRecord(home, os.Getenv)
	current = "trust"
	registered, err := registeredRepositories(paths)
	if err != nil {
		return fail(err)
	}
	var clones []string
	for _, r := range registered {
		if _, err := os.Stat(r.Clone); err == nil {
			clones = append(clones, r.Clone)
		}
	}
	for _, dir := range append([]string{paths.Agents, paths.Workspaces}, clones...) {
		step := "trust " + trustStep(paths, dir)
		current = step
		state, detail, err := trustFolder(w, o.stderr, stdin, plan, record, dir, slices.Contains(clones, dir), "init", assumeYes)
		if state != "" {
			report(step, state, detail)
		}
		if err != nil {
			return fail(err)
		}
	}

	fmt.Fprintln(w)
	if len(problems) > 0 {
		return fail(fmt.Errorf("%s has %s; fix %s, then run `asmai start`", configFile, plural(len(problems), "a problem", "problems"), plural(len(problems), "it", "them")))
	}
	if o.json {
		return o.printJSON(map[string]any{"steps": steps})
	}
	fmt.Fprintln(w, "Init is done: `asmai start` starts the factory, and `asmai repo add` registers a repository for it to work on.")
	return 0
}

func chooseClaudeDefaultModel(w io.Writer, stdin io.Reader, assumeYes bool, requested string) (string, error) {
	if requested != "" {
		if requested != "opus" && requested != "sonnet" {
			return "", fmt.Errorf("Claude's default model %q is not opus or sonnet", requested)
		}
		return requested, nil
	}
	if in, ok := stdin.(*os.File); !ok || !term.IsTerminal(int(in.Fd())) || assumeYes {
		return "opus", nil
	}
	for {
		fmt.Fprint(w, "Claude default model (opus or sonnet) [opus]: ")
		answer := strings.ToLower(strings.TrimSpace(readLine(stdin)))
		switch answer {
		case "", "opus":
			return "opus", nil
		case "sonnet":
			return "sonnet", nil
		default:
			fmt.Fprintln(w, "Choose opus or sonnet.")
		}
	}
}

// errNoTerminal is why trustFolder could not have the user trust a folder:
// stdin is not a terminal.
var errNoTerminal = errors.New("needs you at a terminal")

// trustFolder has the user trust dir, a registered repository's clone when
// clone is set, for the pinned Claude Code of plan, whose record is at
// record, as command, setup or repo add, asks it. A folder trusted already
// is left as it is. Otherwise, on a terminal, it says why dir is to be
// trusted and how to answer Claude Code's question, waits for the user to
// agree, and opens Claude Code there for them to answer it; with no
// terminal it opens none, and its error wraps errNoTerminal. It returns the
// state the step reached and its detail: done or already done, or stopped
// when the user declined, which comes with an error.
func trustFolder(w, stderr io.Writer, stdin io.Reader, plan providers.Plan, record, dir string, clone bool, command string, assumeYes bool) (state, detail string, err error) {
	trusted, err := providers.Trusted(record, dir)
	if err != nil {
		return "", "", err
	}
	if trusted {
		return stepAlreadyDone, fmt.Sprintf("%s trusts %s", plan.Title, dir), nil
	}
	if in, ok := stdin.(*os.File); !ok || !term.IsTerminal(int(in.Fd())) {
		return "", "", fmt.Errorf("trusting %s %w, to answer %s's question there; %s did not open %s. Run `asmai init` in a terminal", dir, errNoTerminal, plan.Title, command, plan.Title)
	}
	explainTrust(w, plan, dir, clone, command)
	if !assumeYes && !agreeToTrust(w, stdin, dir, command) {
		return stepStopped, fmt.Sprintf("You stopped %s before %s opened, so %s is still untrusted", command, plan.Title, dir),
			fmt.Errorf("%s does not trust %s, so no agent can start there; run `asmai init` again when you are ready to answer its question", plan.Title, dir)
	}
	if err := openForTrust(plan.Path, dir, stdin, w, stderr); err != nil {
		return "", "", err
	}
	// What follows starts below what Claude Code left.
	fmt.Fprintln(w)
	if trusted, err = providers.Trusted(record, dir); err != nil {
		return "", "", err
	}
	if !trusted {
		return "", "", fmt.Errorf("%s does not trust %s, so no agent can start there; run `asmai init` again to answer its question", plan.Title, dir)
	}
	return stepDone, fmt.Sprintf("%s trusts %s", plan.Title, dir), nil
}

// explainTrust tells the user, before command, setup or repo add, opens the
// pinned Claude Code in dir, a registered repository's clone when clone is
// set, why dir is to be trusted, what Claude Code will ask, which answer to
// choose, and what the other answer does.
func explainTrust(w io.Writer, plan providers.Plan, dir string, clone bool, command string) {
	why := []string{"Agents start in this folder: every leader's directory is made in it."}
	switch {
	case clone:
		why = []string{
			"Workers start in git worktrees of this clone, one per assignment, and",
			fmt.Sprintf("%s trusts a worktree only when it trusts the clone itself.", plan.Title),
		}
	case filepath.Base(dir) == "workspaces":
		why = []string{"Agents start in this folder: every assignment's workspace is made in it."}
	}
	rule := "  +" + strings.Repeat("-", 72)
	lines := append([]string{fmt.Sprintf("You are needed: trust %s", dir)}, why...)
	lines = append(lines,
		"",
		fmt.Sprintf("Next, %s opens AsmAI's pinned %s %s there. It asks whether", command, plan.Title, plan.Version),
		"you trust this folder, with two answers:",
		"",
		`    "Yes, I trust this folder"   <- choose this one:`,
		"                                    select it with the arrow keys, then press Enter",
		`    "No, exit"`,
		"",
		fmt.Sprintf("Once %s is ready for a prompt, leave it: type /exit and press Enter.", plan.Title),
		fmt.Sprintf("%s%s then goes on.", strings.ToUpper(command[:1]), command[1:]),
		"",
		fmt.Sprintf(`If you choose "No, exit", %s stops with exit status 1, naming this folder`, command),
		"as untrusted. Running `asmai init` again asks only about the folders still",
		"untrusted.",
	)
	fmt.Fprintf(w, "\n%s\n", rule)
	for _, line := range lines {
		fmt.Fprintf(w, "  | %s\n", line)
	}
	fmt.Fprintf(w, "%s\n", rule)
}

// trustStep names the folder dir, in the state directory, for setup's step
// that trusts it: agents/, workspaces/, or repositories/<name>/ for a clone.
func trustStep(paths statedir.Paths, dir string) string {
	if rel, err := filepath.Rel(paths.Dir, dir); err == nil {
		return rel + "/"
	}
	return dir
}

// agreeToTrust asks on w whether command, setup or repo add, is to open
// Claude Code in dir, and reports whether the user agreed on stdin: Enter
// agrees, and n or the end of stdin declines. Anything else asks again.
func agreeToTrust(w io.Writer, stdin io.Reader, dir, command string) bool {
	for {
		fmt.Fprintf(w, "%s%s, or type n and press Enter to stop %s: ", trustPrompt, dir, command)
		answer := readLine(stdin)
		switch strings.ToLower(strings.TrimSpace(answer)) {
		case "":
			if strings.HasSuffix(answer, "\n") {
				return true
			}
			fmt.Fprintln(w)
			return false
		case "n":
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
