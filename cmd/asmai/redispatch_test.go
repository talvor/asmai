// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/talvor/asmai/internal/fakeprovider"
)

// A worker whose turn ends with no result and no blocked report, because its
// push failed or for any other reason outside the factory, leaves its
// assignment active with nothing working on it. The daemon tells the owning
// leader what origin holds, and the leader sends the assignment to the worker
// again in a new dispatch.
func stalledWorkerFlow(t *testing.T, pushed, originUnavailable bool, told string) {
	const branch = "asmai/job-1/1"
	commit := `printf 'hello\n' > greeting.txt && git add -A && git commit -q -m 'Add greeting.txt' && asmai effect commit "$(git rev-parse HEAD)"`
	if pushed {
		commit = `printf 'hello\n' > greeting.txt && git add -A && git commit -q -m 'Add greeting.txt' && git push -q origin ` + branch + ` && asmai effect commit "$(git rev-parse HEAD)"`
	}
	worker := []string{
		fakeRun(`asmai inbox --dispatch 3`, 0, "assignment 1"),
		fakeRun(`asmai redispatch --assignment 1 --reason 'skip my owner' > "$ASMAI_TEST_DIR/worker-refusal" 2>&1; command_exit=$?; cat "$ASMAI_TEST_DIR/worker-refusal"; test "$command_exit" -eq 1`, 0, "only leader@engineering"),
		fakeRun(commit, 0, "effect 1 recorded: commit"),
	}
	if originUnavailable {
		worker = append(worker, fakeRun(`origin=$(git remote get-url origin) && printf '%s' "$origin" > "$ASMAI_TEST_DIR/origin-path" && mv "$origin" "$ASMAI_TEST_DIR/origin-offline"`, 0, ""))
	}
	worker = append(worker,
		// The turn ends: no result, no blocked report.
		hookStep("Stop", "assignment", ""),
		fakeRun(`while [ ! -e "$ASMAI_TEST_DIR/leader-retried" ]; do sleep 0.02; done`, 0, ""),
		`{"expect":"asmai inbox --dispatch 5\r"}`,
		hookStep("UserPromptSubmit", "again", "asmai inbox --dispatch 5"),
		fakeRun(`asmai inbox --dispatch 5 | tee "$ASMAI_TEST_DIR/worker-redispatch"`, 0, "(?s)redispatch from leader@engineering.*leader@engineering sends assignment 1 of job 1 to you again.*Reasons from leader@engineering:\nthe push works now"),
		fakeRun(`git merge --no-edit asmai/job-1-add-a-greeting`, 0, "up to date"),
		fakeRun(`git push -q origin `+branch+` && asmai effect push "origin/`+branch+`@$(git rev-parse HEAD)"`, 0, "effect 2 recorded: push"),
		fakeRun(`asmai result --evidence 'greeting.txt says hello' --test none --check none --gap none --pr-section 'Adds greeting.txt.'`, 0, "result recorded for assignment 1"),
	)
	leader := []string{
		fakeRun(`asmai inbox --dispatch 4 | tee "$ASMAI_TEST_DIR/stalled-notice"`, 0, told),
	}
	if originUnavailable {
		leader = append(leader, fakeRun(`mv "$ASMAI_TEST_DIR/origin-offline" "$(cat "$ASMAI_TEST_DIR/origin-path")"`, 0, ""))
	}
	leader = append(leader,
		fakeRun(`asmai redispatch --assignment 1 --reason 'the push works now' | tee "$ASMAI_TEST_DIR/redispatch-receipt"`, 0, "assignment 1: sent to worker1@engineering again by leader@engineering; it is active, in dispatch 5, a new dispatch"),
		fakeRun(`asmai redispatch --assignment 1 --reason 'duplicate' > "$ASMAI_TEST_DIR/duplicate-refusal" 2>&1; status=$?; cat "$ASMAI_TEST_DIR/duplicate-refusal"; test "$status" -eq 1`, 0, "queued redispatch dispatch 5"),
		fakeRun(`touch "$ASMAI_TEST_DIR/leader-retried"`, 0, ""),
		hookStep("Stop", "report", ""),
		`{"expect":"asmai inbox --dispatch 6\r"}`,
		hookStep("UserPromptSubmit", "result", "asmai inbox --dispatch 6"),
		fakeRun(`asmai inbox --dispatch 6`, 0, "(?s)result from worker1@engineering.*result 1 \\| assignment 1 \\| worker1@engineering"),
		hookStep("Stop", "result", ""),
	)
	assignmentWorkerStop = "again"
	t.Cleanup(func() { assignmentWorkerStop = "assignment" })
	_, testDir := assignmentFlow(t, worker, leader)
	for _, name := range []string{"worker-refusal", "stalled-notice", "redispatch-receipt", "duplicate-refusal", "worker-redispatch"} {
		output, err := os.ReadFile(filepath.Join(testDir, name))
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s:\n%s", name, output)
	}

	// The worker was never started again: the same session took the new dispatch.
	if got := workerOf(t, "worker1@engineering"); got.Generation != 1 {
		t.Errorf("the worker is %+v, want its first session to have taken the redispatch", got)
	}
	states := dispatchStates(t, 0)
	if got := strings.Join(states[3], ","); got != "created,unknown,nudged,delivered,working,stopped" {
		t.Errorf("the worker's first dispatch moved through %s", got)
	}
	if got := strings.Join(states[5], ","); !strings.HasPrefix(got, "created,unknown,nudged,delivered,working") {
		t.Errorf("the redispatch moved through %s, want it delivered and worked on", got)
	}
	if got := len(journaled(t, "assignment.redispatched")); got != 1 {
		t.Errorf("assignments redispatched: %d, want 1", got)
	}
	if got := len(journaled(t, "result.submitted")); got != 1 {
		t.Errorf("results submitted: %d, want the one the redispatch led to", got)
	}
}

func TestOwningLeaderCannotRedispatchWhileWorkerIsStillWorking(t *testing.T) {
	assignmentLeaderBeforeStop = []string{
		fakeRun(`while [ ! -e "$ASMAI_TEST_DIR/worker-working" ]; do sleep 0.02; done`, 0, ""),
		fakeRun(`asmai redispatch --assignment 1 --reason 'try early' > "$ASMAI_TEST_DIR/working-refusal" 2>&1; command_exit=$?; cat "$ASMAI_TEST_DIR/working-refusal"; test "$command_exit" -eq 1`, 0, "latest worker dispatch 3 is working; wait for it to stop"),
		fakeRun(`touch "$ASMAI_TEST_DIR/leader-checked"`, 0, ""),
	}
	t.Cleanup(func() { assignmentLeaderBeforeStop = nil })
	worker := []string{
		fakeRun(`asmai inbox --dispatch 3`, 0, "assignment 1"),
		fakeRun(`touch "$ASMAI_TEST_DIR/worker-working"; while [ ! -e "$ASMAI_TEST_DIR/leader-checked" ]; do sleep 0.02; done`, 0, ""),
		hookStep("Stop", "assignment", ""),
	}
	leader := []string{fakeRun(`asmai inbox --dispatch 4`, 0, "stalled from daemon")}
	_, testDir := assignmentFlow(t, worker, leader)
	output, err := os.ReadFile(filepath.Join(testDir, "working-refusal"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("working-dispatch refusal:\n%s", output)
}

func TestWorkerExitWithoutStopHoldsAssignmentForOwner(t *testing.T) {
	assignmentWorkerNoStop = true
	t.Cleanup(func() { assignmentWorkerNoStop = false })
	worker := []string{
		fakeRun(`asmai inbox --dispatch 3`, 0, "assignment 1"),
		fakeRun(`printf 'draft\n' > greeting.txt && git add -A && git commit -q -m 'Draft greeting'`, 0, ""),
	}
	leader := []string{
		fakeRun(`asmai inbox --dispatch 4 | tee "$ASMAI_TEST_DIR/exit-reconciliation"`, 0, "(?s)reconciliation from daemon.*session exited without a Stop hook or a report.*needs_reconciliation"),
	}
	_, testDir := assignmentFlow(t, worker, leader)
	if got := len(journaled(t, "assignment.needs_reconciliation")); got != 1 {
		t.Errorf("assignments held for reconciliation: %d, want one", got)
	}
	output, err := os.ReadFile(filepath.Join(testDir, "exit-reconciliation"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("worker-exit reconciliation:\n%s", output)
}

func TestAWorkerThatStopsWithoutAReportAndWithoutPushingTellsItsOwnerAndIsRedispatched(t *testing.T) {
	stalledWorkerFlow(t, false, false, "(?s)stalled from daemon.*worker1@engineering's turn for dispatch 3 of assignment 1 of job 1 ended with no result and no blocked report.*still active and nothing is retried.*Its workspace is at commit [0-9a-f]{40}, and origin has no branch asmai/job-1/1: the work is not pushed.*asmai redispatch --assignment 1 --reason")
}

func TestAWorkerThatStopsWithoutAReportAfterPushingTellsItsOwnerWhatOriginHolds(t *testing.T) {
	stalledWorkerFlow(t, true, false, "(?s)stalled from daemon.*Its workspace is at commit ([0-9a-f]{40}), and origin's branch asmai/job-1/1 is at [0-9a-f]{40}, which holds it: the work is pushed")
}

func TestAWorkerThatStopsWhileOriginIsUnavailableGetsAnUnknownOriginStatus(t *testing.T) {
	stalledWorkerFlow(t, false, true, "(?s)stalled from daemon.*Its workspace is at commit [0-9a-f]{40}; origin could not be read, so whether it holds the commit is not known:.*asmai redispatch --assignment 1 --reason")
}

func TestAWorkerCanReportFailedPushAfterAnotherAssignmentAdvancesTheJobTip(t *testing.T) {
	gitIdentity(t)
	testDir := t.TempDir()
	t.Setenv("ASMAI_TEST_DIR", testDir)
	t.Setenv(fakeprovider.ScriptEnv+"_WORKER", filepath.Join(testDir, "worker.jsonl"))
	t.Setenv(fakeprovider.ScriptEnv+"_ENGINEERING", filepath.Join(testDir, "engineering.jsonl"))

	engineering := concat(agentSession,
		turn(1, "handoff",
			fakeRun(`asmai inbox --dispatch 1`, 0, "Add a greeting"),
			fakeRun(`asmai handoff accept --handoff 1`, 0, "reply dispatch 2"),
			fakeRun(`asmai assign --job 1 --outcome 'Write a draft' --criterion 'draft exists'`, 0, "assignment 1 of job 1 given to worker1@engineering; dispatch 3"),
			awaitFile("first-worker-ready"),
			fakeRun(`cp "$ASMAI_TEST_DIR/second-worker.jsonl" "$ASMAI_TEST_DIR/worker.jsonl"`, 0, ""),
			fakeRun(`asmai assign --job 1 --outcome 'Add a greeting' --criterion 'greeting.txt says hello'`, 0, "assignment 2 of job 1 given to worker2@engineering; dispatch 4"),
		),
		turn(5, "result",
			fakeRun(`asmai inbox --dispatch 5`, 0, "result 1 \\| assignment 2"),
			fakeRun(`asmai accept --assignment 2 --reason 'greeting.txt says hello'`, 0, "fast-forwarded"),
			touchFile("tip-advanced"),
		),
		turn(6, "blocked",
			fakeRun(`asmai inbox --dispatch 6 | tee "$ASMAI_TEST_DIR/advanced-tip-blocked-notice"`, 0, "(?s)blocked from worker1@engineering.*assignment 1.*Reason: push failed.*Needs: restore origin access.*taken in: false"),
			touchFile("leader-done"),
		),
		[]string{idleSession},
	)
	firstWorker := concat(agentSession,
		turn(3, "assignment",
			fakeRun(`asmai inbox --dispatch 3`, 0, "assignment 1"),
			fakeRun(`printf 'draft\n' > draft.txt && git add -A && git commit -q -m 'Draft'`, 0, ""),
			fakeRun(`git push "$ASMAI_TEST_DIR/unavailable.git" asmai/job-1/1 > "$ASMAI_TEST_DIR/failed-push" 2>&1; status=$?; cat "$ASMAI_TEST_DIR/failed-push"; test "$status" -ne 0`, 0, "does not appear to be a git repository"),
			touchFile("first-worker-ready"),
			awaitFile("tip-advanced"),
			fakeRun(`asmai result --evidence draft --test none --check none --gap none --pr-section draft > "$ASMAI_TEST_DIR/advanced-tip-result-refusal" 2>&1; status=$?; cat "$ASMAI_TEST_DIR/advanced-tip-result-refusal"; test "$status" -eq 1`, 0, "does not contain the job branch's tip"),
			fakeRun(`asmai blocked --reason 'push failed' --needs 'restore origin access' | tee "$ASMAI_TEST_DIR/advanced-tip-blocked-receipt"`, 0, "blocked recorded for assignment 1"),
		),
		[]string{idleSession},
	)
	secondWorker := concat(agentSession,
		turn(4, "assignment",
			fakeRun(`asmai inbox --dispatch 4`, 0, "assignment 2"),
			fakeRun(`printf 'hello\n' > greeting.txt && git add -A && git commit -q -m 'Greeting'`, 0, ""),
			fakeRun(`git push -q origin asmai/job-1/2 && asmai effect push "origin/asmai/job-1/2@$(git rev-parse HEAD)"`, 0, "recorded: push"),
			fakeRun(`asmai result --evidence 'greeting.txt says hello' --test none --check none --gap none --pr-section 'Adds greeting.txt'`, 0, "result recorded for assignment 2"),
		),
		[]string{idleSession},
	)
	writeFiles(t, testDir, map[string][]string{
		"engineering.jsonl":   engineering,
		"worker.jsonl":        firstWorker,
		"second-worker.jsonl": secondWorker,
	})
	coordination := []string{
		hookStep("SessionStart", "", ""),
		`{"screen":"> "}`,
		`{"expect":"Please add a greeting\r"}`,
		hookStep("UserPromptSubmit", "", "Please add a greeting"),
		fakeRun(`asmai job open --message latest --repository fixture --reading 'Add a greeting' --criterion 'Greeting is printed'`, 0, "Opened job 1"),
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
	registerWritingFixture(t)
	terminal := inTerminal(t, 100, 25)
	waitFor(t, "Coordination prompt", terminal.shows(">"))
	terminal.ptmx.WriteString("Please add a greeting\r")
	waitFor(t, "the leader reading the blocked report", touched(testDir, "leader-done"))
	for _, name := range []string{"failed-push", "advanced-tip-result-refusal", "advanced-tip-blocked-receipt", "advanced-tip-blocked-notice"} {
		output, err := os.ReadFile(filepath.Join(testDir, name))
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s:\n%s", name, output)
	}
}
