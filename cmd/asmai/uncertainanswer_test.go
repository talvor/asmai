// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// uncertainAnswerFlow runs a worker that asks its owning leader a question
// and whose session exits once the answer's nudge, dispatch 6, is typed to it
// and before it acknowledges it, so whether it saw the answer is unknown. The
// owning leader is told, in dispatch 7, and then runs recover. second is the
// script of the worker's next session, if recover starts one; the leader's
// recover steps install it before they start it.
//
// The worker's turn ends with no report, so its owner is told it stalled,
// in dispatch 5, by a goroutine that reads origin first: the leader waits for
// that notice before it answers, or it would race the answer and the hold
// for their dispatches.
func uncertainAnswerFlow(t *testing.T, recover, second []string) (testDir string) {
	t.Helper()
	worker := []string{
		fakeRun(`asmai inbox --dispatch 3`, 0, "assignment 1"),
		fakeRun(`asmai decision request --title 'Which greeting?' --option hello --option hi --recommend hello`, 0, "decision request 1, version 1, recorded for job 1"),
		hookStep("Stop", "assignment", ""),
		fakeRun(`touch "$ASMAI_TEST_DIR/worker-stopped"`, 0, ""),
		// The nudge is typed, and the session exits before submitting it.
		`{"expect":"asmai inbox --dispatch 6\r"}`,
	}
	leader := []string{
		fakeRun(`asmai inbox --dispatch 4`, 0, "Decision request 1, version 1"),
		fakeRun(`while [ ! -e "$ASMAI_TEST_DIR/worker-stopped" ]; do sleep 0.02; done`, 0, ""),
		fakeRun(`until asmai inbox --dispatch 5 > "$ASMAI_TEST_DIR/stalled-notice" 2>/dev/null; do sleep 0.02; done; cat "$ASMAI_TEST_DIR/stalled-notice"`, 0, "stalled from daemon"),
		fakeRun(`asmai decision answer --request 1 --version 1 --answer hello --relied-on "the job's decisions"`, 0, "the answer goes to worker1@engineering, dispatch 6"),
		hookStep("Stop", "report", ""),
		`{"expect":"asmai inbox --dispatch 7\r"}`,
		hookStep("UserPromptSubmit", "reconcile", "asmai inbox --dispatch 7"),
		fakeRun(`asmai inbox --dispatch 7 | tee "$ASMAI_TEST_DIR/answer-reconciliation"`, 0, "(?s)reconciliation from daemon.*worker1@engineering's session exited after decision answer dispatch 6 was typed to it and before it acknowledged it.*decision answer dispatch 6 is kept, never discarded.*assignment 1 \\| job 1 \\| leader@engineering -> worker1@engineering \\| needs_reconciliation"),
	}
	if second != nil {
		steps := append([]string{
			hookStep("SessionStart", "", ""),
			`{"screen":"\u001b[?2004h> "}`,
		}, second...)
		steps = append(steps, `{"expect":"only typed by a test that means to"}`)
		path := writeScript(t, "second-worker", steps)
		// The worker's next session plays it: the fake reads its script as
		// it starts.
		leader = append(leader, fakeRun(`cp '`+path+`' "$ASMAI_FAKE_PROVIDER_SCRIPT_WORKER"`, 0, ""))
	}
	leader = append(leader, recover...)
	leader = append(leader, hookStep("Stop", "reconcile", ""))
	if second != nil {
		// The worker's next session ends its turn with a result.
		leader = append(leader,
			`{"expect":"asmai inbox --dispatch 9\r"}`,
			hookStep("UserPromptSubmit", "result", "asmai inbox --dispatch 9"),
			fakeRun(`asmai inbox --dispatch 9`, 0, "(?s)result from worker1@engineering.*result 1 \\| assignment 1 \\| worker1@engineering"),
			hookStep("Stop", "result", ""),
		)
	}
	assignmentWorkerNoStop = true
	t.Cleanup(func() { assignmentWorkerNoStop = false })
	_, testDir = assignmentFlow(t, worker, leader)
	output, err := os.ReadFile(filepath.Join(testDir, "answer-reconciliation"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("the owning leader was told:\n%s", output)
	// The owner's own fetch shows the assignment held: asmai brief is an
	// agent command, which the test, as the user, cannot run.
	if !strings.Contains(string(output), "assignment 1 | job 1 | leader@engineering -> worker1@engineering | needs_reconciliation") {
		t.Errorf("the owning leader's reconciliation does not show assignment 1 needing reconciliation:\n%s", output)
	}
	return testDir
}

func TestAWorkerThatExitsBeforeAcknowledgingAnAnswerHoldsItsAssignmentAgainstTheAnswer(t *testing.T) {
	uncertainAnswerFlow(t, nil, nil)
	held := journaled(t, "assignment.needs_reconciliation")
	if len(held) != 1 {
		t.Fatalf("assignments held for reconciliation: %d, want one", len(held))
	}
	if got := decode[struct {
		Dispatch int64 `json:"dispatch"`
	}](t, held[0]); got.Dispatch != 6 {
		t.Errorf("the assignment was held against dispatch %d, want the answer's, 6", got.Dispatch)
	}
	if got := answerDispatchStates(t, 6); got != "created,unknown" {
		t.Errorf("the answer's dispatch moved through %s, want it kept unknown", got)
	}
}

// finishing are the steps of a worker's next session that commit, push and
// submit its result once it has what it needs.
func finishing(prompt string) []string {
	const branch = "asmai/test/job-1/1"
	return []string{
		fakeRun(`printf 'hello\n' > greeting.txt && git add -A && git commit -q -m 'Add greeting.txt' && asmai effect commit "$(git rev-parse HEAD)"`, 0, "effect 1 recorded: commit"),
		fakeRun(`git merge --no-edit asmai/test/job-1-add-a-greeting`, 0, "up to date"),
		fakeRun(`git push -q origin `+branch+` && asmai effect push "origin/`+branch+`@$(git rev-parse HEAD)"`, 0, "effect 2 recorded: push"),
		fakeRun(`asmai result --evidence 'greeting.txt says hello' --test none --check none --gap none --pr-section 'Adds greeting.txt.'`, 0, "result recorded for assignment 1"),
		hookStep("Stop", prompt, ""),
	}
}

func TestTheOwnerDeliversAnAnswerItsWorkerExitedBeforeAcknowledgingAgain(t *testing.T) {
	recover := []string{
		fakeRun(`asmai redispatch --assignment 1 --reason 'too early' > "$ASMAI_TEST_DIR/held-refusal" 2>&1; status=$?; cat "$ASMAI_TEST_DIR/held-refusal"; test "$status" -eq 1`, 0, "(?s)holds decision answer dispatch 6.*asmai answer redeliver --dispatch 6.*asmai answer retire --dispatch 6"),
		fakeRun(`asmai answer redeliver --dispatch 6 --reason 'the worker exited before reading it' | tee "$ASMAI_TEST_DIR/redeliver-receipt"`, 0, "decision answer dispatch 6 delivered to worker1@engineering again by leader@engineering, in dispatch 8, a new dispatch; assignment 1 is active again"),
	}
	second := append([]string{
		`{"expect":"asmai inbox --dispatch 8\r"}`,
		hookStep("UserPromptSubmit", "again", "asmai inbox --dispatch 8"),
		fakeRun(`asmai inbox --dispatch 8 | tee "$ASMAI_TEST_DIR/worker-answer"`, 0, "(?s)decision.answer from leader@engineering.*Answer to version 1 of decision request 1, from leader@engineering:\nleader@engineering delivers this decision answer to you again.*dispatch 6, which is not reused.*with the answer below.*\n\nhello\n"),
	}, finishing("again")...)
	testDir := uncertainAnswerFlow(t, recover, second)
	for _, name := range []string{"held-refusal", "redeliver-receipt", "worker-answer"} {
		output, err := os.ReadFile(filepath.Join(testDir, name))
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s:\n%s", name, output)
	}
	if got := answerDispatchStates(t, 6); got != "created,unknown,stopped" {
		t.Errorf("the uncertain answer moved through %s, want it stopped once delivered again", got)
	}
	if got := answerDispatchStates(t, 8); !strings.HasPrefix(got, "created,unknown,nudged,delivered,working") {
		t.Errorf("the answer delivered again moved through %s, want it delivered and worked on", got)
	}
	if got := len(journaled(t, "decision.answer.redelivered")); got != 1 {
		t.Errorf("answers delivered again: %d, want 1", got)
	}
	if got := workerOf(t, "worker1@engineering"); got.Generation != 2 {
		t.Errorf("the worker is %+v, want a second session started for the answer", got)
	}
	if got := len(journaled(t, "result.submitted")); got != 1 {
		t.Errorf("results submitted: %d, want the one the answer led to", got)
	}
}

func TestTheOwnerRetiresAnAnswerItsWorkerExitedBeforeAcknowledgingAndRedispatches(t *testing.T) {
	recover := []string{
		fakeRun(`asmai answer retire --dispatch 6 --reason 'the answer is carried in the redispatch' | tee "$ASMAI_TEST_DIR/retire-receipt"`, 0, "decision answer dispatch 6 retired by leader@engineering, journaled with the answer"),
		fakeRun(`asmai answer redeliver --dispatch 6 --reason 'changed my mind' > "$ASMAI_TEST_DIR/retired-refusal" 2>&1; status=$?; cat "$ASMAI_TEST_DIR/retired-refusal"; test "$status" -eq 1`, 0, "not an uncertain decision answer"),
		fakeRun(`asmai redispatch --assignment 1 --reason 'use hello' | tee "$ASMAI_TEST_DIR/redispatch-receipt"`, 0, "assignment 1: sent to worker1@engineering again by leader@engineering; it is active, in dispatch 8"),
	}
	second := append([]string{
		`{"expect":"asmai inbox --dispatch 8\r"}`,
		hookStep("UserPromptSubmit", "again", "asmai inbox --dispatch 8"),
		fakeRun(`asmai inbox --dispatch 8`, 0, "(?s)redispatch from leader@engineering.*Reasons from leader@engineering:\nuse hello"),
	}, finishing("again")...)
	testDir := uncertainAnswerFlow(t, recover, second)
	for _, name := range []string{"retire-receipt", "retired-refusal", "redispatch-receipt"} {
		output, err := os.ReadFile(filepath.Join(testDir, name))
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s:\n%s", name, output)
	}
	if got := answerDispatchStates(t, 6); got != "created,unknown,stopped" {
		t.Errorf("the retired answer moved through %s", got)
	}
	retired := journaled(t, "decision.answer.retired")
	if len(retired) != 1 {
		t.Fatalf("answers retired: %d, want 1", len(retired))
	}
	if got := decode[struct {
		Dispatch int64    `json:"dispatch"`
		Body     string   `json:"body"`
		Reasons  []string `json:"reasons"`
	}](t, retired[0]); got.Dispatch != 6 || !strings.Contains(got.Body, "hello") || len(got.Reasons) != 1 {
		t.Errorf("the retirement journaled %+v, want the answer's dispatch, the answer and the reason", got)
	}
	if got := len(journaled(t, "result.submitted")); got != 1 {
		t.Errorf("results submitted: %d, want the one the redispatch led to", got)
	}
}
