// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"encoding/json"
	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/skills"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestASessionsSettingsCarryItsHooksAndAllowAsmai(t *testing.T) {
	hook := HookCommand("/home/it's/.local/state/asmai/bin/asmai")
	if want := `'/home/it'\''s/.local/state/asmai/bin/asmai' hook`; hook != want {
		t.Errorf("the hook command is %s, want %s", hook, want)
	}
	guard := WriteGuard{Writable: []string{"/state/workspaces/job-1/assignment-1/tmp"}}
	args := ClaudeCodeArgs(hook, "opus", "Be Coordination.", "", guard, Permissions{})
	if want := []string{"--setting-sources", "", "--strict-mcp-config", "--model", "opus", "--append-system-prompt", "Be Coordination."}; !slices.Equal(args[2:], want) {
		t.Errorf("the command line is %q, want --settings, then %q", args, want)
	}
	if args[0] != "--settings" {
		t.Fatalf("the command line starts %q, want --settings", args[0])
	}
	var settings struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
		Permissions struct {
			Allow                        []string `json:"allow"`
			DefaultMode                  string   `json:"defaultMode"`
			DisableBypassPermissionsMode string   `json:"disableBypassPermissionsMode"`
		} `json:"permissions"`
		Sandbox struct {
			Enabled                  bool     `json:"enabled"`
			FailIfUnavailable        bool     `json:"failIfUnavailable"`
			AutoAllowBashIfSandboxed bool     `json:"autoAllowBashIfSandboxed"`
			ExcludedCommands         []string `json:"excludedCommands"`
			Filesystem               struct {
				AllowWrite []string `json:"allowWrite"`
			} `json:"filesystem"`
		} `json:"sandbox"`
	}
	if err := json.Unmarshal([]byte(args[1]), &settings); err != nil {
		t.Fatal(err)
	}
	if len(settings.Hooks) != 4 {
		t.Errorf("the settings have hooks for %d events, want the four an agent's session reports", len(settings.Hooks))
	}
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "PermissionRequest", "Stop"} {
		m := settings.Hooks[event]
		if len(m) != 1 || m[0].Matcher != "" || len(m[0].Hooks) != 1 || m[0].Hooks[0].Type != "command" || m[0].Hooks[0].Command != hook {
			t.Errorf("the %s hooks are %+v, want one command, %s, for every payload", event, m, hook)
		}
	}
	p := settings.Permissions
	if !slices.Equal(p.Allow, []string{
		"Bash(asmai:*)", "Bash(gh pr view:*)", "Bash(gh pr list:*)",
		"Bash(gh pr checks:*)", "Bash(gh pr status:*)", "Bash(gh issue view:*)", "Bash(gh run view:*)",
		"Bash(gh run list:*)", "Bash(gh run watch:*)", "Bash(git ls-remote:*)", "Bash(git status:*)",
		"Bash(git rev-parse:*)", "Bash(git var:*)",
	}) || p.DefaultMode != "default" || p.DisableBypassPermissionsMode != "disable" {
		t.Errorf("the permissions are %+v, want asmai and the read-only git and gh allowed and the native prompts kept", p)
	}
	if !slices.Contains(p.Allow, AllowedAsmai) {
		t.Errorf("the permissions %q lack %s", p.Allow, AllowedAsmai)
	}
}

// readOnlyGit and readOnlyGH are the git subcommands and gh commands an agent
// may be given a rule for: each only reads. Every write an agent needs, a
// commit, a merge, a push, a pull request open and an edit, is the daemon's,
// asked for with an `asmai` command (ADR 0018).
var (
	readOnlyGit = []string{"ls-remote", "status", "rev-parse", "var"}
	readOnlyGH  = []string{"pr view", "pr list", "pr checks", "pr status", "issue view", "run view", "run list", "run watch"}
)

func TestNoAgentSessionIsGivenAGitOrGHWriteRuleWithTheManagedPolicyOrWithoutIt(t *testing.T) {
	var settings struct {
		Sandbox     struct{ ExcludedCommands []string } `json:"sandbox"`
		Permissions struct{ Allow, Ask []string }       `json:"permissions"`
	}
	rule := regexp.MustCompile(`^Bash\((git|gh) ([a-z-]+(?: [a-z-]+)?):\*\)$`)
	excluded := regexp.MustCompile(`^(git|gh) ([a-z-]+(?: [a-z-]+)?) \*$`)
	for name, perms := range map[string]Permissions{
		"a leader's":                      {},
		"an unattended worker's":          {Unattended: true, Editable: []string{"/w"}},
		"a worker's under the policy":     {Unattended: true, Editable: []string{"/w"}, ManagedOnly: true},
		"a leader's under the policy":     {ManagedOnly: true},
		"an unattended leader's":          {Unattended: true},
		"an unattended leader's, managed": {Unattended: true, ManagedOnly: true},
	} {
		settings.Sandbox.ExcludedCommands, settings.Permissions.Allow, settings.Permissions.Ask = nil, nil, nil
		if err := json.Unmarshal([]byte(ClaudeCodeSettings("hook", WriteGuard{Writable: []string{"/tmp/w"}, Socket: "/run/daemon.sock"}, perms)), &settings); err != nil {
			t.Fatal(err)
		}
		checked := 0
		for _, allowed := range settings.Permissions.Allow {
			if !strings.HasPrefix(allowed, "Bash(git") && !strings.HasPrefix(allowed, "Bash(gh") {
				continue
			}
			checked++
			m := rule.FindStringSubmatch(allowed)
			if m == nil {
				t.Errorf("%s permissions allow %q, which is no exact read-only git or gh command", name, allowed)
			} else if list := map[string][]string{"git": readOnlyGit, "gh": readOnlyGH}[m[1]]; !slices.Contains(list, m[2]) {
				t.Errorf("%s permissions allow `%s %s`, which is not read-only", name, m[1], m[2])
			}
		}
		for _, left := range settings.Sandbox.ExcludedCommands {
			if !strings.HasPrefix(left, "git") && !strings.HasPrefix(left, "gh") {
				continue
			}
			checked++
			m := excluded.FindStringSubmatch(left)
			if m == nil {
				t.Errorf("%s sandbox leaves %q alone, which is no exact read-only git or gh command", name, left)
			} else if list := map[string][]string{"git": readOnlyGit, "gh": readOnlyGH}[m[1]]; !slices.Contains(list, m[2]) {
				t.Errorf("%s sandbox leaves `%s %s` alone, which is not read-only", name, m[1], m[2])
			}
		}
		if checked == 0 {
			t.Errorf("%s settings have no git or gh rule at all, so the test checked nothing", name)
		}
		for _, want := range []string{"add", "commit", "push", "merge", "fetch", "pull", "checkout", "reset", "rebase", "tag"} {
			if slices.Contains(settings.Permissions.Allow, "Bash(git "+want+":*)") {
				t.Errorf("%s permissions allow `git %s`", name, want)
			}
		}
		for _, want := range []string{"pr create", "pr edit", "pr ready", "pr merge", "pr comment", "pr review", "issue create", "issue comment"} {
			if slices.Contains(settings.Permissions.Allow, "Bash(gh "+want+":*)") {
				t.Errorf("%s permissions allow `gh %s`", name, want)
			}
		}
		if len(settings.Permissions.Ask) != 0 {
			t.Errorf("%s permissions ask for %q", name, settings.Permissions.Ask)
		}
	}
}

func TestASessionsWriteGuardIsAlwaysOnAndKeepsTheNativePromptsForTheRest(t *testing.T) {
	var settings struct {
		Sandbox struct {
			Enabled                  bool           `json:"enabled"`
			FailIfUnavailable        bool           `json:"failIfUnavailable"`
			AutoAllowBashIfSandboxed bool           `json:"autoAllowBashIfSandboxed"`
			ExcludedCommands         []string       `json:"excludedCommands"`
			Filesystem               map[string]any `json:"filesystem"`
			Network                  map[string]any `json:"network"`
		} `json:"sandbox"`
		Permissions struct {
			Allow []string `json:"allow"`
			Ask   []string `json:"ask"`
		} `json:"permissions"`
	}
	for name, guard := range map[string]WriteGuard{
		"a worker's": {Writable: []string{"/state/tmp"}},
		"a leader's": {},
	} {
		settings.Sandbox.Filesystem, settings.Sandbox.Network = nil, nil
		if err := json.Unmarshal([]byte(ClaudeCodeSettings("hook", guard, Permissions{})), &settings); err != nil {
			t.Fatal(err)
		}
		sb := settings.Sandbox
		if !sb.Enabled || !sb.FailIfUnavailable || !sb.AutoAllowBashIfSandboxed {
			t.Errorf("%s sandbox is %+v, want it enabled, auto-allowing what it guards, and failing rather than running unguarded", name, sb)
		}
		// Commands the guard does not guard are exactly those allowed to run
		// without a prompt; every other command is either guarded or raises
		// the prompt.
		if want := []string{
			"asmai *", "echo *", "cd *", "gh pr view *", "gh pr list *", "gh pr checks *",
			"gh pr status *", "gh issue view *", "gh run view *", "gh run list *", "gh run watch *",
			"git ls-remote *", "git status *", "git rev-parse *", "git var *",
		}; !slices.Equal(sb.ExcludedCommands, want) {
			t.Errorf("%s sandbox leaves %q alone, want %q", name, sb.ExcludedCommands, want)
		}
		if len(settings.Permissions.Ask) != 0 {
			t.Errorf("%s permissions ask for %q", name, settings.Permissions.Ask)
		}
		if _, ok := sb.Filesystem["allowWrite"]; ok != (len(guard.Writable) > 0) {
			t.Errorf("%s sandbox filesystem is %v for writable %q", name, sb.Filesystem, guard.Writable)
		}
		if len(sb.Network) != 0 {
			t.Errorf("%s sandbox has network allowances: %v", name, sb.Network)
		}
	}
}

func TestASessionsWriteGuardLetsItReachTheDaemonsSocketAndNoOther(t *testing.T) {
	// The socket of a state directory ASMAI_STATE_DIR names, not the
	// default one.
	socket := "/Users/it's/scratch/asmai-state/asmai.sock"
	var settings struct {
		Sandbox map[string]json.RawMessage `json:"sandbox"`
	}
	if err := json.Unmarshal([]byte(ClaudeCodeSettings("hook", WriteGuard{Writable: []string{"/state/tmp"}, Socket: socket}, Permissions{})), &settings); err != nil {
		t.Fatal(err)
	}
	var network map[string]json.RawMessage
	if err := json.Unmarshal(settings.Sandbox["network"], &network); err != nil {
		t.Fatalf("the sandbox's network settings %s: %v", settings.Sandbox["network"], err)
	}
	if len(network) != 1 {
		t.Errorf("the sandbox's network settings are %s, want only the Unix sockets it allows", settings.Sandbox["network"])
	}
	var sockets []string
	if err := json.Unmarshal(network["allowUnixSockets"], &sockets); err != nil {
		t.Fatalf("the sandbox's Unix sockets %s: %v", network["allowUnixSockets"], err)
	}
	if !slices.Equal(sockets, []string{socket}) {
		t.Errorf("the sandbox allows the Unix sockets %q, want exactly the daemon's, %q", sockets, socket)
	}
	// The socket is the only allowance it adds: the guard is otherwise as
	// it is without one.
	var without struct {
		Sandbox map[string]json.RawMessage `json:"sandbox"`
	}
	if err := json.Unmarshal([]byte(ClaudeCodeSettings("hook", WriteGuard{Writable: []string{"/state/tmp"}}, Permissions{})), &without); err != nil {
		t.Fatal(err)
	}
	delete(settings.Sandbox, "network")
	if len(settings.Sandbox) != len(without.Sandbox) {
		t.Errorf("with the daemon's socket the sandbox's settings are %v, want those without it, %v, and the socket", settings.Sandbox, without.Sandbox)
	}
	for key, value := range without.Sandbox {
		if string(settings.Sandbox[key]) != string(value) {
			t.Errorf("with the daemon's socket the sandbox's %s is %s, want %s as without it", key, settings.Sandbox[key], value)
		}
	}
}

func TestASessionsEnvironmentHasNoProviderAPIKey(t *testing.T) {
	environ := []string{
		"HOME=/home/u", "PATH=/usr/bin", "TERM=screen", "ANTHROPIC_API_KEY=x", "ANTHROPIC_AUTH_TOKEN=x",
		"OPENAI_API_KEY=x", "CODEX_API_KEY=x", "CLAUDE_CODE_USE_BEDROCK=1", "ANTHROPIC_SOMETHING_API_KEY=x",
		"CLAUDE_CODE_OAUTH_TOKEN=kept", "ASMAI_SESSION=old",
	}
	env := SessionEnv(environ, "/state/bin", map[string]string{"ASMAI_SESSION": "new"})
	want := []string{
		"HOME=/home/u", "CLAUDE_CODE_OAUTH_TOKEN=kept", "PATH=/state/bin:/usr/bin",
		"TERM=xterm-256color", "COLORTERM=truecolor", "ASMAI_SESSION=new",
	}
	if !slices.Equal(env, want) {
		t.Errorf("the session's environment is\n%s\nwant\n%s", strings.Join(env, "\n"), strings.Join(want, "\n"))
	}
}

func TestEveryListedAPIKeyVariableIsOneAndTheListIsACopy(t *testing.T) {
	names := APIKeyVariables()
	if len(names) == 0 {
		t.Fatal("no API-key variable is listed")
	}
	for _, name := range names {
		if !IsAPIKeyVariable(name) {
			t.Errorf("%s is listed as an API-key variable, but IsAPIKeyVariable disagrees", name)
		}
	}
	names[0] = "CHANGED"
	if APIKeyVariables()[0] == "CHANGED" {
		t.Error("changing the list APIKeyVariables returned changed the list itself")
	}
}

// Resuming a session starts Claude Code with the session's own settings,
// model and instructions, as every start does, and the session to continue.
func TestAResumedSessionIsStartedWithTheSameSettingsAndTheSessionToContinue(t *testing.T) {
	hook := HookCommand("/state/bin/asmai")
	worker := Permissions{Unattended: true, Editable: []string{"/state/workspaces/job-1/assignment-1/checkout"}}
	fresh := ClaudeCodeArgs(hook, "opus", "Be a worker.", "", WriteGuard{}, worker)
	resumed := ClaudeCodeResumeArgs(hook, "opus", "Be a worker.", "", WriteGuard{}, worker, "6b8b4567-327b-4c23-9e0d-2ed1e8c0a8f3")

	if want := append(slices.Clone(fresh), "--resume", "6b8b4567-327b-4c23-9e0d-2ed1e8c0a8f3"); !slices.Equal(resumed, want) {
		t.Errorf("the resumed command line is %q, want %q", resumed, want)
	}
	if len(fresh) != 9 {
		t.Errorf("a new session's command line has %d arguments, want it unchanged by resuming: %q", len(fresh), fresh)
	}
}

func TestASessionsWriteGuardAllowsTheDaemonsSocketByItsResolvedPath(t *testing.T) {
	// Claude Code's guard checks the path a connection resolves to, so a
	// state directory under a symlink, such as macOS's /tmp, is allowed by
	// the directory it links to.
	real, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "state")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	var settings struct {
		Sandbox struct {
			Network struct {
				AllowUnixSockets []string `json:"allowUnixSockets"`
			} `json:"network"`
		} `json:"sandbox"`
	}
	if err := json.Unmarshal([]byte(ClaudeCodeSettings("hook", WriteGuard{Socket: filepath.Join(link, "daemon.sock")}, Permissions{})), &settings); err != nil {
		t.Fatal(err)
	}
	if want := []string{filepath.Join(real, "daemon.sock")}; !slices.Equal(settings.Sandbox.Network.AllowUnixSockets, want) {
		t.Errorf("the sandbox allows the Unix sockets %q, want the daemon's by its resolved path, %q", settings.Sandbox.Network.AllowUnixSockets, want)
	}
}

// permissionsOf returns the permission settings ClaudeCodeSettings gives a
// session perms describes.
func permissionsOf(t *testing.T, perms Permissions) (allow []string, mode, bypass string) {
	t.Helper()
	var settings struct {
		Permissions struct {
			Allow                        []string `json:"allow"`
			DefaultMode                  string   `json:"defaultMode"`
			DisableBypassPermissionsMode string   `json:"disableBypassPermissionsMode"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal([]byte(ClaudeCodeSettings("hook", WriteGuard{}, perms)), &settings); err != nil {
		t.Fatal(err)
	}
	p := settings.Permissions
	return p.Allow, p.DefaultMode, p.DisableBypassPermissionsMode
}

// Coordination's leader, whose terminal is the user's conversation, keeps
// the native prompts; every other agent's session, whose prompts nobody
// would answer, is denied instead, and a worker edits its own workspace and
// temporary directory without a prompt.
func TestOnlyASessionTheUserAnswersKeepsTheNativePrompts(t *testing.T) {
	workspace, tmp := "/state/workspaces/job-1/assignment-1/checkout", "/state/workspaces/job-1/assignment-1/tmp"
	common, _, _ := permissionsOf(t, Permissions{})
	for name, c := range map[string]struct {
		perms Permissions
		mode  string
		files []string
	}{
		"Coordination's leader":                          {Permissions{}, "default", nil},
		"another leader":                                 {Permissions{Unattended: true}, "dontAsk", nil},
		"another leader under the managed policy":        {Permissions{Unattended: true, ManagedOnly: true}, "acceptEdits", nil},
		"Coordination's leader under the managed policy": {Permissions{ManagedOnly: true}, "default", nil},
		"a worker under the managed policy": {Permissions{Unattended: true, ManagedOnly: true, Editable: []string{workspace}}, "acceptEdits", []string{
			"Read(//state/workspaces/job-1/assignment-1/checkout/**)", "Edit(//state/workspaces/job-1/assignment-1/checkout/**)",
		}},
		"a worker": {Permissions{Unattended: true, Editable: []string{workspace, tmp}}, "dontAsk", []string{
			"Read(//state/workspaces/job-1/assignment-1/checkout/**)", "Edit(//state/workspaces/job-1/assignment-1/checkout/**)",
			"Read(//state/workspaces/job-1/assignment-1/tmp/**)", "Edit(//state/workspaces/job-1/assignment-1/tmp/**)",
		}},
	} {
		allow, mode, bypass := permissionsOf(t, c.perms)
		if mode != c.mode || mode != c.perms.Mode() {
			t.Errorf("%s runs in the %q permission mode, want %q", name, mode, c.mode)
		}
		if bypass != "disable" {
			t.Errorf("%s may bypass permissions: %q", name, bypass)
		}
		if want := append(slices.Clone(common), c.files...); !slices.Equal(allow, want) {
			t.Errorf("%s is allowed %q, want %q", name, allow, want)
		}
	}
}

// Claude Code allows a file only when both the path asked for and the path
// it resolves to match a rule, so a directory under a symlink is allowed by
// both its paths.
func TestAWorkersFileRulesCoverItsWorkspaceByBothItsPaths(t *testing.T) {
	real, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "workspace")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	unresolved, err := filepath.Abs(link)
	if err != nil {
		t.Fatal(err)
	}
	allow, _, _ := permissionsOf(t, Permissions{Unattended: true, Editable: []string{unresolved + "/"}})
	for _, path := range []string{unresolved, real} {
		for _, tool := range []string{"Read", "Edit"} {
			if rule := tool + "(/" + path + "/**)"; !slices.Contains(allow, rule) {
				t.Errorf("the worker's rules %q lack %s", allow, rule)
			}
		}
	}
}

func TestTheProbeReportsTheRulesClaudeCodeIgnores(t *testing.T) {
	answer := func(notInEffect bool, managedOnly bool, only ...string) string {
		var rules []string
		for _, r := range CommandRules() {
			if len(only) == 0 || slices.Contains(only, r) {
				rules = append(rules, `{"behavior":"allow","source":"flagSettings","rule":"`+r+`","notInEffect":`+map[bool]string{true: "true", false: "false"}[notInEffect]+`}`)
			}
		}
		return `{"type":"control_response","response":{"subtype":"success","request_id":"asmai-permission-rules","response":{"state":{"rules":[` +
			strings.Join(rules, ",") + `],"managedOnly":` + map[bool]string{true: "true", false: "false"}[managedOnly] + `}}}}`
	}
	probe := func(t *testing.T, output string) (PermissionVerdict, error) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "claude")
		script := "#!/bin/sh\ncat > /dev/null\ncat <<'EOF'\n{\"type\":\"system\",\"subtype\":\"init\"}\n" + output + "\nEOF\n"
		if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
		return ProbePermissionRules(t.Context(), path, t.TempDir(), "/run/asmai.sock", nil)
	}

	if v, err := probe(t, answer(false, false)); err != nil || !v.Honoured() || v.ManagedOnly {
		t.Errorf("with every rule in effect, the verdict is %+v, %v, want honoured", v, err)
	}
	v, err := probe(t, answer(true, true))
	if err != nil || v.Honoured() || !v.ManagedOnly || !slices.Equal(v.Ignored, CommandRules()) {
		t.Errorf("with every rule not in effect, the verdict is %+v, %v, want all of AsmAI's rules ignored", v, err)
	}
	// An administrator who added a rule to the managed settings has it in
	// effect, though the rule AsmAI passes is still ignored.
	managed := `{"behavior":"allow","source":"policySettings","rule":"Bash(asmai:*)"}`
	rest := answer(true, true)
	rest = strings.Replace(rest, `"rules":[`, `"rules":[`+managed+`,`, 1)
	if v, err := probe(t, rest); err != nil || slices.Contains(v.Ignored, "Bash(asmai:*)") || len(v.Ignored) != len(CommandRules())-1 {
		t.Errorf("with one rule in the managed settings, the verdict is %+v, %v, want the others ignored", v, err)
	}
	if _, err := probe(t, `{"type":"control_response","response":{"subtype":"error","request_id":"asmai-permission-rules","error":"Unsupported"}}`); err == nil {
		t.Error("an error response was taken for a verdict")
	}
	if _, err := probe(t, ""); err == nil {
		t.Error("no answer was taken for a verdict")
	}
}

// The route that lets `asmai` run unprompted under the managed policy is an
// administrator's allow rule that covers it, or the sandbox's auto-allow with
// the daemon's socket in effect, which is confirmed on macOS only.
func TestTheProbeFindsTheRouteAsmaiRunsUnprompted(t *testing.T) {
	answer := func(rule string, managedOnly bool, sandbox string) string {
		rules := ""
		if rule != "" {
			rules = `{"behavior":"allow","source":"policySettings","rule":"` + rule + `"}`
		}
		if !managedOnly {
			var own []string
			for _, r := range CommandRules() {
				own = append(own, `{"behavior":"allow","source":"flagSettings","rule":"`+r+`"}`)
			}
			rules = strings.Join(own, ",")
		}
		out := `{"type":"control_response","response":{"subtype":"success","request_id":"asmai-permission-rules","response":{"state":{"rules":[` +
			rules + `],"managedOnly":` + map[bool]string{true: "true", false: "false"}[managedOnly] + `}}}}`
		if sandbox != "" {
			out += "\n" + `{"type":"control_response","response":{"subtype":"success","request_id":"asmai-effective-settings","response":{"effective":{"sandbox":` + sandbox + `}}}}`
		}
		return out
	}
	const on = `{"enabled":true,"autoAllowBashIfSandboxed":true,"network":{"allowUnixSockets":["/run/asmai.sock"]}}`
	probe := func(output string) PermissionVerdict {
		t.Helper()
		path := filepath.Join(t.TempDir(), "claude")
		script := "#!/bin/sh\ncat > /dev/null\ncat <<'EOF'\n" + output + "\nEOF\n"
		if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
		v, err := ProbePermissionRules(t.Context(), path, t.TempDir(), "/run/asmai.sock", nil)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, c := range []struct {
		name   string
		output string
		goos   string
		want   Route
	}{
		{"no policy", answer("", false, on), "darwin", RouteRules},
		{"no policy on linux", answer("", false, on), "linux", RouteRules},
		{"an allow row for asmai", answer("Bash(asmai *)", true, ""), "darwin", RouteAllowRule},
		{"the old colon form of the row", answer("Bash(asmai:*)", true, ""), "linux", RouteAllowRule},
		{"a row for all of Bash", answer("Bash", true, ""), "darwin", RouteAllowRule},
		{"a row for another command", answer("Bash(git status:*)", true, ""), "darwin", RouteNone},
		{"sandbox auto-allow with the socket", answer("", true, on), "darwin", RouteSandbox},
		{"sandbox auto-allow without the socket", answer("", true, `{"enabled":true,"autoAllowBashIfSandboxed":true}`), "darwin", RouteNone},
		{"sandbox auto-allow switched off", answer("", true, `{"enabled":true,"autoAllowBashIfSandboxed":false,"network":{"allowUnixSockets":["/run/asmai.sock"]}}`), "darwin", RouteNone},
		{"sandbox off", answer("", true, `{"enabled":false,"autoAllowBashIfSandboxed":true}`), "darwin", RouteNone},
		{"no settings answer", answer("", true, ""), "darwin", RouteNone},
		{"sandbox auto-allow on linux", answer("", true, on), "linux", RouteUnconfirmed},
		{"no sandbox auto-allow on linux", answer("", true, `{"enabled":true}`), "linux", RouteNone},
	} {
		if got := probe(c.output).Route(c.goos); got != c.want {
			t.Errorf("%s: the route is %d, want %d", c.name, got, c.want)
		}
	}
}

func TestTheInstructionsNameNoGitOrGHWriteExceptToForbidItAndEveryGHCommandTheyNameIsAllowed(t *testing.T) {
	var texts []string
	for _, role := range []string{roles.Coordination, roles.Planning, roles.Research, roles.Engineering, roles.Quality} {
		leader, err := roles.LeaderInstructions(role)
		if err != nil {
			t.Fatal(err)
		}
		texts = append(texts, leader)
		if worker, err := roles.WorkerInstructions(role); err == nil {
			texts = append(texts, worker)
		}
	}
	var settings struct {
		Sandbox     struct{ ExcludedCommands []string } `json:"sandbox"`
		Permissions struct{ Allow []string }            `json:"permissions"`
	}
	if err := json.Unmarshal([]byte(ClaudeCodeSettings("hook", WriteGuard{}, Permissions{})), &settings); err != nil {
		t.Fatal(err)
	}
	// An instruction may name a git write only to forbid it, in the one
	// sentence that says every write is the daemon's: the write verbs it
	// names are then exactly those, and no instruction tells an agent to run
	// one. A gh command it names is one the settings allow, so it is read-only.
	forbidden := regexp.MustCompile("never run `git add`, `git commit`, `git merge`, `git fetch`, `git push` or any other command")
	write := regexp.MustCompile("`(git (?:add|commit|push|merge|fetch|pull|rebase|reset|checkout|switch|stash|tag|cherry-pick|am|apply)|gh (?:pr (?:create|edit|ready|merge|close|comment|review)|issue (?:create|edit|comment|close)))\\b")
	for _, text := range texts {
		text = forbidden.ReplaceAllString(text, "")
		for _, m := range write.FindAllStringSubmatch(text, -1) {
			t.Errorf("the instructions name `%s`, a write no agent is given: the daemon makes it, for an `asmai` command", m[1])
		}
	}
	named := regexp.MustCompile("`gh ([a-z]+ [a-z]+)")
	for _, text := range texts {
		for _, m := range named.FindAllStringSubmatch(text, -1) {
			if !slices.Contains(settings.Sandbox.ExcludedCommands, "gh "+m[1]+" *") || !slices.Contains(settings.Permissions.Allow, "Bash(gh "+m[1]+":*)") {
				t.Errorf("the instructions name `gh %s`, which the settings do not both exclude from the sandbox and allow", m[1])
			}
		}
	}
}

func TestASessionsSkillsAreItsPluginAndTheUserSettingSourceIsLeftOut(t *testing.T) {
	args := ClaudeCodeArgs("hook", "opus", "Be a worker.", "/state/plugins/worker1@engineering", WriteGuard{}, Permissions{Readable: []string{"/state/plugins/worker1@engineering"}})
	i := slices.Index(args, "--setting-sources")
	if i < 0 || args[i+1] != "" {
		t.Errorf("the session was started with %q, want an empty --setting-sources: no source, so no personal skills", args)
	}
	if i := slices.Index(args, "--plugin-dir"); i < 0 || args[i+1] != "/state/plugins/worker1@engineering" {
		t.Errorf("the session was started with %q, want --plugin-dir with its plugin", args)
	}
	var settings struct {
		Permissions struct {
			Allow []string `json:"allow"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal([]byte(args[1]), &settings); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(settings.Permissions.Allow, "Read(//state/plugins/worker1@engineering/**)") {
		t.Errorf("the session's rules are %q, want its plugin's files readable", settings.Permissions.Allow)
	}
	if slices.Contains(ClaudeCodeArgs("hook", "opus", "x", "", WriteGuard{}, Permissions{}), "--plugin-dir") {
		t.Error("a session with no skills was given a plugin")
	}
}

func TestEveryRoleStartsAndResumesWithoutUserSettingSources(t *testing.T) {
	for _, role := range roles.All {
		for _, agent := range []struct {
			name string
			list []string
		}{
			{"leader", skills.LeaderList(role)},
			{"worker", skills.WorkerList(role)},
		} {
			t.Run(role+"/"+agent.name, func(t *testing.T) {
				plugin := ""
				if len(agent.list) > 0 {
					plugin = "/state/plugins/" + agent.name + "@" + role
				}
				for name, args := range map[string][]string{
					"start":  ClaudeCodeArgs("hook", "opus", "instructions", plugin, WriteGuard{}, Permissions{}),
					"resume": ClaudeCodeResumeArgs("hook", "opus", "instructions", plugin, WriteGuard{}, Permissions{}, "session-id"),
				} {
					i := slices.Index(args, "--setting-sources")
					if i < 0 || i+1 == len(args) || args[i+1] != "" {
						t.Errorf("%s: %q does not leave out user setting sources", name, args)
					}
					i = slices.Index(args, "--plugin-dir")
					if plugin == "" && i >= 0 || plugin != "" && (i < 0 || i+1 == len(args) || args[i+1] != plugin) {
						t.Errorf("%s: %q has the wrong plugin for %q", name, args, agent.list)
					}
				}
			})
		}
	}
}

func TestEverySessionHidesClaudeCodesOwnSkills(t *testing.T) {
	for _, role := range roles.All {
		for _, agent := range []struct {
			name string
			list []string
		}{
			{"leader", skills.LeaderList(role)},
			{"worker", skills.WorkerList(role)},
		} {
			plugin := ""
			if len(agent.list) > 0 {
				plugin = "/state/plugins/" + agent.name + "@" + role
			}
			for name, args := range map[string][]string{
				"start":  ClaudeCodeArgs("hook", "opus", "instructions", plugin, WriteGuard{}, Permissions{}),
				"resume": ClaudeCodeResumeArgs("hook", "opus", "instructions", plugin, WriteGuard{}, Permissions{}, "session-id"),
			} {
				var settings struct {
					DisableBundledSkills bool              `json:"disableBundledSkills"`
					SkillOverrides       map[string]string `json:"skillOverrides"`
				}
				if err := json.Unmarshal([]byte(args[1]), &settings); err != nil {
					t.Fatal(err)
				}
				if !settings.DisableBundledSkills {
					t.Errorf("%s's %s at %s keeps Claude Code's bundled skills, its code-review among them", role, agent.name, name)
				}
				want := map[string]string{"design": "off", "doctor": "off", "plugin-authoring": "off"}
				if !maps.Equal(settings.SkillOverrides, want) {
					t.Errorf("%s's %s at %s overrides the skills %v, want %v", role, agent.name, name, settings.SkillOverrides, want)
				}
				// Overrides name skills by bare name, and the session's own are
				// the plugin's, so none of its own is turned off.
				for skill := range settings.SkillOverrides {
					if slices.Contains(agent.list, skill) {
						t.Errorf("%s's %s at %s turns off %s, which is on its list", role, agent.name, name, skill)
					}
				}
			}
		}
	}
}

func TestWhatClaudeCodeCannotHideIsRecordedForDoctor(t *testing.T) {
	unhidden := ClaudeCodeUnhidden()
	if len(unhidden) == 0 {
		t.Fatal("nothing is recorded, want Claude Code's built-in slash commands")
	}
	for _, u := range unhidden {
		if u.What == "" || u.Why == "" || len(u.Commands) == 0 {
			t.Errorf("%+v does not say what is not hidden, why, and which commands C33 checks", u)
		}
		for _, command := range u.Commands {
			if slices.Contains(claudeCodeOwnSkills, command) || command == "code-review" {
				t.Errorf("%s is recorded as not hidden, but the settings hide it", command)
			}
		}
	}
	unhidden[0].Commands[0] = "changed"
	if ClaudeCodeUnhidden()[0].Commands[0] == "changed" {
		t.Error("the record is shared with its callers")
	}
}

// No agent runs Lavish (04 rule 22): no permission rule lets a session run
// it or the user's own lavish-axi without a prompt, and no excluded command
// of the write guard names it.
func TestNoPermissionRuleLetsAnAgentRunLavish(t *testing.T) {
	allow, excluded := commandRules(false)
	for _, rule := range append(allow, excluded...) {
		if strings.Contains(strings.ToLower(rule), "lavish") {
			t.Errorf("the rule %q covers Lavish", rule)
		}
	}
}

func TestUnderTheManagedPolicyAsmaiRunsSandboxedByTheResolvedSocketAndNothingElseChanges(t *testing.T) {
	real, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "state")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	guard := WriteGuard{Writable: []string{"/state/tmp"}, Socket: filepath.Join(link, "daemon.sock")}
	var settings struct {
		Sandbox struct {
			ExcludedCommands []string `json:"excludedCommands"`
			Filesystem       struct {
				AllowWrite []string `json:"allowWrite"`
			} `json:"filesystem"`
			Network struct {
				AllowUnixSockets []string `json:"allowUnixSockets"`
			} `json:"network"`
		} `json:"sandbox"`
	}
	if err := json.Unmarshal([]byte(ClaudeCodeSettings("hook", guard, Permissions{ManagedOnly: true})), &settings); err != nil {
		t.Fatal(err)
	}
	sb := settings.Sandbox
	for _, command := range []string{"asmai *", "echo *", "cd *"} {
		if slices.Contains(sb.ExcludedCommands, command) {
			t.Errorf("under the policy the sandbox still leaves %q alone: %q", command, sb.ExcludedCommands)
		}
	}
	if !slices.Contains(sb.ExcludedCommands, "gh pr view *") || !slices.Contains(sb.ExcludedCommands, "git status *") {
		t.Errorf("under the policy the sandbox leaves %q alone, want gh and git as without it", sb.ExcludedCommands)
	}
	if want := []string{filepath.Join(real, "daemon.sock")}; !slices.Equal(sb.Network.AllowUnixSockets, want) {
		t.Errorf("the sandbox allows the Unix sockets %q, want %q", sb.Network.AllowUnixSockets, want)
	}
	if !slices.Equal(sb.Filesystem.AllowWrite, []string{"/state/tmp"}) {
		t.Errorf("the sandbox allows writes to %q, want the guard's", sb.Filesystem.AllowWrite)
	}
}

func TestWithoutTheManagedPolicyTheSettingsAreUnchanged(t *testing.T) {
	guard := WriteGuard{Writable: []string{"/state/tmp"}, Socket: "/run/daemon.sock"}
	if a, b := ClaudeCodeSettings("hook", guard, Permissions{Editable: []string{"/w"}}), ClaudeCodeSettings("hook", guard, Permissions{Editable: []string{"/w"}, ManagedOnly: false}); a != b {
		t.Errorf("settings differ:\n%s\n%s", a, b)
	}
	var s struct {
		Sandbox struct {
			ExcludedCommands []string `json:"excludedCommands"`
		} `json:"sandbox"`
	}
	if err := json.Unmarshal([]byte(ClaudeCodeSettings("hook", guard, Permissions{})), &s); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(s.Sandbox.ExcludedCommands, "asmai *") {
		t.Errorf("without the policy the sandbox leaves %q alone, want asmai among them", s.Sandbox.ExcludedCommands)
	}
}
