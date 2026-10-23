// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/providers"
	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/store"
)

// The cases of ADR 0018, which has AsmAI run on an account whose managed
// settings set allowManagedPermissionRulesOnly without approving a tool call
// on its own authority. Where the host's account has no such policy, C55 and
// C56 qualify what the pinned Claude Code says and does without it, and C56
// gives its session the settings a session has under the policy, which
// ignores every permission rule AsmAI passes with --settings: the allow rules
// are taken out of them, which is what the policy does. Where the account has
// the policy, they qualify it as it is. The development tests play each
// verdict with the fake provider's managed-rules flags.

// permissionRulesCheck returns the check `asmai start` printed for the
// permission rules.
func permissionRulesCheck(checks []daemon.Check) (daemon.Check, bool) {
	for _, c := range checks {
		if c.Name == daemon.PermissionRulesName {
			return c, true
		}
	}
	return daemon.Check{}, false
}

// onlyPermissionRules reports whether the permission rules check is the only
// one of checks that failed, which is a verdict of C55's and not a host that
// cannot run the case.
func onlyPermissionRules(checks []daemon.Check) bool {
	failed := 0
	for _, c := range checks {
		if !c.OK {
			if c.Name != daemon.PermissionRulesName {
				return false
			}
			failed++
		}
	}
	return failed > 0
}

// hostPlatform is the platform the harness and the asmai it runs are on. A
// test replaces it to play the verdicts of another.
var hostPlatform = runtime.GOOS

// c55 qualifies the permission probe (ADR 0018) against the pinned Claude
// Code: it makes the same control requests the daemon does, with no model
// call, and requires the verdict they give to be one AsmAI acts on as the
// ADR says, and `asmai start`'s check to say so.
//
//   - Without the managed policy, every rule AsmAI passes is in effect, and
//     the check passes with no warning.
//   - Under it, none of AsmAI's rules is in effect (an administrator's own
//     are), and the check passes with no warning where an administrator's
//     allow rule covers `asmai`, or the sandbox auto-allows with the
//     daemon's socket allowed on a platform where that is confirmed (macOS);
//     it passes with a warning that `asmai` may be prompted where the sandbox
//     route cannot be confirmed without running it (Linux, which C56 probes);
//     and it fails, naming the policy and the one rule or setting an
//     administrator can add, where neither route is open, so that no agent
//     starts.
//   - Claude Code names the platform it runs on, if it does, as this host's.
//
// The host's own account decides which verdict it has: the case requires the
// verdict to be coherent with the check, whichever it is, and says which it
// saw.
func (h *Harness) c55(ctx context.Context, f *Factory, r *Result) error {
	path, started, err := h.readyWith(ctx, f)
	if err != nil {
		var refused startChecksFailed
		if !errors.As(err, &refused) || !onlyPermissionRules(refused.Checks) {
			return err
		}
	}
	verdict, err := providers.ProbePermissionRules(ctx, path, f.paths.Dir, f.paths.Socket, f.env)
	if err != nil {
		return fmt.Errorf("the probe of the permission rules: %w", err)
	}
	check, ok := permissionRulesCheck(started.Checks)
	if !ok {
		return errors.New("asmai start printed no check of the permission rules")
	}
	observation, err := probeVerdictHolds(verdict, hostPlatform, check)
	if err != nil {
		return err
	}
	r.observe("%s", observation)
	if verdict.Platform != "" {
		if verdict.Platform != hostPlatform {
			return fmt.Errorf("Claude Code says it runs on %q, but this host is %s", verdict.Platform, hostPlatform)
		}
		r.observe("Claude Code named its platform, %s, in its effective settings", verdict.Platform)
	}
	return nil
}

// probeVerdictHolds checks that verdict, which the probe made on a host of
// platform goos, is one AsmAI acts on as ADR 0018 says, and that check, the
// one `asmai start` printed, is what the ADR says for it. It returns what the
// verdict was.
func probeVerdictHolds(verdict providers.PermissionVerdict, goos string, check daemon.Check) (string, error) {
	route := verdict.Route(goos)
	if !verdict.ManagedOnly {
		if !verdict.Honoured() {
			return "", fmt.Errorf("Claude Code reports no managed policy (managedOnly is false), yet does not apply %s of AsmAI's own rules: %s", fmt.Sprint(len(verdict.Ignored)), strings.Join(verdict.Ignored, ", "))
		}
		if route != providers.RouteRules {
			return "", fmt.Errorf("without the managed policy the route is %v, want AsmAI's own rules", route)
		}
		if !check.OK || check.Warning != "" {
			return "", fmt.Errorf("without the managed policy the permission rules check is %+v, want it passed with no warning", check)
		}
		return fmt.Sprintf("the account has no managed policy: Claude Code applies all %d of the rules AsmAI passes with --settings, and the permission rules check passes with no warning", len(providers.CommandRules())), nil
	}
	if len(verdict.Ignored) != len(providers.CommandRules()) {
		return "", fmt.Errorf("under the managed policy Claude Code ignores %d of the %d rules AsmAI passes (%s), want all of them: the policy applies only managed rules", len(verdict.Ignored), len(providers.CommandRules()), strings.Join(verdict.Ignored, ", "))
	}
	policy := "the account's managed settings set " + providers.ManagedPolicySetting + ", so Claude Code ignores all " + fmt.Sprint(len(providers.CommandRules())) + " of the rules AsmAI passes"
	switch route {
	case providers.RouteAllowRule, providers.RouteSandbox:
		if !check.OK || check.Warning != "" {
			return "", fmt.Errorf("under the managed policy, with `asmai` unprompted by %s, the permission rules check is %+v, want it passed with no warning", routeName(route), check)
		}
		return fmt.Sprintf("%s; `asmai` runs unprompted by %s, and the permission rules check passes with no warning", policy, routeName(route)), nil
	case providers.RouteUnconfirmed:
		if !check.OK || !strings.Contains(check.Warning, "`asmai` may be prompted") || !strings.Contains(check.Warning, "Bash(asmai *)") {
			return "", fmt.Errorf("under the managed policy, with the sandbox's route unconfirmed on %s, the permission rules check is %+v, want it passed with a warning that `asmai` may be prompted and the rule an administrator can allow", goos, check)
		}
		return fmt.Sprintf("%s; the sandbox auto-allows but whether it lets `asmai` reach the daemon's socket cannot be confirmed on %s without running it, so the check passes with a warning that `asmai` may be prompted (C56 runs it)", policy, goos), nil
	}
	if check.OK {
		return "", fmt.Errorf("under the managed policy, with neither an allow rule for `asmai` nor the sandbox's route, the permission rules check passed (%+v), want it to fail so that no agent starts", check)
	}
	for _, want := range []string{"`asmai` cannot run unprompted", providers.ManagedPolicySetting} {
		if !strings.Contains(check.Problem, want) {
			return "", fmt.Errorf("the failed permission rules check says %q, want it to say %q", check.Problem, want)
		}
	}
	for _, want := range []string{"Bash(asmai *)", "sandbox.autoAllowBashIfSandboxed: true", "account without that policy", "Nothing is needed for git, gh or workspace edits"} {
		if !strings.Contains(check.Fix, want) {
			return "", fmt.Errorf("the failed permission rules check's fix says %q, want it to say %q", check.Fix, want)
		}
	}
	return policy + ", and neither an administrator's allow rule for `asmai` nor the sandbox's auto-allow with the daemon's socket is in effect, so the check fails, names the policy and asks for the one rule or setting, and no agent starts", nil
}

func routeName(route providers.Route) string {
	switch route {
	case providers.RouteAllowRule:
		return "an administrator's allow rule"
	case providers.RouteSandbox:
		return "the sandbox's auto-allow with the daemon's socket"
	}
	return fmt.Sprint(int(route))
}

// withoutAllowRules returns args, an agent session's command line, with the
// allow rules of its --settings taken out, which is what a managed policy
// that applies only managed permission rules does to them. Everything else a
// session is given stays as it is.
func withoutAllowRules(args []string) ([]string, error) {
	args = slices.Clone(args)
	i := slices.Index(args, "--settings")
	if i < 0 || i+1 >= len(args) {
		return nil, errors.New("the session's command line has no --settings")
	}
	var settings map[string]any
	if err := json.Unmarshal([]byte(args[i+1]), &settings); err != nil {
		return nil, fmt.Errorf("reading the session's --settings: %w", err)
	}
	permissions, _ := settings["permissions"].(map[string]any)
	if permissions == nil {
		return nil, errors.New("the session's --settings has no permissions")
	}
	permissions["allow"] = []string{}
	data, err := json.Marshal(settings)
	if err != nil {
		return nil, err
	}
	args[i+1] = string(data)
	return args, nil
}

// c56 qualifies that an agent's `asmai` runs sandboxed and unprompted under
// the managed policy, by the route ADR 0018 chooses, in the pinned Claude
// Code's real session. The session has the command line the code under test
// builds for an unattended Engineering worker under the policy (acceptEdits
// mode, `asmai` not among the commands left out of the sandbox, the
// daemon's socket allowed with sandbox.network.allowUnixSockets), with every
// allow rule taken out of its settings, as the policy ignores them: nothing
// allows `asmai` but the sandbox's auto-allow. The session runs `asmai status
// --json` as one Bash call, never outside the sandbox. It must not be denied,
// fail, be retried outside the sandbox or raise a native permission prompt,
// and what it prints must be the running daemon's, by its process ID: a
// command that could not reach the daemon's socket from the sandbox says the
// daemon is not running.
//
// It is the probe of the sandbox route that Linux needs. Claude Code's own
// documentation is unclear whether its sandbox lets a command connect to one
// Unix socket on Linux, which is why the permission rules check passes there
// with a warning (C55). Run on a Linux host, a pass shows the route works
// there and a failure shows it does not; either way the verdict is the host's
// own and the case says which platform it ran on.
func (h *Harness) c56(ctx context.Context, f *Factory, r *Result) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	if err := checkGuardDependencies(); err != nil {
		return err
	}
	path, _, err := h.readyWith(ctx, f)
	if err != nil {
		var refused startChecksFailed
		if !errors.As(err, &refused) || !onlyPermissionRules(refused.Checks) {
			return err
		}
		r.observe("the account's managed policy leaves no route for `asmai` (C55): the session below has only what the daemon would give it, and asmai start refuses it")
	}
	pid, running := f.Running()
	if !running {
		return errors.New("the factory's daemon is not running")
	}
	g, err := newGuardFixture()
	if err != nil {
		return err
	}
	defer os.RemoveAll(g.root)
	instructions, err := roles.WorkerInstructions(roles.Engineering)
	if err != nil {
		return err
	}
	hooks := filepath.Join(g.marks, "hooks.jsonl")
	perms := providers.Permissions{Unattended: true, ManagedOnly: true, Editable: []string{g.workspace, g.tmp}}
	args := providers.ClaudeCodeArgs(hookLog(hooks), guardModel, instructions, "", providers.WriteGuard{Writable: []string{g.tmp}, Socket: f.paths.Socket}, perms)
	if args, err = withoutAllowRules(args); err != nil {
		return err
	}
	command := "asmai status --json"
	turn, err := h.Provider.Ask(ctx, f, path, AskSpec{
		Dir:  g.workspace,
		Args: args,
		Env:  []string{"PATH=" + filepath.Dir(f.asmai) + string(os.PathListSeparator) + envLookup(f.env, "PATH")},
		Prompt: "You are being qualified, as an Engineering worker, in your own session. Run this command as one single Bash tool call, exactly as written, with no other command in the call and never with dangerouslyDisableSandbox. " +
			"If it is refused or fails, do not retry it, change it or work around it. Then reply with the single word done. The command: `" + command + "`",
	})
	if err != nil {
		return err
	}
	tried := turn.Tried(command)
	if len(tried) == 0 {
		return fmt.Errorf("inconclusive: the session never ran `%s` as one Bash call, so the harness cannot show how its sandbox treats `asmai`", command)
	}
	for _, c := range tried {
		switch {
		case c.Unsandboxed:
			return fmt.Errorf("`%s` was retried outside the sandbox (dangerouslyDisableSandbox), so the sandbox stopped it, and it is `asmai` outside the sandbox that raises the native prompt under the policy: %s%s", command, strings.TrimSpace(c.Output), routeNote())
		case c.Denied:
			return fmt.Errorf("`%s` was denied in the worker's session, which has no allow rule for it: %s%s", command, strings.TrimSpace(c.Output), routeNote())
		case c.Failed:
			return fmt.Errorf("`%s` failed in the worker's session: %s%s", command, strings.TrimSpace(c.Output), routeNote())
		}
	}
	var status struct {
		Daemon struct {
			Running bool `json:"running"`
			PID     int  `json:"pid"`
		} `json:"daemon"`
	}
	output := strings.TrimSpace(tried[0].Output)
	if err := json.Unmarshal([]byte(output), &status); err != nil {
		return fmt.Errorf("`%s` printed %q, which is not the JSON of asmai status%s", command, output, routeNote())
	}
	if !status.Daemon.Running || status.Daemon.PID != pid {
		return fmt.Errorf("the sandboxed `%s` printed %q, but the daemon is running with pid %d: the command did not reach the daemon's socket from the sandbox%s", command, output, pid, routeNote())
	}
	r.observe("on %s, an Engineering worker's real session in acceptEdits mode with its allow rules taken out, as the managed policy ignores them, ran `%s` sandboxed, never retried outside the sandbox, with no denial, and it reached the daemon (pid %d) through the socket the settings allow with sandbox.network.allowUnixSockets", runtime.GOOS, command, pid)
	asked, err := permissionRequests(hooks)
	if err != nil {
		return err
	}
	if len(asked) > 0 {
		return fmt.Errorf("the worker's session raised a native permission prompt, which nobody can answer in its terminal, for %s%s", strings.Join(asked, ", "), routeNote())
	}
	if !exists(hooks) {
		return errors.New("inconclusive: the session's hooks reported nothing, so the harness cannot show that it raised no native prompt")
	}
	r.observe("the session's hooks reported no PermissionRequest")
	if runtime.GOOS != "darwin" {
		r.observe("this host is %s: ADR 0018 leaves the sandbox route unconfirmed there, and this run confirms it for this host and this Claude Code; until the permission probe's verdict says so too, `asmai start` keeps its warning", runtime.GOOS)
	}
	return nil
}

// routeNote is what a failure of C56 says of the platform it ran on.
func routeNote() string {
	if runtime.GOOS == "darwin" {
		return ""
	}
	return fmt.Sprintf(". This is the sandbox route that ADR 0018 leaves unconfirmed on %s: Claude Code's sandbox did not let `asmai` reach the daemon's socket unprompted here, so the probe's verdict there stays unconfirmed and the stall relay is the safety net", runtime.GOOS)
}

// relayWrite is the request that has Coordination's leader raise a native
// permission prompt for writing file, as nativePromptWrite does.
func relayWrite(file string, number int64) string {
	return fmt.Sprintf("Qualification exercise %d. This is a check of your permission prompts, not a job: do not open one or hand anything to another role. With the Write tool, and no other tool, create the file %s in your working directory, holding the single line ok. If it is refused, do nothing else. Then reply with the single word done.", number, file)
}

// relayWaits is how many of the harness's waits a step of C57 may take.
const relayWaits = 4

// c57 qualifies that the keys the daemon's stall relay types answer a real
// native permission prompt as ADR 0018 says (the user's answer to a stall is
// relayed, never decided by the daemon): with the pinned Claude Code, Enter
// takes the prompt's default, which must be its one-time Yes, and the tool
// runs; Escape cancels it, and the tool does not run; and neither adds an
// allow rule, so the next such write raises a prompt again. The keys are
// those the daemon types (daemon.PromptKeys). The prompts are Coordination's
// leader's, the one real agent a case can bring to a native prompt without a
// job and a worker, in the same Claude Code interface a worker's prompt would
// be in; the daemon's own relay, from the user's witnessed answer to the keys
// in a worker's terminal, is played by the development tests with the fake
// provider.
func (h *Harness) c57(ctx context.Context, f *Factory, r *Result) error {
	if _, err := h.ready(ctx, f); err != nil {
		return err
	}
	leader, ok, err := f.Leader()
	if err != nil {
		return err
	}
	if !ok || leader.State != store.AgentRunning {
		return errors.New("Coordination's leader is not running")
	}
	allow, _ := daemon.PromptKeys(store.PromptAllow)
	deny, _ := daemon.PromptKeys(store.PromptDeny)
	dir := filepath.Join(f.paths.Agents, leader.Agent)
	number := time.Now().UnixNano()
	if h.Number != nil {
		number = h.Number()
	}

	// round has the leader write file, waits for its native prompt, answers
	// it with keys and waits for the turn to end.
	round := func(file string, keys []byte, requireDefaultYes bool) error {
		before, err := factoryJournal(f)
		if err != nil {
			return err
		}
		baseline := lastEntry(before)
		if err := f.Submit(relayWrite(file, number)); err != nil {
			return err
		}
		if err := h.awaitJournal(ctx, f, "the leader's native permission prompt for "+file, func(entries []store.Entry) (bool, error) {
			requested, finished := permissionRequestedSince(entries, baseline, leader.Agent, leader.Generation)
			if finished {
				return false, fmt.Errorf("inconclusive: the leader finished its turn without raising a native permission prompt for %s, so the harness cannot show how the relay's keys answer one", file)
			}
			return requested, nil
		}); err != nil {
			return err
		}
		if requireDefaultYes {
			// Give the menu a moment to draw after the hook reported it.
			var option string
			for range 10 {
				lines, err := f.Screen()
				if err != nil {
					return err
				}
				if option = selectedMenuOption(lines); option != "" {
					break
				}
				time.Sleep(time.Second)
			}
			if !isAllowOnce(option) {
				return fmt.Errorf("the native prompt for %s has %q selected, not the one-time Yes: the daemon's Enter would not take the prompt's default as the relay says", file, option)
			}
		}
		entries, err := factoryJournal(f)
		if err != nil {
			return err
		}
		after := lastEntry(entries)
		if err := f.Keys(string(keys)); err != nil {
			return err
		}
		return h.awaitJournal(ctx, f, "the leader's turn to end after the keys", func(entries []store.Entry) (bool, error) {
			return stoppedSince(entries, after, leader.Agent, leader.Generation), nil
		})
	}

	allowed := "qualification-relay-allow.txt"
	if err := round(allowed, allow, true); err != nil {
		return err
	}
	if data, err := os.ReadFile(filepath.Join(dir, allowed)); err != nil || strings.TrimSpace(string(data)) != "ok" {
		return fmt.Errorf("after the daemon's Enter at the prompt the leader's Write tool wrote %s as %q (%v), want it to have run", allowed, data, err)
	}
	r.observe("the keys the daemon types for Allow once, Enter, taken at a real native permission prompt of %s whose selected choice was the one-time Yes, ran the Write tool: %s was written", leader.Agent, allowed)

	denied := "qualification-relay-deny.txt"
	if err := round(denied, deny, false); err != nil {
		return fmt.Errorf("a second prompt after the first was allowed once, which would show Allow once adds no allow rule: %w", err)
	}
	if exists(filepath.Join(dir, denied)) {
		return fmt.Errorf("after the daemon's Escape at the prompt the leader's Write tool wrote %s: Escape did not cancel it", denied)
	}
	r.observe("a second write raised a native prompt again, so Allow once added no allow rule, and the keys the daemon types for Deny, Escape, cancelled it: %s was not written, and the leader's turn ended", denied)
	return nil
}

// stoppedSince reports whether agent's session in generation reported Stop in
// a journal entry after baseline.
func stoppedSince(entries []store.Entry, baseline int64, agent string, generation int) bool {
	for _, e := range entries {
		if e.ID <= baseline || e.Kind != store.KindObservation {
			continue
		}
		var d struct {
			Agent      string `json:"agent"`
			Generation int    `json:"generation"`
			Event      string `json:"event"`
		}
		if json.Unmarshal(e.Data, &d) == nil && d.Agent == agent && d.Generation == generation && d.Event == "Stop" {
			return true
		}
	}
	return false
}

// awaitJournal reads the factory's journal until done says it has what it
// waits for, or the wait runs out.
func (h *Harness) awaitJournal(ctx context.Context, f *Factory, what string, done func([]store.Entry) (bool, error)) error {
	deadline := time.Now().Add(relayWaits * h.Wait)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, err := factoryJournal(f)
		if err == nil {
			ok, err := done(entries)
			if err != nil {
				return err
			}
			if ok {
				return nil
			}
		}
		if time.Now().After(deadline) {
			lines, _ := f.Screen()
			return fmt.Errorf("waiting for %s: it did not happen within %s%s", what, relayWaits*h.Wait, h.screenNote(lines))
		}
		time.Sleep(h.Poll)
	}
}
