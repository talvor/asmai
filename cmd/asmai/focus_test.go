// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// The conversation has one focused job, which the status line shows. A new
// request's job takes the focus; a message naming another job applies to it
// and leaves the focus where it is, and so does a factory-wide question,
// which opens no job. "Switch to 1" moves the focus: Coordination's
// `asmai focus` records the change with the user's witnessed message, says
// so and prints the job's brief.
func TestTheFocusedJobMovesOnlyWithANewRequestOrASwitchAndTheStatusLineShowsIt(t *testing.T) {
	testDir := t.TempDir()
	t.Setenv("ASMAI_TEST_DIR", testDir)
	says := func(words string) []string {
		return []string{
			`{"expect": "` + words + `\r"}`,
			`{"hook": "UserPromptSubmit", "payload": {"hook_event_name": "UserPromptSubmit", "prompt": "` + words + `"}}`,
		}
	}
	script := []string{`{"screen": "> "}`}
	script = append(script, says("Fix the greeting")...)
	script = append(script,
		fakeRun(`asmai job open --message latest --repository fixture --reading 'Fix the greeting' --criterion 'Greets'`, 0, "(?s)Opened job 1.*It has the conversation's focus"),
		fakeRun(`touch "$ASMAI_TEST_DIR/first"`, 0, ""),
	)
	script = append(script, says("Add a farewell")...)
	script = append(script,
		fakeRun(`asmai job open --message latest --repository fixture --reading 'Add a farewell' --criterion 'Says farewell'`, 0, "(?s)Opened job 2.*It has the conversation's focus"),
		fakeRun(`touch "$ASMAI_TEST_DIR/opened"`, 0, ""),
	)
	// The message names job 1, so it applies to job 1: Coordination reads
	// its brief, and the focus stays on job 2.
	script = append(script, says("1: is it tested?")...)
	script = append(script,
		fakeRun(`asmai brief 1`, 0, "User's words: Fix the greeting"),
		fakeRun(`touch "$ASMAI_TEST_DIR/named"`, 0, ""),
	)
	// A factory-wide question needs no job.
	script = append(script, says("what runs?")...)
	script = append(script,
		fakeRun(`asmai jobs`, 0, "(?s)Fix the greeting.*Add a farewell"),
		fakeRun(`touch "$ASMAI_TEST_DIR/asked"`, 0, ""),
	)
	script = append(script, says("switch to 1")...)
	script = append(script,
		fakeRun(`asmai focus 1 --message latest`, 0, `(?s)^The conversation's focus moves from job 2 to job 1, at the user's word in witnessed message [0-9]+\. Tell the user that job 1 has the focus\.\nJob 1 \| role: coordination.*User's words: Fix the greeting`),
		fakeRun(`asmai focus 1 --message latest`, 0, "^Job 1 already has the conversation's focus, since witnessed message [0-9]+"),
		fakeRun(`asmai focus 9 --message latest`, 1, "job 9 does not exist"),
		fakeRun(`asmai focus 2`, 1, "--message latest"),
		fakeRun(`touch "$ASMAI_TEST_DIR/switched"`, 0, ""),
		`{"expect": "only typed by a test that means to"}`,
	)
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

	const columns, rows = 200, 20
	a := inTerminal(t, columns, rows)
	statusLine := func(want string) func() bool {
		return func() bool {
			return strings.TrimSpace(a.lines()[rows-1]) == "leader@coordination · conversation · Ctrl-] leaves · ↑/↓ history · "+want+" · opus"
		}
	}
	waitFor(t, "Coordination's prompt", a.shows(">"))
	a.ptmx.WriteString("Fix the greeting\r")
	waitFor(t, "the first job opened", touched(testDir, "first"))
	waitFor(t, "the status line to show it focused", statusLine("focus job 1 · running job 1"))
	a.ptmx.WriteString("Add a farewell\r")
	waitFor(t, "the second job opened", touched(testDir, "opened"))
	waitFor(t, "the status line to show the newer job focused", statusLine("focus job 2 · running jobs 1, 2"))

	a.ptmx.WriteString("1: is it tested?\r")
	waitFor(t, "Coordination to read the job the message names", touched(testDir, "named"))
	a.ptmx.WriteString("what runs?\r")
	waitFor(t, "Coordination to answer the factory-wide question", touched(testDir, "asked"))
	if opened := journaled(t, "job.opened"); len(opened) != 2 {
		t.Errorf("after a factory-wide question, %d jobs are open, want the 2 requested: it needs no job", len(opened))
	}
	if changes := journaled(t, "focus.changed"); len(changes) != 2 {
		t.Errorf("after a message naming job 1 and a factory-wide question, the focus changed %d times, want only as each of the 2 jobs opened", len(changes))
	}
	if !statusLine("focus job 2 · running jobs 1, 2")() {
		t.Errorf("after a message naming job 1, the status line is %q, want the focus still on job 2", a.lines()[rows-1])
	}

	a.ptmx.WriteString("switch to 1\r")
	waitFor(t, "Coordination to switch", touched(testDir, "switched"))
	waitFor(t, "the status line to show job 1 focused", statusLine("focus job 1 · running jobs 1, 2"))
	witnessed := journaled(t, "message.witnessed")
	var words struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(witnessed[len(witnessed)-1].Data, &words); err != nil || words.Text != "switch to 1" {
		t.Fatalf("the latest witnessed message is %s (%v), want the switch", witnessed[len(witnessed)-1].Data, err)
	}
	changes := journaled(t, "focus.changed")
	if len(changes) != 3 {
		t.Fatalf("the focus changed %d times, want once for each job opened and once for the switch: %+v", len(changes), changes)
	}
	var change struct {
		Job      int64 `json:"job"`
		Previous int64 `json:"previous"`
		Witness  int64 `json:"witness"`
	}
	if err := json.Unmarshal(changes[2].Data, &change); err != nil || change.Job != 1 || change.Previous != 2 || change.Witness != witnessed[len(witnessed)-1].ID {
		t.Errorf("the switch is journaled as %s (%v), want job 1, from job 2, citing witnessed message %d", changes[2].Data, err, witnessed[len(witnessed)-1].ID)
	}

	if _, stderr, code := runAsmai(t, "focus", "2", "--message", "latest"); code != 1 || !strings.Contains(stderr, "agent command") {
		t.Errorf("the user moving the focus with a command: code %d, %s; want it refused, as the user switches in the conversation", code, stderr)
	}
}
