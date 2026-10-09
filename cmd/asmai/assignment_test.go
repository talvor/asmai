// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/talvor/asmai/internal/fakeprovider"
	"github.com/talvor/asmai/internal/statedir"
)

// writingFixture is a repository registered by its user's checkout: its
// origin is a bare repository on this host, with one commit on main.
type writingFixture struct {
	origin, checkout string
	// main is the commit main was at when the repository was registered, and
	// untouched the state of the user's checkout then.
	main, untouched string
}

// gitIdentity makes git commit as one identity wherever the factory's agents
// run it, with no configuration of the host's, and ignore the variables of a
// hook that point it at another repository. The daemon takes the environment
// it is started with, so a test calls it first.
func gitIdentity(t *testing.T) {
	t.Helper()
	for name, value := range map[string]string{
		"GIT_AUTHOR_NAME": "T", "GIT_AUTHOR_EMAIL": "t@example.test", "GIT_COMMITTER_NAME": "T", "GIT_COMMITTER_EMAIL": "t@example.test",
		"GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_SYSTEM": "/dev/null",
	} {
		t.Setenv(name, value)
	}
	for _, name := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR", "GIT_PREFIX"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
}

// registerWritingFixture registers a repository whose main holds one file,
// by the user's checkout of it, which also holds a commit and a file no one
// else has, so that a workspace made from the checkout would show.
func registerWritingFixture(t *testing.T) writingFixture {
	t.Helper()
	work, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := writingFixture{origin: filepath.Join(work, "fixture.git"), checkout: filepath.Join(work, "fixture")}
	gitIn(t, work, "init", "--bare", "-b", "main", f.origin)
	gitIn(t, work, "clone", "--quiet", f.origin, "fixture")
	gitIn(t, f.checkout, "checkout", "-b", "main")
	if err := os.WriteFile(filepath.Join(f.checkout, "README.md"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, f.checkout, "add", "README.md")
	gitIn(t, f.checkout, "commit", "-m", "initial")
	gitIn(t, f.checkout, "push", "--quiet", "origin", "main")
	f.main = gitIn(t, f.checkout, "rev-parse", "HEAD")
	gitIn(t, f.checkout, "commit", "--allow-empty", "-m", "mine alone")
	if err := os.WriteFile(filepath.Join(f.checkout, "draft.txt"), []byte("not committed"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.untouched = checkoutState(t, f.checkout)
	if _, stderr, code := runAsmai(t, "repo", "add", f.checkout, "--name", "fixture"); code != 0 {
		t.Fatal(stderr)
	}
	return f
}

// writeScript writes the fake's script for an agent to a file of its own and
// returns its path.
func writeScript(t *testing.T, name string, steps []string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name+".jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(steps, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// hookStep is a hook payload the fake delivers, with the fields every step of
// these scripts gives it.
func hookStep(event, promptID, prompt string) string {
	payload := map[string]string{"hook_event_name": event, "transcript_path": "/fake/transcript.jsonl"}
	if promptID != "" {
		payload["prompt_id"] = promptID
	}
	if prompt != "" {
		payload["prompt"] = prompt
	}
	encoded, _ := json.Marshal(payload)
	return fmt.Sprintf(`{"hook":%q,"payload":%s}`, event, encoded)
}

// assignmentFlow runs a job from the user's message to the report of its one
// writing assignment: Coordination hands it to Engineering, whose leader
// accepts the handoff and assigns the work, and the worker does what its
// script says. The dispatches are numbered in the order they are made: 1 the
// handoff, 2 its answer, 3 the assignment, 4 the worker's report.
//
// leader is what Engineering's leader does once the report reaches it, and
// worker what the worker does after it fetches its assignment. It returns
// when both have finished.
func assignmentFlow(t *testing.T, worker, leader []string) (fixture writingFixture, testDir string) {
	t.Helper()
	gitIdentity(t)
	testDir = t.TempDir()
	t.Setenv("ASMAI_TEST_DIR", testDir)
	engineering := append([]string{
		hookStep("SessionStart", "", ""),
		`{"screen":"\u001b[?2004h> "}`,
		`{"expect":"asmai inbox --dispatch 1\r"}`,
		hookStep("UserPromptSubmit", "handoff", "asmai inbox --dispatch 1"),
		fakeRun(`asmai inbox --dispatch 1`, 0, "(?s)Add a greeting.*Greeting is printed"),
		fakeRun(`asmai brief 1`, 0, "(?s)Job branch: asmai/job-1-add-a-greeting \\(started from [0-9a-f]{40}, tip [0-9a-f]{40}\\).*Read-only view of the job at the job branch's tip: \\S+/views/job-1"),
		fakeRun(`asmai handoff accept --handoff 1`, 0, "reply dispatch 2"),
		fakeRun(`asmai assign --job 1 --outcome 'Add greeting.txt, with a check that it says hello' --criterion 'greeting.txt says hello' --criterion 'the check passes'`, 0, "assignment 1 of job 1 given to worker1@engineering; dispatch 3"),
		hookStep("Stop", "handoff", ""),
		`{"expect":"asmai inbox --dispatch 4\r"}`,
		hookStep("UserPromptSubmit", "report", "asmai inbox --dispatch 4"),
	}, leader...)
	engineering = append(engineering,
		hookStep("Stop", "report", ""),
		fakeRun(`touch "$ASMAI_TEST_DIR/leader-done"`, 0, ""),
		`{"expect":"only typed by a test that means to"}`,
	)
	t.Setenv(fakeprovider.ScriptEnv+"_ENGINEERING", writeScript(t, "engineering", engineering))

	workerScript := append([]string{
		hookStep("SessionStart", "", ""),
		`{"screen":"\u001b[?2004h> "}`,
		`{"expect":"asmai inbox --dispatch 3\r"}`,
		hookStep("UserPromptSubmit", "assignment", "asmai inbox --dispatch 3"),
	}, worker...)
	workerScript = append(workerScript,
		hookStep("Stop", "assignment", ""),
		fakeRun(`touch "$ASMAI_TEST_DIR/worker-done"`, 0, ""),
		`{"expect":"only typed by a test that means to"}`,
	)
	t.Setenv(fakeprovider.ScriptEnv+"_WORKER", writeScript(t, "worker", workerScript))

	coordination := []string{
		hookStep("SessionStart", "", ""),
		`{"screen":"> "}`,
		`{"expect":"Please add a greeting\r"}`,
		hookStep("UserPromptSubmit", "", "Please add a greeting"),
		fakeRun(`asmai job open --message latest --repository fixture --reading 'Add a greeting' --criterion 'Greeting is printed'`, 0, "Opened job 1"),
		fakeRun(`asmai assign --job 1 --outcome x --criterion y`, 1, "only leader@engineering"),
		fakeRun(`asmai handoff send --job 1 --to engineering --outcome 'Add a greeting' --decisions none --evidence none --constraints none --permissions none --criterion 'Greeting is printed'`, 0, "dispatch 1"),
		hookStep("Stop", "", ""),
		`{"expect":"asmai inbox --dispatch 2\r"}`,
		hookStep("UserPromptSubmit", "reply", "asmai inbox --dispatch 2"),
		fakeRun(`asmai inbox --dispatch 2`, 0, "accepted from leader@engineering"),
		hookStep("Stop", "reply", ""),
		`{"expect":"only typed by a test that means to"}`,
	}
	readyFactory(t, coordination)
	t.Cleanup(func() {
		if t.Failed() {
			log, _, _ := runAsmai(t, "log")
			t.Logf("the daemon's log:\n%s", log)
		}
	})
	if _, stderr, code := runAsmai(t, "start"); code != 0 {
		t.Fatal(stderr)
	}
	fixture = registerWritingFixture(t)
	a := inTerminal(t, 100, 25)
	waitFor(t, "Coordination prompt", a.shows(">"))
	a.ptmx.WriteString("Please add a greeting\r")
	waitFor(t, "the worker's turn", touched(testDir, "worker-done"))
	waitFor(t, "the leader reading the report", touched(testDir, "leader-done"))
	return fixture, testDir
}

func decode[T any](t *testing.T, e journalEntry) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(e.Data, &v); err != nil {
		t.Fatalf("%s: %v", e.Data, err)
	}
	return v
}

func TestAWritingAssignmentIsDoneInAWorkspaceOfAsmAIsCloneAndSubmittedAsAResult(t *testing.T) {
	const branch = "asmai/job-1/1"
	worker := []string{
		fakeRun(`asmai inbox --dispatch 3`, 0, "(?s)assignment 1 \\| job 1 \\| leader@engineering -> worker1@engineering \\| active.*Add greeting.txt.*greeting.txt says hello.*the check passes.*Assignment branch: asmai/job-1/1, made from the job branch asmai/job-1-add-a-greeting at [0-9a-f]{40}"),
		fakeRun(`asmai assign --job 1 --outcome x --criterion y`, 1, "only leader@engineering"),
		fakeRun(`pwd -P > "$ASMAI_TEST_DIR/workspace"; printf '%s\n%s\n' "$ASMAI_SLOT" "$TMPDIR" > "$ASMAI_TEST_DIR/worker-env"; git rev-parse --git-common-dir > "$ASMAI_TEST_DIR/common-dir"`, 0, ""),
		fakeRun(`printf 'hello\n' > greeting.txt && printf '#!/bin/sh\ntest "$(cat greeting.txt)" = hello\n' > check.sh && chmod +x check.sh && ./check.sh`, 0, ""),
		fakeRun(`asmai result --evidence x --test none --check none --gap none --pr-section x`, 1, "(?s)changes that are not committed.*check.sh.*greeting.txt"),
		fakeRun(`git add -A && git commit -q -m 'Add greeting.txt and its check' && asmai effect commit "$(git rev-parse HEAD)"`, 0, "effect 1 recorded: commit"),
		fakeRun(`git merge --no-edit asmai/job-1-add-a-greeting`, 0, "up to date"),
		fakeRun(`asmai effect push "origin/other-branch@$(git rev-parse HEAD)"`, 0, "effect 2 recorded: push"),
		fakeRun(`asmai result --evidence x --test none --check none --gap none --pr-section x`, 1, "origin does not have the assignment branch"),
		fakeRun(`git push -q origin `+branch+` && asmai effect push "origin/`+branch+`@$(git rev-parse HEAD)"`, 0, "effect 3 recorded: push"),
		fakeRun(`asmai result --evidence './check.sh exits 0 at HEAD' --artifact 'greeting.txt' --test 'check.sh asserts greeting.txt says hello' --check './check.sh -> passed' --gap none --pr-section 'Adds greeting.txt, saying hello. Before: no greeting.txt. After: check.sh passes.'`, 0, "result recorded for assignment 1"),
	}
	leader := []string{
		fakeRun(`asmai inbox --dispatch 4`, 0, "(?s)result 1 \\| assignment 1 \\| worker1@engineering \\| commit [0-9a-f]{40}.*Artifacts:.*branch asmai/job-1/1 at.*greeting.txt.*Evidence:.*check.sh exits 0.*Tests added:.*check.sh asserts.*Checks run:.*\\./check.sh -> passed.*Gaps: none.*Effects:.*commit.*push origin/asmai/job-1/1@.*taken in: true.*PR section:.*Adds greeting.txt"),
		fakeRun(`asmai brief 1`, 0, "1 worker1@engineering \\(owner leader@engineering\\) submitted on branch asmai/job-1/1"),
	}
	fixture, testDir := assignmentFlow(t, worker, leader)
	stateDir := filepath.Join(os.Getenv("HOME"), ".local", "state", "asmai")

	// The job branch is named for the job, starts from the default branch as
	// origin has it, and records the commit it started from.
	made := journaled(t, "job.branch")
	if len(made) != 1 {
		t.Fatalf("job branches made: %+v", made)
	}
	jobBranch := decode[struct {
		Name        string `json:"name"`
		StartedFrom string `json:"started_from"`
		Tip         string `json:"tip"`
		View        string `json:"view"`
	}](t, made[0])
	if jobBranch.Name != "asmai/job-1-add-a-greeting" || jobBranch.StartedFrom != fixture.main || jobBranch.Tip != fixture.main {
		t.Errorf("the job branch is %+v, want asmai/job-1-add-a-greeting started from and at %s", jobBranch, fixture.main)
	}
	clone := filepath.Join(stateDir, "repositories", "fixture")
	if got := gitIn(t, clone, "rev-parse", "refs/heads/"+jobBranch.Name); got != fixture.main {
		t.Errorf("the job branch in AsmAI's clone is at %s, want it still at %s: only an accepted result moves it", got, fixture.main)
	}

	// The leaders' view is the job at the branch's tip, and nobody edits it.
	if got := gitIn(t, jobBranch.View, "rev-parse", "HEAD"); got != fixture.main {
		t.Errorf("the view is at %s, want the job branch's tip %s", got, fixture.main)
	}
	if info, err := os.Stat(filepath.Join(jobBranch.View, "README.md")); err != nil || info.Mode().Perm()&0o222 != 0 {
		t.Errorf("the view's README.md is %v (%v), want it read-only", info, err)
	}
	if _, err := os.Stat(filepath.Join(jobBranch.View, "draft.txt")); err == nil {
		t.Error("the view holds a file only the user's checkout has")
	}

	// The assignment is recorded with its owner, worker, outcome and
	// acceptance criteria, in a workspace of AsmAI's clone on its own branch
	// from the job branch's tip.
	created := journaled(t, "assignment.created")
	if len(created) != 1 {
		t.Fatalf("assignments created: %+v", created)
	}
	assignment := decode[struct {
		ID        int      `json:"id"`
		Job       int      `json:"job"`
		Owner     string   `json:"owner"`
		Worker    string   `json:"worker"`
		Outcome   string   `json:"outcome"`
		Criteria  []string `json:"acceptance_criteria"`
		State     string   `json:"state"`
		Workspace struct {
			Slot   int    `json:"slot"`
			Path   string `json:"path"`
			Tmp    string `json:"tmp"`
			Branch string `json:"branch"`
			Base   string `json:"base"`
		} `json:"workspace"`
	}](t, created[0])
	if assignment.ID != 1 || assignment.Job != 1 || assignment.Owner != "leader@engineering" || assignment.Worker != "worker1@engineering" ||
		assignment.Outcome != "Add greeting.txt, with a check that it says hello" || strings.Join(assignment.Criteria, "|") != "greeting.txt says hello|the check passes" ||
		assignment.State != "active" {
		t.Errorf("the assignment is %+v", assignment)
	}
	if w := assignment.Workspace; w.Slot != 1 || w.Branch != branch || w.Base != fixture.main || !strings.HasPrefix(w.Path, filepath.Join(stateDir, "workspaces")) || filepath.Dir(w.Path) != filepath.Dir(w.Tmp) {
		t.Errorf("the workspace is %+v", w)
	}

	// The worker ran in that workspace, with its slot and its own TMPDIR, and
	// the workspace is a checkout of AsmAI's clone, not of the user's.
	readTest := func(name string) string {
		data, err := os.ReadFile(filepath.Join(testDir, name))
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(data))
	}
	realWorkspace, _ := filepath.EvalSymlinks(assignment.Workspace.Path)
	if got := readTest("workspace"); got != realWorkspace {
		t.Errorf("the worker ran in %s, want its workspace %s", got, realWorkspace)
	}
	if got, want := readTest("worker-env"), "1\n"+assignment.Workspace.Tmp; got != want {
		t.Errorf("the worker's ASMAI_SLOT and TMPDIR were %q, want %q", got, want)
	}
	common := readTest("common-dir")
	if !filepath.IsAbs(common) {
		common = filepath.Join(readTest("workspace"), common)
	}
	realClone, _ := filepath.EvalSymlinks(filepath.Join(clone, ".git"))
	if got, _ := filepath.EvalSymlinks(common); got != realClone {
		t.Errorf("the workspace belongs to %s, want AsmAI's clone %s", got, realClone)
	}
	if got := checkoutState(t, fixture.checkout); got != fixture.untouched {
		t.Errorf("the user's checkout changed:\nbefore: %s\nafter: %s", fixture.untouched, got)
	}

	// Its branch is on origin with the worker's commit on top of the job
	// branch's tip, and main is where it was.
	pushed := gitIn(t, fixture.origin, "rev-parse", "refs/heads/"+branch)
	if got := gitIn(t, fixture.origin, "rev-parse", pushed+"^"); got != fixture.main {
		t.Errorf("the assignment branch on origin is at %s, whose parent is %s, want it made from %s", pushed, got, fixture.main)
	}
	if got := gitIn(t, fixture.origin, "show", pushed+":greeting.txt"); got != "hello" {
		t.Errorf("greeting.txt on the assignment branch is %q", got)
	}
	if got := gitIn(t, fixture.origin, "rev-parse", "refs/heads/main"); got != fixture.main {
		t.Errorf("main on origin moved to %s", got)
	}
	if out := gitIn(t, fixture.origin, "branch", "--list", "asmai/*"); strings.Contains(out, "job-1-add-a-greeting") {
		t.Errorf("the job branch was pushed: %s", out)
	}

	// Each effect is recorded, tagged with the worker's dispatch.
	effects := journaled(t, "effect.recorded")
	if len(effects) != 3 {
		t.Fatalf("effects recorded: %+v", effects)
	}
	for i, kind := range []string{"commit", "push", "push"} {
		e := decode[map[string]any](t, effects[i])
		if e["kind"] != kind || e["dispatch"] != float64(3) || e["agent"] != "worker1@engineering" || e["assignment"] != float64(1) {
			t.Errorf("effect %d is %v, want a %s in dispatch 3 by worker1@engineering", i+1, e, kind)
		}
	}
	if e := decode[map[string]any](t, effects[2]); e["ref"] != "origin/"+branch+"@"+pushed {
		t.Errorf("the push is of %v, want origin/%s@%s", e["ref"], branch, pushed)
	}

	// The result is a record, at the commit the workspace was at.
	results := journaled(t, "result.submitted")
	if len(results) != 1 {
		t.Fatalf("results submitted: %+v", results)
	}
	result := decode[map[string]any](t, results[0])
	if result["commit"] != pushed || result["kind"] != "result" || result["took_in_tip"] != true || result["job_tip"] != fixture.main {
		t.Errorf("the result is %v, want it at %s with the tip taken in", result, pushed)
	}
	if got := fmt.Sprint(result["tests"], result["gaps"], result["checks"]); got != "[check.sh asserts greeting.txt says hello] [] [map[command:./check.sh outcome:passed]]" {
		t.Errorf("the result's tests, gaps and checks are %s", got)
	}
	if got := len(result["effects"].([]any)); got != 3 {
		t.Errorf("the result lists %d effects, want 3", got)
	}

	// The dispatches are correlated: the worker's went from created to
	// stopped on a nudge's acknowledgment, and its report is a dispatch of the
	// leader's.
	states := map[int64][]string{}
	for _, e := range journaled(t, "dispatch.changed") {
		d := decode[struct {
			ID    int64  `json:"id"`
			State string `json:"state"`
		}](t, e)
		states[d.ID] = append(states[d.ID], d.State)
	}
	if got := strings.Join(states[3], ","); got != "created,unknown,nudged,delivered,working,stopped" {
		t.Errorf("the assignment's dispatch states: %s", got)
	}
	if got := strings.Join(states[4], ","); got != "created,unknown,nudged,delivered,working,stopped" {
		t.Errorf("the report's dispatch states: %s", got)
	}
	found := false
	for _, a := range agentsListed(t) {
		if a.Agent == "worker1@engineering" {
			found = a.Role == "engineering" && a.Model == "opus" && a.Generation == 1
		}
	}
	if !found {
		t.Errorf("worker1@engineering did not run once on Engineering's worker model: %+v", agentsListed(t))
	}
}

func TestAWorkerThatCannotGoOnReportsItBlockedAndItsAssignmentStaysActive(t *testing.T) {
	const branch = "asmai/job-1/1"
	worker := []string{
		fakeRun(`asmai inbox --dispatch 3`, 0, "assignment 1"),
		fakeRun(`asmai blocked`, 1, "needs --reason"),
		fakeRun(`asmai effect`, 2, "name the kind of effect"),
		fakeRun(`asmai effect commit`, 2, "name what the effect is of"),
		fakeRun(`printf 'draft\n' > notes.txt && git add -A && git commit -q -m 'Notes so far' && git push -q origin `+branch+` && asmai effect push "origin/`+branch+`@$(git rev-parse HEAD)"`, 0, "effect 1 recorded: push"),
		fakeRun(`asmai blocked --reason 'The greeting could be a file or a command' --needs 'the user to choose which'`, 0, "blocked recorded for assignment 1"),
		fakeRun(`asmai result --evidence x --test none --check none --gap none --pr-section x`, 1, "already ended in a blocked report"),
	}
	leader := []string{
		fakeRun(`asmai inbox --dispatch 4`, 0, "(?s)blocked 1 \\| assignment 1 \\| worker1@engineering.*Reason: The greeting could be a file or a command.*Needs: the user to choose which.*Effects:.*push origin/asmai/job-1/1@"),
		fakeRun(`asmai brief 1`, 0, "1 worker1@engineering \\(owner leader@engineering\\) active on branch asmai/job-1/1"),
	}
	fixture, _ := assignmentFlow(t, worker, leader)
	if got := gitIn(t, fixture.origin, "show", "refs/heads/"+branch+":notes.txt"); got != "draft" {
		t.Errorf("the blocked worker's pushed notes.txt is %q", got)
	}
	reports := journaled(t, "blocked.reported")
	if len(reports) != 1 || len(journaled(t, "result.submitted")) != 0 {
		t.Errorf("blocked reports %+v and results %+v, want one blocked report and no result", reports, journaled(t, "result.submitted"))
	}
}

func TestAResultRejectsBlankEvidenceBeforeCallingTheDaemon(t *testing.T) {
	var stdout, stderr bytes.Buffer
	o := &output{stdout: &stdout, stderr: &stderr}
	code := result(o, statedir.Paths{}, resultFlags{
		evidence: listFlags{" \t\n"}, tests: listFlags{"none"}, checks: listFlags{"none"}, gaps: listFlags{"none"}, prSection: "changes",
	})
	if code == 0 || !strings.Contains(stderr.String(), "evidence cannot be blank") {
		t.Errorf("a blank-evidence result exited %d with stderr %q", code, stderr.String())
	}
}

func TestTheAssignmentCommandsAreAgentCommands(t *testing.T) {
	factoryHome(t)
	startUnready(t)
	for _, args := range [][]string{
		{"assign", "--job", "1", "--outcome", "x", "--criterion", "y"},
		{"effect", "push", "origin/x"},
		{"result", "--evidence", "x", "--test", "none", "--check", "none", "--gap", "none", "--pr-section", "x"},
		{"blocked", "--reason", "x"},
		{"accept", "--assignment", "1", "--reason", "x"},
		{"reject", "--assignment", "1", "--reason", "x"},
		{"cancel", "--assignment", "1", "--reason", "x"},
	} {
		if _, stderr, code := runAsmai(t, args...); code != 1 || !strings.Contains(stderr, "is an agent command") {
			t.Errorf("asmai %s exited %d printing %q, want a refusal: it is an agent command", args[0], code, stderr)
		}
	}
}

func TestAWorkersReportSaysEveryListItHasAndEveryCheckWithItsOutcome(t *testing.T) {
	for _, tt := range []struct {
		values []string
		want   []string
		err    string
	}{
		{nil, nil, "give --test, or --test none"},
		{[]string{"none"}, nil, ""},
		{[]string{" none "}, nil, ""},
		{[]string{"a test", "another"}, []string{"a test", "another"}, ""},
		{[]string{"a test", "none"}, nil, "only --test none"},
		{[]string{" "}, nil, "is not a value"},
	} {
		got, err := explicit("test", tt.values)
		if (err == nil) != (tt.err == "") || (err != nil && !strings.Contains(err.Error(), tt.err)) || strings.Join(got, "|") != strings.Join(tt.want, "|") {
			t.Errorf("explicit(%q) = %q, %v; want %q and error %q", tt.values, got, err, tt.want, tt.err)
		}
	}
	checks, err := parseChecks([]string{"go test ./... -> passed at HEAD", "make lint -> clean -> passed"})
	if err != nil || len(checks) != 2 || checks[0].Command != "go test ./..." || checks[0].Outcome != "passed at HEAD" || checks[1].Command != "make lint -> clean" || checks[1].Outcome != "passed" {
		t.Errorf("the checks read as %+v (%v)", checks, err)
	}
	for _, bad := range []string{"go test ./...", "-> passed", "go test -> ", "go test ->passed"} {
		if _, err := parseChecks([]string{bad}); err == nil {
			t.Errorf("--check %q was accepted", bad)
		}
	}
}
