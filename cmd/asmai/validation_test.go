// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/talvor/asmai/internal/fakeprovider"
)

// turn is one dispatch of an agent: it waits for the nudge, reports the
// prompt the provider submitted, runs the commands and ends the turn.
func turn(dispatch int, prompt string, steps ...string) []string {
	s := []string{
		fmt.Sprintf(`{"expect":"asmai inbox --dispatch %d\r"}`, dispatch),
		hookStep("UserPromptSubmit", prompt, fmt.Sprintf("asmai inbox --dispatch %d", dispatch)),
	}
	s = append(s, steps...)
	return append(s, hookStep("Stop", prompt, ""))
}

// agentSession is the start of an agent's session: a lifecycle hook and a prompt
// drawn with bracketed paste on, which is when its input is ready.
var agentSession = []string{hookStep("SessionStart", "", ""), `{"screen":"\u001b[?2004h> "}`}

// idleSession ends a script by waiting for input that no one types, so that the
// session lasts until the daemon stops it.
var idleSession = `{"expect":"only typed by a test that means to"}`

// touchFile is a step that touches a file of the test's directory, which a script
// does once its turn has finished, and awaitFile one that waits for it.
func touchFile(name string) string {
	return fakeRun(`touch "$ASMAI_TEST_DIR/`+name+`"`, 0, "")
}

func awaitFile(name string) string {
	return fakeRun(`while [ ! -e "$ASMAI_TEST_DIR/`+name+`" ]; do sleep 0.02; done`, 0, "")
}

// headOf reads the exact head commit a handoff to Quality names, from the
// inbox of the dispatch that carries it.
func headOf(dispatch int) string {
	return fmt.Sprintf(`"$(asmai inbox --dispatch %d | sed -n 's/^Head to validate: //p')"`, dispatch)
}

// writeScripts writes each script to a file of the test's directory, and
// returns the directory's path.
func writeFiles(t *testing.T, dir string, files map[string][]string) {
	t.Helper()
	for name, steps := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(strings.Join(steps, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// TestQualityValidatesTheFinishedJobBranchAtItsExactHeadAndABlockingFindingSendsEngineeringBack
// drives a job end to end with the fake provider: its work is accepted, the
// base moves and a worker merges it in, Engineering hands the finished branch
// to Quality, whose leader starts on demand and assigns a read-only
// validation at the exact head, a blocking finding sends Engineering back to
// assign a fix, and Quality's re-validation of the new head clears it.
func TestQualityValidatesTheFinishedJobBranchAtItsExactHeadAndABlockingFindingSendsEngineeringBack(t *testing.T) {
	gitIdentity(t)
	testDir := t.TempDir()
	t.Setenv("ASMAI_TEST_DIR", testDir)
	const (
		jobBranch = "asmai/job-1-add-a-greeting"
		sha       = "[0-9a-f]{40}"
	)
	// Each worker session plays whatever its script file holds when it
	// starts, so each leader puts the script of the next one in place.
	t.Setenv(fakeprovider.ScriptEnv+"_WORKER", filepath.Join(testDir, "worker.jsonl"))
	t.Setenv(fakeprovider.ScriptEnv+"_QUALITY_WORKER", filepath.Join(testDir, "quality-worker.jsonl"))
	t.Setenv(fakeprovider.ScriptEnv+"_ENGINEERING", filepath.Join(testDir, "engineering.jsonl"))
	t.Setenv(fakeprovider.ScriptEnv+"_QUALITY", filepath.Join(testDir, "quality.jsonl"))

	handoffToQuality := func(criterion string) string {
		return `asmai handoff send --job 1 --to quality --outcome 'Validate the job branch at its exact head' --decisions none --evidence 'results 1 and 2 accepted, with their checks' --constraints none --permissions none --criterion '` + criterion + `'`
	}
	pushWorker := func(branch string) string {
		return `git push -q origin ` + branch + ` && asmai effect push "origin/` + branch + `@$(git rev-parse HEAD)"`
	}

	engineering := concat(agentSession,
		turn(1, "handoff",
			fakeRun(`asmai inbox --dispatch 1`, 0, "(?s)Add a greeting.*Greeting is printed"),
			fakeRun(`asmai handoff accept --handoff 1`, 0, "reply dispatch 2"),
			fakeRun(`asmai assign --job 1 --outcome 'Add greeting.txt, with a check that it says hello' --criterion 'greeting.txt says hello' --criterion 'the check passes'`, 0, "assignment 1 of job 1 given to worker1@engineering; dispatch 3"),
		),
		// The writing result is accepted, and the job branch holds it.
		turn(4, "result1",
			fakeRun(`asmai inbox --dispatch 4`, 0, "result [0-9]+ \\| assignment 1 \\| worker1@engineering"),
			awaitFile("w1-done"),
			fakeRun(`asmai accept --assignment 1 --reason 'greeting.txt says hello, which check.sh asserts' --reason 'check.sh passed at the commit'`, 0, "fast-forwarded"),
			// Quality's leader is not running: nothing has asked it to start.
			fakeRun(`asmai agents | grep -c 'leader@quality' | grep -qx 0`, 0, ""),
			// The base moves, so the branch has not taken in the latest base,
			// and the daemon will not hand it over.
			fakeRun(`base="$(cat "$ASMAI_TEST_DIR/origin")" && git clone -q "$base" "$ASMAI_TEST_DIR/mover" && cd "$ASMAI_TEST_DIR/mover" && printf 'moved\n' > base-moved.txt && git add -A && git commit -q -m 'Move the base' && git push -q origin main`, 0, ""),
			fakeRun(handoffToQuality("checks re-run"), 1, "(?s)has not taken in the latest base.*origin's main is at "+sha+".*Assign a worker to merge origin/main"),
			fakeRun(`asmai agents | grep -c 'leader@quality' | grep -qx 0`, 0, ""),
			fakeRun(`cp "$ASMAI_TEST_DIR/worker-2.jsonl" "$ASMAI_TEST_DIR/worker.jsonl"`, 0, ""),
			fakeRun(`asmai assign --job 1 --outcome 'Take in the latest base: merge origin/main into your assignment branch' --criterion 'the job branch has the moved base' --criterion 'the checks pass after the merge'`, 0, "assignment 2 of job 1 given to worker1@engineering; dispatch 5"),
		),
		// The merge of the base is accepted through the normal path, and the
		// finished branch is handed to Quality.
		turn(6, "result2",
			fakeRun(`asmai inbox --dispatch 6`, 0, "(?s)result [0-9]+ \\| assignment 2 \\| worker1@engineering.*Merges origin/main"),
			awaitFile("w2-done"),
			fakeRun(`asmai accept --assignment 2 --reason 'the base is merged in' --reason 'the checks pass after the merge'`, 0, "fast-forwarded"),
			fakeRun(`asmai handoff send --job 1 --to coordination --outcome x --decisions none --evidence none --constraints none --permissions none --criterion y`, 1, "Engineering hands validation to Quality"),
			fakeRun(handoffToQuality("checks re-run at the head"), 0, "handoff 2 sent to leader@quality for job 1; dispatch 7"),
			fakeRun(`asmai agents`, 0, "(?m)^leader@quality +"),
		),
		turn(8, "accepted", fakeRun(`asmai inbox --dispatch 8`, 0, "accepted from leader@quality")),
		// Quality's report: a blocking, an advisory and a needs-you finding.
		turn(11, "validation1",
			fakeRun(`asmai inbox --dispatch 11`, 0, "(?s)validation from leader@quality.*Validation delivered by leader@quality: Validation of commit "+sha+": 1 blocking, 1 advisory, 1 needs-you.*Validation report at commit "+sha+".*finding 1 \\| blocking \\| greeting.txt says hello.*finding 2 \\| advisory.*finding 3 \\| needs-you"),
			fakeRun(`asmai brief 1`, 0, "(?s)Findings:\n- 1 blocking \\(open\\), found at "+sha+": greeting.txt says hello.*- 2 advisory, found at.*- 3 needs-you, found at"),
			fakeRun(`cp "$ASMAI_TEST_DIR/worker-3.jsonl" "$ASMAI_TEST_DIR/worker.jsonl"`, 0, ""),
			fakeRun(`asmai assign --job 1 --outcome 'Fix blocking finding 1: greeting.txt must greet the world' --criterion 'greeting.txt says hello, world' --criterion 'check.sh asserts it and passes'`, 0, "assignment 4 of job 1 given to worker1@engineering; dispatch 12"),
		),
		turn(13, "result4",
			fakeRun(`asmai inbox --dispatch 13`, 0, "result [0-9]+ \\| assignment 4 \\| worker1@engineering"),
			awaitFile("w3-done"),
			fakeRun(`asmai accept --assignment 4 --reason 'greeting.txt greets the world' --reason 'check.sh asserts it'`, 0, "fast-forwarded"),
			// Engineering's fix does not clear the finding: only Quality's
			// re-validation does.
			fakeRun(`asmai brief 1`, 0, "- 1 blocking \\(open\\)"),
			fakeRun(handoffToQuality("checks re-run at the new head and finding 1 confirmed resolved"), 0, "handoff 3 sent to leader@quality for job 1; dispatch 14"),
		),
		turn(15, "accepted2", fakeRun(`asmai inbox --dispatch 15`, 0, "accepted from leader@quality")),
		turn(18, "validation2",
			fakeRun(`asmai inbox --dispatch 18`, 0, "(?s)validation from leader@quality.*0 blocking, 0 advisory, 0 needs-you finding\\(s\\); 1 earlier blocking finding\\(s\\) resolved, 0 still open.*Findings: none.*finding 1 resolved"),
			fakeRun(`asmai brief 1`, 0, "(?s)Findings:\n- 1 blocking \\(cleared by report [0-9]+\\)"),
			touchFile("leader-done"),
		),
		[]string{idleSession},
	)

	quality := concat(agentSession,
		turn(7, "handoff2",
			fakeRun(`asmai inbox --dispatch 7`, 0, "(?s)handoff 2 \\| leader@engineering -> leader@quality.*Validate the job branch at its exact head.*Head to validate: "+sha),
			fakeRun(`asmai handoff accept --handoff 2`, 0, "reply dispatch 8"),
			fakeRun(`asmai assign --job 1 --outcome x --criterion y`, 1, "only leader@engineering may assign writing work"),
			fakeRun(`asmai assign --read-only --job 1 --outcome x --criterion y`, 1, "give --commit"),
			fakeRun(`asmai assign --read-only --commit 0000000000000000000000000000000000000000 --job 1 --outcome x --criterion y`, 1, "validation is fixed at the exact head"),
			fakeRun(`asmai assign --read-only --commit `+headOf(7)+` --job 1 --outcome 'Validate the job branch at its exact head' --criterion 'the repository checks re-ran at the commit, with their outcomes' --criterion 'the change is reviewed against the repository instructions and the job mandate'`, 0, "assignment 3 of job 1 given to worker1@quality; dispatch 9"),
			fakeRun(`asmai assign --read-only --commit `+headOf(7)+` --job 1 --outcome again --criterion x`, 1, "already has validation 3 in progress"),
		),
		turn(10, "report1",
			fakeRun(`asmai inbox --dispatch 10`, 0, "(?s)result [0-9]+ \\| assignment 3 \\| worker1@quality \\| commit "+sha+".*Validation report at commit "+sha+".*Artifacts:.*validated commit "+sha+" of job branch "+jobBranch+", in a read-only workspace.*Evidence:.*Checks run:.*\\./check.sh -> passed.*Findings:.*blocking \\| greeting.txt says hello.*advisory \\| check.sh has no usage comment.*needs-you \\| the request said"),
			awaitFile("v1-done"),
			fakeRun(`asmai accept --assignment 3 --reason 'the checks re-ran at the exact head' --reason 'each finding has a kind and a reason'`, 0, "(?s)validation 3 accepted by leader@quality at commit "+sha+".*delivered to leader@engineering as dispatch 11; nothing about the job branch changed"),
			fakeRun(`asmai accept --assignment 3 --reason x`, 1, "assignment 3 is accepted"),
		),
		turn(14, "handoff3",
			fakeRun(`asmai inbox --dispatch 14`, 0, "(?s)handoff 3 \\| leader@engineering -> leader@quality.*Head to validate: "+sha),
			fakeRun(`asmai handoff accept --handoff 3`, 0, "reply dispatch 15"),
			fakeRun(`cp "$ASMAI_TEST_DIR/quality-worker-2.jsonl" "$ASMAI_TEST_DIR/quality-worker.jsonl"`, 0, ""),
			fakeRun(`asmai assign --read-only --commit `+headOf(14)+` --job 1 --outcome 'Validate the new head, and confirm finding 1 resolved' --criterion 'the checks re-ran at the commit' --criterion 'finding 1 is resolved or still open'`, 0, "assignment 5 of job 1 given to worker1@quality; dispatch 16"),
		),
		turn(17, "report2",
			fakeRun(`asmai inbox --dispatch 17`, 0, "(?s)Validation report at commit "+sha+".*Findings: none.*finding 1 resolved"),
			awaitFile("v2-done"),
			fakeRun(`asmai accept --assignment 5 --reason 'the checks re-ran at the new head' --reason 'finding 1 is confirmed resolved'`, 0, "delivered to leader@engineering as dispatch 18"),
			touchFile("quality-done"),
		),
		[]string{idleSession},
	)

	// The first validation.
	validator1 := concat(agentSession,
		turn(9, "validate1",
			fakeRun(`asmai inbox --dispatch 9`, 0, "(?s)assignment 3 \\| job 1 \\| leader@quality -> worker1@quality \\| active.*Read-only workspace: \\S+/assignment-3/repo.*Fixed at commit: "+sha+", the head of the job branch "+jobBranch+"; it has no branch.*Job intent:.*User's words: Please add a greeting.*Coordination's reading: Add a greeting.*Mandate: tested-pr.*Greeting is printed.*Earlier blocking findings still open.*- none"),
			fakeRun(`asmai assign --job 1 --outcome x --criterion y`, 1, "only leader@engineering"),
			fakeRun(`pwd -P > "$ASMAI_TEST_DIR/validation-workspace"; git rev-parse HEAD > "$ASMAI_TEST_DIR/validated-head"; git branch --show-current > "$ASMAI_TEST_DIR/validation-branch"; git status --porcelain > "$ASMAI_TEST_DIR/validation-status"`, 0, ""),
			fakeRun(`test "$(git rev-parse HEAD)" = "$(asmai inbox --dispatch 9 | sed -n 's/^Fixed at commit: \([0-9a-f]*\),.*/\1/p')"`, 0, ""),
			fakeRun(`./check.sh`, 0, ""),
			// A Quality worker that changes the work cannot report it.
			fakeRun(`git commit -q --allow-empty -m 'a fix Quality must not make'`, 0, ""),
			fakeRun(`asmai result --evidence x --check none --gap none --finding none`, 1, "validation never changes the work"),
			fakeRun(`git reset -q --hard "$(cat "$ASMAI_TEST_DIR/validated-head")"`, 0, ""),
			fakeRun(`asmai result --evidence x --check none --gap none`, 1, "a Quality worker's validation report gives --finding instead"),
			fakeRun(`asmai result --evidence x --check none --gap none --finding 'minor: x'`, 1, "is not '<blocking|advisory|needs-you>: <text>'"),
			fakeRun(`asmai result --evidence x --check none --gap none --finding 'blocking: x' --pr-section y`, 1, "has findings, not tests or a PR section"),
			fakeRun(`asmai result --evidence './check.sh exits 0 at the head commit' --check './check.sh -> passed' --gap none --finding 'blocking: greeting.txt says hello, but the request asked the greeting for the world' --finding 'advisory: check.sh has no usage comment' --finding 'needs-you: the request said greet the world, while the job criterion says only that a greeting is printed'`, 0, "validation report recorded for assignment 3 at commit "+sha+"; sent to leader@quality as dispatch 10"),
		),
		[]string{touchFile("v1-done"), idleSession},
	)
	// The second validation says what became of the blocking finding.
	validator2 := concat(agentSession,
		turn(16, "validate2",
			fakeRun(`asmai inbox --dispatch 16`, 0, "(?s)assignment 5 \\| job 1 \\| leader@quality -> worker1@quality.*Earlier blocking findings still open.*- finding 1, found at "+sha+": greeting.txt says hello"),
			fakeRun(`./check.sh`, 0, ""),
			fakeRun(`asmai result --evidence x --check './check.sh -> passed' --gap none --finding none`, 1, "(?s)says whether each earlier blocking finding is resolved.*1 \\(greeting.txt says hello"),
			fakeRun(`asmai result --evidence x --check './check.sh -> passed' --gap none --finding none --resolved 1 --resolved 7`, 1, "finding 7 is not an open blocking finding of job 1"),
			fakeRun(`asmai result --evidence './check.sh exits 0 at the new head' --check './check.sh -> passed' --gap none --finding none --resolved 1`, 0, "validation report recorded for assignment 5 at commit "+sha),
		),
		[]string{touchFile("v2-done"), idleSession},
	)

	// The writing workers.
	worker1 := concat(agentSession, turn(3, "assignment",
		fakeRun(`asmai inbox --dispatch 3`, 0, "assignment 1"),
		fakeRun(`printf 'hello\n' > greeting.txt && printf '#!/bin/sh\ntest "$(cat greeting.txt)" = hello\n' > check.sh && chmod +x check.sh && ./check.sh`, 0, ""),
		fakeRun(`git add -A && git commit -q -m 'Add greeting.txt and its check' && asmai effect commit "$(git rev-parse HEAD)"`, 0, "effect 1 recorded: commit"),
		fakeRun(`git merge --no-edit `+jobBranch, 0, "up to date"),
		fakeRun(pushWorker("asmai/job-1/1"), 0, "effect 2 recorded: push"),
		fakeRun(`asmai result --evidence './check.sh exits 0 at HEAD' --test 'check.sh asserts greeting.txt says hello' --check './check.sh -> passed' --gap none --pr-section 'Adds greeting.txt, saying hello.'`, 0, "result recorded for assignment 1"),
	), []string{touchFile("w1-done"), idleSession})
	worker2 := concat(agentSession, turn(5, "assignment",
		fakeRun(`asmai inbox --dispatch 5`, 0, "assignment 2"),
		fakeRun(`git fetch -q origin && git merge --no-edit origin/main`, 0, "Merge made"),
		fakeRun(`./check.sh && test -f base-moved.txt`, 0, ""),
		fakeRun(pushWorker("asmai/job-1/2"), 0, "effect [0-9]+ recorded: push"),
		fakeRun(`asmai result --evidence './check.sh exits 0 after the merge' --test none --check './check.sh -> passed' --gap none --pr-section 'Merges origin/main, which moved, into the job branch.'`, 0, "result recorded for assignment 2"),
	), []string{touchFile("w2-done"), idleSession})
	worker3 := concat(agentSession, turn(12, "assignment",
		fakeRun(`asmai inbox --dispatch 12`, 0, "(?s)assignment 4.*Fix blocking finding 1"),
		fakeRun(`printf 'hello, world\n' > greeting.txt && printf '#!/bin/sh\ntest "$(cat greeting.txt)" = "hello, world"\n' > check.sh && ./check.sh`, 0, ""),
		fakeRun(`git add -A && git commit -q -m 'Greet the world' && git merge --no-edit `+jobBranch, 0, "up to date"),
		fakeRun(pushWorker("asmai/job-1/4"), 0, "effect [0-9]+ recorded: push"),
		fakeRun(`asmai result --evidence './check.sh exits 0 at HEAD' --test 'check.sh asserts the world is greeted' --check './check.sh -> passed' --gap none --pr-section 'greeting.txt greets the world.'`, 0, "result recorded for assignment 4"),
	), []string{touchFile("w3-done"), idleSession})

	writeFiles(t, testDir, map[string][]string{
		"engineering.jsonl":      engineering,
		"quality.jsonl":          quality,
		"worker.jsonl":           worker1,
		"worker-2.jsonl":         worker2,
		"worker-3.jsonl":         worker3,
		"quality-worker.jsonl":   validator1,
		"quality-worker-2.jsonl": validator2,
	})

	coordination := []string{
		hookStep("SessionStart", "", ""),
		`{"screen":"> "}`,
		`{"expect":"Please add a greeting\r"}`,
		hookStep("UserPromptSubmit", "", "Please add a greeting"),
		fakeRun(`asmai job open --message latest --repository fixture --reading 'Add a greeting' --criterion 'Greeting is printed'`, 0, "Opened job 1"),
		fakeRun(`asmai handoff send --job 1 --to quality --outcome x --decisions none --evidence none --constraints none --permissions none --criterion y`, 1, "only leader@engineering hands a job branch to Quality"),
		fakeRun(`asmai handoff send --job 1 --to engineering --outcome 'Add a greeting' --decisions none --evidence none --constraints none --permissions none --criterion 'Greeting is printed'`, 0, "dispatch 1"),
		hookStep("Stop", "", ""),
		`{"expect":"asmai inbox --dispatch 2\r"}`,
		hookStep("UserPromptSubmit", "reply", "asmai inbox --dispatch 2"),
		fakeRun(`asmai inbox --dispatch 2`, 0, "accepted from leader@engineering"),
		hookStep("Stop", "reply", ""),
		idleSession,
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
	fixture := registerWritingFixture(t)
	if err := os.WriteFile(filepath.Join(testDir, "origin"), []byte(fixture.origin), 0o600); err != nil {
		t.Fatal(err)
	}
	terminal := inTerminal(t, 100, 25)
	waitFor(t, "Coordination prompt", terminal.shows(">"))
	terminal.ptmx.WriteString("Please add a greeting\r")
	waitFor(t, "Quality's leader accepting the second validation", touched(testDir, "quality-done"))
	waitFor(t, "Engineering's leader reading it", touched(testDir, "leader-done"))

	stateDir := filepath.Join(os.Getenv("HOME"), ".local", "state", "asmai")
	clone := filepath.Join(stateDir, "repositories", "fixture")

	// The job branch holds the accepted work and the merged base, and was
	// never pushed: validation is before the daemon's push.
	jobTip := gitIn(t, clone, "rev-parse", "refs/heads/"+jobBranch)
	if got := gitIn(t, clone, "show", jobTip+":greeting.txt"); got != "hello, world" {
		t.Errorf("greeting.txt on the job branch is %q, want the fixed one", got)
	}
	if got := gitIn(t, clone, "show", jobTip+":base-moved.txt"); got != "moved" {
		t.Errorf("base-moved.txt on the job branch is %q, want the merged base", got)
	}
	if out := gitIn(t, fixture.origin, "branch", "--list", "asmai/job-1-*"); out != "" {
		t.Errorf("the job branch was pushed: %s", out)
	}

	// Quality is a leader started on demand, once, by Engineering's handoff.
	handoffs := journaled(t, "handoff.sent")
	if len(handoffs) != 3 {
		t.Fatalf("handoffs sent: %+v", handoffs)
	}
	type handoffEntry struct {
		ID       int    `json:"id"`
		Sender   string `json:"sender"`
		Receiver string `json:"receiver"`
		Head     string `json:"head"`
	}
	var heads []string
	for _, e := range handoffs[1:] {
		h := decode[handoffEntry](t, e)
		if h.Sender != "leader@engineering" || h.Receiver != "leader@quality" || h.Head == "" {
			t.Errorf("handoff %+v, want Engineering's to Quality's leader naming the head", h)
		}
		heads = append(heads, h.Head)
	}
	if decode[handoffEntry](t, handoffs[0]).Head != "" {
		t.Errorf("the handoff to Engineering names a head to validate")
	}
	started := 0
	for _, a := range agentsListed(t) {
		if a.Agent == "leader@quality" {
			started = a.Generation
		}
	}
	if started != 1 {
		t.Errorf("Quality's leader ran %d times, want it started once on demand", started)
	}

	// Each validation is a read-only assignment of Quality's leader, fixed at
	// the exact head the handoff named, with no branch.
	type assignmentEntry struct {
		ID        int    `json:"id"`
		Owner     string `json:"owner"`
		Worker    string `json:"worker"`
		ReadOnly  bool   `json:"read_only"`
		Commit    string `json:"commit"`
		Workspace struct {
			Branch string `json:"branch"`
			Base   string `json:"base"`
			Path   string `json:"path"`
		} `json:"workspace"`
		Intent struct {
			Words    string   `json:"words"`
			Mandate  string   `json:"mandate"`
			Criteria []string `json:"acceptance_criteria"`
		} `json:"intent"`
	}
	var validations []assignmentEntry
	writing := 0
	for _, e := range journaled(t, "assignment.created") {
		a := decode[assignmentEntry](t, e)
		if !a.ReadOnly {
			writing++
			if a.Owner != "leader@engineering" || a.Worker != "worker1@engineering" || a.Workspace.Branch == "" {
				t.Errorf("writing assignment %+v", a)
			}
			continue
		}
		validations = append(validations, a)
	}
	if writing != 3 || len(validations) != 2 {
		t.Fatalf("assignments created: %d writing and %d read-only, want 3 and 2", writing, len(validations))
	}
	for i, v := range validations {
		if v.Owner != "leader@quality" || v.Worker != "worker1@quality" || v.Commit != heads[i] || v.Workspace.Base != heads[i] || v.Workspace.Branch != "" ||
			v.Intent.Words != "Please add a greeting" || v.Intent.Mandate != "tested-pr" || strings.Join(v.Intent.Criteria, "|") != "Greeting is printed" {
			t.Errorf("validation %d is %+v, want Quality's read-only assignment fixed at %s with the job's intent", i+1, v, heads[i])
		}
	}
	if heads[0] == heads[1] || heads[1] != jobTip {
		t.Errorf("the validations are at %v, want two heads, the second the job branch's tip %s", heads, jobTip)
	}

	// The first worker ran in a clean, detached workspace at the exact head.
	read := func(name string) string {
		data, err := os.ReadFile(filepath.Join(testDir, name))
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(data))
	}
	if got := read("validated-head"); got != heads[0] {
		t.Errorf("Quality's worker was at %s, want the exact head %s", got, heads[0])
	}
	if got := read("validation-branch"); got != "" {
		t.Errorf("Quality's worker was on branch %q, want none", got)
	}
	if got := read("validation-status"); got != "" {
		t.Errorf("Quality's workspace was not clean: %s", got)
	}
	realState, _ := filepath.EvalSymlinks(stateDir)
	realWorkspace := filepath.Join(realState, "workspaces", "job-1", "assignment-3", "repo")
	if got := read("validation-workspace"); got != realWorkspace {
		t.Errorf("Quality's worker ran in %s, want its workspace %s", got, realWorkspace)
	}

	// The read-only workspaces are removed when their assignments end, and
	// no branch of Quality's was ever made or pushed.
	for _, v := range validations {
		if _, err := os.Stat(v.Workspace.Path); !os.IsNotExist(err) {
			t.Errorf("the read-only workspace %s is still there (%v)", v.Workspace.Path, err)
		}
	}
	if got := len(journaled(t, "workspace.read-only.removed")); got != 2 {
		t.Errorf("%d read-only workspaces were journaled as removed, want 2", got)
	}
	if out := gitIn(t, fixture.origin, "branch", "--list", "asmai/job-1/3", "asmai/job-1/5", "asmai/quality*"); out != "" {
		t.Errorf("origin has a branch of Quality's: %s", out)
	}
	if out := gitIn(t, clone, "branch", "--list", "asmai/job-1/3", "asmai/job-1/5"); out != "" {
		t.Errorf("AsmAI's clone has a branch of Quality's: %s", out)
	}
	if out := gitIn(t, clone, "worktree", "list"); strings.Contains(out, "assignment-3") || strings.Contains(out, "assignment-5") {
		t.Errorf("AsmAI's clone still has Quality's workspaces:\n%s", out)
	}

	// The validation reports record the exact head, the checks re-run and
	// each finding with its kind and text; the second clears the first's
	// blocking finding.
	type reportEntry struct {
		Assignment int    `json:"assignment"`
		Kind       string `json:"kind"`
		Commit     string `json:"commit"`
		Checks     []struct {
			Command string `json:"command"`
			Outcome string `json:"outcome"`
		} `json:"checks"`
		Validation struct {
			Findings []struct {
				Kind string `json:"kind"`
				Text string `json:"text"`
			} `json:"findings"`
			Resolved []int `json:"resolved"`
		} `json:"validation"`
	}
	var reports []reportEntry
	for _, e := range journaled(t, "result.submitted") {
		if r := decode[reportEntry](t, e); r.Assignment == 3 || r.Assignment == 5 {
			reports = append(reports, r)
		}
	}
	if len(reports) != 2 {
		t.Fatalf("validation reports: %+v", reports)
	}
	first, second := reports[0], reports[1]
	if first.Commit != heads[0] || len(first.Checks) != 1 || first.Checks[0].Command != "./check.sh" || first.Checks[0].Outcome != "passed" || len(first.Validation.Findings) != 3 {
		t.Errorf("the first validation report is %+v", first)
	}
	for i, want := range []string{"blocking", "advisory", "needs-you"} {
		if got := first.Validation.Findings[i].Kind; got != want || first.Validation.Findings[i].Text == "" {
			t.Errorf("finding %d is %+v, want kind %s with its text", i+1, first.Validation.Findings[i], want)
		}
	}
	if second.Commit != heads[1] || len(second.Validation.Findings) != 0 || len(second.Validation.Resolved) != 1 || second.Validation.Resolved[0] != 1 {
		t.Errorf("the second validation report is %+v", second)
	}

	// Findings are recorded when Quality's leader accepts, and only the
	// second acceptance clears the blocking one.
	type acceptedEntry struct {
		Assignment int `json:"assignment"`
		Findings   []struct {
			ID   int    `json:"id"`
			Kind string `json:"kind"`
		} `json:"findings"`
		Cleared []int `json:"cleared"`
	}
	accepted := journaled(t, "validation.accepted")
	if len(accepted) != 2 {
		t.Fatalf("validations accepted: %+v", accepted)
	}
	if a := decode[acceptedEntry](t, accepted[0]); a.Assignment != 3 || len(a.Findings) != 3 || len(a.Cleared) != 0 {
		t.Errorf("the first acceptance is %+v", a)
	}
	if a := decode[acceptedEntry](t, accepted[1]); a.Assignment != 5 || len(a.Findings) != 0 || len(a.Cleared) != 1 || a.Cleared[0] != 1 {
		t.Errorf("the second acceptance is %+v", a)
	}
	// The fix was a new writing assignment of the Engineering leader.
	fixes := 0
	for _, e := range journaled(t, "assignment.created") {
		if a := decode[assignmentEntry](t, e); a.ID == 4 && !a.ReadOnly && a.Owner == "leader@engineering" {
			fixes++
		}
	}
	if fixes != 1 {
		t.Errorf("the fix was not assigned as a new writing assignment by Engineering's leader")
	}
}

// concat joins lists of script steps.
func concat(lists ...[]string) []string {
	var all []string
	for _, l := range lists {
		all = append(all, l...)
	}
	return all
}
