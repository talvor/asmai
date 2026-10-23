// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/talvor/asmai/internal/fakeprovider"
)

// sessionModes returns the permission mode each agent session was started in,
// by the first line of its instructions, from the fake provider's log of the
// arguments of every start. The fake's start for the permission probe, which
// runs in print mode, is not a session.
func sessionModes(t *testing.T, argsLog string) map[string]string {
	t.Helper()
	// A session that has not started has not logged its arguments.
	data, _ := os.ReadFile(argsLog)
	modes := map[string]string{}
	for line := range strings.Lines(string(data)) {
		var args []string
		if err := json.Unmarshal([]byte(line), &args); err != nil {
			t.Fatalf("the log of arguments holds %q: %v", line, err)
		}
		if slices.Contains(args, "-p") {
			continue
		}
		var settings, instructions string
		for i, arg := range args {
			switch arg {
			case "--settings":
				settings = args[i+1]
			case "--append-system-prompt":
				instructions, _, _ = strings.Cut(strings.TrimSpace(args[i+1]), "\n")
			}
		}
		var parsed struct {
			Permissions struct {
				DefaultMode string `json:"defaultMode"`
			} `json:"permissions"`
		}
		if err := json.Unmarshal([]byte(settings), &parsed); err != nil {
			t.Fatalf("--settings is %q: %v", settings, err)
		}
		modes[instructions] = parsed.Permissions.DefaultMode
	}
	return modes
}

// startedWith starts the factory with the configuration's providers.claude
// table set to claude, and returns what `asmai start` printed. Coordination's
// leader is the one session it starts, and its terminal is the user's, so it
// keeps its native prompts whatever the switch.
func startedWith(t *testing.T, claude string) string {
	t.Helper()
	argsLog := filepath.Join(t.TempDir(), "args.log")
	t.Setenv(fakeprovider.ArgsLogEnv, argsLog)
	readyFactoryConfigured(t, idleLeader, staffing+claude)
	stdout, stderr, code := runAsmai(t, "start")
	if code != 0 {
		t.Fatalf("asmai start exited %d, printing %q", code, stdout+stderr)
	}
	waitFor(t, "Coordination's leader to start", func() bool { return len(sessionModes(t, argsLog)) == 1 })
	for agent, mode := range sessionModes(t, argsLog) {
		if !strings.Contains(agent, "Coordination's leader") || mode != "default" {
			t.Errorf("%q started in the %q mode, want Coordination's leader in the default mode", agent, mode)
		}
	}
	return stdout + stderr
}

// Auto mode is an opt-in tier: with the switch off, `asmai start` says nothing
// of it.
func TestAutoModeIsOffUnlessTheUserSwitchesItOn(t *testing.T) {
	for name, claude := range map[string]string{
		"no setting":   "",
		"switched off": "\n[providers.claude]\nauto_mode = false\n",
	} {
		t.Run(name, func(t *testing.T) {
			// Even where Claude Code would not have it, nobody asked for it.
			t.Setenv(fakeprovider.AutoModeUnavailableEnv, "1")
			if output := startedWith(t, claude); strings.Contains(output, "auto mode") || strings.Contains(output, "auto_mode") {
				t.Errorf("asmai start printed\n%s\nwant nothing of auto mode with it off", output)
			}
		})
	}
}

// With the switch on and Claude Code reporting auto mode available, `asmai
// start` has nothing to warn of.
func TestAutoModeSwitchedOnAndAvailableIsNotWarnedOf(t *testing.T) {
	if output := startedWith(t, "\n[providers.claude]\nauto_mode = true\n"); strings.Contains(output, "warning") {
		t.Errorf("asmai start printed\n%s\nwant no warning where auto mode is available", output)
	}
}

// With the switch on but auto mode not available, Claude Code would start a
// session that asks for it in its default mode without a word, and nobody
// answers the prompts of most agents: AsmAI reads the availability from
// Claude Code, launches in the mode it would otherwise use, and says so.
func TestAutoModeSwitchedOnButUnavailableIsSaidSo(t *testing.T) {
	for name, tc := range map[string]struct {
		env string
		why string
	}{
		"the administrator disabled it":      {fakeprovider.DisableAutoModeEnv, "permissions.disableAutoMode"},
		"Claude Code reports it unavailable": {fakeprovider.AutoModeUnavailableEnv, "auto mode unavailable"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(tc.env, "1")
			output := startedWith(t, "\n[providers.claude]\nauto_mode = true\n")
			for _, want := range []string{"warning", "auto_mode is on", "not available", tc.why, "dontAsk"} {
				if !strings.Contains(output, want) {
					t.Errorf("asmai start printed\n%s\nwant it to say %q", output, want)
				}
			}
		})
	}
}
