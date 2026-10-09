// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const resultFlagsForGreeting = `asmai result --evidence './check.sh exits 0 at HEAD' --artifact 'greeting.txt' --test 'check.sh asserts greeting.txt says hello' --check './check.sh -> passed' --gap none --pr-section 'Adds greeting.txt, saying hello. Before: no greeting.txt. After: check.sh passes.'`

// greetingWorker is a worker that does the greeting assignment and submits its
// result in the assignment's first dispatch, 3.
func greetingWorker(branch string) []string {
	return []string{
		fakeRun(`asmai inbox --dispatch 3`, 0, "assignment 1"),
		fakeRun(`asmai accept --assignment 1 --reason x`, 1, "only leader@engineering may run asmai accept"),
		fakeRun(`asmai reject --assignment 1 --reason x`, 1, "only leader@engineering may run asmai reject"),
		fakeRun(`asmai cancel --assignment 1 --reason x`, 1, "only leader@engineering may run asmai cancel"),
		fakeRun(`printf 'hello\n' > greeting.txt && printf '#!/bin/sh\ntest "$(cat greeting.txt)" = hello\n' > check.sh && chmod +x check.sh && ./check.sh`, 0, ""),
		fakeRun(`git add -A && git commit -q -m 'Add greeting.txt and its check' && asmai effect commit "$(git rev-parse HEAD)"`, 0, "effect 1 recorded: commit"),
		fakeRun(`git merge --no-edit asmai/job-1-add-a-greeting`, 0, "up to date"),
		fakeRun(`git push -q origin `+branch+` && asmai effect push "origin/`+branch+`@$(git rev-parse HEAD)"`, 0, "effect 2 recorded: push"),
		fakeRun(resultFlagsForGreeting, 0, "result recorded for assignment 1"),
	}
}

func TestCancellationWithoutConfirmedPushStaysIncompleteAndRetainsWorker(t *testing.T) {
	const branch = "asmai/job-1/1"
	worker := []string{
		fakeRun(`asmai inbox --dispatch 3`, 0, "assignment 1"),
		fakeRun(`printf 'draft\n' > notes.txt && git add -A && git commit -q -m 'Draft' && git push -q origin `+branch+` && asmai effect push "origin/`+branch+`@$(git rev-parse HEAD)"`, 0, "effect 1 recorded: push"),
		fakeRun(`asmai blocked --reason 'Which greeting?' --needs 'a choice'`, 0, "blocked recorded for assignment 1"),
		fakeRun(`asmai result --evidence x --test none --check none --gap none --pr-section x`, 1, "already ended in a blocked report"),
		hookStep("Stop", "assignment", ""),
		`{"expect":"asmai inbox --dispatch 5\r"}`,
		hookStep("UserPromptSubmit", "assignment", "asmai inbox --dispatch 5"),
		fakeRun(`asmai inbox --dispatch 5`, 0, "(?s)dispatch 5 \\| job 1 \\| cancellation from leader@engineering.*assignment 1 \\| job 1 \\| leader@engineering -> worker1@engineering \\| cancelling"),
		fakeRun(`printf 'unconfirmed\\n' > notes.txt && git add -A && git commit -q -m 'Unpushed work' && touch "$ASMAI_TEST_DIR/worker-done"`, 0, ""),
	}
	leader := []string{
		fakeRun(`asmai inbox --dispatch 4`, 0, "blocked 1"),
		fakeRun(`asmai cancel --assignment 1 --reason 'stop this work'`, 0, "cancellation requested"),
		fakeRun(`while [ ! -e "$ASMAI_TEST_DIR/worker-done" ]; do sleep 0.02; done`, 0, ""),
		fakeRun(`asmai brief 1`, 0, "cancelling"),
	}
	fixture, _ := assignmentFlow(t, worker, leader)
	if cancelled := journaled(t, "assignment.cancelled"); len(cancelled) != 0 {
		t.Fatalf("unconfirmed cancellation was recorded: %+v", cancelled)
	}
	if got := journaled(t, "assignment.cancelling"); len(got) != 1 {
		t.Fatalf("cancellation requests: %+v", got)
	}
	if w := workerOf(t, "worker1@engineering"); w.State == "stopped" {
		t.Errorf("worker allocation was freed after unconfirmed push: %+v", w)
	}
	if got := gitIn(t, fixture.origin, "show-ref", "--verify", "refs/heads/"+branch); got == "" {
		t.Fatal("expected assignment branch to exist on origin")
	}
}

func TestAcceptanceCleanupFailureIsReportedAndNotJournaledAsRemoved(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin, err := filepath.Abs(".test-acceptance-cleanup-bin")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(bin) })
	wrapper := filepath.Join(bin, "git")
	script := fmt.Sprintf("#!/bin/sh\nif [ \"$1 $2 $3\" = \"branch -D asmai/job-1/1\" ]; then echo injected cleanup failure >&2; exit 1; fi\nexec %q \"$@\"\n", gitPath)
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	const branch = "asmai/job-1/1"
	leader := []string{
		fakeRun(`asmai inbox --dispatch 4`, 0, "result 1"),
		waitForWorker,
		fakeRun(`asmai accept --assignment 1 --reason 'criteria met'`, 1, "injected cleanup failure"),
	}
	assignmentFlow(t, greetingWorker(branch), leader)
	if len(journaled(t, "result.accepted")) != 1 {
		t.Fatal("acceptance should remain recorded before cleanup fails")
	}
	if got := journaled(t, "workspace.removed"); len(got) != 0 {
		t.Errorf("failed cleanup was journaled as removed: %+v", got)
	}
}

// waitForWorker makes the leader wait until its worker has finished its turn.
var waitForWorker = fakeRun(`while [ ! -e "$ASMAI_TEST_DIR/worker-done" ]; do sleep 0.02; done`, 0, "")

// workerOf returns the agent as asmai agents lists it.
func workerOf(t *testing.T, name string) agentJSON {
	t.Helper()
	for _, a := range agentsListed(t) {
		if a.Agent == name {
			return a
		}
	}
	t.Fatalf("asmai agents does not list %s: %+v", name, agentsListed(t))
	return agentJSON{}
}

func TestAcceptingAResultFastForwardsTheJobBranchEndsTheAssignmentAndRemovesItsWorkspace(t *testing.T) {
	const branch = "asmai/job-1/1"
	leader := []string{
		fakeRun(`asmai inbox --dispatch 4`, 0, "result 1"),
		fakeRun(`asmai accept`, 1, "accept needs --assignment"),
		fakeRun(`asmai accept --assignment 1`, 1, "accept needs --reason"),
		fakeRun(`asmai accept --assignment 1 --reason ' '`, 1, "--reason cannot be blank"),
		fakeRun(`asmai accept --assignment 9 --reason x`, 1, "assignment 9 does not exist"),
		waitForWorker,
		fakeRun(`asmai accept --assignment 1 --reason 'greeting.txt says hello: check.sh asserts it and passed at the commit' --reason 'the check passes: ./check.sh -> passed at the commit'`, 0,
			"(?s)assignment 1 accepted by leader@engineering at commit [0-9a-f]{40}; the assignment has ended.*job branch asmai/job-1-add-a-greeting fast-forwarded to [0-9a-f]{40}.*read-only view of the job refreshed: \\S+/views/job-1"),
		fakeRun(`asmai accept --assignment 1 --reason x`, 1, "assignment 1 is accepted, so it has no result to accept"),
		fakeRun(`asmai reject --assignment 1 --reason x`, 1, "assignment 1 is accepted, so it has no result to reject"),
		fakeRun(`asmai cancel --assignment 1 --reason x`, 1, "assignment 1 is accepted, so it cannot be cancelled"),
		fakeRun(`asmai brief 1`, 0, "(?s)Job branch: asmai/job-1-add-a-greeting \\(started from [0-9a-f]{40}, tip [0-9a-f]{40}\\).*1 worker1@engineering \\(owner leader@engineering\\) accepted on branch asmai/job-1/1"),
	}
	fixture, _ := assignmentFlow(t, greetingWorker(branch), leader)
	stateDir := filepath.Join(os.Getenv("HOME"), ".local", "state", "asmai")
	clone := filepath.Join(stateDir, "repositories", "fixture")
	const jobBranch = "asmai/job-1-add-a-greeting"

	// The job branch is at the accepted commit, which is the worker's commit
	// on top of where it was, and nothing else moved it.
	accepted := gitIn(t, fixture.origin, "rev-parse", "refs/heads/"+branch)
	if got := gitIn(t, clone, "rev-parse", "refs/heads/"+jobBranch); got != accepted {
		t.Errorf("the job branch is at %s, want the accepted commit %s", got, accepted)
	}
	if got := gitIn(t, clone, "rev-parse", accepted+"^"); got != fixture.main {
		t.Errorf("the accepted commit's parent is %s, want where the job branch was, %s", got, fixture.main)
	}
	if out := gitIn(t, clone, "log", "--format=%H", jobBranch+"^.."+jobBranch); out != accepted {
		t.Errorf("the job branch gained %q, want only the accepted commit", out)
	}
	if got := gitIn(t, fixture.origin, "rev-parse", "refs/heads/main"); got != fixture.main {
		t.Errorf("main on origin moved to %s", got)
	}
	if out := gitIn(t, fixture.origin, "branch", "--list", "asmai/job-1-*"); out != "" {
		t.Errorf("the job branch was pushed: %s", out)
	}

	// The moves are journaled with the acceptance, in one step: the branch's
	// move cites the assignment and the result, and the acceptance records
	// the deciding leader, the commit and the reasons.
	moves := journaled(t, "job.branch.moved")
	if len(moves) != 1 {
		t.Fatalf("job branch moves: %+v", moves)
	}
	move := decode[map[string]any](t, moves[0])
	if move["from"] != fixture.main || move["to"] != accepted || move["assignment"] != float64(1) || move["branch"] != jobBranch {
		t.Errorf("the move is %v, want %s from %s to %s", move, jobBranch, fixture.main, accepted)
	}
	acceptances := journaled(t, "result.accepted")
	if len(acceptances) != 1 {
		t.Fatalf("acceptances: %+v", acceptances)
	}
	acceptance := decode[struct {
		Assignment int      `json:"assignment"`
		Kind       string   `json:"kind"`
		Leader     string   `json:"leader"`
		Report     int      `json:"report"`
		Commit     string   `json:"commit"`
		Reasons    []string `json:"reasons"`
	}](t, acceptances[0])
	if acceptance.Assignment != 1 || acceptance.Kind != "accepted" || acceptance.Leader != "leader@engineering" || acceptance.Report != 1 || acceptance.Commit != accepted || len(acceptance.Reasons) != 2 ||
		!strings.Contains(acceptance.Reasons[0], "greeting.txt says hello") {
		t.Errorf("the acceptance is %+v", acceptance)
	}
	if moves[0].ID != acceptances[0].ID-1 {
		t.Errorf("the move is journal entry %d and the acceptance %d, want them recorded together", moves[0].ID, acceptances[0].ID)
	}

	// The leaders' view follows the branch's tip.
	view := decode[struct {
		View string `json:"view"`
	}](t, journaled(t, "job.branch")[0]).View
	if got := gitIn(t, view, "rev-parse", "HEAD"); got != accepted {
		t.Errorf("the view is at %s, want the accepted commit %s", got, accepted)
	}
	if got, err := os.ReadFile(filepath.Join(view, "greeting.txt")); err != nil || string(got) != "hello\n" {
		t.Errorf("the view's greeting.txt is %q (%v)", got, err)
	}
	if info, err := os.Stat(filepath.Join(view, "greeting.txt")); err != nil || info.Mode().Perm()&0o222 != 0 {
		t.Errorf("the refreshed view's greeting.txt is %v (%v), want it read-only", info, err)
	}

	// The writing workspace is removed, with the worker that carried the
	// assignment, and origin keeps the assignment branch.
	workspace := filepath.Join(stateDir, "workspaces", "job-1", "assignment-1")
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Errorf("the accepted assignment's workspace %s is still there (%v)", workspace, err)
	}
	if out := gitIn(t, clone, "branch", "--list", branch); out != "" {
		t.Errorf("AsmAI's clone still has the assignment branch: %s", out)
	}
	if out := gitIn(t, clone, "worktree", "list", "--porcelain"); strings.Contains(out, "assignment-1") {
		t.Errorf("AsmAI's clone still lists the workspace: %s", out)
	}
	if removed := journaled(t, "workspace.removed"); len(removed) != 1 {
		t.Errorf("workspaces removed: %+v", removed)
	}
	if w := workerOf(t, "worker1@engineering"); w.State != "stopped" || w.Generation != 1 {
		t.Errorf("worker1@engineering is %+v, want its one session stopped with its assignment", w)
	}
	if got := checkoutState(t, fixture.checkout); got != fixture.untouched {
		t.Errorf("the user's checkout changed:\nbefore: %s\nafter: %s", fixture.untouched, got)
	}
}

func TestRejectingAResultReturnsTheAssignmentToTheSameWorkerInANewDispatchAndTheCorrectedResultIsAccepted(t *testing.T) {
	const branch = "asmai/job-1/1"
	worker := append(greetingWorker(branch),
		hookStep("Stop", "assignment", ""),
		`{"expect":"asmai inbox --dispatch 5\r"}`,
		hookStep("UserPromptSubmit", "assignment", "asmai inbox --dispatch 5"),
		fakeRun(`asmai inbox --dispatch 5`, 0, "(?s)dispatch 5 \\| job 1 \\| rejection from leader@engineering.*Reasons from leader@engineering:\ncheck.sh passes on an empty greeting.*assignment 1 \\| job 1 \\| leader@engineering -> worker1@engineering \\| active.*result 1 \\| assignment 1"),
		fakeRun(`printf '#!/bin/sh\ntest -s greeting.txt && test "$(cat greeting.txt)" = hello\n' > check.sh && ./check.sh`, 0, ""),
		fakeRun(`git add -A && git commit -q -m 'Check that the greeting is not empty' && asmai effect commit "$(git rev-parse HEAD)"`, 0, "effect 3 recorded: commit"),
		fakeRun(`git push -q origin `+branch+` && asmai effect push "origin/`+branch+`@$(git rev-parse HEAD)"`, 0, "effect 4 recorded: push"),
		fakeRun(`asmai result --evidence './check.sh fails on an empty greeting.txt and passes on this one' --test 'check.sh asserts greeting.txt is not empty' --check './check.sh -> passed' --gap none --pr-section 'The check now rejects an empty greeting.'`, 0, "(?s)result recorded for assignment 1 at commit [0-9a-f]{40}; sent to leader@engineering as dispatch 6"),
	)
	leader := []string{
		fakeRun(`asmai inbox --dispatch 4`, 0, "result 1"),
		fakeRun(`asmai reject --assignment 1`, 1, "reject needs --reason"),
		fakeRun(`asmai reject --assignment 1 --reason 'check.sh passes on an empty greeting' --reason 'the check passes criterion is not met by a check that cannot fail'`, 0,
			"assignment 1: result rejected by leader@engineering; it is active again with worker1@engineering, in dispatch 5"),
		fakeRun(`asmai reject --assignment 1 --reason x`, 1, "assignment 1 is active, so it has no result to reject"),
		fakeRun(`asmai accept --assignment 1 --reason x`, 1, "assignment 1 is active, so it has no result to accept"),
		hookStep("Stop", "report", ""),
		`{"expect":"asmai inbox --dispatch 6\r"}`,
		hookStep("UserPromptSubmit", "report", "asmai inbox --dispatch 6"),
		fakeRun(`asmai inbox --dispatch 6`, 0, "(?s)result 2 \\| assignment 1 \\| worker1@engineering.*Tests added:.*check.sh asserts greeting.txt is not empty"),
		waitForWorker,
		fakeRun(`asmai accept --assignment 1 --reason 'the check fails on an empty greeting.txt and passes on the corrected one'`, 0, "assignment 1 accepted by leader@engineering"),
	}
	fixture, _ := assignmentFlow(t, worker, leader)
	stateDir := filepath.Join(os.Getenv("HOME"), ".local", "state", "asmai")
	clone := filepath.Join(stateDir, "repositories", "fixture")
	const jobBranch = "asmai/job-1-add-a-greeting"

	// The rejection is recorded with its leader, the result it is about and
	// its reasons, and the work went to the same worker in a new dispatch.
	rejections := journaled(t, "result.rejected")
	if len(rejections) != 1 {
		t.Fatalf("rejections: %+v", rejections)
	}
	rejection := decode[struct {
		Assignment int      `json:"assignment"`
		Kind       string   `json:"kind"`
		Leader     string   `json:"leader"`
		Report     int      `json:"report"`
		Reasons    []string `json:"reasons"`
	}](t, rejections[0])
	if rejection.Assignment != 1 || rejection.Kind != "rejected" || rejection.Leader != "leader@engineering" || rejection.Report != 1 || len(rejection.Reasons) != 2 || rejection.Reasons[0] != "check.sh passes on an empty greeting" {
		t.Errorf("the rejection is %+v", rejection)
	}
	var returned []map[string]any
	for _, e := range journaled(t, "message.created") {
		if m := decode[map[string]any](t, e); m["kind"] == "rejection" {
			returned = append(returned, m)
		}
	}
	if len(returned) != 1 || returned[0]["recipient"] != "worker1@engineering" || returned[0]["sender"] != "leader@engineering" || returned[0]["dispatch"] != float64(5) || returned[0]["assignment"] != float64(1) {
		t.Errorf("the rejection's message is %v, want dispatch 5 from the leader to worker1@engineering about assignment 1", returned)
	}
	if w := workerOf(t, "worker1@engineering"); w.Generation != 1 {
		t.Errorf("worker1@engineering ran %d sessions, want the same one to take the work back", w.Generation)
	}

	// The job branch did not move on the rejection: it moved once, to the
	// corrected result, which is on top of the first.
	moves := journaled(t, "job.branch.moved")
	if len(moves) != 1 {
		t.Fatalf("job branch moves: %+v", moves)
	}
	final := gitIn(t, fixture.origin, "rev-parse", "refs/heads/"+branch)
	if move := decode[map[string]any](t, moves[0]); move["from"] != fixture.main || move["to"] != final || move["report"] != float64(2) {
		t.Errorf("the move is %v, want %s from %s to the second result at %s", move, jobBranch, fixture.main, final)
	}
	if got := gitIn(t, clone, "rev-parse", "refs/heads/"+jobBranch); got != final {
		t.Errorf("the job branch is at %s, want the corrected result %s", got, final)
	}
	if got := gitIn(t, fixture.origin, "show", final+":check.sh"); !strings.Contains(got, "test -s greeting.txt") {
		t.Errorf("the accepted check.sh is %q", got)
	}
	if got := gitIn(t, clone, "rev-list", "--count", fixture.main+".."+jobBranch); got != "2" {
		t.Errorf("the job branch is %s commits past main, want the worker's two", got)
	}
}

func TestCancellingAnAssignmentMakesItsWorkerPushItsBranchAndStopBeforeItEnds(t *testing.T) {
	const branch = "asmai/job-1/1"
	worker := []string{
		fakeRun(`asmai inbox --dispatch 3`, 0, "assignment 1"),
		fakeRun(`printf 'draft\n' > notes.txt && git add -A && git commit -q -m 'Draft' && git push -q origin `+branch+` && asmai effect push "origin/`+branch+`@$(git rev-parse HEAD)"`, 0, "effect 1 recorded: push"),
		fakeRun(`asmai blocked --reason 'Which greeting?' --needs 'a choice'`, 0, "blocked recorded for assignment 1"),
		hookStep("Stop", "assignment", ""),
		`{"expect":"asmai inbox --dispatch 5\r"}`,
		hookStep("UserPromptSubmit", "assignment", "asmai inbox --dispatch 5"),
		fakeRun(`asmai inbox --dispatch 5`, 0, "(?s)dispatch 5 \\| job 1 \\| cancellation from leader@engineering.*Reasons from leader@engineering:\nthe user dropped the greeting.*assignment 1 \\| job 1 \\| leader@engineering -> worker1@engineering \\| cancelling"),
		fakeRun(`asmai result --evidence x --test none --check none --gap none --pr-section x`, 1, "does not give worker1@engineering an assignment to report on"),
		fakeRun(`printf 'wip\n' > notes.txt && git add -A && git commit -q -m 'Work in progress' && git push -q origin `+branch+` && asmai effect push "origin/`+branch+`@$(git rev-parse HEAD)" && git rev-parse HEAD > "$ASMAI_TEST_DIR/final-commit"`, 0, "effect 2 recorded: push"),
		// The daemon ends the worker once this turn stops; what the test
		// waits for is done before that.
		fakeRun(`touch "$ASMAI_TEST_DIR/worker-done"`, 0, ""),
	}
	leader := []string{
		fakeRun(`asmai inbox --dispatch 4`, 0, "blocked 1"),
		fakeRun(`asmai cancel --assignment 1`, 1, "cancel needs --reason"),
		fakeRun(`asmai cancel --assignment 1 --reason 'the user dropped the greeting'`, 0,
			"assignment 1: cancellation requested by leader@engineering; worker1@engineering is told to push asmai/job-1/1 and stop, in dispatch 5"),
		fakeRun(`asmai cancel --assignment 1 --reason x`, 1, "so it cannot be cancelled"),
		fakeRun(`asmai accept --assignment 1 --reason x`, 1, "so it has no result to accept"),
	}
	fixture, testDir := assignmentFlow(t, worker, leader)
	stateDir := filepath.Join(os.Getenv("HOME"), ".local", "state", "asmai")
	clone := filepath.Join(stateDir, "repositories", "fixture")

	waitFor(t, "the assignment to be cancelled", func() bool { return len(journaled(t, "assignment.cancelled")) == 1 })
	waitFor(t, "the cancelled worker to stop", func() bool { return workerOf(t, "worker1@engineering").State == "stopped" })

	requested := journaled(t, "assignment.cancelling")
	if len(requested) != 1 {
		t.Fatalf("cancellations requested: %+v", requested)
	}
	decision := decode[struct {
		Kind    string   `json:"kind"`
		Leader  string   `json:"leader"`
		Reasons []string `json:"reasons"`
	}](t, requested[0])
	if decision.Kind != "cancelled" || decision.Leader != "leader@engineering" || strings.Join(decision.Reasons, "|") != "the user dropped the greeting" {
		t.Errorf("the cancellation is %+v", decision)
	}

	// The worker pushed its assignment branch, with the work it did in the
	// cancellation, before it stopped, and the assignment ended after that.
	data, err := os.ReadFile(filepath.Join(testDir, "final-commit"))
	if err != nil {
		t.Fatal(err)
	}
	final := strings.TrimSpace(string(data))
	if got := gitIn(t, fixture.origin, "rev-parse", "refs/heads/"+branch); got != final {
		t.Errorf("origin's assignment branch is at %s, want the worker's last commit %s", got, final)
	}
	if got := gitIn(t, fixture.origin, "show", final+":notes.txt"); got != "wip" {
		t.Errorf("notes.txt on the pushed branch is %q", got)
	}
	cancelled := decode[struct {
		Assignment int    `json:"assignment"`
		Worker     string `json:"worker"`
		Commit     string `json:"commit"`
		Pushed     bool   `json:"pushed"`
	}](t, journaled(t, "assignment.cancelled")[0])
	if cancelled.Assignment != 1 || cancelled.Worker != "worker1@engineering" || cancelled.Commit != final || !cancelled.Pushed {
		t.Errorf("the assignment ended as %+v, want it cancelled with %s pushed", cancelled, final)
	}
	if len(journaled(t, "result.submitted")) != 0 || len(journaled(t, "result.accepted")) != 0 {
		t.Error("a cancelled assignment submitted or had a result accepted")
	}
	if got := gitIn(t, clone, "rev-parse", "refs/heads/asmai/job-1-add-a-greeting"); got != fixture.main {
		t.Errorf("the job branch is at %s, want it where it was, %s: a cancellation moves nothing", got, fixture.main)
	}
}
