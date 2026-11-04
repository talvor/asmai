// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// What the daemon tells Coordination asks for each line about a job to carry
// the job's number, and for what needs the user to be a flagged block shown
// once: a decision Coordination asks is shown in one flagged block, a second
// presentation is refused, and the status line keeps the decision visible
// until the user answers it; a cancellation comes to Coordination with the
// one tagged line to give the user.
func TestCoordinationsLinesCarryTheJobAndAPendingDecisionStaysOnTheStatusLineUntilAnswered(t *testing.T) {
	testDir := t.TempDir()
	t.Setenv("ASMAI_TEST_DIR", testDir)
	block := `\u001b[2J\u001b[H` +
		`job 1: opened to fix the fixture\r\n` +
		`⚑ job 1: which cache? Answer in your next message.\r\n` +
		`  - redis\r\n  - in-memory (recommended)\r\n> `
	script := []string{
		`{"screen": "> "}`,
		`{"expect": "Please fix the fixture\r"}`,
		`{"hook": "UserPromptSubmit", "payload": {"hook_event_name": "UserPromptSubmit", "prompt": "Please fix the fixture"}}`,
		fakeRun(`asmai job open --message latest --repository fixture --reading 'Fix the fixture' --mandate tested-pr --criterion 'Tests pass'`, 0, "Opened job 1"),
		fakeRun(`asmai decision request --job 1 --title 'Which cache?' --option redis --option in-memory --recommend in-memory`, 0,
			"(?s)recorded for job 1.*next: Show the user once, in a flagged block whose first line starts `⚑ job 1:`.*`asmai decision present --request 1 --version 1`.*never show it again or remind them of it"),
		`{"screen": "` + block + `"}`,
		fakeRun(`asmai decision present --request 1 --version 1`, 0, "(?s)presented, for job 1.*the status line shows it until they answer it: never show it again or remind them of it"),
		fakeRun(`asmai decision present --request 1 --version 1`, 1, "presented already"),
		fakeRun(`touch "$ASMAI_TEST_DIR/shown"; while [ ! -e "$ASMAI_TEST_DIR/answer" ]; do sleep 0.02; done`, 0, ""),
		`{"expect": "in-memory please\r"}`,
		`{"hook": "UserPromptSubmit", "payload": {"hook_event_name": "UserPromptSubmit", "prompt": "in-memory please"}}`,
		fakeRun(`asmai decision record --request 1 --version 1 --cites latest --choice in-memory --reading 'The user picked the in-memory cache.'`, 0, "recorded"),
		fakeRun(`touch "$ASMAI_TEST_DIR/answered"`, 0, ""),
		`{"expect": "Cancel job 1\r"}`,
		`{"hook": "UserPromptSubmit", "payload": {"hook_event_name": "UserPromptSubmit", "prompt": "Cancel job 1"}}`,
		fakeRun(`asmai job cancel 1 --message latest`, 0, "Job 1 is cancelled"),
		fakeRun(`asmai inbox`, 0, "(?s)\\| job 1 \\| job.cancelled from daemon\nJob 1 is cancelled at the user's word.*Tell the user in one line that starts `job 1:`\\."),
		fakeRun(`touch "$ASMAI_TEST_DIR/done"`, 0, ""),
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

	const columns, rows = 200, 12
	a := inTerminal(t, columns, rows)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("Coordination's terminal:\n%s", strings.Join(a.lines(), "\n"))
		}
	})
	waitFor(t, "Coordination's prompt", a.shows(">"))
	a.ptmx.WriteString("Please fix the fixture\r")
	waitFor(t, "the decision to be presented", touched(testDir, "shown"))
	const pending = "job 1: decision request 1 waits on you in the conversation"
	waitFor(t, "the pending decision on the status line", func() bool {
		return strings.Contains(a.lines()[rows-1], pending)
	})
	flagged := 0
	for _, line := range a.lines()[:rows-1] {
		if strings.HasPrefix(line, "⚑ job 1:") {
			flagged++
		}
	}
	if flagged != 1 {
		t.Errorf("the decision shows in %d flagged blocks, want one:\n%s", flagged, strings.Join(a.lines(), "\n"))
	}

	// The decision stays on the status line until the user answers it.
	time.Sleep(time.Second)
	if line := a.lines()[rows-1]; !strings.Contains(line, pending) {
		t.Errorf("before the user answered, the status line is %q, want it to show %q", line, pending)
	}
	if err := os.WriteFile(filepath.Join(testDir, "answer"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	a.ptmx.WriteString("in-memory please\r")
	waitFor(t, "the answer to be recorded", touched(testDir, "answered"))
	waitFor(t, "the answered decision to leave the status line", func() bool {
		return !strings.Contains(a.lines()[rows-1], "decision request 1")
	})

	a.ptmx.WriteString("Cancel job 1\r")
	waitFor(t, "the cancellation to reach Coordination", touched(testDir, "done"))
}
