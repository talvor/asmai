// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// closedReason returns why dispatch id was stopped, as its dispatch.changed
// journal entry records it.
func closedReason(t *testing.T, id int64) string {
	t.Helper()
	closed := ""
	for _, e := range journaled(t, "dispatch.changed") {
		d := decode[struct {
			ID     int64  `json:"id"`
			State  string `json:"state"`
			Closed string `json:"closed"`
		}](t, e)
		if d.ID == id && d.State == "stopped" {
			closed = d.Closed
		}
	}
	return closed
}

func logOutputs(t *testing.T, testDir string, names ...string) {
	t.Helper()
	for _, name := range names {
		output, err := os.ReadFile(filepath.Join(testDir, name))
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s:\n%s", name, output)
	}
}

// A worker's turn ends while the sub-agents it started still run, so the
// daemon closes its dispatch. When they finish, the same session commits,
// pushes and submits its result, which goes on that dispatch with no blocked
// report and no redispatch (#243).
func TestAWorkersResultIsTakenAfterItsTurnEndedWhileItsSubAgentsRan(t *testing.T) {
	const branch = "asmai/test/job-1/1"
	worker := []string{
		fakeRun(`asmai inbox --dispatch 3`, 0, "assignment 1"),
		// The turn ends with the sub-agents still running.
		hookStep("Stop", "assignment", ""),
		fakeRun(`while [ ! -e "$ASMAI_TEST_DIR/stalled-read" ]; do sleep 0.02; done`, 0, ""),
		// A blocked report still needs a current dispatch.
		fakeRun(`asmai blocked --reason x --needs y > "$ASMAI_TEST_DIR/late-blocked-refusal" 2>&1; status=$?; cat "$ASMAI_TEST_DIR/late-blocked-refusal"; test "$status" -eq 1`, 0, "no current dispatch to report on"),
		// The sub-agents finish and the worker lands their work.
		fakeRun(`printf 'hello\n' > greeting.txt && git add -A && git commit -q -m 'Add greeting.txt' && asmai effect commit "$(git rev-parse HEAD)" | tee "$ASMAI_TEST_DIR/late-effect"`, 0, "effect 1 recorded: commit"),
		fakeRun(`git merge --no-edit asmai/test/job-1-add-a-greeting`, 0, "up to date"),
		fakeRun(`git push -q origin `+branch+` && asmai effect push "origin/`+branch+`@$(git rev-parse HEAD)"`, 0, "effect 2 recorded: push"),
		fakeRun(`asmai result --evidence 'greeting.txt says hello' --test none --check none --gap none --pr-section 'Adds greeting.txt.' 2>&1 | tee "$ASMAI_TEST_DIR/late-result"`, 0, "result recorded for assignment 1"),
		// A second result on the submitted assignment is refused.
		fakeRun(`asmai result --evidence again --test none --check none --gap none --pr-section again > "$ASMAI_TEST_DIR/second-result-refusal" 2>&1; status=$?; cat "$ASMAI_TEST_DIR/second-result-refusal"; test "$status" -eq 1`, 0, "(?s)assignment 1 is submitted, so it takes no new report.*asmai inbox"),
	}
	leader := []string{
		fakeRun(`asmai inbox --dispatch 4 | tee "$ASMAI_TEST_DIR/stalled-notice"`, 0, "(?s)stalled from daemon.*turn for dispatch 3 of assignment 1 of job 1 ended with no result"),
		fakeRun(`touch "$ASMAI_TEST_DIR/stalled-read"`, 0, ""),
		hookStep("Stop", "report", ""),
		`{"expect":"asmai inbox --dispatch 5\r"}`,
		hookStep("UserPromptSubmit", "result", "asmai inbox --dispatch 5"),
		fakeRun(`asmai inbox --dispatch 5 | tee "$ASMAI_TEST_DIR/leader-result"`, 0, "(?s)result from worker1@engineering.*result 1 \\| assignment 1 \\| worker1@engineering"),
		fakeRun(`asmai brief 1 | tee "$ASMAI_TEST_DIR/leader-brief"`, 0, "1 worker1@engineering \\(owner leader@engineering\\) submitted on branch asmai/test/job-1/1"),
	}
	_, testDir := assignmentFlow(t, worker, leader)
	logOutputs(t, testDir, "stalled-notice", "late-blocked-refusal", "late-effect", "late-result", "second-result-refusal", "leader-result", "leader-brief")

	if got := closedReason(t, 3); got != "turn-ended" {
		t.Errorf("the journal says dispatch 3 was closed as %q, want turn-ended", got)
	}
	submitted := journaled(t, "result.submitted")
	if len(submitted) != 1 {
		t.Fatalf("results submitted: %d, want the late one", len(submitted))
	}
	if got := decode[struct {
		Dispatch int64 `json:"dispatch"`
	}](t, submitted[0]); got.Dispatch != 3 {
		t.Errorf("the late result went on dispatch %d, want the closed dispatch 3", got.Dispatch)
	}
	if got := len(journaled(t, "assignment.redispatched")); got != 0 {
		t.Errorf("assignments redispatched: %d, want none", got)
	}
	if got := len(journaled(t, "blocked.submitted")); got != 0 {
		t.Errorf("blocked reports: %d, want none", got)
	}
	if got := workerOf(t, "worker1@engineering"); got.Generation != 1 {
		t.Errorf("the worker is %+v, want its first session to have reported", got)
	}
}

// Once the owning leader redispatches a stalled assignment, the old dispatch
// whose turn ended takes no late result: the worker is told to fetch the new
// one.
func TestALateResultIsRefusedOnceTheAssignmentIsRedispatched(t *testing.T) {
	const branch = "asmai/test/job-1/1"
	worker := []string{
		fakeRun(`asmai inbox --dispatch 3`, 0, "assignment 1"),
		hookStep("Stop", "assignment", ""),
		fakeRun(`while [ ! -e "$ASMAI_TEST_DIR/leader-retried" ]; do sleep 0.02; done`, 0, ""),
		fakeRun(`asmai result --evidence x --test none --check none --gap none --pr-section x > "$ASMAI_TEST_DIR/redispatched-refusal" 2>&1; status=$?; cat "$ASMAI_TEST_DIR/redispatched-refusal"; test "$status" -eq 1`, 0, "(?s)no current dispatch to report on, and no dispatch whose turn ended takes it: dispatch 5 of assignment 1 is .* and waits for you: fetch it with `asmai inbox`"),
		`{"expect":"asmai inbox --dispatch 5\r"}`,
		hookStep("UserPromptSubmit", "again", "asmai inbox --dispatch 5"),
		fakeRun(`asmai inbox --dispatch 5`, 0, "redispatch from leader@engineering"),
		fakeRun(`printf 'hello\n' > greeting.txt && git add -A && git commit -q -m 'Add greeting.txt' && asmai effect commit "$(git rev-parse HEAD)"`, 0, "effect 1 recorded: commit"),
		fakeRun(`git merge --no-edit asmai/test/job-1-add-a-greeting`, 0, "up to date"),
		fakeRun(`git push -q origin `+branch+` && asmai effect push "origin/`+branch+`@$(git rev-parse HEAD)"`, 0, "effect 2 recorded: push"),
		fakeRun(`asmai result --evidence 'greeting.txt says hello' --test none --check none --gap none --pr-section 'Adds greeting.txt.'`, 0, "result recorded for assignment 1"),
	}
	leader := []string{
		fakeRun(`asmai inbox --dispatch 4`, 0, "stalled from daemon"),
		fakeRun(`asmai redispatch --assignment 1 --reason 'try again'`, 0, "in dispatch 5, a new dispatch"),
		fakeRun(`touch "$ASMAI_TEST_DIR/leader-retried"`, 0, ""),
		hookStep("Stop", "report", ""),
		`{"expect":"asmai inbox --dispatch 6\r"}`,
		hookStep("UserPromptSubmit", "result", "asmai inbox --dispatch 6"),
		fakeRun(`asmai inbox --dispatch 6`, 0, "(?s)result from worker1@engineering.*result 1 \\| assignment 1"),
		hookStep("Stop", "result", ""),
	}
	assignmentWorkerStop = "again"
	t.Cleanup(func() { assignmentWorkerStop = "assignment" })
	_, testDir := assignmentFlow(t, worker, leader)
	logOutputs(t, testDir, "redispatched-refusal")
	submitted := journaled(t, "result.submitted")
	if len(submitted) != 1 {
		t.Fatalf("results submitted: %d, want the redispatch's one", len(submitted))
	}
	if got := decode[struct {
		Dispatch int64 `json:"dispatch"`
	}](t, submitted[0]); got.Dispatch != 5 {
		t.Errorf("the result went on dispatch %d, want the redispatch 5", got.Dispatch)
	}
}

// A stalled assignment the owning leader cancels takes no late result.
func TestALateResultIsRefusedOnceTheAssignmentIsCancelled(t *testing.T) {
	const branch = "asmai/test/job-1/1"
	worker := []string{
		fakeRun(`asmai inbox --dispatch 3`, 0, "assignment 1"),
		fakeRun(`printf 'draft\n' > notes.txt && git add -A && git commit -q -m 'Draft'`, 0, ""),
		hookStep("Stop", "assignment", ""),
		fakeRun(`while [ ! -e "$ASMAI_TEST_DIR/leader-cancelled" ]; do sleep 0.02; done`, 0, ""),
		fakeRun(`asmai result --evidence x --test none --check none --gap none --pr-section x > "$ASMAI_TEST_DIR/cancelled-refusal" 2>&1; status=$?; cat "$ASMAI_TEST_DIR/cancelled-refusal"; test "$status" -eq 1`, 0, "(?s)no current dispatch to report on, and no dispatch whose turn ended takes it: assignment 1 is cancelling, so it takes no new report.*asmai inbox"),
		`{"expect":"asmai inbox --dispatch 5\r"}`,
		hookStep("UserPromptSubmit", "cancel", "asmai inbox --dispatch 5"),
		fakeRun(`asmai inbox --dispatch 5`, 0, "cancellation from leader@engineering"),
		fakeRun(`git push -q origin `+branch+` && asmai effect push "origin/`+branch+`@$(git rev-parse HEAD)"`, 0, "effect 1 recorded: push"),
		fakeRun(`touch "$ASMAI_TEST_DIR/worker-done"`, 0, ""),
	}
	leader := []string{
		fakeRun(`asmai inbox --dispatch 4`, 0, "stalled from daemon"),
		fakeRun(`asmai cancel --assignment 1 --reason 'dropped'`, 0, "cancellation requested"),
		fakeRun(`touch "$ASMAI_TEST_DIR/leader-cancelled"`, 0, ""),
	}
	assignmentWorkerStop = "cancel"
	t.Cleanup(func() { assignmentWorkerStop = "assignment" })
	_, testDir := assignmentFlow(t, worker, leader)
	logOutputs(t, testDir, "cancelled-refusal")
	if got := len(journaled(t, "result.submitted")); got != 0 {
		t.Errorf("results submitted: %d, want none for the cancelled assignment", got)
	}
}
