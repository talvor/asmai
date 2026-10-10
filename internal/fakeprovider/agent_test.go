// SPDX-License-Identifier: Apache-2.0

package fakeprovider_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/talvor/asmai/internal/fakeprovider"
)

// agentScript is a scripted agent: asked to commit, it runs git in its
// working directory between screens, as Claude Code's Bash tool would, and
// reports what its environment told it.
var agentScript = []string{
	`{"hook": "SessionStart", "payload": {"session_id": "fake-session", "hook_event_name": "SessionStart", "source": "startup"}}`,
	`{"screen": "> \r\n"}`,
	`{"expect": "commit it\r"}`,
	`{"hook": "UserPromptSubmit", "payload": {"session_id": "fake-session", "hook_event_name": "UserPromptSubmit", "prompt": "commit it"}}`,
	`{"hook": "PreToolUse", "payload": {"session_id": "fake-session", "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_input": {"command": "git commit"}}}`,
	`{"run": "git init -q && echo hello > hello.txt && git add hello.txt && git commit -q -m 'Say hello' && git log --format=%s", "status": 0, "output": "^Say hello\n$"}`,
	`{"hook": "PostToolUse", "payload": {"session_id": "fake-session", "hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_input": {"command": "git commit"}}}`,
	`{"run": "printf '%s' \"$ASMAI_TEST_JOB\"", "output": "^job-17$"}`,
	`{"run": "test -f missing.txt", "status": 1}`,
	`{"screen": "Committed hello.txt\r\n"}`,
	`{"hook": "Stop", "payload": {"session_id": "fake-session", "hook_event_name": "Stop", "stop_hook_active": false}}`,
}

// startAgent starts the fake in dir the way the daemon starts Claude Code:
// the only argument is --settings, and the script comes from the environment,
// with env added to it.
func startAgent(t *testing.T, dir, script, settings string, env ...string) *session {
	t.Helper()
	cmd := exec.Command(fakeProvider, "--settings", settings)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(),
		fakeprovider.ScriptEnv+"="+script,
		"GIT_AUTHOR_NAME=Fake Agent", "GIT_AUTHOR_EMAIL=agent@example.com",
		"GIT_COMMITTER_NAME=Fake Agent", "GIT_COMMITTER_EMAIL=agent@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
	), env...)
	return startCmd(t, cmd)
}

// agentSettings is Claude Code settings, as JSON, whose hooks log to log. They
// name the PreToolUse hook for the Bash tool only, as Claude Code's matchers
// do, and one for Edit and Write that must not run.
func agentSettings(log hookLog) string {
	hook := func(matcher, event string) string {
		return `{"matcher": "` + matcher + `", "hooks": [{"type": "command", "command": "` + jsonString(logCommand(log, event)) + `"}]}`
	}
	return `{"permissions": {"allow": ["Bash"]}, "hooks": {` +
		`"SessionStart": [` + hook("", "SessionStart") + `],` +
		`"PreToolUse": [` + hook("Bash", "PreToolUse") + `, ` + hook("Edit|Write", "Edit") + `],` +
		`"PostToolUse": [` + hook("*", "PostToolUse") + `],` +
		`"Stop": [` + hook("", "Stop") + `]}}`
}

func TestAScriptedAgentLaunchedLikeClaudeCodeRunsCommandsInItsSession(t *testing.T) {
	for name, settings := range map[string]func(t *testing.T, log hookLog) string{
		"from a file": func(t *testing.T, log hookLog) string { return settingsFile(t, agentSettings(log)) },
		"as JSON":     func(t *testing.T, log hookLog) string { return agentSettings(log) },
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			log := hookLog(filepath.Join(t.TempDir(), "hooks.log"))
			s := startAgent(t, dir, writeScript(t, agentScript...), settings(t, log), "ASMAI_TEST_JOB=job-17")
			s.waitForScreen("> \r\n")
			s.typeKeys("commit it\r")
			s.waitForScreen("Committed hello.txt\r\n")
			if code := s.exit(); code != 0 {
				t.Fatalf("fake exited %d, want 0 (stderr %q)", code, s.stderr.String())
			}

			out, err := exec.Command("git", "-C", dir, "log", "--format=%an: %s").CombinedOutput()
			if err != nil || string(out) != "Fake Agent: Say hello\n" {
				t.Errorf("the agent's working directory has log %q (%v), want its commit", out, err)
			}
			var events []string
			for _, d := range log.deliveries(t) {
				event, _, _ := strings.Cut(d, "\t")
				events = append(events, event)
			}
			if want := []string{"SessionStart", "PreToolUse", "PostToolUse", "Stop"}; !slices.Equal(events, want) {
				t.Errorf("hooks received %q, want %q", events, want)
			}
		})
	}
}

func TestTheFakeStopsAtACommandThatDiffersFromTheScript(t *testing.T) {
	for name, tc := range map[string]struct {
		line, want string
	}{
		"a wrong exit status": {`{"run": "echo nope; exit 3", "status": 0}`, `line 2: "echo nope; exit 3" exited 3, want 0; it printed "nope\n"`},
		"a wrong output":      {`{"run": "echo nope", "output": "^yes"}`, `line 2: "echo nope" printed "nope\n", which does not match "^yes"`},
	} {
		t.Run(name, func(t *testing.T) {
			s := startAgent(t, t.TempDir(), writeScript(t, `{"screen": "ready\r\n"}`, tc.line, `{"screen": "too far\r\n"}`), `{}`)
			if code := s.exit(); code != 1 {
				t.Errorf("fake exited %d, want 1", code)
			}
			if !strings.Contains(s.stderr.String(), tc.want) {
				t.Errorf("stderr %q does not say %q", s.stderr.String(), tc.want)
			}
			if strings.Contains(s.drawn(), "too far") {
				t.Errorf("fake went on past the failed command: %q", s.drawn())
			}
		})
	}
}

func TestTheFakeRefusesSettingsItCannotRun(t *testing.T) {
	for name, tc := range map[string]struct {
		settings, want string
	}{
		"not JSON":           {`{hooks}`, `--settings: invalid character`},
		"a missing file":     {`no-such-settings.json`, `--settings: open no-such-settings.json`},
		"a prompt hook":      {`{"hooks": {"Stop": [{"hooks": [{"type": "prompt", "prompt": "done?"}]}]}}`, `Stop: the fake runs only hooks of type "command"`},
		"a bad tool matcher": {`{"hooks": {"PreToolUse": [{"matcher": "(", "hooks": [{"type": "command", "command": "true"}]}]}}`, `PreToolUse matcher: error parsing regexp`},
	} {
		t.Run(name, func(t *testing.T) {
			s := startAgent(t, t.TempDir(), writeScript(t, `{"screen": "ready\r\n"}`), tc.settings)
			if code := s.exit(); code != 2 {
				t.Errorf("fake exited %d, want 2", code)
			}
			if !strings.Contains(s.stderr.String(), tc.want) {
				t.Errorf("stderr %q does not say %q", s.stderr.String(), tc.want)
			}
		})
	}
}

// jsonString is s escaped for a JSON string literal.
func jsonString(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\t", `\t`).Replace(s)
}

// A session the daemon resumes after a restart is started with --resume and
// the provider's own identifier of the session it continues; the fake plays
// the script for resumed sessions, where there is one, and says what Claude
// Code says of a session it has no record of when it is told to.
func TestAResumedSessionPlaysTheResumedScriptOrFailsAsClaudeCodeDoesForAMissingSession(t *testing.T) {
	first := writeScript(t, `{"screen": "first session\r\n"}`)
	resumed := writeScript(t, `{"screen": "resumed session\r\n"}`)
	start := func(t *testing.T, args []string, env ...string) *session {
		t.Helper()
		cmd := exec.Command(fakeProvider, append([]string{"--settings", "{}"}, args...)...)
		cmd.Dir = t.TempDir()
		cmd.Env = append(append(os.Environ(), fakeprovider.ScriptEnv+"="+first), env...)
		return startCmd(t, cmd)
	}

	s := start(t, nil, fakeprovider.ResumedScriptEnv+"="+resumed)
	s.waitForScreen("first session\r\n")
	if code := s.exit(); code != 0 {
		t.Errorf("a new session exited %d, want 0 (stderr %q)", code, s.stderr.String())
	}

	s = start(t, []string{"--resume", "6b8b4567"}, fakeprovider.ResumedScriptEnv+"="+resumed)
	s.waitForScreen("resumed session\r\n")
	if code := s.exit(); code != 0 {
		t.Errorf("a resumed session exited %d, want 0 (stderr %q)", code, s.stderr.String())
	}

	// With no script of its own for a resumed session, it plays the usual.
	s = start(t, []string{"--resume", "6b8b4567"})
	s.waitForScreen("first session\r\n")
	if code := s.exit(); code != 0 {
		t.Errorf("a resumed session with no script of its own exited %d, want 0", code)
	}

	s = start(t, []string{"--resume", "6b8b4567"}, fakeprovider.ResumeFailsEnv+"=1", fakeprovider.ResumedScriptEnv+"="+resumed)
	if code := s.exit(); code != 1 {
		t.Errorf("a session that cannot be resumed exited %d, want 1", code)
	}
	if !strings.Contains(s.stderr.String(), "No conversation found with session ID: 6b8b4567") || strings.Contains(s.drawn(), "resumed session") {
		t.Errorf("a session that cannot be resumed printed %q and drew %q", s.stderr.String(), s.drawn())
	}
	// A new session is not affected by it.
	s = start(t, nil, fakeprovider.ResumeFailsEnv+"=1")
	s.waitForScreen("first session\r\n")
	if code := s.exit(); code != 0 {
		t.Errorf("a new session exited %d with resuming made to fail, want 0", code)
	}
}
