// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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
		fakeRun(`asmai job open --message latest --repository fixture --reading 'Greet the user by name' --criterion 'Greets by name'`, 0, "Opened job 2"),
		fakeRun(`asmai job link --job 2 --follows 1`, 0, "(?s)Job 2 is linked as a follow-up to job 1.*starts afresh: job 1 has no pull request"),
		fakeRun(`asmai job link --job 2 --follows 1`, 0, "linked as a follow-up to job 1"),
		fakeRun(`asmai job link --job 1 --follows 2`, 1, "not a follow-up to job 2|a follow-up is a new job linked to one opened before it"),
		fakeRun(`asmai job link --job 2`, 1, "needs --job"),
		fakeRun(`asmai brief 2`, 0, "Follow-up to job 1"),
		fakeRun(`asmai start`, 1, "ask the user to run.*asmai start"),
		fakeRun(`asmai repo remove fixture`, 1, "ask the user to run.*asmai repo remove fixture"),
		fakeRun(`asmai export`, 1, "an agent cannot run"),
		fakeRun(`ASMAI_SESSION=unknown asmai jobs`, 1, "unknown or superseded"),
		fakeRun(`touch "$ASMAI_TEST_DIR/done"`, 0, ""),
		`{"expect": "only typed by a test that means to"}`,
	}
	stateDir := readyFactory(t, script)
	if err := os.Remove(filepath.Join(stateDir, "branch-id")); err != nil {
		t.Fatal(err)
	}

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

	a := inTerminal(t, 100, 25)
	waitFor(t, "Coordination's prompt", a.shows(">"))
	a.ptmx.WriteString("Please fix the fixture\r")
	waitFor(t, "the witnessed request", func() bool { return len(journaled(t, "message.witnessed")) == 1 })
	w := journaled(t, "message.witnessed")[0]
	if _, stderr, code := runAsmai(t, "job", "open", "--message", "latest", "--repository", "fixture", "--reading", "Fix the fixture", "--criterion", "Tests pass"); code != 1 || !strings.Contains(stderr, "agent command") {
		t.Fatalf("user opening a job: code %d, %s", code, stderr)
	}
	if _, stderr, code := runAsmai(t, "job", "link", "--job", "2", "--follows", "1"); code != 1 || !strings.Contains(stderr, "agent command") {
		t.Fatalf("user linking a job: code %d, %s", code, stderr)
	}
	observations := journaled(t, "observation")
	if err := os.WriteFile(filepath.Join(testDir, "observation"), []byte(strconv.FormatInt(observations[len(observations)-1].ID, 10)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(testDir, "witness"), []byte(strconv.FormatInt(w.ID, 10)), 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the fake leader to open and read the job", touched(testDir, "done"))
	id, err := os.ReadFile(filepath.Join(stateDir, "branch-id"))
	if err != nil || !regexp.MustCompile(`^[a-z0-9]{6}\n$`).Match(id) {
		t.Fatalf("first job kept branch ID %q (%v), want six lower-case letters or digits", id, err)
	}
	branch := "asmai/" + strings.TrimSpace(string(id)) + "/job-1-fix-the-fixture"
	if got := gitIn(t, filepath.Join(stateDir, "repositories", "fixture"), "branch", "--list", branch); !strings.Contains(got, branch) {
		t.Fatalf("job branch in the clone is %q, want %s", got, branch)
	}
	t.Logf("first job branch in the running factory: %s", branch)

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
	if entries := journaled(t, "job.opened"); len(entries) != 2 || !strings.Contains(string(entries[0].Data), `"number": 1`) || !strings.Contains(string(entries[0].Data), `"role": "coordination"`) {
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
	if kept, err := os.ReadFile(filepath.Join(stateDir, "branch-id")); err != nil || string(kept) != string(id) {
		t.Fatalf("after restart branch ID is %q (%v), want %q", kept, err, id)
	}
	t.Logf("after daemon restart the stored branch ID remains %s", strings.TrimSpace(string(id)))
	t.Setenv("ASMAI_SESSION", string(credential))
	if _, stderr, code := runAsmai(t, "jobs"); code != 1 || !strings.Contains(stderr, "unknown or superseded") {
		t.Fatalf("stale credential: code %d, %s", code, stderr)
	}
}

func TestRunningFactoryRefusesAJobNumberAlreadyOnOriginBeforeRecordingIt(t *testing.T) {
	testDir := t.TempDir()
	t.Setenv("ASMAI_TEST_DIR", testDir)
	script := []string{
		`{"screen": "> "}`,
		`{"expect": "Please fix the fixture\r"}`,
		`{"hook": "UserPromptSubmit", "payload": {"hook_event_name": "UserPromptSubmit", "prompt": "Please fix the fixture"}}`,
		fakeRun(`asmai job open --message latest --repository fixture --reading 'Fix the fixture' --mandate tested-pr --criterion 'Tests pass' > "$ASMAI_TEST_DIR/refusal" 2>&1; status=$?; cat "$ASMAI_TEST_DIR/refusal"; test "$status" -eq 1`, 0, `asmai/team/job-1-fix-the-fixture conflicts with asmai/team/job-1-other-title already on origin; change branch_prefix`),
		fakeRun(`touch "$ASMAI_TEST_DIR/refused"`, 0, ""),
		fakeRun(`while [ ! -f "$ASMAI_TEST_DIR/continue" ]; do sleep 0.02; done; asmai job open --message latest --repository fixture --reading 'Fix the fixture' --mandate tested-pr --criterion 'Tests pass'`, 0, "Opened job 1"),
		fakeRun(`touch "$ASMAI_TEST_DIR/done"`, 0, ""),
		`{"expect": "only typed by a test that means to"}`,
	}
	stateDir := readyFactoryConfigured(t, script, "[branches]\nbranch_prefix = \"team\"\n\n"+staffing)
	origin := filepath.Join(t.TempDir(), "fixture.git")
	gitIn(t, t.TempDir(), "init", "--bare", "-b", "main", origin)
	work := t.TempDir()
	gitIn(t, work, "clone", "--quiet", origin, "fixture")
	checkout := filepath.Join(work, "fixture")
	commitSetUp(t, checkout, "initial")
	gitIn(t, checkout, "push", "--quiet", "origin", "HEAD:main")
	gitIn(t, checkout, "push", "--quiet", "origin", "HEAD:refs/heads/asmai/team/job-1-other-title")
	if _, stderr, code := runAsmai(t, "repo", "add", origin); code != 0 {
		t.Fatalf("repo add: %s", stderr)
	}
	a := inTerminal(t, 100, 25)
	waitFor(t, "Coordination's prompt", a.shows(">"))
	a.ptmx.WriteString("Please fix the fixture\r")
	waitFor(t, "the origin collision refusal", touched(testDir, "refused"))
	refusal, err := os.ReadFile(filepath.Join(testDir, "refusal"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("asmai job open: %s", refusal)
	if entries := journaled(t, "job.opened"); len(entries) != 0 {
		t.Fatalf("a refused job was recorded: %+v", entries)
	}
	if got := gitIn(t, filepath.Join(stateDir, "repositories", "fixture"), "branch", "--list", "asmai/team/job-1-fix-the-fixture"); got != "" {
		t.Fatalf("a refused branch was made: %s", got)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "views", "job-1")); !os.IsNotExist(err) {
		t.Fatalf("a refused job left a view: %v", err)
	}
	jobs, stderr, code := runAsmai(t, "jobs")
	if code != 0 || strings.Contains(jobs, "Fix the fixture") {
		t.Fatalf("asmai jobs after refusal exited %d: %s%s", code, jobs, stderr)
	}
	t.Logf("asmai jobs after refusal: %s", strings.TrimSpace(jobs))
	gitIn(t, checkout, "push", "--quiet", "origin", ":refs/heads/asmai/team/job-1-other-title")
	if err := os.WriteFile(filepath.Join(testDir, "continue"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "job 1 to open after the collision cleared", touched(testDir, "done"))
	branch := "asmai/team/job-1-fix-the-fixture"
	if got := gitIn(t, filepath.Join(stateDir, "repositories", "fixture"), "branch", "--list", branch); !strings.Contains(got, branch) {
		t.Fatalf("job 1 under the configured prefix has branch %q, want %s", got, branch)
	}
	if kept, err := os.ReadFile(filepath.Join(stateDir, "branch-id")); err != nil || string(kept) != "test\n" {
		t.Fatalf("configured prefix changed stored ID to %q (%v), want test", kept, err)
	}
	t.Logf("job 1 opened after clearing the collision: %s (stored ID remains test)", branch)
}

func TestTwoFreshInstallsOnOneOriginOpenDistinctJobOneBranches(t *testing.T) {
	origin := filepath.Join(t.TempDir(), "fixture.git")
	gitIn(t, t.TempDir(), "init", "--bare", "-b", "main", origin)
	work := t.TempDir()
	gitIn(t, work, "clone", "--quiet", origin, "fixture")
	checkout := filepath.Join(work, "fixture")
	commitSetUp(t, checkout, "initial")
	gitIn(t, checkout, "push", "--quiet", "origin", "HEAD:main")
	var branches []string
	for _, install := range []string{"first install", "second install"} {
		t.Run(install, func(t *testing.T) {
			testDir := t.TempDir()
			t.Setenv("ASMAI_TEST_DIR", testDir)
			script := []string{
				`{"screen": "> "}`,
				`{"expect": "Please fix the fixture\r"}`,
				`{"hook": "UserPromptSubmit", "payload": {"hook_event_name": "UserPromptSubmit", "prompt": "Please fix the fixture"}}`,
				fakeRun(`asmai job open --message latest --repository fixture --reading 'Fix the fixture' --mandate tested-pr --criterion 'Tests pass'`, 0, "Opened job 1"),
				fakeRun(`touch "$ASMAI_TEST_DIR/done"`, 0, ""),
				`{"expect": "only typed by a test that means to"}`,
			}
			stateDir := readyFactory(t, script)
			if err := os.Remove(filepath.Join(stateDir, "branch-id")); err != nil {
				t.Fatal(err)
			}
			if _, stderr, code := runAsmai(t, "repo", "add", origin); code != 0 {
				t.Fatalf("repo add: %s", stderr)
			}
			a := inTerminal(t, 100, 25)
			waitFor(t, "Coordination's prompt", a.shows(">"))
			a.ptmx.WriteString("Please fix the fixture\r")
			waitFor(t, "job 1 opening", touched(testDir, "done"))
			id, err := os.ReadFile(filepath.Join(stateDir, "branch-id"))
			if err != nil || !regexp.MustCompile(`^[a-z0-9]{6}\n$`).Match(id) {
				t.Fatalf("install branch ID %q (%v), want six lower-case letters or digits", id, err)
			}
			branch := "asmai/" + strings.TrimSpace(string(id)) + "/job-1-fix-the-fixture"
			clone := filepath.Join(stateDir, "repositories", "fixture")
			if got := gitIn(t, clone, "branch", "--list", branch); !strings.Contains(got, branch) {
				t.Fatalf("job 1 branch in the clone is %q, want %s", got, branch)
			}
			gitIn(t, clone, "push", "--quiet", "origin", branch)
			branches = append(branches, branch)
			t.Logf("job 1 pushed to shared origin: %s", branch)
		})
	}
	if branches[0] == branches[1] {
		t.Fatalf("two fresh installs made the same job 1 branch: %s", branches[0])
	}
	for _, branch := range branches {
		if got := gitIn(t, checkout, "ls-remote", "--heads", origin, "refs/heads/"+branch); !strings.Contains(got, "refs/heads/"+branch) {
			t.Errorf("shared origin lacks %s: %s", branch, got)
		}
	}
}

func TestRunningFactoryKeepsAnOpenedJobWhenOriginLookupFails(t *testing.T) {
	testDir := t.TempDir()
	t.Setenv("ASMAI_TEST_DIR", testDir)
	script := []string{
		`{"screen": "> "}`,
		`{"expect": "Please fix the fixture\r"}`,
		`{"hook": "UserPromptSubmit", "payload": {"hook_event_name": "UserPromptSubmit", "prompt": "Please fix the fixture"}}`,
		fakeRun(`while [ ! -f "$ASMAI_TEST_DIR/offline" ]; do sleep 0.02; done; asmai job open --message latest --repository fixture --reading 'Fix the fixture' --mandate tested-pr --criterion 'Tests pass'`, 0, "Opened job 1"),
		fakeRun(`touch "$ASMAI_TEST_DIR/done"`, 0, ""),
		`{"expect": "only typed by a test that means to"}`,
	}
	stateDir := readyFactory(t, script)
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
	a := inTerminal(t, 100, 25)
	waitFor(t, "Coordination's prompt", a.shows(">"))
	a.ptmx.WriteString("Please fix the fixture\r")
	waitFor(t, "the witnessed request", func() bool { return len(journaled(t, "message.witnessed")) == 1 })
	clone := filepath.Join(stateDir, "repositories", "fixture")
	gitIn(t, clone, "remote", "set-url", "origin", filepath.Join(testDir, "unavailable.git"))
	if err := os.WriteFile(filepath.Join(testDir, "offline"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "job 1 to be recorded despite the origin outage", touched(testDir, "done"))
	jobs := jobsListed(t)
	if len(jobs) != 1 || jobs[0].Number != 1 {
		t.Fatalf("job open while origin was unavailable recorded %+v, want job 1", jobs)
	}
	if made := journaled(t, "job.branch"); len(made) != 0 {
		t.Fatalf("job open unexpectedly made a branch while origin was unavailable: %+v", made)
	}
	t.Logf("asmai jobs while origin lookup failed: job %d %q remains open; branch awaits retry", jobs[0].Number, jobs[0].Reading)
}
