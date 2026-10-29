// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/talvor/asmai/internal/fakeprovider"
)

func TestAResearchJobInARepositoryEndsAsANotesPullRequestMarkedReadyWhenCIIsGreen(t *testing.T) {
	deliverNotes(t, false)
}

func TestAResearchJobInARepositoryWithNoCIEndsAsANotesPullRequestMarkedReadyStraightAway(t *testing.T) {
	deliverNotes(t, true)
}

// deliverNotes drives a research-only job in a repository end to end with the
// fake provider and gh replaced at its boundary: Research's worker commits its
// notes to its assignment branch, Research's leader accepts them, asks the
// daemon to push the job branch with no Quality validation, and opens a draft
// pull request with a short notes-only body, which it records. With CI, the
// daemon waits while the head has no checks and while they run, and marks the
// pull request ready once they are green; in a repository registered with
// --no-ci it marks it ready straight away and never reads a check. Either way
// the job ends, its branches stay on origin and Coordination's leader reports
// the link.
func deliverNotes(t *testing.T, noCI bool) {
	gitIdentity(t)
	testDir := t.TempDir()
	t.Setenv("ASMAI_TEST_DIR", testDir)
	const (
		jobBranch = "asmai/test/job-1-research-the-greeting"
		url       = "https://github.com/talvor/asmai-fixture/pull/7"
		sha       = "[0-9a-f]{40}"
		report    = "job 1: notes ready for review: " + url
	)
	t.Setenv(fakeprovider.ScriptEnv+"_RESEARCH_WORKER", filepath.Join(testDir, "worker.jsonl"))
	t.Setenv(fakeprovider.ScriptEnv+"_RESEARCH", filepath.Join(testDir, "research.jsonl"))

	research := concat(agentSession,
		turn(1, "handoff",
			fakeRun(`asmai inbox --dispatch 1`, 0, "Research the greeting"),
			fakeRun(`asmai handoff accept --handoff 1`, 0, "reply dispatch 2"),
			fakeRun(`asmai assign --job 1 --outcome 'Research how the greeting is spelled' --criterion 'the notes cite their source' --skill research`, 0, "dispatch 3"),
		),
		turn(4, "result",
			fakeRun(`asmai inbox --dispatch 4`, 0, "(?s)result [0-9]+ \\| assignment 1.*docs/research/greeting.md"),
			awaitFile("w1-done"),
			fakeRun(`asmai accept --assignment 1 --reason 'the notes cite their source'`, 0, "fast-forwarded"),
			// Only the delivery owner of a job asks for a push, and Quality
			// is asked for nothing.
			fakeRun(`asmai handoff send --job 1 --to quality --outcome 'Validate the notes' --decisions none --evidence none --constraints none --permissions none --criterion 'cited'`, 1, "only leader@engineering hands a job branch to Quality for validation"),
			fakeRun(`asmai push 1`, 0, "the daemon pushed "+jobBranch),
			fakeRun(`printf 'Notes only: this pull request changes no code.\n\n- docs/research/greeting.md: how the greeting is spelled, with its source.\n\nRefs #12\n' > "$ASMAI_TEST_DIR/body.md"`, 0, ""),
			fakeRun(`gh pr create --draft --base main --head `+jobBranch+` --title 'Research the greeting' --body-file "$ASMAI_TEST_DIR/body.md"`, 0, "^"+url+"\n$"),
			fakeRun(`asmai effect pull-request `+url, 0, "pull request #7 of job 1 recorded"),
			touchFile("leader-done"),
		),
		[]string{idleSession},
	)
	worker := concat(agentSession, turn(3, "assignment",
		fakeRun(`asmai inbox --dispatch 3`, 0, "assignment 1"),
		fakeRun(`mkdir -p docs/research && printf '# The greeting\nIt is spelled hello (https://example.test/spec#greeting).\n' > docs/research/greeting.md && git add -A && git commit -q -m 'Add the greeting research notes'`, 0, ""),
		fakeRun(`git push -q origin asmai/test/job-1/1 && asmai effect push "origin/asmai/test/job-1/1@$(git rev-parse HEAD)"`, 0, "recorded: push"),
		fakeRun(`asmai result --evidence 'https://example.test/spec#greeting' --artifact docs/research/greeting.md --test none --check none --gap none --skill research --pr-section 'Adds the research notes on the greeting.'`, 0, "result recorded for assignment 1"),
	), []string{touchFile("w1-done"), idleSession})
	writeFiles(t, testDir, map[string][]string{"research.jsonl": research, "worker.jsonl": worker})

	coordination := concat(
		[]string{
			hookStep("SessionStart", "", ""),
			`{"screen":"> "}`,
			`{"expect":"Please research the greeting\r"}`,
			hookStep("UserPromptSubmit", "", "Please research the greeting"),
			fakeRun(`asmai job open --message latest --repository fixture --mandate research-only --reading 'Research the greeting' --criterion 'A cited answer'`, 0, "Opened job 1"),
			fakeRun(`asmai handoff send --job 1 --to engineering --outcome 'Build the greeting' --decisions none --evidence none --constraints none --permissions none --criterion 'A cited answer'`, 1, "authorizes only its research outcome, never code"),
			fakeRun(`asmai handoff send --job 1 --to research --outcome 'Research the greeting' --decisions none --evidence none --constraints none --permissions none --criterion 'A cited answer'`, 0, "dispatch 1"),
			hookStep("Stop", "", ""),
		},
		turn(2, "reply", fakeRun(`asmai inbox --dispatch 2`, 0, "accepted from leader@research")),
		turn(5, "delivery",
			fakeRun(`asmai inbox --dispatch 5`, 0, "(?s)message [0-9]+ \\| dispatch 5 \\| job 1 \\| delivery from daemon\nJob 1 is delivered: its notes pull request #7, "+url+", is ready for review at head "+sha+", "+map[bool]string{false: "with every check green \\(CI / test\\)", true: "straight away, as its repository is declared as having no CI"}[noCI]+"\\. It holds only the job's notes.*Quality validated nothing.*AsmAI never merges.*in a line tagged with job 1, with the link"),
			`{"screen":"`+report+`\r\n> "}`,
			touchFile("coordination-done"),
		),
		[]string{idleSession},
	)
	bin := filepath.Join(testDir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(fakeCIGH), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	readyFactory(t, coordination)
	calls := filepath.Join(testDir, "gh", "calls")
	t.Cleanup(func() {
		if t.Failed() {
			log, _, _ := runAsmai(t, "log")
			t.Logf("the daemon's log:\n%s", log)
			data, _ := os.ReadFile(calls)
			t.Logf("gh's calls:\n%s", data)
		}
	})
	if _, stderr, code := runAsmai(t, "start"); code != 0 {
		t.Fatal(stderr)
	}
	var addArgs []string
	if noCI {
		addArgs = append(addArgs, "--no-ci")
	}
	fixture := registerWritingFixtureAs(t, nil, addArgs...)
	if err := os.WriteFile(filepath.Join(testDir, "origin"), []byte(fixture.origin), 0o600); err != nil {
		t.Fatal(err)
	}
	terminal := inTerminal(t, 100, 25)
	waitFor(t, "Coordination prompt", terminal.shows(">"))
	terminal.ptmx.WriteString("Please research the greeting\r")
	waitFor(t, "Research's leader recording the pull request", touched(testDir, "leader-done"))

	stateDir := filepath.Join(os.Getenv("HOME"), ".local", "state", "asmai")
	clone, err := filepath.EvalSymlinks(filepath.Join(stateDir, "repositories", "fixture"))
	if err != nil {
		t.Fatal(err)
	}
	head := gitIn(t, clone, "rev-parse", "refs/heads/"+jobBranch)
	checksCall := "pr view 7 --json number,url,title,isDraft,headRefName,headRefOid,isCrossRepository,state,statusCheckRollup"
	looked := func() int {
		data, _ := os.ReadFile(calls)
		return strings.Count(string(data), ": "+checksCall+"\n")
	}
	readied := func() bool {
		data, _ := os.ReadFile(calls)
		return strings.Contains(string(data), ": pr ready ") || strings.Contains(string(data), ": pr edit ")
	}
	writeRollup := func(rollup string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(testDir, "rollup"), []byte(rollup), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if !noCI {
		// A head with no checks is never green, and while its check runs the
		// daemon waits too: nothing is marked ready.
		waitFor(t, "the daemon looking at the head's checks three times", func() bool { return looked() >= 3 })
		if readied() {
			t.Fatal("the pull request was marked ready while its head had no checks")
		}
		writeRollup(`[{"__typename":"CheckRun","name":"test","workflowName":"CI","status":"IN_PROGRESS","conclusion":""}]`)
		waitFor(t, "the daemon recording the running check", func() bool { return len(journaled(t, "checks.observed")) >= 2 })
		before := looked()
		waitFor(t, "the daemon looking again", func() bool { return looked() >= before+2 })
		if readied() {
			t.Fatal("the pull request was marked ready while its check was running")
		}
		writeRollup(`[{"__typename":"CheckRun","name":"test","workflowName":"CI","status":"COMPLETED","conclusion":"SUCCESS","detailsUrl":"https://github.com/talvor/asmai-fixture/actions/runs/1"}]`)
	}
	waitFor(t, "Coordination's leader reporting the delivery", touched(testDir, "coordination-done"))
	waitFor(t, "Coordination's report on the user's terminal", terminal.shows(report))
	waitFor(t, "the daemon removing what was local to the job", func() bool { return len(journaled(t, "job.local.removed")) == 1 })

	// The delivery records the pull request and the head, and the job ended in
	// the same step. With no CI, the daemon never read a check, and the
	// delivery says so.
	type deliveryEntry struct {
		Job    int           `json:"job"`
		Number int           `json:"number"`
		URL    string        `json:"url"`
		Head   string        `json:"head"`
		Checks []interface{} `json:"checks"`
		NoCI   bool          `json:"no_ci"`
	}
	delivered := journaled(t, "job.delivered")
	if len(delivered) != 1 {
		t.Fatalf("deliveries: %+v", delivered)
	}
	d := decode[deliveryEntry](t, delivered[0])
	if d.Job != 1 || d.Number != 7 || d.URL != url || d.Head != head || d.NoCI != noCI || (len(d.Checks) == 0) != noCI {
		t.Errorf("the delivery is %+v, want pull request 7 at %s with no CI %v", d, head, noCI)
	}
	if ended := journaled(t, "job.ended"); len(ended) != 1 || ended[0].ID != delivered[0].ID+1 {
		t.Errorf("the job ended as %+v", ended)
	}
	if noCI {
		if n := looked() + len(journaled(t, "checks.observed")); n != 0 {
			t.Errorf("the daemon read the checks of a repository with no CI (%d times)", n)
		}
	}

	// Nobody validated it: Quality was handed nothing, and the job has the one
	// assignment of its worker.
	if n := len(journaled(t, "validation.accepted")); n != 0 {
		t.Errorf("a notes job was validated %d times", n)
	}
	if n := len(journaled(t, "handoff.sent")); n != 1 {
		t.Errorf("the job's leaders sent %d handoffs, want only Coordination's to Research", n)
	}
	if n := len(journaled(t, "assignment.created")); n != 1 {
		t.Errorf("the job has %d assignments, want its worker's one", n)
	}

	// The pull request was opened as a draft with the short notes-only body,
	// and the daemon marked it ready once, as the only change to it.
	data, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	var readies []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		dir, args, _ := strings.Cut(line, ": ")
		switch {
		case strings.HasPrefix(args, "pr create"):
			if !strings.HasPrefix(args, "pr create --draft ") {
				t.Errorf("the pull request was opened with gh %s, want a draft", args)
			}
		case strings.HasPrefix(args, "pr ready"), strings.HasPrefix(args, "pr edit"):
			readies = append(readies, args)
			if dir != clone {
				t.Errorf("the daemon ran gh %s in %s, not the job's clone", args, dir)
			}
		case strings.HasPrefix(args, "pr merge"), strings.HasPrefix(args, "pr review"):
			t.Errorf("gh ran %q", args)
		}
	}
	if len(readies) != 1 || readies[0] != "pr ready 7" {
		t.Errorf("gh marked the pull request ready with %q, want one pr ready 7", readies)
	}
	body, _ := os.ReadFile(filepath.Join(testDir, "gh", "body-7.md"))
	if !strings.Contains(string(body), "Notes only") || strings.Contains(string(body), "## Summary") || strings.Count(string(body), "\n") > 6 {
		t.Errorf("the pull request's body is %q, want a short notes-only one", body)
	}
	if draft, _ := os.ReadFile(filepath.Join(testDir, "gh", "pr-7", "draft")); strings.TrimSpace(string(draft)) != "false" {
		t.Errorf("the pull request is left draft=%s", draft)
	}

	// The job branch holds the notes and stays on origin with the worker's
	// assignment branch.
	if got := gitIn(t, fixture.origin, "rev-parse", "refs/heads/"+jobBranch); got != head {
		t.Errorf("origin's job branch is at %s, want %s", got, head)
	}
	if got := gitIn(t, fixture.origin, "show", "refs/heads/"+jobBranch+":docs/research/greeting.md"); !strings.Contains(got, "hello") {
		t.Errorf("the job branch's notes are %q", got)
	}
	if out := gitIn(t, fixture.origin, "branch", "--list", "asmai/test/job-1/1"); out == "" {
		t.Error("the worker's assignment branch is gone from origin")
	}
	if _, err := os.Lstat(filepath.Join(stateDir, "views", "job-1")); err == nil {
		t.Error("the job's view is still there")
	}
}

// A prototype stays on its own assignment branch on origin: its leader accepts
// it with --prototype, so the job branch does not move, and its verdict is a
// note on the job branch, written by an assignment of its own, that points to
// the prototype's branch. The notes pull request holds the note and not the
// prototype.
func TestAPrototypeStaysOnItsAssignmentBranchWithAVerdictNotePointingToItInTheNotesPullRequest(t *testing.T) {
	gitIdentity(t)
	testDir := t.TempDir()
	t.Setenv("ASMAI_TEST_DIR", testDir)
	const (
		jobBranch = "asmai/test/job-1-prototype-the-greeting"
		url       = "https://github.com/talvor/asmai-fixture/pull/7"
		sha       = "[0-9a-f]{40}"
		report    = "job 1: notes ready for review: " + url
	)
	t.Setenv(fakeprovider.ScriptEnv+"_RESEARCH_WORKER", filepath.Join(testDir, "worker.jsonl"))
	t.Setenv(fakeprovider.ScriptEnv+"_RESEARCH", filepath.Join(testDir, "research.jsonl"))

	research := concat(agentSession,
		turn(1, "handoff",
			fakeRun(`asmai inbox --dispatch 1`, 0, "Prototype the greeting"),
			fakeRun(`asmai handoff accept --handoff 1`, 0, "reply dispatch 2"),
			fakeRun(`asmai assign --job 1 --outcome 'Prototype how the greeting is formed; the prototype stays on its own branch' --criterion 'the prototype answers the question'`, 0, "dispatch 3"),
		),
		turn(4, "prototype",
			fakeRun(`asmai inbox --dispatch 4`, 0, "(?s)result [0-9]+ \\| assignment 1.*prototype/greeting.sh"),
			awaitFile("w1-done"),
			fakeRun(`asmai accept --assignment 1 --prototype --reason 'the prototype answers the question'`, 0, "(?s)assignment 1 accepted by leader@research as a prototype that stays on its own branch asmai/test/job-1/1 at commit "+sha+" on origin; the assignment has ended\njob branch "+jobBranch+" did not move: it is at "+sha+", and the prototype is not in the job's pull request.*verdict is a note on the job branch, written by a new assignment that names branch asmai/test/job-1/1"),
			fakeRun(`asmai brief 1`, 0, "- 1 worker1@research \\(owner leader@research\\) accepted as a prototype that stays on its own branch asmai/test/job-1/1 at "+sha+" on origin, not on the job branch: Prototype how the greeting is formed"),
			fakeRun(`cp "$ASMAI_TEST_DIR/worker-2.jsonl" "$ASMAI_TEST_DIR/worker.jsonl"`, 0, ""),
			fakeRun(`asmai assign --job 1 --outcome 'Write the verdict note on the prototype on branch asmai/test/job-1/1' --criterion 'the note points to branch asmai/test/job-1/1'`, 0, "dispatch 5"),
		),
		turn(6, "verdict",
			fakeRun(`asmai inbox --dispatch 6`, 0, "(?s)result [0-9]+ \\| assignment 2.*verdict.md"),
			awaitFile("w2-done"),
			fakeRun(`asmai accept --assignment 2 --reason 'the note points to branch asmai/test/job-1/1'`, 0, "fast-forwarded"),
			fakeRun(`asmai push 1`, 0, "the daemon pushed "+jobBranch),
			fakeRun(`printf 'Notes only: this pull request changes no code.\n\n- docs/research/greeting-verdict.md: the verdict on the prototype, which stays on branch asmai/test/job-1/1.\n' > "$ASMAI_TEST_DIR/body.md"`, 0, ""),
			fakeRun(`gh pr create --draft --base main --head `+jobBranch+` --title 'Prototype the greeting' --body-file "$ASMAI_TEST_DIR/body.md"`, 0, "^"+url+"\n$"),
			fakeRun(`asmai effect pull-request `+url, 0, "pull request #7 of job 1 recorded"),
			touchFile("leader-done"),
		),
		[]string{idleSession},
	)
	worker1 := concat(agentSession, turn(3, "prototype",
		fakeRun(`asmai inbox --dispatch 3`, 0, "assignment 1"),
		fakeRun(`mkdir -p prototype && printf 'echo hello\n' > prototype/greeting.sh && git add -A && git commit -q -m 'Prototype the greeting'`, 0, ""),
		fakeRun(`git push -q origin asmai/test/job-1/1 && asmai effect push "origin/asmai/test/job-1/1@$(git rev-parse HEAD)"`, 0, "recorded: push"),
		fakeRun(`asmai result --evidence 'prototype/greeting.sh prints hello' --artifact prototype/greeting.sh --test none --check none --gap none --pr-section 'A prototype that stays on its own branch.'`, 0, "result recorded for assignment 1"),
	), []string{touchFile("w1-done"), idleSession})
	worker2 := concat(agentSession, turn(5, "verdict",
		fakeRun(`asmai inbox --dispatch 5`, 0, "assignment 2"),
		fakeRun(`mkdir -p docs/research && printf '# The greeting prototype\nVerdict: the greeting is formed as hello. The prototype is on branch asmai/test/job-1/1 on origin.\n' > docs/research/greeting-verdict.md && git add -A && git commit -q -m 'Add the greeting prototype verdict'`, 0, ""),
		fakeRun(`git push -q origin asmai/test/job-1/2 && asmai effect push "origin/asmai/test/job-1/2@$(git rev-parse HEAD)"`, 0, "recorded: push"),
		fakeRun(`asmai result --evidence 'the verdict names branch asmai/test/job-1/1' --artifact docs/research/greeting-verdict.md --test none --check none --gap none --pr-section 'Adds the verdict on the prototype.'`, 0, "result recorded for assignment 2"),
	), []string{touchFile("w2-done"), idleSession})
	writeFiles(t, testDir, map[string][]string{"research.jsonl": research, "worker.jsonl": worker1, "worker-2.jsonl": worker2})

	coordination := concat(
		[]string{
			hookStep("SessionStart", "", ""),
			`{"screen":"> "}`,
			`{"expect":"Please prototype the greeting\r"}`,
			hookStep("UserPromptSubmit", "", "Please prototype the greeting"),
			fakeRun(`asmai job open --message latest --repository fixture --mandate research-only --reading 'Prototype the greeting' --criterion 'A verdict'`, 0, "Opened job 1"),
			fakeRun(`asmai handoff send --job 1 --to research --outcome 'Prototype the greeting' --decisions none --evidence none --constraints none --permissions none --criterion 'A verdict'`, 0, "dispatch 1"),
			hookStep("Stop", "", ""),
		},
		turn(2, "reply", fakeRun(`asmai inbox --dispatch 2`, 0, "accepted from leader@research")),
		turn(7, "delivery",
			fakeRun(`asmai inbox --dispatch 7`, 0, "(?s)Job 1 is delivered: its notes pull request #7, "+url+", is ready for review at head "+sha+", straight away, as its repository is declared as having no CI"),
			`{"screen":"`+report+`\r\n> "}`,
			touchFile("coordination-done"),
		),
		[]string{idleSession},
	)
	bin := filepath.Join(testDir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(fakeCIGH), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
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
	fixture := registerWritingFixtureAs(t, nil, "--no-ci")
	if err := os.WriteFile(filepath.Join(testDir, "origin"), []byte(fixture.origin), 0o600); err != nil {
		t.Fatal(err)
	}
	terminal := inTerminal(t, 100, 25)
	waitFor(t, "Coordination prompt", terminal.shows(">"))
	terminal.ptmx.WriteString("Please prototype the greeting\r")
	waitFor(t, "Coordination's report on the user's terminal", terminal.shows(report))
	waitFor(t, "the daemon removing what was local to the job", func() bool { return len(journaled(t, "job.local.removed")) == 1 })

	// The prototype is on its own branch on origin, and only there.
	if out := gitIn(t, fixture.origin, "ls-tree", "-r", "--name-only", "refs/heads/asmai/test/job-1/1"); !strings.Contains(out, "prototype/greeting.sh") {
		t.Errorf("the prototype's branch on origin holds %q, want the prototype", out)
	}
	if out := gitIn(t, fixture.origin, "ls-tree", "-r", "--name-only", "refs/heads/"+jobBranch); strings.Contains(out, "prototype/") || !strings.Contains(out, "docs/research/greeting-verdict.md") {
		t.Errorf("the job branch on origin holds %q, want the verdict note and not the prototype", out)
	}

	// The verdict note points to the prototype's branch.
	if note := gitIn(t, fixture.origin, "show", "refs/heads/"+jobBranch+":docs/research/greeting-verdict.md"); !strings.Contains(note, "asmai/test/job-1/1") {
		t.Errorf("the verdict note is %q, want it to point to branch asmai/test/job-1/1", note)
	}

	// The prototype's acceptance moved no job branch: the job branch moved
	// once, for the verdict note.
	if kept := journaled(t, "prototype.kept"); len(kept) != 1 {
		t.Errorf("the journal holds %d prototype.kept entries, want one", len(kept))
	}
	if moved := journaled(t, "job.branch.moved"); len(moved) != 1 {
		t.Errorf("the job branch moved %d times, want once for the verdict note", len(moved))
	}
}
