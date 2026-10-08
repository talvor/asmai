// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func fakeRun(command string, status int, output string) string {
	return fmt.Sprintf(`{"run":%q,"status":%d,"output":%q}`, command, status, output)
}

func TestCoordinationOpensAJobFromTheUsersWitnessedMessage(t *testing.T) {
	testDir := t.TempDir()
	t.Setenv("ASMAI_TEST_DIR", testDir)
	script := []string{
		`{"screen": "> "}`,
		fakeRun(`printf %s "$ASMAI_SESSION" > "$ASMAI_TEST_DIR/credential"`, 0, ""),
		`{"expect": "Please fix the fixture\r"}`,
		`{"hook": "UserPromptSubmit", "payload": {"hook_event_name": "UserPromptSubmit", "prompt": "Please fix the fixture"}}`,
		fakeRun(`while [ ! -f "$ASMAI_TEST_DIR/witness" ]; do sleep 0.02; done; asmai job open --message "$(cat "$ASMAI_TEST_DIR/observation")" --repository fixture --reading 'Fix the fixture' --criterion 'Tests pass'`, 1, "not a witnessed message"),
		fakeRun(`asmai job open --repository fixture --reading 'Fix the fixture' --criterion 'Tests pass'`, 1, "needs --message"),
		fakeRun(`asmai job open --message latest --repository absent --reading 'Fix the fixture' --criterion 'Tests pass'`, 1, "asmai repo add"),
		fakeRun(`asmai job open --message latest --repository fixture --reading 'Fix the fixture' --mandate tested-pr --criterion 'Tests pass' --criterion 'Open a reviewed PR'`, 0, "Opened job 1"),
		fakeRun(`asmai jobs`, 0, "(?s)JOB.*1.*coordination.*fixture.*Fix the fixture"),
		fakeRun(`asmai brief 1`, 0, "(?s)Witnessed message: [0-9]+.*User's words: Please fix the fixture.*Tests pass.*Open a reviewed PR"),
		fakeRun(`asmai start`, 1, "ask the user to run.*asmai start"),
		fakeRun(`asmai repo remove fixture`, 1, "ask the user to run.*asmai repo remove fixture"),
		fakeRun(`asmai export`, 1, "an agent cannot run"),
		fakeRun(`ASMAI_SESSION=unknown asmai jobs`, 1, "unknown or superseded"),
		fakeRun(`touch "$ASMAI_TEST_DIR/done"`, 0, ""),
		`{"expect": "only typed by a test that means to"}`,
	}
	readyFactory(t, script)

	origin := filepath.Join(t.TempDir(), "fixture.git")
	gitIn(t, t.TempDir(), "init", "--bare", "-b", "main", origin)
	work := t.TempDir()
	gitIn(t, work, "clone", "--quiet", origin, "fixture")
	checkout := filepath.Join(work, "fixture")
	gitIn(t, checkout, "commit", "--allow-empty", "-m", "initial")
	gitIn(t, checkout, "push", "--quiet", "origin", "HEAD:main")
	if _, stderr, code := runAsmai(t, "repo", "add", origin); code != 0 {
		t.Fatalf("repo add: %s", stderr)
	}

	a := inTerminal(t, 100, 25)
	waitFor(t, "Coordination's prompt", a.shows(">"))
	a.ptmx.WriteString("Please fix the fixture\r")
	waitFor(t, "the witnessed request", func() bool { return len(journaled(t, "message.witnessed")) == 1 })
	w := journaled(t, "message.witnessed")[0]
	if _, stderr, code := runAsmai(t, "job", "open", "--message", "latest", "--repository", "fixture", "--reading", "Fix the fixture", "--criterion", "Tests pass"); code != 1 || !strings.Contains(stderr, "agent command") {
		t.Fatalf("user opening a job: code %d, %s", code, stderr)
	}
	observations := journaled(t, "observation")
	if err := os.WriteFile(filepath.Join(testDir, "observation"), []byte(strconv.FormatInt(observations[len(observations)-1].ID, 10)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(testDir, "witness"), []byte(strconv.FormatInt(w.ID, 10)), 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the fake leader to open and read the job", touched(testDir, "done"))

	var shown struct {
		Job struct {
			Number     int64    `json:"number"`
			Witness    int64    `json:"witness"`
			Words      string   `json:"words"`
			Reading    string   `json:"reading"`
			Mandate    string   `json:"mandate"`
			Role       string   `json:"role"`
			Repository string   `json:"repository"`
			Criteria   []string `json:"acceptance_criteria"`
		} `json:"job"`
	}
	asmaiJSON(t, &shown, "job", "1")
	if shown.Job.Number != 1 || shown.Job.Witness != w.ID || shown.Job.Words != "Please fix the fixture" || shown.Job.Reading != "Fix the fixture" || shown.Job.Mandate != "tested-pr" || shown.Job.Role != "coordination" || shown.Job.Repository != "fixture" || len(shown.Job.Criteria) != 2 {
		t.Fatalf("job 1 = %+v", shown.Job)
	}
	if entries := journaled(t, "job.opened"); len(entries) != 1 || !strings.Contains(string(entries[0].Data), `"number": 1`) || !strings.Contains(string(entries[0].Data), `"role": "coordination"`) {
		t.Fatalf("job journal = %+v", entries)
	}
	credential, err := os.ReadFile(filepath.Join(testDir, "credential"))
	if err != nil {
		t.Fatal(err)
	}
	if _, stderr, code := runAsmai(t, "stop"); code != 0 {
		t.Fatalf("stop: %s", stderr)
	}
	if _, stderr, code := runAsmai(t, "start"); code != 0 {
		t.Fatalf("restart: %s", stderr)
	}
	t.Setenv("ASMAI_SESSION", string(credential))
	if _, stderr, code := runAsmai(t, "jobs"); code != 1 || !strings.Contains(stderr, "unknown or superseded") {
		t.Fatalf("stale credential: code %d, %s", code, stderr)
	}
}
