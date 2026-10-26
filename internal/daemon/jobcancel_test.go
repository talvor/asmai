// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/lavish"
	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/store"
)

// fakeCloseGH is a gh on the PATH that shows one pull request, #7 of the job
// branch, in the state the test's pr-state file holds, and closes it: it
// keeps the comment it was closed with and leaves the pull request CLOSED.
// With a close-fails file, closing fails as GitHub would.
const fakeCloseGH = `#!/bin/sh
case "$1 $2" in
"pr view")
	printf '{"number":7,"url":"https://github.com/talvor/asmai-fixture/pull/7","title":"Add a greeting","isDraft":false,"headRefName":"%s","headRefOid":"abc","state":"%s"}\n' "$(cat "$ASMAI_TEST_DIR/pr-branch")" "$(cat "$ASMAI_TEST_DIR/pr-state")" ;;
"pr close")
	[ -e "$ASMAI_TEST_DIR/close-fails" ] && { echo "HTTP 502: Bad Gateway" >&2; exit 1; }
	[ "$3 $4" = "7 --comment" ] || { echo "the fake gh closes only #7 with a comment: $*" >&2; exit 1; }
	printf '%s' "$5" > "$ASMAI_TEST_DIR/close-comment"
	echo CLOSED > "$ASMAI_TEST_DIR/pr-state" ;;
*)
	echo "the fake gh does not do: $*" >&2; exit 1 ;;
esac
`

// cancellableJob is a job of Engineering's with an active assignment, the
// first, whose result was rejected, and a submitted one, the second, whose
// result nobody has reviewed; and, with a pull request, its job branch pushed
// and an open pull request recorded. The user has asked Coordination's
// leader to cancel it.
type cancellableJob struct {
	twoResults
	testDir string
	words   string
	witness int64
}

const cancelWords = "Cancel job 1: I no longer want a greeting"

func newCancellableJob(t *testing.T, f twoResults, withPullRequest bool) cancellableJob {
	t.Helper()
	f.withWorkers(t)
	if _, err := f.decide(CommandReject, f.assignments[0].ID, "greet by name"); err != nil {
		t.Fatal(err)
	}
	testDir := t.TempDir()
	bin := filepath.Join(testDir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(fakeCloseGH), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ASMAI_TEST_DIR", testDir)
	branch := JobBranchName(f.job)
	for name, text := range map[string]string{"pr-branch": branch, "pr-state": "OPEN"} {
		if err := os.WriteFile(filepath.Join(testDir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if withPullRequest {
		owner := f.assignments[0].Owner
		at := time.Now()
		if err := f.s.ChangeDispatch(f.reported[0].ID, 1, store.DispatchCreated, store.DispatchWorking, "", at); err != nil {
			t.Fatal(err)
		}
		git(t, f.clone, "push", "origin", "refs/heads/"+branch+":refs/heads/"+branch)
		tip := git(t, f.clone, "rev-parse", "refs/heads/"+branch)
		if _, err := f.s.JobBranchPushed(store.Push{Job: f.job.Number, Branch: branch, Commit: tip, Leader: owner, Dispatch: f.reported[0].ID}, 1, nil, at); err != nil {
			t.Fatal(err)
		}
		pr := store.PullRequest{Job: f.job.Number, Number: 7, URL: "https://github.com/talvor/asmai-fixture/pull/7", Title: "Add a greeting", Branch: branch, Head: tip, State: "OPEN", Leader: owner, Dispatch: f.reported[0].ID}
		if _, _, err := f.s.PullRequestRecorded(pr, 1, pr.URL, at); err != nil {
			t.Fatal(err)
		}
	}
	_, witness, err := f.s.ObservedWitnessed(store.Witnessed{Agent: "leader@coordination", Role: roles.Coordination, Generation: 1, Terminal: "/dev/pts/test", Text: cancelWords}, "UserPromptSubmit", json.RawMessage(`{"prompt":"cancel"}`), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return cancellableJob{twoResults: f, testDir: testDir, words: cancelWords, witness: witness}
}

// cancel runs asmai job cancel with the credential, citing the user's words.
func (c cancellableJob) cancel(credential string, job int64) (Response, error) {
	var resp Response
	cancellation, err := c.d.cancelJob(Request{Session: credential, Command: CommandJobCancel, Job: job, Witness: c.witness})
	if err == nil {
		resp.Cancellation = &cancellation
	}
	return resp, err
}

// stops has each cancelling worker stop with its branch pushed, as it does
// on its cancellation dispatch.
func (c cancellableJob) stops(t *testing.T) {
	t.Helper()
	for _, a := range c.assignments {
		worker, err := roles.ParseAddress(a.Worker)
		if err != nil {
			t.Fatal(err)
		}
		c.d.cancelled(sessionRef{address: worker, generation: 1}, a.ID)
	}
}

func TestCoordinationCancelsAJobAtTheUsersWordAndEveryBranchStaysOnOrigin(t *testing.T) {
	c := newCancellableJob(t, newTwoResults(t), true)
	branch := JobBranchName(c.job)
	resp, err := c.cancel("coordination", c.job.Number)
	if err != nil {
		t.Fatal(err)
	}
	got := resp.Cancellation
	if got.Job.State != store.JobCancelled || got.Job.CancelReason != c.words || got.Job.CancelWitness != c.witness {
		t.Errorf("the job is %+v, want it cancelled with the user's own words", got.Job)
	}
	if len(got.Cancelling) != 2 {
		t.Fatalf("cancelling %+v, want both assignments", got.Cancelling)
	}

	// The pull request is closed with a comment giving the user's words,
	// and the closing is the daemon's own effect.
	if state, _ := os.ReadFile(filepath.Join(c.testDir, "pr-state")); strings.TrimSpace(string(state)) != "CLOSED" {
		t.Errorf("the pull request is %s, want it closed", state)
	}
	if comment, err := os.ReadFile(filepath.Join(c.testDir, "close-comment")); err != nil || !strings.Contains(string(comment), c.words) {
		t.Errorf("the pull request was closed with the comment %q (%v), want the user's words", comment, err)
	}
	if got.Closed == nil || got.Closed.Agent != store.Daemon || got.Closed.Kind != store.EffectPullRequestClosed || got.Closed.Ref != "https://github.com/talvor/asmai-fixture/pull/7" {
		t.Errorf("the closing was recorded as %+v, want the daemon's effect on the pull request", got.Closed)
	}

	// Each worker is told to push its branch and stop, with the reason.
	for _, a := range c.assignments {
		inbox, err := c.s.Inbox(a.Worker, 0, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		last := inbox[len(inbox)-1]
		if last.Kind != store.MessageCancellation || !strings.Contains(last.Body, c.words) || !strings.Contains(last.Body, "push your assignment branch") {
			t.Errorf("%s was last told %s: %q, want its cancellation with the user's words", a.Worker, last.Kind, last.Body)
		}
	}
	// Engineering's leader is told its job was cancelled.
	inbox, err := c.s.Inbox("leader@engineering", 0, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(inbox, func(m store.Message) bool {
		return m.Kind == store.MessageJobCancelled && strings.Contains(m.Body, c.words)
	}) {
		t.Errorf("Engineering's leader was not told the job was cancelled: %+v", inbox)
	}

	// A cancelling assignment's result is never accepted.
	if _, err := c.decide(CommandAccept, c.assignments[1].ID, "file-2.txt is there"); err == nil || !strings.Contains(err.Error(), "cancelling") {
		t.Errorf("accepting a cancelled job's result returned %v, want it refused", err)
	}

	// Each assignment ends as cancelled once its push is confirmed.
	c.stops(t)
	for _, a := range c.assignments {
		if a, err := c.s.Assignment(a.ID); err != nil || a.State != store.AssignmentCancelled {
			t.Errorf("assignment %d is %s (%v), want it cancelled", a.ID, a.State, err)
		}
	}
	// Every branch stays on origin.
	for i, a := range c.assignments {
		if tip := git(t, c.origin, "rev-parse", "refs/heads/"+a.Workspace.Branch); tip != c.commits[i] {
			t.Errorf("origin's %s is at %s, want it kept at %s", a.Workspace.Branch, tip, c.commits[i])
		}
	}
	if tip := git(t, c.origin, "rev-parse", "refs/heads/"+branch); tip != c.started {
		t.Errorf("origin's job branch is at %s, want it kept at %s", tip, c.started)
	}

	// The brief shows the job cancelled with the user's words.
	shown := callWith(t, c.d.jobCommand, Request{Command: CommandBrief, Job: c.job.Number})
	if shown.Error != "" || !strings.Contains(shown.Brief, "state: cancelled") || !strings.Contains(shown.Brief, "Cancelled at the user's word, in witnessed message") || !strings.Contains(shown.Brief, c.words) {
		t.Errorf("the brief is %q (%s), want the job cancelled with the user's words", shown.Brief, shown.Error)
	}
}

func TestTheJobsTabShowsACancelledJobWithTheUsersWordsAsEnded(t *testing.T) {
	c := newCancellableJob(t, newTwoResults(t), true)
	if _, err := c.cancel("coordination", c.job.Number); err != nil {
		t.Fatal(err)
	}
	c.stops(t)
	snapshot, err := c.d.dashboardSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Jobs) != 1 {
		t.Fatalf("the Jobs tab shows %+v, want the one job", snapshot.Jobs)
	}
	job := snapshot.Jobs[0]
	if job.Stage != StageCancelled || job.Stage.Word() != "Cancelled" || job.Reason != c.words {
		t.Errorf("the Jobs tab shows %+v, want the job cancelled with the user's words", job)
	}
	if job.Assignments != (AssignmentCounts{Cancelled: 2}) {
		t.Errorf("the job's assignments count %+v, want both cancelled", job.Assignments)
	}
	detail, err := c.d.dashboardJob(c.job.Number)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Stage != StageCancelled || detail.Reason != c.words {
		t.Errorf("the job detail is %+v, want it cancelled with the user's words", detail)
	}
}

func TestAJobWithNoPullRequestCancelsCleanly(t *testing.T) {
	c := newCancellableJob(t, newTwoResults(t), false)
	if err := os.WriteFile(filepath.Join(c.testDir, "close-fails"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	resp, err := c.cancel("coordination", c.job.Number)
	if err != nil {
		t.Fatalf("cancelling a job with no pull request returned %v", err)
	}
	if got := resp.Cancellation; got.Closed != nil || got.Job.State != store.JobCancelled || len(got.Cancelling) != 2 {
		t.Errorf("the cancellation is %+v, want the job cancelled with nothing closed", got)
	}
	inbox, err := c.s.Inbox("leader@coordination", 0, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(inbox, func(m store.Message) bool {
		return m.Kind == store.MessageJobCancelled && strings.Contains(m.Body, "no open pull request")
	}) {
		t.Errorf("Coordination's leader was not told what the cancellation did: %+v", inbox)
	}
}

func TestCancellingAJobWithdrawsItsRequestsAndDiscardsItsUnreviewedResults(t *testing.T) {
	f := newLavishFactory(t)
	r := f.request(t, 0, "")
	c := newCancellableJob(t, f.twoResults, false)
	resp, err := c.cancel("coordination", c.job.Number)
	if err != nil {
		t.Fatal(err)
	}
	got := resp.Cancellation
	if len(got.Withdrawn) != 1 || got.Withdrawn[0].ID != r.ID {
		t.Errorf("withdrew %+v, want the pending request", got.Withdrawn)
	}
	result, err := c.s.LatestResult(c.assignments[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Discarded, []int64{result.ID}) {
		t.Errorf("discarded %v, want the unreviewed result %d", got.Discarded, result.ID)
	}
	withdrawn := f.waitFor(t, r, "the page of the withdrawn request to end", func(r store.DecisionRequest) bool { return r.PageClosed })
	if withdrawn.State != store.RequestWithdrawn {
		t.Errorf("the request is %s, want it withdrawn", withdrawn.State)
	}
	if pages, err := lavish.Pages(f.d.cfg.Paths.Lavish); err != nil || len(pages) != 0 {
		t.Errorf("Lavish's pages = %+v (%v), want none", pages, err)
	}
}

func TestJobCancelRefusesUnknownFinishedAndCancelledJobsAndChangesNothing(t *testing.T) {
	c := newCancellableJob(t, newTwoResults(t), true)
	openJob(t, c.d.cfg.Paths, "fixture", store.JobEnded)
	unchanged := func(t *testing.T, try func() error, want string) {
		t.Helper()
		before, err := c.s.Journal()
		if err != nil {
			t.Fatal(err)
		}
		if err := try(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("job cancel returned %v, want a refusal saying %q", err, want)
		}
		after, err := c.s.Journal()
		if err != nil {
			t.Fatal(err)
		}
		if len(after) != len(before) {
			t.Errorf("a refused cancellation journaled %d entries", len(after)-len(before))
		}
	}
	unchanged(t, func() error { _, err := c.cancel("coordination", 99); return err }, "job 99 does not exist")
	unchanged(t, func() error { _, err := c.cancel("coordination", 2); return err }, "job 2 is finished")

	// A closing that fails cancels nothing and says so.
	if err := os.WriteFile(filepath.Join(c.testDir, "close-fails"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	unchanged(t, func() error { _, err := c.cancel("coordination", c.job.Number); return err }, "nothing is cancelled: closing job 1's pull request #7")
	if j, err := c.s.Job(c.job.Number); err != nil || j.State != store.JobOpen {
		t.Errorf("after a failed closing the job is %+v (%v), want it still open", j, err)
	}
	if err := os.Remove(filepath.Join(c.testDir, "close-fails")); err != nil {
		t.Fatal(err)
	}

	if _, err := c.cancel("coordination", c.job.Number); err != nil {
		t.Fatal(err)
	}
	unchanged(t, func() error { _, err := c.cancel("coordination", c.job.Number); return err }, "job 1 is already cancelled")
}

func TestOnlyCoordinationsLeaderMayCancelAJob(t *testing.T) {
	c := newCancellableJob(t, newTwoResults(t), true)
	for _, credential := range []string{"credential", "worker0"} {
		if err := c.d.authorize(Request{Session: credential, Command: CommandJobCancel}); err == nil || !strings.Contains(err.Error(), "only leader@coordination may run asmai job cancel") {
			t.Errorf("authorizing job cancel for %s returned %v, want it refused", credential, err)
		}
		if _, err := c.cancel(credential, c.job.Number); err == nil || !strings.Contains(err.Error(), "only leader@coordination may cancel a job") {
			t.Errorf("job cancel by %s returned %v, want it refused", credential, err)
		}
	}
	if err := c.d.authorize(Request{Command: CommandJobCancel}); err == nil {
		t.Error("the user's own job cancel was allowed: the user cancels only by telling Coordination's leader")
	}
	if j, err := c.s.Job(c.job.Number); err != nil || j.State != store.JobOpen {
		t.Errorf("after the refusals the job is %+v (%v), want it open", j, err)
	}
	// The owning leader still cancels its own worker's assignment.
	if _, err := c.decide(CommandCancel, c.assignments[0].ID, "the scope changed"); err != nil {
		t.Errorf("the owning leader's cancellation returned %v", err)
	}
}
