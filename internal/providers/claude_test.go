// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestASessionsSettingsCarryItsHooksAndAllowAsmai(t *testing.T) {
	hook := HookCommand("/home/it's/.local/state/asmai/bin/asmai")
	if want := `'/home/it'\''s/.local/state/asmai/bin/asmai' hook`; hook != want {
		t.Errorf("the hook command is %s, want %s", hook, want)
	}
	guard := WriteGuard{Socket: "/state/daemon.sock", Writable: []string{"/state/workspaces/job-1/assignment-1/tmp"}}
	args := ClaudeCodeArgs(hook, "opus", "Be Coordination.", guard)
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
			Network struct {
				AllowUnixSockets []string `json:"allowUnixSockets"`
				AllowedDomains   []string `json:"allowedDomains"`
			} `json:"network"`
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
	if !slices.Equal(p.Allow, []string{"Bash(asmai:*)", "Bash(git:*)", "Bash(gh:*)"}) || p.DefaultMode != "default" || p.DisableBypassPermissionsMode != "disable" {
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
		"a worker's": {Socket: "/state/daemon.sock", Writable: []string{"/state/tmp"}},
		"a leader's": {Socket: "/state/daemon.sock"},
		"no socket":  {},
	} {
		settings.Sandbox.Filesystem, settings.Sandbox.Network = nil, nil
		if err := json.Unmarshal([]byte(ClaudeCodeSettings("hook", guard)), &settings); err != nil {
			t.Fatal(err)
		}
		sb := settings.Sandbox
		if !sb.Enabled || !sb.FailIfUnavailable || !sb.AutoAllowBashIfSandboxed {
			t.Errorf("%s sandbox is %+v, want it enabled, auto-allowing what it guards, and failing rather than running unguarded", name, sb)
		}
		// Commands the guard does not guard are exactly those allowed to run
		// without a prompt; every other command is either guarded or raises
		// the prompt.
		if want := []string{"asmai *", "git *", "gh *"}; !slices.Equal(sb.ExcludedCommands, want) {
			t.Errorf("%s sandbox leaves %q alone, want %q", name, sb.ExcludedCommands, want)
		}
		if len(settings.Permissions.Ask) != 0 {
			t.Errorf("%s permissions ask for %q", name, settings.Permissions.Ask)
		}
		if _, ok := sb.Filesystem["allowWrite"]; ok != (len(guard.Writable) > 0) {
			t.Errorf("%s sandbox filesystem is %v for writable %q", name, sb.Filesystem, guard.Writable)
		}
		if got, ok := sb.Network["allowUnixSockets"]; ok != (guard.Socket != "") || (ok && !slices.Equal(anyStrings(got), []string{guard.Socket})) {
			t.Errorf("%s sandbox network is %v for socket %q", name, sb.Network, guard.Socket)
		}
		if _, ok := sb.Network["allowedDomains"]; ok {
			t.Errorf("%s sandbox allows network domains: %v", name, sb.Network)
		}
	}
}

func anyStrings(v any) []string {
	var out []string
	for _, e := range v.([]any) {
		out = append(out, e.(string))
	}
	return out
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
