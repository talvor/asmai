// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
)

// A worker whose turn ends with no result and no blocked report, because its
// push failed or for any other reason outside the factory, leaves its
// assignment active with nothing working on it. The daemon tells the owning
// leader what origin holds, and the leader sends the assignment to the worker
// again in a new dispatch.
func stalledWorkerFlow(t *testing.T, pushed bool, told string) {
	const branch = "asmai/job-1/1"
	commit := `printf 'hello\n' > greeting.txt && git add -A && git commit -q -m 'Add greeting.txt' && asmai effect commit "$(git rev-parse HEAD)"`
	if pushed {
		commit = `printf 'hello\n' > greeting.txt && git add -A && git commit -q -m 'Add greeting.txt' && git push -q origin ` + branch + ` && asmai effect commit "$(git rev-parse HEAD)"`
	}
	worker := []string{
		fakeRun(`asmai inbox --dispatch 3`, 0, "assignment 1"),
		fakeRun(commit, 0, "effect 1 recorded: commit"),
		// The turn ends: no result, no blocked report.
		hookStep("Stop", "assignment", ""),
		`{"expect":"asmai inbox --dispatch 5\r"}`,
		hookStep("UserPromptSubmit", "again", "asmai inbox --dispatch 5"),
		fakeRun(`asmai inbox --dispatch 5`, 0, "(?s)redispatch from leader@engineering.*leader@engineering sends assignment 1 of job 1 to you again.*Reasons from leader@engineering:\nthe push works now"),
		fakeRun(`git merge --no-edit asmai/job-1-add-a-greeting`, 0, "up to date"),
		fakeRun(`git push -q origin `+branch+` && asmai effect push "origin/`+branch+`@$(git rev-parse HEAD)"`, 0, "effect 2 recorded: push"),
		fakeRun(`asmai result --evidence 'greeting.txt says hello' --test none --check none --gap none --pr-section 'Adds greeting.txt.'`, 0, "result recorded for assignment 1"),
	}
	leader := []string{
		fakeRun(`asmai inbox --dispatch 4`, 0, told),
		fakeRun(`asmai redispatch --assignment 1 --reason 'the push works now'`, 0, "assignment 1: sent to worker1@engineering again by leader@engineering; it is active, in dispatch 5, a new dispatch"),
		hookStep("Stop", "report", ""),
		`{"expect":"asmai inbox --dispatch 6\r"}`,
		hookStep("UserPromptSubmit", "result", "asmai inbox --dispatch 6"),
		fakeRun(`asmai inbox --dispatch 6`, 0, "(?s)result from worker1@engineering.*result 1 \\| assignment 1 \\| worker1@engineering"),
		hookStep("Stop", "result", ""),
	}
	assignmentWorkerStop = "again"
	t.Cleanup(func() { assignmentWorkerStop = "assignment" })
	assignmentFlow(t, worker, leader)

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

func TestAWorkerThatStopsWithoutAReportAndWithoutPushingTellsItsOwnerAndIsRedispatched(t *testing.T) {
	stalledWorkerFlow(t, false, "(?s)stalled from daemon.*worker1@engineering's turn for dispatch 3 of assignment 1 of job 1 ended with no result and no blocked report.*still active and nothing is retried.*Its workspace is at commit [0-9a-f]{40}, and origin has no branch asmai/job-1/1: the work is not pushed.*asmai redispatch --assignment 1 --reason")
}

func TestAWorkerThatStopsWithoutAReportAfterPushingTellsItsOwnerWhatOriginHolds(t *testing.T) {
	stalledWorkerFlow(t, true, "(?s)stalled from daemon.*Its workspace is at commit ([0-9a-f]{40}), and origin's branch asmai/job-1/1 is at [0-9a-f]{40}, which holds it: the work is pushed")
}
