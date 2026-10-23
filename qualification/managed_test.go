// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/fakeprovider"
	"github.com/talvor/asmai/internal/providers"
)

// withEnv adds environment variables to what the harness's factory and its
// fake Claude Code run in.
func withEnv(h *Harness, kv ...string) *Harness {
	h.Env = append(h.Env, kv...)
	return h
}

func TestC55PassesWithoutTheManagedPolicyWhenClaudeCodeAppliesEveryRule(t *testing.T) {
	r := result(t, harness(t, signedIn, startsDirectly), "C55")
	if r.Outcome != Passed {
		t.Fatalf("C55 %s: %s", r.Outcome, r.Failure)
	}
	requireEvidence(t, r, "the account has no managed policy: Claude Code applies all ", "of the rules AsmAI passes with --settings, and the permission rules check passes with no warning")
}

// Every verdict ADR 0018 gives under the policy, played with the fake
// provider's managed-rules flags.
func TestC55QualifiesEachVerdictUnderTheManagedPolicy(t *testing.T) {
	for name, c := range map[string]struct {
		env      []string
		platform string
		want     []string
	}{
		"an administrator's allow rule covers asmai": {
			env:      []string{fakeprovider.ManagedSandboxOffEnv + "=1", fakeprovider.ManagedRulesAllowEnv + "=Bash(asmai *)"},
			platform: "darwin",
			want:     []string{"set allowManagedPermissionRulesOnly", "`asmai` runs unprompted by an administrator's allow rule, and the permission rules check passes with no warning"},
		},
		"the sandbox auto-allows with the daemon's socket on a confirmed platform": {
			platform: "darwin",
			want:     []string{"`asmai` runs unprompted by the sandbox's auto-allow with the daemon's socket, and the permission rules check passes with no warning", "Claude Code named its platform, darwin"},
		},
		"the sandbox route cannot be confirmed on linux": {
			platform: "linux",
			want:     []string{"cannot be confirmed on linux without running it, so the check passes with a warning that `asmai` may be prompted (C56 runs it)"},
		},
		"the sandbox's auto-allow is off": {
			env:      []string{fakeprovider.ManagedSandboxOffEnv + "=1"},
			platform: "darwin",
			want:     []string{"neither an administrator's allow rule for `asmai` nor the sandbox's auto-allow with the daemon's socket is in effect, so the check fails, names the policy and asks for the one rule or setting, and no agent starts"},
		},
		"the daemon's socket is not allowed": {
			env:      []string{fakeprovider.ManagedSocketsOffEnv + "=1"},
			platform: "darwin",
			want:     []string{"so the check fails, names the policy and asks for the one rule or setting, and no agent starts"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			was := hostPlatform
			hostPlatform = c.platform
			t.Cleanup(func() { hostPlatform = was })
			env := append([]string{fakeprovider.ManagedRulesOnlyEnv + "=1", fakeprovider.PlatformEnv + "=" + c.platform}, c.env...)
			r := result(t, withEnv(harness(t, signedIn, startsDirectly), env...), "C55")
			if r.Outcome != Passed {
				t.Fatalf("C55 %s: %s", r.Outcome, r.Failure)
			}
			requireEvidence(t, r, c.want...)
		})
	}
}

// A platform Claude Code names that is not the host's fails the case.
func TestC55FailsWhenClaudeCodeNamesAnotherPlatformThanTheHosts(t *testing.T) {
	was := hostPlatform
	hostPlatform = "darwin"
	t.Cleanup(func() { hostPlatform = was })
	r := result(t, withEnv(harness(t, signedIn, startsDirectly), fakeprovider.PlatformEnv+"=linux"), "C55")
	if r.Outcome != Failed || !strings.Contains(r.Failure, `Claude Code says it runs on "linux", but this host is darwin`) {
		t.Errorf("C55 %s: %q, want it to fail on the platform", r.Outcome, r.Failure)
	}
}

// What the probe says and what `asmai start` says must agree, whichever the
// verdict.
func TestTheProbesVerdictAndTheStartCheckMustAgree(t *testing.T) {
	rules := providers.CommandRules()
	none := providers.PermissionVerdict{ManagedOnly: true, Ignored: rules}
	allowRule := providers.PermissionVerdict{ManagedOnly: true, Ignored: rules, AsmaiAllowed: true}
	sandbox := providers.PermissionVerdict{ManagedOnly: true, Ignored: rules, SandboxAutoAllow: true, SocketControl: true}
	refused := daemon.Check{Name: daemon.PermissionRulesName, Problem: "`asmai` cannot run unprompted: this account's managed settings set " + providers.ManagedPolicySetting,
		Fix: "ask an administrator to allow `Bash(asmai *)`, or to set `sandbox.autoAllowBashIfSandboxed: true`, or run AsmAI under an account without that policy. Nothing is needed for git, gh or workspace edits"}
	passed := daemon.Check{Name: daemon.PermissionRulesName, OK: true}
	warned := daemon.Check{Name: daemon.PermissionRulesName, OK: true, Warning: "`asmai` may be prompted: allow `Bash(asmai *)`"}
	for name, c := range map[string]struct {
		verdict providers.PermissionVerdict
		goos    string
		check   daemon.Check
		want    string
	}{
		"no policy, passed":                       {providers.PermissionVerdict{}, "darwin", passed, ""},
		"no policy, a rule ignored":               {providers.PermissionVerdict{Ignored: rules[:1]}, "darwin", passed, "does not apply 1 of AsmAI's own rules"},
		"no policy, refused":                      {providers.PermissionVerdict{}, "darwin", refused, "want it passed with no warning"},
		"no policy, warned":                       {providers.PermissionVerdict{}, "linux", warned, "want it passed with no warning"},
		"policy, a rule applied":                  {providers.PermissionVerdict{ManagedOnly: true, Ignored: rules[1:]}, "darwin", refused, "want all of them"},
		"an allow rule, passed":                   {allowRule, "darwin", passed, ""},
		"an allow rule, refused":                  {allowRule, "darwin", refused, "want it passed with no warning"},
		"an allow rule, warned":                   {allowRule, "darwin", warned, "want it passed with no warning"},
		"the sandbox on darwin, passed":           {sandbox, "darwin", passed, ""},
		"the sandbox on darwin, warned":           {sandbox, "darwin", warned, "want it passed with no warning"},
		"the sandbox on darwin, refused":          {sandbox, "darwin", refused, "want it passed with no warning"},
		"the sandbox on linux, warned":            {sandbox, "linux", warned, ""},
		"the sandbox on linux, passed":            {sandbox, "linux", passed, "want it passed with a warning that `asmai` may be prompted"},
		"the sandbox on linux, refused":           {sandbox, "linux", refused, "want it passed with a warning that `asmai` may be prompted"},
		"no route, refused":                       {none, "darwin", refused, ""},
		"no route, passed":                        {none, "darwin", passed, "want it to fail so that no agent starts"},
		"no route, warned":                        {none, "darwin", warned, "want it to fail so that no agent starts"},
		"no route, a refusal without a fix":       {none, "darwin", daemon.Check{Name: daemon.PermissionRulesName, Problem: refused.Problem}, `want it to say "Bash(asmai *)"`},
		"no route, a refusal that blames nothing": {none, "darwin", daemon.Check{Name: daemon.PermissionRulesName, Problem: "it is not working", Fix: refused.Fix}, "`asmai` cannot run unprompted"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := probeVerdictHolds(c.verdict, c.goos, c.check)
			switch {
			case c.want == "" && err != nil:
				t.Errorf("a coherent verdict was rejected: %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Errorf("probeVerdictHolds = %v, want an error saying %q", err, c.want)
			}
		})
	}
}

func TestOnlyAFailedPermissionRulesCheckIsAVerdictOfC55(t *testing.T) {
	rules := daemon.Check{Name: daemon.PermissionRulesName}
	other := daemon.Check{Name: "directory trust"}
	ok := daemon.Check{Name: "provider", OK: true}
	for name, c := range map[string]struct {
		checks []daemon.Check
		want   bool
	}{
		"only the permission rules": {[]daemon.Check{ok, rules}, true},
		"another check as well":     {[]daemon.Check{rules, other}, false},
		"another check alone":       {[]daemon.Check{ok, other}, false},
		"nothing failed":            {[]daemon.Check{ok}, false},
	} {
		if got := onlyPermissionRules(c.checks); got != c.want {
			t.Errorf("%s: onlyPermissionRules = %v, want %v", name, got, c.want)
		}
	}
}

func TestAManagedSessionHasNoAllowRulesAndKeepsTheRestOfItsSettings(t *testing.T) {
	args := providers.ClaudeCodeArgs("hook", "sonnet", "instructions", "", providers.WriteGuard{Socket: "/state/daemon.sock"}, providers.Permissions{Unattended: true, ManagedOnly: true, Editable: []string{"/work"}})
	stripped, err := withoutAllowRules(args)
	if err != nil {
		t.Fatal(err)
	}
	var before, after struct {
		Permissions struct {
			Allow       []string `json:"allow"`
			DefaultMode string   `json:"defaultMode"`
		} `json:"permissions"`
		Sandbox struct {
			Excluded []string `json:"excludedCommands"`
			Network  struct {
				Sockets []string `json:"allowUnixSockets"`
			} `json:"network"`
		} `json:"sandbox"`
	}
	for settings, into := range map[string]any{argAfter(args, "--settings"): &before, argAfter(stripped, "--settings"): &after} {
		if err := json.Unmarshal([]byte(settings), into); err != nil {
			t.Fatal(err)
		}
	}
	if len(before.Permissions.Allow) == 0 {
		t.Fatal("a session's settings have no allow rules to take out")
	}
	if len(after.Permissions.Allow) != 0 || after.Permissions.DefaultMode != "acceptEdits" || slices.Contains(after.Sandbox.Excluded, "asmai *") || !slices.Contains(after.Sandbox.Network.Sockets, "/state/daemon.sock") {
		t.Errorf("the session's settings are %+v, want no allow rule, acceptEdits, no exclusion of asmai and the daemon's socket allowed", after)
	}
	if !slices.Equal(before.Sandbox.Excluded, after.Sandbox.Excluded) {
		t.Errorf("taking out the allow rules changed the exclusions: %v, %v", before.Sandbox.Excluded, after.Sandbox.Excluded)
	}
	if _, err := withoutAllowRules([]string{"-p"}); err == nil {
		t.Error("a command line with no --settings was accepted")
	}
}

// statusSession plays an Engineering worker for C56. It checks the command
// line it is given is the one a worker has under the managed policy, and runs
// the command the prompt names with the asmai under test, in the factory's
// own home, as the sandbox lets it: unless blind, which has the sandbox keep
// the command from the daemon's socket, as a platform whose sandbox cannot
// allow one socket does. retry, denied and request play a session that
// retried outside the sandbox, was denied, or raised a native prompt.
type statusSession struct {
	t                          *testing.T
	blind, retry, denied, fail bool
	request, never             bool
}

func (s statusSession) ask(spec AskSpec) (Turn, error) {
	t := s.t
	var settings struct {
		Permissions struct {
			Allow       []string `json:"allow"`
			DefaultMode string   `json:"defaultMode"`
		} `json:"permissions"`
		Sandbox struct {
			Excluded []string `json:"excludedCommands"`
			Network  struct {
				Sockets []string `json:"allowUnixSockets"`
			} `json:"network"`
			AutoAllow bool `json:"autoAllowBashIfSandboxed"`
		} `json:"sandbox"`
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(argAfter(spec.Args, "--settings")), &settings); err != nil {
		t.Fatal(err)
	}
	if len(settings.Permissions.Allow) != 0 || settings.Permissions.DefaultMode != "acceptEdits" || !settings.Sandbox.AutoAllow ||
		slices.Contains(settings.Sandbox.Excluded, "asmai *") || len(settings.Sandbox.Network.Sockets) != 1 || !strings.HasSuffix(settings.Sandbox.Network.Sockets[0], "daemon.sock") {
		t.Errorf("the session's settings are %+v, want a worker's under the managed policy: no allow rule, acceptEdits, sandbox auto-allow, asmai not left out of the sandbox and the daemon's socket allowed", settings)
	}
	payload := `{"hook_event_name":"Stop"}`
	if s.request {
		payload = `{"hook_event_name":"PermissionRequest","tool_name":"Bash"}`
	}
	hook := exec.Command("sh", "-c", settings.Hooks["Stop"][0].Hooks[0].Command)
	hook.Stdin = strings.NewReader(payload)
	if out, err := hook.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if s.never {
		return Turn{Reply: "done"}, nil
	}
	_, command, _ := strings.Cut(spec.Prompt, "The command: `")
	command = strings.TrimSuffix(command, "`")
	if !strings.Contains(strings.Join(spec.Env, " "), "PATH=") {
		t.Errorf("the session's environment %v has no PATH that finds asmai", spec.Env)
	}
	c := Command{Text: command, Unsandboxed: s.retry}
	switch {
	case s.denied:
		c.Denied, c.Failed, c.Output = true, true, "Permission to use Bash has been denied because Claude Code is running in don't ask mode."
	case s.fail:
		c.Failed, c.Output = true, "asmai: command not found"
	case s.blind:
		c.Output = `{"daemon":{"running":false},"state_dir":"/somewhere"}`
	default:
		run := exec.Command("sh", "-c", command)
		run.Env = append(os.Environ(), spec.Env...)
		out, err := run.CombinedOutput()
		c.Output, c.Failed = string(out), err != nil
	}
	return Turn{Commands: []Command{c}, Reply: "done"}, nil
}

func statusHarness(t *testing.T, s statusSession) *Harness {
	t.Helper()
	s.t = t
	checkGuardDependencies = func() error { return nil }
	t.Cleanup(func() { checkGuardDependencies = guardDependencies })
	h := harness(t, fakeClaude{auth: AuthStatus{SignedIn: true, Method: "claude.ai"}, ask: s.ask}, startsDirectly)
	h.Env = envValue(h.Env, "PATH", filepath.Dir(asmaiBin)+string(os.PathListSeparator)+os.Getenv("PATH"))
	return h
}

func TestC56PassesWhenAsmaiRunsSandboxedAndReachesTheDaemonWithNoAllowRule(t *testing.T) {
	r := result(t, statusHarness(t, statusSession{}), "C56")
	if r.Outcome != Passed {
		t.Fatalf("C56 %s: %s", r.Outcome, r.Failure)
	}
	requireEvidence(t, r, "on "+runtime.GOOS+", an Engineering worker's real session in acceptEdits mode with its allow rules taken out, as the managed policy ignores them, ran `asmai status --json` sandboxed, never retried outside the sandbox", "and it reached the daemon (pid ", "the session's hooks reported no PermissionRequest")
	if runtime.GOOS != "darwin" {
		requireEvidence(t, r, "ADR 0018 leaves the sandbox route unconfirmed there")
	}
}

// It also qualifies under the managed policy itself: the check that refuses
// there leaves the daemon running, and the session is the same.
func TestC56RunsUnderAPolicyThatLeavesNoRoute(t *testing.T) {
	h := withEnv(statusHarness(t, statusSession{}), fakeprovider.ManagedRulesOnlyEnv+"=1", fakeprovider.ManagedSandboxOffEnv+"=1", fakeprovider.PlatformEnv+"=darwin")
	r := result(t, h, "C56")
	if r.Outcome != Passed {
		t.Fatalf("C56 %s: %s", r.Outcome, r.Failure)
	}
	requireEvidence(t, r, "the account's managed policy leaves no route for `asmai` (C55)")
}

func TestC56FailsWhenTheSandboxCannotRunAsmaiUnprompted(t *testing.T) {
	for name, c := range map[string]struct {
		session statusSession
		want    string
	}{
		"the command is retried outside the sandbox": {statusSession{retry: true}, "was retried outside the sandbox (dangerouslyDisableSandbox)"},
		"the command is denied":                      {statusSession{denied: true}, "was denied in the worker's session, which has no allow rule for it"},
		"the command fails":                          {statusSession{fail: true}, "failed in the worker's session: asmai: command not found"},
		"the command never runs":                     {statusSession{never: true}, "inconclusive: the session never ran `asmai status --json`"},
		"the daemon's socket cannot be reached":      {statusSession{blind: true}, "the command did not reach the daemon's socket from the sandbox"},
		"a native prompt is raised":                  {statusSession{request: true}, "raised a native permission prompt"},
	} {
		t.Run(name, func(t *testing.T) {
			r := result(t, statusHarness(t, c.session), "C56")
			if r.Outcome != Failed || !strings.Contains(r.Failure, c.want) {
				t.Errorf("C56 %s: %q, want it to fail with %q", r.Outcome, r.Failure, c.want)
			}
			if runtime.GOOS != "darwin" && !strings.Contains(r.Failure, "ADR 0018 leaves unconfirmed") && c.session.blind {
				t.Errorf("C56's failure %q does not say this is the route the ADR leaves unconfirmed on %s", r.Failure, runtime.GOOS)
			}
		})
	}
}

// relayLeader scripts Coordination's leader for C57: it raises a native
// prompt for each of the two files, with the menu starting on selected, takes
// the keys the case types, and writes the first file only when it is allowed
// to, as Claude Code does. A leader that writes the file even when the prompt
// is cancelled is the one the case must refuse; one that raises no prompt for
// the second write is one that was given an allow rule.
type relayLeader struct {
	// menu is the choice the prompts start on, from 1.
	menu int
	// writesAnyway writes the second file though Escape cancelled it, and
	// ignoresEnter does not write the first though Enter took the Yes.
	writesAnyway, ignoresEnter bool
	// remembers has the second write run with no prompt, as if allowed.
	remembers bool
	// noPrompt has the leader reply with no prompt at all.
	noPrompt bool
}

// expectKeys is the script step that waits for keys.
func expectKeys(keys []byte) string {
	encoded, _ := json.Marshal(map[string]string{"expect": string(keys)})
	return string(encoded)
}

func relayScripts(t *testing.T, h *Harness, scripts string, l relayLeader) {
	t.Helper()
	h.Number = func() int64 { return 57 }
	allowFile, denyFile := "qualification-relay-allow.txt", "qualification-relay-deny.txt"
	menu := func(file string) string {
		options := []string{"1. Yes", "2. Yes, and switch to accept edits (shift+tab)", "3. No"}
		var b strings.Builder
		b.WriteString("\\u001b[2J\\u001b[H❯ " + relayWrite(file, 57) + "\\r\\nDo you want to create " + file + "?\\r\\n")
		for i, option := range options {
			if i+1 == l.menu {
				b.WriteString(" ❯ " + option + "\\r\\n")
			} else {
				b.WriteString("   " + option + "\\r\\n")
			}
		}
		return `{"screen":"` + b.String() + `"}`
	}
	permission := func(file string) string {
		return `{"hook":"PermissionRequest","payload":{"hook_event_name":"PermissionRequest","session_id":"fake-session","tool_name":"Write","tool_input":{"file_path":"/state/agents/leader@coordination/` + file + `"}}}`
	}
	allowKeys, _ := daemon.PromptKeys("Allow once")
	denyKeys, _ := daemon.PromptKeys("Deny")
	write := func(file string) string { return scriptRun("printf 'ok\\n' > "+file, 0, "") }
	steps := []string{fmt.Sprintf(`{"expect":%q}`, relayWrite(allowFile, 57)), `{"expect":"\r"}`, hook("UserPromptSubmit", "", relayWrite(allowFile, 57))}
	switch {
	case l.noPrompt:
		steps = append(steps, `{"screen":"\u001b[2J\u001b[HI cannot write files.\r\n> "}`, hook("Stop", "", ""))
	default:
		steps = append(steps, permission(allowFile), menu(allowFile), expectKeys(allowKeys))
		if !l.ignoresEnter {
			steps = append(steps, write(allowFile))
		}
		steps = append(steps, `{"screen":"\u001b[2J\u001b[Hdone\r\n> "}`, hook("Stop", "", ""))
		steps = append(steps, fmt.Sprintf(`{"expect":%q}`, relayWrite(denyFile, 57)), `{"expect":"\r"}`, hook("UserPromptSubmit", "", relayWrite(denyFile, 57)))
		if l.remembers {
			steps = append(steps, write(denyFile), `{"screen":"\u001b[2J\u001b[Hdone\r\n> "}`, hook("Stop", "", ""))
		} else {
			steps = append(steps, permission(denyFile), menu(denyFile), expectKeys(denyKeys))
			if l.writesAnyway {
				steps = append(steps, write(denyFile))
			}
			steps = append(steps, `{"screen":"\u001b[2J\u001b[HCancelled.\r\n> "}`, hook("Stop", "", ""))
		}
	}
	steps = append(steps, `{"expect":"only typed by a test that means to"}`)
	writeSessionScript(t, scripts, "coordination-1", scriptedLeader(steps...)...)
}

func TestC57PassesWhenEnterTakesTheOneTimeYesEscapeCancelsAndNeitherAddsARule(t *testing.T) {
	scripts := t.TempDir()
	h := dashboardHarness(t, scripts)
	relayScripts(t, h, scripts, relayLeader{menu: 1})

	r := result(t, h, "C57")

	if r.Outcome != Passed {
		t.Fatalf("C57 %s: %s", r.Outcome, r.Failure)
	}
	requireEvidence(t, r,
		"the keys the daemon types for Allow once, Enter, taken at a real native permission prompt of leader@coordination whose selected choice was the one-time Yes, ran the Write tool: qualification-relay-allow.txt was written",
		"a second write raised a native prompt again, so Allow once added no allow rule, and the keys the daemon types for Deny, Escape, cancelled it: qualification-relay-deny.txt was not written",
	)
}

func TestC57Fails(t *testing.T) {
	for name, c := range map[string]struct {
		leader relayLeader
		want   string
	}{
		"the prompt starts on a choice that is remembered": {relayLeader{menu: 2}, "has \"2. yes, and switch to accept edits (shift+tab)\" selected, not the one-time Yes"},
		"enter does not run the tool":                      {relayLeader{menu: 1, ignoresEnter: true}, "wrote qualification-relay-allow.txt as \"\""},
		"escape does not cancel":                           {relayLeader{menu: 1, writesAnyway: true}, "Escape did not cancel it"},
		"allowing once added a rule":                       {relayLeader{menu: 1, remembers: true}, "a second prompt after the first was allowed once, which would show Allow once adds no allow rule: inconclusive: the leader finished its turn without raising a native permission prompt"},
		"no prompt is raised":                              {relayLeader{menu: 1, noPrompt: true}, "inconclusive: the leader finished its turn without raising a native permission prompt for qualification-relay-allow.txt"},
	} {
		t.Run(name, func(t *testing.T) {
			scripts := t.TempDir()
			h := dashboardHarness(t, scripts)
			relayScripts(t, h, scripts, c.leader)

			r := result(t, h, "C57")

			if r.Outcome != Failed || !strings.Contains(r.Failure, c.want) {
				t.Errorf("C57 %s: %q, want it to fail with %q", r.Outcome, r.Failure, c.want)
			}
		})
	}
}

func TestTheKeysTheRelayTypesAreTheOnesC57Qualifies(t *testing.T) {
	if keys, ok := daemon.PromptKeys("Allow once"); !ok || string(keys) != "\r" {
		t.Errorf("the keys for Allow once are %q, want Enter", keys)
	}
	if keys, ok := daemon.PromptKeys("Deny"); !ok || string(keys) != "\x1b" {
		t.Errorf("the keys for Deny are %q, want Escape", keys)
	}
	if _, ok := daemon.PromptKeys("Allow always"); ok {
		t.Error("the relay has keys for a choice that is neither of the prompt's options")
	}
}
