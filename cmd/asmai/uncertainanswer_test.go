// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// uncertainAnswerFlow runs a worker that asks its owning leader a question
// and whose session exits once the answer's nudge, dispatch 5, is typed to it
// and before it acknowledges it, so whether it saw the answer is unknown. The
// owning leader is told, in dispatch 6, and then runs recover. second is the
// script of the worker's next session, if recover starts one; the leader's
// recover steps install it before they start it.
func uncertainAnswerFlow(t *testing.T, recover, second []string) (testDir string) {
	t.Helper()
	worker := []string{
		fakeRun(`asmai inbox --dispatch 3`, 0, "assignment 1"),
		fakeRun(`asmai decision request --title 'Which greeting?' --option hello --option hi --recommend hello`, 0, "decision request 1, version 1, recorded for job 1"),
		hookStep("Stop", "assignment", ""),
		// The nudge is typed, and the session exits before submitting it.
		`{"expect":"asmai inbox --dispatch 5\r"}`,
	}
	leader := []string{
		fakeRun(`asmai inbox --dispatch 4`, 0, "Decision request 1, version 1"),
		fakeRun(`asmai decision answer --request 1 --version 1 --answer hello --relied-on "the job's decisions"`, 0, "the answer goes to worker1@engineering, dispatch 5"),
		hookStep("Stop", "report", ""),
		`{"expect":"asmai inbox --dispatch 6\r"}`,
		hookStep("UserPromptSubmit", "reconcile", "asmai inbox --dispatch 6"),
		fakeRun(`asmai inbox --dispatch 6 | tee "$ASMAI_TEST_DIR/answer-reconciliation"`, 0, "(?s)reconciliation from daemon.*worker1@engineering's session exited after decision answer dispatch 5 was typed to it and before it acknowledged it.*decision answer dispatch 5 is kept, never discarded"),
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
			`{"expect":"asmai inbox --dispatch 8\r"}`,
			hookStep("UserPromptSubmit", "result", "asmai inbox --dispatch 8"),
			fakeRun(`asmai inbox --dispatch 8`, 0, "(?s)result from worker1@engineering.*result 1 \\| assignment 1 \\| worker1@engineering"),
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
	}](t, held[0]); got.Dispatch != 5 {
		t.Errorf("the assignment was held against dispatch %d, want the answer's, 5", got.Dispatch)
	}
	if got := answerDispatchStates(t, 5); got != "created,unknown" {
		t.Errorf("the answer's dispatch moved through %s, want it kept unknown", got)
	}
	var shown struct {
		Assignments []struct {
			ID    int64  `json:"id"`
			State string `json:"state"`
		} `json:"assignments"`
	}
	asmaiJSON(t, &shown, "brief", "1")
	if len(shown.Assignments) != 1 || shown.Assignments[0].State != "needs_reconciliation" {
		t.Errorf("the brief shows %+v, want assignment 1 needing reconciliation", shown.Assignments)
	}
}
