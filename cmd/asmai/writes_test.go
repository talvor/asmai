// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/talvor/asmai/internal/daemon"
)

// isolatedHome gives the test a home with no factory in it, so that a command
// that asks the daemon finds none, and never the user's own.
func isolatedHome(t *testing.T) {
	t.Helper()
	home, err := os.MkdirTemp("", "home")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("HOME", home)
	t.Setenv("ASMAI_STATE_DIR", "")
}

// An agent asks the daemon for each git and gh write with a command of its
// own, and what the command lacks is refused before the daemon is asked, in
// the session's own words.
func TestTheWriteCommandsRefuseWhatIsMissingBeforeTheyAskTheDaemon(t *testing.T) {
	isolatedHome(t)
	t.Setenv(daemon.SessionCredential, "worker-session")
	for _, tt := range []struct {
		args   []string
		code   int
		stderr string
	}{
		{[]string{"commit"}, 1, "commit needs --message"},
		{[]string{"commit", "--message", " "}, 1, "commit needs --message"},
		{[]string{"merge"}, 1, "merge needs the ref to take in"},
		{[]string{"merge", "--abort", "origin/main"}, 1, "merge --abort takes no ref"},
		{[]string{"pr"}, 2, "asmai pr: want open or update"},
		{[]string{"pr", "open"}, 2, "name the job number"},
		{[]string{"pr", "open", "1"}, 1, "pr open needs --title"},
		{[]string{"pr", "open", "1", "--title", "t"}, 1, "name the file that holds the body with --body-file"},
		{[]string{"pr", "open", "1", "--title", "t", "--body-file", "/no/such/file"}, 1, "reading the body"},
		{[]string{"pr", "update", "1"}, 1, "name the file that holds the body with --body-file"},
		{[]string{"pr", "update", "one", "--body-file", "-"}, 1, "job number"},
	} {
		var stdout, stderr bytes.Buffer
		code := run(tt.args, strings.NewReader(""), &stdout, &stderr)
		if code != tt.code || !strings.Contains(stderr.String(), tt.stderr) {
			t.Errorf("asmai %s exited %d printing %q, want %d and %q", strings.Join(tt.args, " "), code, stderr.String(), tt.code, tt.stderr)
		}
	}
}

// An agent may run them, and nothing else the factory has: the write commands
// are among the agent's own.
func TestTheWriteCommandsAreCommandsAnAgentMayRun(t *testing.T) {
	isolatedHome(t)
	t.Setenv(daemon.SessionCredential, "worker-session")
	for _, args := range [][]string{{"commit", "--message", "m"}, {"merge", "--abort"}, {"push"}, {"pr", "open", "1", "--title", "t", "--body-file", "-"}, {"pr", "update", "1", "--body-file", "-"}} {
		var stdout, stderr bytes.Buffer
		run(args, strings.NewReader("body"), &stdout, &stderr)
		if strings.Contains(stderr.String(), "an agent cannot run") {
			t.Errorf("asmai %s is refused to an agent: %s", strings.Join(args, " "), stderr.String())
		}
	}
	// Whereas a git or gh write is no command of the factory's at all.
	var stdout, stderr bytes.Buffer
	if code := run([]string{"git-commit"}, strings.NewReader(""), &stdout, &stderr); code != 2 {
		t.Errorf("asmai git-commit exited %d, want 2 as an unknown command", code)
	}
}

// Where there is no factory, the user's own run of one finds none.
func TestTheWriteCommandsFindNoFactoryWhereThereIsNone(t *testing.T) {
	isolatedHome(t)
	t.Setenv(daemon.SessionCredential, "")
	var stdout, stderr bytes.Buffer
	run([]string{"commit", "--message", "m"}, strings.NewReader(""), &stdout, &stderr)
	if !strings.Contains(stderr.String(), "not running") && !strings.Contains(stderr.String(), "no such file") && !strings.Contains(stderr.String(), "daemon") {
		t.Errorf("asmai commit printed %q, want it to say there is no daemon", stderr.String())
	}
}
