// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"encoding/json"
	"maps"
	"path/filepath"
	"slices"
	"strings"
)

// HookEvents are the Claude Code lifecycle events an agent session's hooks
// report to the daemon.
var HookEvents = []string{"SessionStart", "UserPromptSubmit", "PermissionRequest", "Stop", "SessionEnd"}

// AllowedAsmai is the permission rule that lets an agent run `asmai` without
// a native permission prompt.
const AllowedAsmai = "Bash(asmai:*)"

// ClaudeCodeSettings returns the settings an agent session of Claude Code is
// given with --settings: a hook for each of HookEvents that runs
// hookCommand, and the permission rule that allows `asmai`. They keep Claude
// Code's native permission prompts on, whatever the user's own settings say:
// the default permission mode, with bypassing permissions disabled.
func ClaudeCodeSettings(hookCommand string) string {
	type hook struct {
		Type    string `json:"type"`
		Command string `json:"command"`
	}
	type matcher struct {
		Hooks []hook `json:"hooks"`
	}
	hooks := map[string][]matcher{}
	for _, event := range HookEvents {
		hooks[event] = []matcher{{Hooks: []hook{{Type: "command", Command: hookCommand}}}}
	}
	settings := map[string]any{
		"hooks": hooks,
		"permissions": map[string]any{
			"allow":                        []string{AllowedAsmai},
			"defaultMode":                  "default",
			"disableBypassPermissionsMode": "disable",
		},
	}
	data, _ := json.Marshal(settings)
	return string(data)
}

// ClaudeCodeArgs returns the command line an agent session of Claude Code is
// started with: everything the session is given is on it, so nothing is
// written to ~/.claude. The settings come from ClaudeCodeSettings, Claude
// Code loads only the user's own settings files besides them, never a
// project's, and the role's instructions are appended to its system prompt.
func ClaudeCodeArgs(hookCommand, model, instructions string) []string {
	return []string{
		"--settings", ClaudeCodeSettings(hookCommand),
		"--setting-sources", "user",
		"--model", model,
		"--append-system-prompt", instructions,
	}
}

// HookCommand is the shell command a hook runs: the asmai executable at path
// with `hook`.
func HookCommand(path string) string {
	return shellQuote(path) + " hook"
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// apiKeyVariables are the provider variables that would switch an agent away
// from the user's subscription: API keys and tokens, and the switches to
// API billing through a cloud provider.
var apiKeyVariables = []string{
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_AUTH_TOKEN",
	"ANTHROPIC_FOUNDRY_API_KEY",
	"AWS_BEARER_TOKEN_BEDROCK",
	"CLAUDE_CODE_USE_BEDROCK",
	"CLAUDE_CODE_USE_VERTEX",
	"CLAUDE_CODE_USE_FOUNDRY",
	"OPENAI_API_KEY",
	"CODEX_API_KEY",
	"AZURE_OPENAI_API_KEY",
}

// IsAPIKeyVariable reports whether the environment variable name is a
// provider API-key variable, which no agent session starts with.
func IsAPIKeyVariable(name string) bool {
	if slices.Contains(apiKeyVariables, name) {
		return true
	}
	for _, prefix := range []string{"ANTHROPIC_", "OPENAI_", "CODEX_", "CLAUDE_"} {
		if strings.HasPrefix(name, prefix) && strings.HasSuffix(name, "_API_KEY") {
			return true
		}
	}
	return false
}

// SessionEnv returns an agent session's environment: environ, the daemon's
// own, without any provider API-key variable, with bin first on the PATH so
// that `asmai` is the daemon's copy, the terminal AsmAI emulates, and set
// added.
func SessionEnv(environ []string, bin string, set map[string]string) []string {
	// Without a PATH, a shell's own default stands behind bin.
	path := bin + string(filepath.ListSeparator) + "/usr/bin:/bin"
	var env []string
	for _, kv := range environ {
		name, value, _ := strings.Cut(kv, "=")
		switch {
		case IsAPIKeyVariable(name), name == "TERM", name == "COLORTERM", set[name] != "":
		case name == "PATH":
			if value != "" {
				path = bin + string(filepath.ListSeparator) + value
			}
		default:
			env = append(env, kv)
		}
	}
	env = append(env, "PATH="+path, "TERM=xterm-256color", "COLORTERM=truecolor")
	for _, name := range slices.Sorted(maps.Keys(set)) {
		env = append(env, name+"="+set[name])
	}
	return env
}
