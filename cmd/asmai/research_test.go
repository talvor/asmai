// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/talvor/asmai/internal/fakeprovider"
)

// A research-only job that targets no repository runs in scratch workspaces,
// ends when its owning leader accepts its outcome, and has Coordination told
// where the results are. The workspaces are kept until the user removes them
// with `asmai job clean`, which is refused while the job is open.
func TestAResearchOnlyJobWithNoRepositoryRunsInScratchWorkspacesAndEndsOnTheLeadersAcceptance(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ASMAI_TEST_DIR", dir)
	research := []string{
		hookStep("SessionStart", "", ""),
		`{"screen":"\u001b[?2004h> "}`,
		`{"expect":"asmai inbox --dispatch 1\r"}`,
		hookStep("UserPromptSubmit", "handoff", "asmai inbox --dispatch 1"),
		fakeRun(`asmai inbox --dispatch 1`, 0, "(?s)job 1.*handoff.*How the greeting is spelled"),
		fakeRun(`asmai brief 1`, 0, "(?s)Repository: none: its assignments work in scratch workspaces.*Mandate: research-only"),
		fakeRun(`asmai handoff accept --handoff 1`, 0, "reply dispatch 2"),
		fakeRun(`asmai assign --job 1 --outcome 'Find how the greeting is spelled' --criterion 'the answer names its source' --skill triage`, 1, "triage"),
		fakeRun(`asmai assign --job 1 --read-only --commit abc --outcome x --criterion y`, 1, "only leader@quality may assign read-only validation"),
		fakeRun(`asmai assign --job 1 --outcome 'Find how the greeting is spelled' --criterion 'the answer names its source' --skill research`, 0, "(?s)assignment 1 of job 1 given to worker1@research; dispatch 3.*scratch workspace, empty, with no repository, no branch and no commit"),
		hookStep("Stop", "handoff", ""),
		`{"expect":"asmai inbox --dispatch 4\r"}`,
		hookStep("UserPromptSubmit", "report", "asmai inbox --dispatch 4"),
		fakeRun(`asmai inbox --dispatch 4`, 0, "(?s)result 1 \\| assignment 1 \\| worker1@research \\| in a scratch workspace, no commit.*Artifacts:.*scratch workspace \\S+/job-1/assignment-1/scratch.*findings.md.*Evidence:.*https://example.test/spec#greeting.*Gaps: none"),
		fakeRun(`asmai job clean 1`, 1, "an agent cannot run this command; ask the user to run `asmai job clean 1`"),
		// The user tries to clean the job's scratch workspaces while it is open.
		fakeRun(`touch "$ASMAI_TEST_DIR/result-read"; while [ ! -e "$ASMAI_TEST_DIR/clean-refused" ]; do sleep 0.02; done`, 0, ""),
		fakeRun(`asmai accept --assignment 1 --reason 'the answer names its source' --ends-job`, 0, "(?s)assignment 1 accepted by leader@research; the assignment has ended, and nothing else moved: its job has no repository.*job 1 has ended on your acceptance of its outcome: leader@coordination is told where the results are, in dispatch 5, and the scratch workspaces stay until the user runs `asmai job clean 1`"),
		fakeRun(`asmai brief 1`, 0, "(?s)state: ended.*accepted in scratch workspace \\S+/job-1/assignment-1/scratch"),
		hookStep("Stop", "report", ""),
		fakeRun(`touch "$ASMAI_TEST_DIR/research-done"`, 0, ""),
		`{"expect":"only typed by a test that means to"}`,
	}
	t.Setenv(fakeprovider.ScriptEnv+"_RESEARCH", writeScript(t, "research", research))
	worker := []string{
		hookStep("SessionStart", "", ""),
		`{"screen":"\u001b[?2004h> "}`,
		`{"expect":"asmai inbox --dispatch 3\r"}`,
		hookStep("UserPromptSubmit", "assignment", "asmai inbox --dispatch 3"),
		fakeRun(`asmai inbox --dispatch 3`, 0, "(?s)assignment 1 \\| job 1 \\| leader@research -> worker1@research \\| active.*Find how the greeting is spelled.*the answer names its source.*Skill to run: research.*Your skill list for this assignment: research, to-questionnaire.*Scratch workspace: \\S+/job-1/assignment-1/scratch \\(slot 1.*It is empty: the job has no repository"),
		fakeRun(`pwd -P > "$ASMAI_TEST_DIR/workspace"; ls -A | wc -l > "$ASMAI_TEST_DIR/workspace-entries"; git rev-parse --git-dir > "$ASMAI_TEST_DIR/git-dir" 2>&1`, 128, ""),
		fakeRun(`asmai assign --job 1 --outcome x --criterion y`, 1, "only leader@engineering"),
		fakeRun(`printf '# Findings\nThe greeting is spelled hello (https://example.test/spec#greeting).\n' > findings.md`, 0, ""),
		fakeRun(`asmai result --evidence 'https://example.test/spec#greeting' --artifact findings.md --test none --check none --gap none --skill research`, 0, "result recorded for assignment 1 in its scratch workspace"),
		hookStep("Stop", "assignment", ""),
		fakeRun(`touch "$ASMAI_TEST_DIR/worker-done"`, 0, ""),
		`{"expect":"only typed by a test that means to"}`,
	}
	t.Setenv(fakeprovider.ScriptEnv+"_RESEARCH_WORKER", writeScript(t, "research-worker", worker))
	coordination := []string{
		hookStep("SessionStart", "", ""),
		`{"screen":"> "}`,
		`{"expect":"Please find out how the greeting is spelled\r"}`,
		hookStep("UserPromptSubmit", "", "Please find out how the greeting is spelled"),
		fakeRun(`asmai job open --message latest --reading 'Find out how the greeting is spelled' --criterion 'A cited answer'`, 1, "a tested-pr job needs a registered repository"),
		fakeRun(`asmai job open --message latest --reading 'Find out how the greeting is spelled' --mandate research-only --criterion 'A cited answer'`, 0, "Opened job 1, which has no repository: its assignments work in scratch workspaces."),
		fakeRun(`asmai handoff send --job 1 --to engineering --outcome 'Build the greeting' --decisions none --evidence none --constraints none --permissions none --criterion 'A cited answer'`, 1, "authorizes only its research outcome, never code"),
		fakeRun(`asmai handoff send --job 1 --to planning --outcome 'Plan the greeting' --decisions none --evidence none --constraints none --permissions none --criterion 'A cited answer'`, 1, "authorizes only its research outcome: hand it to Research"),
		fakeRun(`asmai handoff send --job 1 --to research --outcome 'How the greeting is spelled' --decisions none --evidence none --constraints none --permissions none --criterion 'A cited answer'`, 0, "dispatch 1"),
		hookStep("Stop", "", ""),
		`{"expect":"asmai inbox --dispatch 2\r"}`,
		hookStep("UserPromptSubmit", "reply", "asmai inbox --dispatch 2"),
		fakeRun(`asmai inbox --dispatch 2`, 0, "accepted from leader@research"),
		hookStep("Stop", "reply", ""),
		`{"expect":"asmai inbox --dispatch 5\r"}`,
		hookStep("UserPromptSubmit", "ended", "asmai inbox --dispatch 5"),
		fakeRun(`asmai inbox --dispatch 5`, 0, "(?s)Job 1 has ended: its owning leader, leader@research, accepted its outcome against its research-only mandate \\(the answer names its source\\).*no repository.*asmai job clean 1.*assignment 1 \\(worker1@research\\), Find how the greeting is spelled: the results are in \\S+/job-1/assignment-1/scratch.*artifact: findings.md.*evidence: https://example.test/spec#greeting"),
		hookStep("Stop", "ended", ""),
		fakeRun(`touch "$ASMAI_TEST_DIR/coordination-done"`, 0, ""),
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
	a := inTerminal(t, 100, 25)
	waitFor(t, "Coordination prompt", a.shows(">"))
	a.ptmx.WriteString("Please find out how the greeting is spelled\r")
	waitFor(t, "the worker's turn", touched(dir, "worker-done"))
	waitFor(t, "the leader reading the result", touched(dir, "result-read"))

	// The results are in the worker's scratch workspace: empty when it began,
	// and with no git in it.
	workspace, err := os.ReadFile(filepath.Join(dir, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	scratch := strings.TrimSpace(string(workspace))
	if entries, _ := os.ReadFile(filepath.Join(dir, "workspace-entries")); strings.TrimSpace(string(entries)) != "0" {
		t.Errorf("the scratch workspace held %q entries when the worker began, want none", entries)
	}
	if want := filepath.Join("job-1", "assignment-1", "scratch"); !strings.HasSuffix(scratch, want) {
		t.Errorf("the worker's workspace is %s, want it to end in %s", scratch, want)
	}

	// While the job is open, `asmai job clean` is refused and removes nothing.
	if _, stderr, code := runAsmai(t, "job", "clean", "1"); code != 1 || !strings.Contains(stderr, "job 1 is still open") {
		t.Errorf("asmai job clean on an open job exited %d printing %q, want it refused", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(scratch, "findings.md")); err != nil {
		t.Errorf("a refused clean removed the results: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "clean-refused"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the leader accepting the outcome", touched(dir, "research-done"))
	waitFor(t, "Coordination told where the results are", touched(dir, "coordination-done"))

	// The job ended on the leader's acceptance, and the scratch workspaces and
	// their results are kept.
	var shown struct {
		Job struct {
			State      string `json:"state"`
			Mandate    string `json:"mandate"`
			Repository string `json:"repository"`
		} `json:"job"`
	}
	asmaiJSON(t, &shown, "job", "1")
	if shown.Job.State != "ended" || shown.Job.Mandate != "research-only" || shown.Job.Repository != "" {
		t.Errorf("job 1 is %+v, want an ended research-only job with no repository", shown.Job)
	}
	if stdout, _, _ := runAsmai(t, "jobs"); !strings.Contains(stdout, "ended") || !strings.Contains(stdout, "none") {
		t.Errorf("asmai jobs printed %q, want the ended job with no repository", stdout)
	}
	if got, err := os.ReadFile(filepath.Join(scratch, "findings.md")); err != nil || !strings.Contains(string(got), "hello") {
		t.Errorf("the ended job's findings are %q (%v), want them kept", got, err)
	}
	if ended := journaled(t, "job.ended"); len(ended) != 1 {
		t.Errorf("the journal holds %d job.ended entries, want one", len(ended))
	}
	if moved := journaled(t, "job.branch.moved"); len(moved) != 0 {
		t.Errorf("a job with no repository moved a job branch: %+v", moved)
	}

	// The user removes them with `asmai job clean`.
	stdout, stderr, code := runAsmai(t, "job", "clean", "1")
	if code != 0 || !strings.Contains(stdout, "Removed job 1's scratch workspaces") {
		t.Fatalf("asmai job clean on the ended job exited %d printing %q, %q", code, stdout, stderr)
	}
	if _, err := os.Stat(scratch); err == nil {
		t.Errorf("the scratch workspace %s is still there after the clean", scratch)
	}
	if cleaned := journaled(t, "job.scratch.cleaned"); len(cleaned) != 1 {
		t.Errorf("the journal holds %d job.scratch.cleaned entries, want one", len(cleaned))
	}
	if stdout, _, code := runAsmai(t, "job", "clean", "1"); code != 0 || !strings.Contains(stdout, "already removed") {
		t.Errorf("a second asmai job clean exited %d printing %q, want nothing to remove", code, stdout)
	}
	if _, stderr, code := runAsmai(t, "job", "clean", "9"); code != 1 || !strings.Contains(stderr, "job 9 does not exist") {
		t.Errorf("asmai job clean 9 exited %d printing %q", code, stderr)
	}
	if _, stderr, code := runAsmai(t, "job", "clean"); code != 2 || !strings.Contains(stderr, "name the job number") {
		t.Errorf("asmai job clean with no number exited %d printing %q", code, stderr)
	}
}
