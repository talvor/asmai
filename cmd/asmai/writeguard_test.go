// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// userGitConfig makes git commit as a user whose identity is in their own git
// configuration, as it is for a real user, not in the environment. Nothing
// else in the environment names an identity, and the host's configuration is
// left out. The daemon takes the environment it is started with, so a test
// calls it first.
func userGitConfig(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(path, []byte("[user]\n\tname = Una User\n\temail = una@example.test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", path)
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	for _, name := range []string{
		"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL", "EMAIL",
		"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR", "GIT_PREFIX",
	} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
}

// repositoryFiles are a repository's instruction files, and the provider
// configuration that would be visible if a session loaded it: a hook that
// leaves a mark, a permission rule, an environment variable and an MCP server.
var repositoryFiles = map[string]string{
	"AGENTS.md": "Instruction canary: the repository's check is `make check`.\n",
	"CLAUDE.md": "Claude canary: commit subjects are imperative.\n",
	".claude/settings.json": `{
  "hooks": {
    "SessionStart": [{"hooks": [{"type": "command", "command": "touch \"$ASMAI_TEST_DIR/repository-hook-ran\""}]}],
    "UserPromptSubmit": [{"hooks": [{"type": "command", "command": "touch \"$ASMAI_TEST_DIR/repository-hook-ran\""}]}]
  },
  "permissions": {"allow": ["Bash(repository-rule-canary:*)"]},
  "env": {"REPOSITORY_ENV_CANARY": "loaded"}
}
`,
	".mcp.json": `{"mcpServers": {"repository-mcp-canary": {"command": "touch", "args": ["repository-mcp-ran"]}}}` + "\n",
}

// sessionArgs returns the command line of the session that agent started,
// from the journal.
func sessionArgs(t *testing.T, agent string) []string {
	t.Helper()
	for _, e := range journaled(t, "session.started") {
		start := decode[struct {
			Agent string   `json:"agent"`
			Args  []string `json:"args"`
		}](t, e)
		if start.Agent == agent {
			return start.Args
		}
	}
	t.Fatalf("the journal has no session start of %s", agent)
	return nil
}

func argValue(args []string, flag string) string {
	if i := slices.Index(args, flag); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

func TestAWorkerCommitsAsTheUserWithAsmAITrailersInsideTheWriteGuardFollowingTheRepositoryInstructionsButNotItsProviderConfiguration(t *testing.T) {
	const branch = "asmai/job-1/1"
	trailers := "AsmAI-Job: 1\nAsmAI-Agent: worker1@engineering\nAsmAI-Dispatch: 3\n"
	worker := []string{
		fakeRun(`asmai inbox --dispatch 3`, 0, "assignment 1"),
		// The identity is the user's own, from their git configuration, and
		// the session holds none of AsmAI's.
		fakeRun(`git var GIT_AUTHOR_IDENT | sed 's/ [0-9]* [-+][0-9]*$//'`, 0, "^Una User <una@example.test>\n$"),
		fakeRun(`env | grep -c '^GIT_\(AUTHOR\|COMMITTER\)_' || true`, 0, "^0\n$"),
		fakeRun(`printf 'hello\n' > greeting.txt && git add -A && git commit --no-verify -q -m 'Add greeting.txt' && git log -1 --format='%an <%ae>|%cn <%ce>%n%B'`, 0, "(?s)^Una User <una@example.test>\\|Una User <una@example.test>\nAdd greeting.txt\n\n"+trailers),
		// Amending never doubles the trailers, and a trailer written by hand
		// is replaced by the real one.
		fakeRun(`git commit --no-verify -q --amend -m 'Add greeting.txt

AsmAI-Job: 99' && git log -1 --format=%B | grep -c '^AsmAI-'`, 0, "^3\n$"),
		fakeRun(`git log -1 --format=%B | grep -c 'AsmAI-Job: 99' || true`, 0, "^0\n$"),
		fakeRun(`asmai effect commit "$(git rev-parse HEAD)"`, 0, "effect 1 recorded: commit"),
		fakeRun(`git merge --no-edit asmai/job-1-add-a-greeting`, 0, "up to date"),
		fakeRun(`git push -q origin `+branch+` && asmai effect push "origin/`+branch+`@$(git rev-parse HEAD)"`, 0, "effect 2 recorded: push"),
		fakeRun(`asmai result --evidence 'greeting.txt says hello' --test none --check none --gap none --pr-section 'Adds greeting.txt.'`, 0, "result recorded for assignment 1"),
	}
	leader := []string{
		fakeRun(`asmai inbox --dispatch 4`, 0, "result 1 \\| assignment 1 \\| worker1@engineering"),
		// A leader commits nothing, so it is given no trailers.
		fakeRun(`printf 'msg\n' > "$ASMAI_TEST_DIR/message" && asmai git-hook prepare-commit-msg "$ASMAI_TEST_DIR/message"`, 1, "a leader's work goes through its workers"),
	}
	fixture, testDir := assignmentFlowWith(t, userGitConfig, repositoryFiles, worker, leader)
	stateDir := filepath.Join(os.Getenv("HOME"), ".local", "state", "asmai")

	// Every commit the worker pushed is the user's and carries the trailers.
	pushed := gitIn(t, fixture.origin, "rev-parse", "refs/heads/"+branch)
	for _, commit := range strings.Fields(gitIn(t, fixture.origin, "rev-list", fixture.main+".."+pushed)) {
		if got := gitIn(t, fixture.origin, "log", "-1", "--format=%an <%ae>|%cn <%ce>", commit); got != "Una User <una@example.test>|Una User <una@example.test>" {
			t.Errorf("commit %s was made by %s, want the user's own identity", commit, got)
		}
		if got := gitIn(t, fixture.origin, "log", "-1", "--format=%(trailers:only,unfold)", commit); got+"\n" != trailers {
			t.Errorf("commit %s carries the trailers %q, want %q", commit, got, trailers)
		}
	}

	// AsmAI set no identity and no hooks in the clone, and wrote its hooks
	// into its own state directory.
	clone := filepath.Join(stateDir, "repositories", "fixture")
	for _, key := range []string{"user.name", "user.email", "core.hooksPath", "credential.helper"} {
		if out, err := exec.Command("git", "-C", clone, "config", "--local", "--get", key).Output(); err == nil {
			t.Errorf("AsmAI's clone sets %s to %s", key, strings.TrimSpace(string(out)))
		}
	}
	if info, err := os.Stat(filepath.Join(stateDir, "githooks", "prepare-commit-msg")); err != nil || info.Mode().Perm()&0o111 == 0 {
		t.Errorf("the prepare-commit-msg hook is %v (%v), want an executable in AsmAI's state directory", info, err)
	}

	// The worker's session was started with the write guard on, only the
	// user's own settings, no repository MCP servers, and the repository's
	// instruction files on its command line, not its provider configuration.
	args := sessionArgs(t, "worker1@engineering")
	if argValue(args, "--setting-sources") != "user" || !slices.Contains(args, "--strict-mcp-config") {
		t.Errorf("the worker was started with %q, want only the user's settings and no MCP servers beside those given", args)
	}
	instructions := argValue(args, "--append-system-prompt")
	for _, want := range []string{
		"You are an Engineering worker in AsmAI",
		"Stay inside the workspace: any write outside it is an effect to record",
		"never widen your assignment or the job's mandate",
		"# The repository's instruction files", "## AGENTS.md", "Instruction canary: the repository's check is `make check`.",
		"## CLAUDE.md", "Claude canary: commit subjects are imperative.",
	} {
		if !strings.Contains(instructions, want) {
			t.Errorf("the worker's instructions lack %q", want)
		}
	}
	for _, unwanted := range []string{"repository-hook-ran", "repository-rule-canary", "REPOSITORY_ENV_CANARY", "repository-mcp-canary"} {
		for _, arg := range args {
			if strings.Contains(arg, unwanted) {
				t.Errorf("the worker was given the repository's provider configuration (%s): %s", unwanted, arg)
			}
		}
	}
	for _, name := range []string{"repository-hook-ran", "repository-mcp-ran"} {
		if _, err := os.Stat(filepath.Join(testDir, name)); err == nil {
			t.Errorf("the repository's provider configuration ran: %s", name)
		}
	}

	// Every agent's session has the write guard on, with its commands that
	// need the user's credentials and the daemon outside it and allowed.
	for _, agent := range []string{"leader@coordination", "leader@engineering", "worker1@engineering"} {
		args := sessionArgs(t, agent)
		var settings struct {
			Permissions struct {
				Allow        []string `json:"allow"`
				DefaultMode  string   `json:"defaultMode"`
				BypassDenied string   `json:"disableBypassPermissionsMode"`
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
				} `json:"network"`
			} `json:"sandbox"`
		}
		if err := json.Unmarshal([]byte(argValue(args, "--settings")), &settings); err != nil {
			t.Fatalf("%s: %v", agent, err)
		}
		sb := settings.Sandbox
		if !sb.Enabled || !sb.FailIfUnavailable || !sb.AutoAllowBashIfSandboxed || !slices.Equal(sb.ExcludedCommands, []string{
			"asmai *", "gh --version", "gh pr create *", "gh pr view *", "gh pr list *", "gh pr checks *",
			"gh pr status *", "gh issue view *", "gh run view *", "gh run list *", "gh run watch *",
			"git add *", "git commit *", "git push *", "git fetch *", "git ls-remote *",
			"git status *", "git rev-parse *", "git merge *", "git var *",
		}) || !slices.Equal(settings.Permissions.Allow, []string{
			"Bash(asmai:*)", "Bash(gh --version)", "Bash(gh pr create:*)", "Bash(gh pr view:*)", "Bash(gh pr list:*)",
			"Bash(gh pr checks:*)", "Bash(gh pr status:*)", "Bash(gh issue view:*)", "Bash(gh run view:*)",
			"Bash(gh run list:*)", "Bash(gh run watch:*)", "Bash(git add:*)", "Bash(git commit:*)",
			"Bash(git push:*)", "Bash(git fetch:*)", "Bash(git ls-remote:*)", "Bash(git status:*)",
			"Bash(git rev-parse:*)", "Bash(git merge:*)", "Bash(git var:*)",
		}) ||
			settings.Permissions.DefaultMode != "default" || settings.Permissions.BypassDenied != "disable" ||
			!slices.Equal(sb.Network.AllowUnixSockets, []string{filepath.Join(stateDir, "daemon.sock")}) {
			t.Errorf("%s was started with the settings %s, want the write guard on and the native prompts kept", agent, argValue(args, "--settings"))
		}
		wantWritable := []string(nil)
		if agent == "worker1@engineering" {
			wantWritable = []string{filepath.Join(stateDir, "workspaces", "job-1", "assignment-1", "tmp")}
		}
		if !slices.Equal(sb.Filesystem.AllowWrite, wantWritable) {
			t.Errorf("%s's write guard lets commands write %q, want %q", agent, sb.Filesystem.AllowWrite, wantWritable)
		}
		if agent != "worker1@engineering" && strings.Contains(argValue(args, "--append-system-prompt"), "instruction files") {
			t.Errorf("%s was given a repository's instruction files", agent)
		}
	}
}
