// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The provider's terminal is one row shorter than the user's, and its screen
// draws in full above the status line at that height. In the middle of
// Coordination's turn, with nothing more drawn and no turn ended, the
// status line shows the job Coordination opened as it runs, the decision it
// asked the user and its own native prompt, as the daemon records each.
func TestTheStatusLineShowsWhatWaitsAndTheRunningJobsUnderTheProvidersShorterScreen(t *testing.T) {
	testDir := t.TempDir()
	t.Setenv("ASMAI_TEST_DIR", testDir)
	script := []string{
		`{"screen": "> "}`,
		`{"expect": "Greet the user\r"}`,
		`{"hook": "UserPromptSubmit", "payload": {"hook_event_name": "UserPromptSubmit", "prompt": "Greet the user"}}`,
		fakeRun(`stty size < /dev/tty > "$ASMAI_TEST_DIR/size"`, 0, ""),
		// The first and the last row of the provider's screen.
		`{"screen": "\u001b[2J\u001b[Htop row\u001b[999;1Hbottom row"}`,
		fakeRun(`touch "$ASMAI_TEST_DIR/drawn"; while [ ! -e "$ASMAI_TEST_DIR/go" ]; do sleep 0.02; done`, 0, ""),
		fakeRun(`asmai job open --message latest --repository fixture --reading 'Greet the user' --criterion 'Greets'`, 0, "Opened job 1"),
		fakeRun(`asmai decision request --job 1 --title 'Which greeting?' --option hello --option hi`, 0, "decision request 1, version 1, recorded for job 1; it goes to user and is answered in conversation"),
		`{"hook": "PermissionRequest", "payload": {"hook_event_name": "PermissionRequest", "tool_name": "Bash", "tool_input": {"command": "rm -rf build"}}}`,
		`{"expect": "only typed by a test that means to"}`,
	}
	readyFactory(t, script)
	origin := filepath.Join(t.TempDir(), "fixture.git")
	gitIn(t, t.TempDir(), "init", "--bare", "-b", "main", origin)
	work := t.TempDir()
	gitIn(t, work, "clone", "--quiet", origin, "fixture")
	checkout := filepath.Join(work, "fixture")
	commitSetUp(t, checkout, "initial")
	gitIn(t, checkout, "push", "--quiet", "origin", "HEAD:main")
	if _, stderr, code := runAsmai(t, "repo", "add", origin); code != 0 {
		t.Fatalf("repo add: %s", stderr)
	}

	const columns, rows = 300, 31
	a := inTerminal(t, columns, rows)
	waitFor(t, "Coordination's prompt", a.shows(">"))
	a.ptmx.WriteString("Greet the user\r")
	waitFor(t, "the screen drawn at the provider's height", touched(testDir, "drawn"))
	hints := "leader@coordination · conversation · Ctrl-] leaves · ↑/↓ history"
	waitFor(t, "the screen over a status line with nothing waiting", func() bool {
		lines := a.lines()
		return lines[0] == "top row" && lines[rows-2] == "bottom row" && strings.TrimSpace(lines[rows-1]) == hints+" · opus"
	})
	if size, err := os.ReadFile(filepath.Join(testDir, "size")); err != nil || strings.TrimSpace(string(size)) != "30 300" {
		t.Errorf("the provider's terminal is %q rows and columns (%v), want one row fewer than the user's 31 by 300", size, err)
	}

	if err := os.WriteFile(filepath.Join(testDir, "go"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	want := hints + " · leader@coordination waits at a native prompt the user answers in the conversation: Bash `rm -rf build`" +
		" · job 1: decision request 1 waits on you in the conversation · running job 1 · opus"
	waitFor(t, "the status line to show what waits and the running job", func() bool {
		return strings.TrimSpace(a.lines()[rows-1]) == want
	})
	if lines := a.lines(); lines[0] != "top row" || lines[rows-2] != "bottom row" {
		t.Errorf("as the status line changed, the provider's screen became %q, want it as the provider drew it", lines)
	}
}
