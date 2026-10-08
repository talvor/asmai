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
	args := ClaudeCodeArgs(hook, "opus", "Be Coordination.")
	if want := []string{"--setting-sources", "user", "--model", "opus", "--append-system-prompt", "Be Coordination."}; !slices.Equal(args[2:], want) {
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
	}
	if err := json.Unmarshal([]byte(args[1]), &settings); err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "PermissionRequest", "Stop"} {
		m := settings.Hooks[event]
		if len(m) != 1 || m[0].Matcher != "" || len(m[0].Hooks) != 1 || m[0].Hooks[0].Type != "command" || m[0].Hooks[0].Command != hook {
			t.Errorf("the %s hooks are %+v, want one command, %s, for every payload", event, m, hook)
		}
	}
	p := settings.Permissions
	if !slices.Equal(p.Allow, []string{"Bash(asmai:*)"}) || p.DefaultMode != "default" || p.DisableBypassPermissionsMode != "disable" {
		t.Errorf("the permissions are %+v, want asmai allowed and the native prompts kept", p)
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
