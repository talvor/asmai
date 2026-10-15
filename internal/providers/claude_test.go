// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"encoding/json"
	"os"
	"path/filepath"
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
	args := ClaudeCodeArgs(hook, "opus", "Be Coordination.", guard, Permissions{})
	if want := []string{"--setting-sources", "user", "--strict-mcp-config", "--model", "opus", "--append-system-prompt", "Be Coordination."}; !slices.Equal(args[2:], want) {
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
		"Bash(asmai:*)", "Bash(gh pr create:*)", "Bash(gh pr view:*)", "Bash(gh pr list:*)",
		"Bash(gh pr checks:*)", "Bash(gh pr status:*)", "Bash(gh issue view:*)", "Bash(gh run view:*)",
		"Bash(gh run list:*)", "Bash(gh run watch:*)", "Bash(git add:*)", "Bash(git commit:*)",
		"Bash(git push:*)", "Bash(git fetch:*)", "Bash(git ls-remote:*)", "Bash(git status:*)",
		"Bash(git rev-parse:*)", "Bash(git merge:*)", "Bash(git var:*)",
	}) || p.DefaultMode != "default" || p.DisableBypassPermissionsMode != "disable" {
		t.Errorf("the permissions are %+v, want asmai, git and gh allowed and the native prompts kept", p)
	}
	if !slices.Contains(p.Allow, AllowedAsmai) {
		t.Errorf("the permissions %q lack %s", p.Allow, AllowedAsmai)
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
			"asmai *", "echo *", "gh pr create *", "gh pr view *", "gh pr list *", "gh pr checks *",
			"gh pr status *", "gh issue view *", "gh run view *", "gh run list *", "gh run watch *",
			"git add *", "git commit *", "git push *", "git fetch *", "git ls-remote *",
			"git status *", "git rev-parse *", "git merge *", "git var *",
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
	fresh := ClaudeCodeArgs(hook, "opus", "Be a worker.", WriteGuard{}, worker)
	resumed := ClaudeCodeResumeArgs(hook, "opus", "Be a worker.", WriteGuard{}, worker, "6b8b4567-327b-4c23-9e0d-2ed1e8c0a8f3")

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
		"Coordination's leader": {Permissions{}, "default", nil},
		"another leader":        {Permissions{Unattended: true}, "dontAsk", nil},
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
		return ProbePermissionRules(t.Context(), path, t.TempDir(), nil)
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
