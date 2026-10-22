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

// separatorCommands are the read-only commands an agent puts between its
// `asmai` commands in one compound command, such as `echo ----` in
// `asmai brief 1; echo ----; asmai inbox`. Claude Code takes a call out of
// the guard only when every command in it matches an exclusion, so each of
// them is excluded too: without it, the compound command stays in the guard,
// which on Linux cannot let it reach the daemon's socket, and its retry
// outside the guard raises the native permission prompt. They need no
// permission rule, as Claude Code allows read-only commands, and a redirect
// to a file keeps a call that has one in the guard. `cd` is one of them: a
// leader's working directory is not the read-only view it reads the job's code
// in, so it runs `cd <view> && gh pr create ...`, which would otherwise stay in
// the guard, where gh reaches no network host.
var separatorCommands = []string{"echo", "cd"}

var unguardedGHCommands = []string{
	"pr create", "pr edit", "pr view", "pr list", "pr checks", "pr status", "issue view", "run view", "run list", "run watch",
}

var unguardedGitSubcommands = []string{
	"add", "commit", "push", "fetch", "ls-remote", "status", "rev-parse", "merge", "var",
}

// WriteGuard is what an agent session's write guard needs to know of the
// session. The guard is Claude Code's sandbox for the commands it runs:
// writes stay in the session's working directory, and any command it does not
// allow raises the native permission prompt in the agent's terminal, or is
// denied in a session that runs in dontAsk mode.
type WriteGuard struct {
	// Writable are the directories besides the working directory and the
	// system's temporary directory that a guarded command may write, such as
	// a worker's own temporary directory.
	Writable []string
	// Socket is the daemon's Unix socket, the only Unix socket a guarded
	// command may connect to, so that an `asmai` command the guard does not
	// leave alone, such as one in a compound command with other commands,
	// still reaches the daemon. Claude Code allows it on macOS only: on
	// Linux its guard cannot allow one socket, so a guarded command reaches
	// none, and a compound command of `asmai` and separatorCommands is left
	// alone by the guard instead. Empty allows none.
	Socket string
}

// The Claude Code permission modes an agent session runs in. In the default
// mode, an action the session's rules do not allow raises the native
// permission prompt in its terminal. In dontAsk mode it is denied instead,
// with no prompt and no approval given, and the session is told so.
const (
	PermissionModeDefault = "default"
	PermissionModeDontAsk = "dontAsk"
)

// Permissions is what an agent session's permission rules need to know of
// the session, besides the commands every session runs without a prompt.
type Permissions struct {
	// Unattended is set for a session in whose terminal nobody answers a
	// native permission prompt: every agent's but Coordination's leader,
	// whose terminal is the user's conversation. It runs in dontAsk mode.
	Unattended bool
	// Editable are the directories in which the session's Edit and Write
	// tools may read, change and make files without a prompt, such as a
	// worker's workspace and its own temporary directory.
	Editable []string
	// Readable are the directories whose files the session's Read tool may
	// read without a prompt, such as the plugin its skills are delivered in:
	// a skill points at the files beside it.
	Readable []string
}

// Mode returns the permission mode of a session p describes.
func (p Permissions) Mode() string {
	if p.Unattended {
		return PermissionModeDontAsk
	}
	return PermissionModeDefault
}

// fileRules returns the Read and Edit permission rules for the files under
// each of dirs, which are absolute. Claude Code writes an absolute path with
// a leading //, and allows a file by a rule only when both the path the
// session asks for and the path it resolves to match one, so a directory
// under a symlink, such as macOS's /tmp, is also allowed by the path it
// resolves to.
func fileRules(dirs []string) []string {
	var rules []string
	for _, dir := range dirs {
		paths := []string{filepath.Clean(dir)}
		if resolved, err := filepath.EvalSymlinks(dir); err == nil && resolved != paths[0] {
			paths = append(paths, resolved)
		}
		for _, path := range paths {
			pattern := "/" + strings.TrimSuffix(path, "/") + "/**"
			rules = append(rules, "Read("+pattern+")", "Edit("+pattern+")")
		}
	}
	return rules
}

// readRules returns the Read permission rules for the files under each of
// dirs, which are absolute, written as fileRules writes them.
func readRules(dirs []string) []string {
	var rules []string
	for _, rule := range fileRules(dirs) {
		if strings.HasPrefix(rule, "Read(") {
			rules = append(rules, rule)
		}
	}
	return rules
}

// commandRules returns the permission rules every session is given for the
// commands it runs without a prompt, and the patterns the write guard leaves
// alone.
func commandRules() (allow, excluded []string) {
	for _, command := range unguardedCommands {
		allow = append(allow, "Bash("+command+":*)")
		excluded = append(excluded, command+" *")
	}
	for _, command := range separatorCommands {
		excluded = append(excluded, command+" *")
	}
	for _, command := range unguardedGHCommands {
		allow = append(allow, "Bash(gh "+command+":*)")
		excluded = append(excluded, "gh "+command+" *")
	}
	for _, subcommand := range unguardedGitSubcommands {
		allow = append(allow, "Bash(git "+subcommand+":*)")
		excluded = append(excluded, "git "+subcommand+" *")
	}
	return allow, excluded
}

// CommandRules returns the permission rules every agent session is given for
// the commands it runs without a prompt: `asmai`, and selected git and gh
// operations. Under a Claude Code managed policy that applies only managed
// permission rules, an administrator adds them to the managed settings.
func CommandRules() []string {
	allow, _ := commandRules()
	return allow
}

// claudeCodeOwnSkills are the skills of Claude Code's own that the pinned
// version still lists to a session once disableBundledSkills has removed the
// skills it bundles: design and doctor, and plugin-authoring from its
// built-in plugin of that name. skillOverrides turns each off by name, which
// leaves the session's own skills alone, as Claude Code names those with the
// plugin's name ahead of theirs. Case C33 qualifies that nothing else is
// listed.
var claudeCodeOwnSkills = []string{"design", "doctor", "plugin-authoring"}

// Unhidden is something of a pinned provider's own that AsmAI cannot keep
// from an agent session, which `asmai doctor` lists (08 rule 19).
type Unhidden struct {
	// What names it, and Why says why it is not hidden and what is done
	// about it.
	What, Why string
	// Commands are the slash commands it covers that case C33 checks the
	// session lists as commands but not as skills.
	Commands []string
}

// ClaudeCodeUnhidden returns what the pinned Claude Code cannot hide from an
// agent session. Every skill of the user's, of a repository's and of Claude
// Code's own is hidden, its built-in code-review among them, so only
// Claude Code's built-in slash commands remain.
func ClaudeCodeUnhidden() []Unhidden {
	return []Unhidden{{
		What: "Claude Code's built-in slash commands, such as /security-review and /init",
		Why: "Claude Code has no setting that removes them, so they stay listed and can be typed in an agent's terminal. " +
			"They are hidden from the agent itself, which Claude Code refuses when it tries to run one, " +
			"and the operating guide tells agents to use only the skills on their list",
		Commands: []string{"security-review", "init"},
	}}
}

// ClaudeCodeSettings returns the settings an agent session of Claude Code is
// given with --settings: a hook for each of HookEvents that runs
// hookCommand, the permission rules for `asmai` and selected git and gh
// operations and for the files in the directories perms makes editable, and
// the write guard guard describes, which is always on and lets a guarded
// command connect to no Unix socket but the daemon's: Claude Code does not
// start the session when it cannot guard it. Bypassing permissions is
// disabled, whatever the user's own settings say. The permission mode is
// perms' Mode: the default mode keeps Claude Code's native permission
// prompts, and dontAsk turns each into a denial the session sees, for a
// session whose prompts nobody would answer. Neither approves anything the
// rules do not allow. Claude Code's own skills are hidden from the session:
// the skills it bundles, its built-in code-review among them, are removed
// with disableBundledSkills, and the rest it lists, claudeCodeOwnSkills, are
// turned off with skillOverrides (08 rule 19).
func ClaudeCodeSettings(hookCommand string, guard WriteGuard, perms Permissions) string {
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
	allow, excluded := commandRules()
	allow = append(allow, fileRules(perms.Editable)...)
	allow = append(allow, readRules(perms.Readable)...)
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
		// Claude Code's guard checks the path a connection resolves to, so
		// a socket under a symlink, such as macOS's /tmp, is allowed by the
		// directory it links to.
		sandbox["network"] = map[string]any{"allowUnixSockets": []string{resolvedSocket(guard.Socket)}}
	}
	overrides := map[string]string{}
	for _, name := range claudeCodeOwnSkills {
		overrides[name] = "off"
	}
	settings := map[string]any{
		"disableBundledSkills": true,
		"skillOverrides":       overrides,
		"hooks":                hooks,
		"permissions": map[string]any{
			"allow":                        allow,
			"defaultMode":                  perms.Mode(),
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
// loads no user or repository setting sources and no MCP servers, because none
// are given on the command line. The role's instructions are appended to its
// system prompt, with repository instruction files when the session works in
// a repository.
//
// The session's skills are the plugin at pluginDir, which --plugin-dir gives
// it for this session alone, and no setting source is loaded: the user's own
// skills live in the user setting source and a repository's in
// `.claude/skills`, its project setting source, so leaving both out keeps
// them from the session, and the flag settings above, which hide Claude
// Code's own, are all it has. A session with no skills has no pluginDir.
func ClaudeCodeArgs(hookCommand, model, instructions, pluginDir string, guard WriteGuard, perms Permissions) []string {
	args := []string{
		"--settings", ClaudeCodeSettings(hookCommand, guard, perms),
		"--setting-sources", "",
		"--strict-mcp-config",
	}
	if pluginDir != "" {
		args = append(args, "--plugin-dir", pluginDir)
	}
	return append(args,
		"--model", model,
		"--append-system-prompt", instructions,
	)
}

// ClaudeCodeResumeArgs returns the command line a session of Claude Code is
// started with to resume the native session sessionID: ClaudeCodeArgs, and
// the session to continue. The settings, model and instructions are those of
// the new session, as Claude Code takes them again on every start.
func ClaudeCodeResumeArgs(hookCommand, model, instructions, pluginDir string, guard WriteGuard, perms Permissions, sessionID string) []string {
	return append(ClaudeCodeArgs(hookCommand, model, instructions, pluginDir, guard, perms), "--resume", sessionID)
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
