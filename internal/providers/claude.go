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
var HookEvents = []string{"SessionStart", "UserPromptSubmit", "PermissionRequest", "Stop"}

// AllowedAsmai is the permission rule that lets an agent run `asmai` without
// a native permission prompt.
const AllowedAsmai = "Bash(asmai:*)"

// The commands an agent runs without a native permission prompt, besides the
// commands Claude Code's write guard allows: `asmai`, and selected git and gh operations with
// network access. They run outside the guard, as they need the user's own
// credentials, their network, the daemon's socket and the clone's git
// directory, which is outside the workspace. Each is both a permission rule
// and a pattern the guard leaves alone.
var unguardedCommands = []string{"asmai"}

var unguardedGHCommands = []string{
	"pr create", "pr view", "pr list", "pr checks", "pr status", "issue view", "run view", "run list", "run watch",
}

var unguardedGitSubcommands = []string{
	"add", "commit", "push", "fetch", "ls-remote", "status", "rev-parse", "merge", "var",
}

// WriteGuard is what an agent session's write guard needs to know of the
// session. The guard is Claude Code's sandbox for the commands it runs:
// writes stay in the session's working directory, and any command it does not
// allow raises the native permission prompt in the agent's terminal.
type WriteGuard struct {
	// Socket is the daemon's socket, which a guarded command may connect to
	// so that `asmai` works wherever it runs.
	Socket string
	// Writable are the directories besides the working directory and the
	// system's temporary directory that a guarded command may write, such as
	// a worker's own temporary directory.
	Writable []string
}

// ClaudeCodeSettings returns the settings an agent session of Claude Code is
// given with --settings: a hook for each of HookEvents that runs
// hookCommand, the permission rules for `asmai` and selected git and gh operations, and the
// write guard guard describes, which is always on: Claude Code does not start
// the session when it cannot guard it. They keep Claude Code's native
// permission prompts on, whatever the user's own settings say: the default
// permission mode, with bypassing permissions disabled.
func ClaudeCodeSettings(hookCommand string, guard WriteGuard) string {
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
	var allow, excluded []string
	for _, command := range unguardedCommands {
		allow = append(allow, "Bash("+command+":*)")
		excluded = append(excluded, command+" *")
	}
	allow = append(allow, "Bash(gh --version)")
	excluded = append(excluded, "gh --version")
	for _, command := range unguardedGHCommands {
		allow = append(allow, "Bash(gh "+command+":*)")
		excluded = append(excluded, "gh "+command+" *")
	}
	for _, subcommand := range unguardedGitSubcommands {
		allow = append(allow, "Bash(git "+subcommand+":*)")
		excluded = append(excluded, "git "+subcommand+" *")
	}
	sandbox := map[string]any{
		"enabled":                  true,
		"failIfUnavailable":        true,
		"autoAllowBashIfSandboxed": true,
		"excludedCommands":         excluded,
	}
	if len(guard.Writable) > 0 {
		sandbox["filesystem"] = map[string]any{"allowWrite": guard.Writable}
	}
	if guard.Socket != "" {
		sandbox["network"] = map[string]any{"allowUnixSockets": []string{guard.Socket}}
	}
	settings := map[string]any{
		"hooks": hooks,
		"permissions": map[string]any{
			"allow":                        allow,
			"defaultMode":                  "default",
			"disableBypassPermissionsMode": "disable",
		},
		"sandbox": sandbox,
	}
	data, _ := json.Marshal(settings)
	return string(data)
}

// ClaudeCodeArgs returns the command line an agent session of Claude Code is
// started with: everything the session is given is on it, so nothing is
// written to ~/.claude. The settings come from ClaudeCodeSettings. Claude Code
// loads only the user's own settings files besides them, never a
// repository's, and no MCP server but those given on the command line, which
// are none: so none of the repository's provider configuration loads. The
// role's instructions are appended to its system prompt, with the
// repository's instruction files when the session works in a repository.
func ClaudeCodeArgs(hookCommand, model, instructions string, guard WriteGuard) []string {
	return []string{
		"--settings", ClaudeCodeSettings(hookCommand, guard),
		"--setting-sources", "user",
		"--strict-mcp-config",
		"--model", model,
		"--append-system-prompt", instructions,
	}
}

// HookCommand is the shell command a hook runs: the asmai executable at path
// with `hook`.
func HookCommand(path string) string {
	return ShellQuote(path) + " hook"
}

// ShellQuote quotes s as one word of a POSIX shell command.
func ShellQuote(s string) string {
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

// APIKeyVariables returns the names of the provider variables that would
// switch an agent away from the user's subscription, which no agent session
// starts with. IsAPIKeyVariable also covers names that follow their pattern.
func APIKeyVariables() []string {
	return slices.Clone(apiKeyVariables)
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
