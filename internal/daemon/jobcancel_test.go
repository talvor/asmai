// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/lavish"
	"github.com/talvor/asmai/internal/providers"
	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/store"
)

// fakeCloseGH is a gh on the PATH that shows one pull request, #7 of the job
// branch, in the state the test's pr-state file holds, and closes it: it
// keeps the comment it was closed with and leaves the pull request CLOSED.
// It shows the bodies of the comments the pull request was closed with. With a
// close-fails file, closing fails as GitHub would.
const fakeCloseGH = `#!/bin/sh
case "$1 $2 $5" in
"pr view comments")
	[ "$3 $6 $7" = "7 --jq .comments[].body" ] || { echo "the fake gh shows only #7's comment bodies: $*" >&2; exit 1; }
	[ -e "$ASMAI_TEST_DIR/close-comment" ] && cat "$ASMAI_TEST_DIR/close-comment" ;;
"pr view "*)
	printf '{"number":7,"url":"https://github.com/talvor/asmai-fixture/pull/7","title":"Add a greeting","isDraft":false,"headRefName":"%s","headRefOid":"abc","state":"%s"}\n' "$(cat "$ASMAI_TEST_DIR/pr-branch")" "$(cat "$ASMAI_TEST_DIR/pr-state")" ;;
"pr close "*)
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
	return cancellableJob{twoResults: f, testDir: testDir, words: cancelWords, witness: witnessed(t, f.s, cancelWords)}
}

// witnessed has the user tell Coordination's leader text, and returns the
// witnessed message.
func witnessed(t *testing.T, s *store.Store, text string) int64 {
	t.Helper()
	_, witness, err := s.ObservedWitnessed(store.Witnessed{Agent: "leader@coordination", Role: roles.Coordination, Generation: 1, Terminal: "/dev/pts/test", Text: text}, "UserPromptSubmit", json.RawMessage(`{"prompt":"cancel"}`), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return witness
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

// sessionsEnded has the daemon start the workers of the job's assignments
// on a Claude Code that records how it was started, their sessions having
// ended, and returns where it records it.
func (c cancellableJob) sessionsEnded(t *testing.T) string {
	t.Helper()
	configFile, out := recordingClaude(t, c.s, c.d.cfg.Paths, c.origin, "trunk")
	startsRecordingClaude(t, c.d, c.d.cfg.Paths, configFile)
	for _, a := range c.assignments {
		c.d.leaders[a.Worker].session = nil
	}
	return out
}

// startedForCancellation fails t unless the worker of assignment a runs in
// a new session for its assignment, whose dispatch is a cancellation that
// tells it to push its assignment branch.
func (c cancellableJob) startedForCancellation(t *testing.T, a store.Assignment, out string) {
	t.Helper()
	c.d.mu.Lock()
	l := c.d.leaders[a.Worker]
	running, carries := l.session != nil, l.assignment
	c.d.mu.Unlock()
	if !running || carries != a.ID {
		t.Errorf("%s is running %v, carrying assignment %d; want it started again for assignment %d", a.Worker, running, carries, a.ID)
	}
	w, err := c.s.WorkerDispatch(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if w.Kind != store.MessageCancellation || w.Dispatch.NativeSession != "" {
		t.Errorf("%s was started for %s dispatch %d resuming %q, want its cancellation in a new session", a.Worker, w.Kind, w.Dispatch.ID, w.Dispatch.NativeSession)
	}
	inbox, err := c.s.Inbox(a.Worker, 0, w.Dispatch.ID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if last := inbox[len(inbox)-1]; last.Dispatch != w.Dispatch.ID || !strings.Contains(last.Body, "push your assignment branch") {
		t.Errorf("%s was last told %q in dispatch %d, want its cancellation in dispatch %d", a.Worker, last.Body, last.Dispatch, w.Dispatch.ID)
	}
	if args := waitForFile(t, filepath.Join(out, fmt.Sprintf("args-%d", a.Workspace.Slot))); strings.Contains(args, "--resume") {
		t.Errorf("%s was started resuming a session: %s", a.Worker, args)
	}
}

// endedWithoutPush fails t unless assignment a, whose workspace is gone,
// ended as cancelled with no push, its worker never started for it, the
// journal saying so, and its branch stays on origin at commit.
func (c cancellableJob) endedWithoutPush(t *testing.T, a store.Assignment, commit, out string) {
	t.Helper()
	if state := assignmentState(t, c.s, a.ID); state != store.AssignmentCancelled {
		t.Errorf("assignment %d, whose workspace is gone, is %s, want it cancelled", a.ID, state)
	}
	c.d.mu.Lock()
	l := c.d.leaders[a.Worker]
	running := l != nil && l.session != nil
	c.d.mu.Unlock()
	if running {
		t.Errorf("%s was started for assignment %d, whose workspace is gone", a.Worker, a.ID)
	}
	if _, err := os.Stat(filepath.Join(out, fmt.Sprintf("args-%d", a.Workspace.Slot))); err == nil {
		t.Errorf("a provider was started for %s, whose workspace is gone", a.Worker)
	}
	entries, err := c.s.Journal()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(entries, func(e store.Entry) bool {
		var data struct {
			Assignment    int64 `json:"assignment"`
			Pushed        bool  `json:"pushed"`
			WorkspaceGone bool  `json:"workspace_gone"`
		}
		return e.Kind == store.KindAssignmentCancelled && json.Unmarshal(e.Data, &data) == nil && data.Assignment == a.ID && !data.Pushed && data.WorkspaceGone
	}) {
		t.Errorf("the journal has no entry of assignment %d ending cancelled with no push because its workspace is gone", a.ID)
	}
	if tip := git(t, c.origin, "rev-parse", "refs/heads/"+a.Workspace.Branch); tip != commit {
		t.Errorf("origin's %s is at %s, want it kept at %s", a.Workspace.Branch, tip, commit)
	}
}

// toldEndedWithoutPush fails t unless Coordination's leader and
// Engineering's leader were told that assignment gone, whose workspace is
// gone, ended as cancelled with no push, its branch staying on origin, and
// that only the worker of assignment pushing pushes and stops.
func (c cancellableJob) toldEndedWithoutPush(t *testing.T, gone, pushing store.Assignment) {
	t.Helper()
	ended := fmt.Sprintf("Assignments [%d] ended as cancelled with no push", gone.ID)
	for leader, pushes := range map[string]string{
		"leader@coordination": fmt.Sprintf("Assignments [%d] are cancelling: their workers push", pushing.ID),
		"leader@engineering":  fmt.Sprintf("told the workers of assignments [%d] to push", pushing.ID),
	} {
		inbox, err := c.s.Inbox(leader, 0, 0, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		i := slices.IndexFunc(inbox, func(m store.Message) bool { return m.Kind == store.MessageJobCancelled })
		if i < 0 {
			t.Errorf("%s was not told the job was cancelled: %+v", leader, inbox)
			continue
		}
		body := inbox[i].Body
		if !strings.Contains(body, ended) || !strings.Contains(body, "pushed earlier stays on origin") || !strings.Contains(body, pushes) {
			t.Errorf("%s was told %q, want assignment %d ended with no push, its branch on origin, and only assignment %d pushing", leader, body, gone.ID, pushing.ID)
		}
	}
}

// localViewRemoved fails t unless the cancelled job's local view is gone.
func (c cancellableJob) localViewRemoved(t *testing.T) {
	t.Helper()
	if done, err := c.s.LocalRemoved(c.job.Number); err != nil || !done {
		t.Errorf("the cancelled job's local view removed = %v (%v), want it removed", done, err)
	}
}

func TestAJobCancelEndsAnAssignmentWhoseWorkspaceIsGoneAsCancelledWithNoPush(t *testing.T) {
	for _, held := range []bool{false, true} {
		t.Run(map[bool]string{false: "active", true: "needing reconciliation"}[held], func(t *testing.T) {
			c := newCancellableJob(t, newTwoResults(t), true)
			gone, other := c.assignments[0], c.assignments[1]
			if held {
				if _, err := c.s.AssignmentNeedsReconciliation(gone.ID, 0, "its turn was cut off", "reconcile it", time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.RemoveAll(gone.Workspace.Path); err != nil {
				t.Fatal(err)
			}
			out := c.sessionsEnded(t)
			resp, err := c.cancel("coordination", c.job.Number)
			if err != nil {
				t.Fatal(err)
			}
			// Coordination's leader is shown it ended, not that it pushes.
			if !slices.ContainsFunc(resp.Cancellation.Cancelling, func(a store.Assignment) bool {
				return a.ID == gone.ID && a.State == store.AssignmentCancelled
			}) {
				t.Errorf("the cancellation shows %+v, want assignment %d cancelled", resp.Cancellation.Cancelling, gone.ID)
			}
			c.endedWithoutPush(t, gone, c.commits[0], out)
			c.toldEndedWithoutPush(t, gone, other)
			c.startedForCancellation(t, other, out)
			// The job's local view stays while the other assignment holds its
			// workspace, and goes once its push is confirmed.
			if done, err := c.s.LocalRemoved(c.job.Number); err != nil || done {
				t.Errorf("the cancelled job's local view removed = %v (%v), want it kept while an assignment holds it", done, err)
			}
			c.stops(t)
			if state := assignmentState(t, c.s, other.ID); state != store.AssignmentCancelled {
				t.Errorf("assignment %d is %s, want it cancelled", other.ID, state)
			}
			c.localViewRemoved(t)
			snapshot, err := c.d.dashboardSnapshot()
			if err != nil {
				t.Fatal(err)
			}
			if len(snapshot.Jobs) != 1 || snapshot.Jobs[0].Assignments != (AssignmentCounts{Cancelled: 2}) {
				t.Errorf("the Jobs tab shows %+v, want both assignments cancelled", snapshot.Jobs)
			}
		})
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
		w, err := c.s.WorkerDispatch(a.ID)
		if err != nil {
			t.Fatal(err)
		}
		inbox, err := c.s.Inbox(a.Worker, 0, w.Dispatch.ID, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		last := inbox[len(inbox)-1]
		if last.Kind != store.MessageCancellation || !strings.Contains(last.Body, c.words) || !strings.Contains(last.Body, "push your assignment branch") {
			t.Errorf("%s was last told %s: %q, want its cancellation with the user's words", a.Worker, last.Kind, last.Body)
		}
	}
	// Engineering's leader is told its job was cancelled.
	inbox, err := c.s.Inbox("leader@engineering", 0, 0, time.Now())
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

func TestAnAssignmentNeedingReconciliationIsCancelledWithItsJob(t *testing.T) {
	c := newCancellableJob(t, newTwoResults(t), true)
	held := c.assignments[0]
	if _, err := c.s.AssignmentNeedsReconciliation(held.ID, 0, "its turn was cut off", "reconcile it", time.Now()); err != nil {
		t.Fatal(err)
	}
	out := c.sessionsEnded(t)
	resp, err := c.cancel("coordination", c.job.Number)
	if err != nil {
		t.Fatal(err)
	}
	got := resp.Cancellation
	if got.Job.State != store.JobCancelled || got.Job.CancelReason != c.words {
		t.Errorf("the job is %+v, want it cancelled with the user's words", got.Job)
	}
	if !slices.ContainsFunc(got.Cancelling, func(a store.Assignment) bool { return a.ID == held.ID }) {
		t.Fatalf("cancelling %+v, want the assignment needing reconciliation among them", got.Cancelling)
	}
	if a, err := c.s.Assignment(held.ID); err != nil || a.State != store.AssignmentCancelling {
		t.Errorf("assignment %d is %s (%v), want it cancelling", held.ID, a.State, err)
	}
	w, err := c.s.WorkerDispatch(held.ID)
	if err != nil {
		t.Fatal(err)
	}
	inbox, err := c.s.Inbox(held.Worker, 0, w.Dispatch.ID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if last := inbox[len(inbox)-1]; last.Kind != store.MessageCancellation || !strings.Contains(last.Body, "push your assignment branch") {
		t.Errorf("%s was last told %s: %q, want its cancellation", held.Worker, last.Kind, last.Body)
	}
	// The held worker is started again, in a new session, for it.
	c.startedForCancellation(t, held, out)
	told, err := c.s.Inbox("leader@coordination", 0, 0, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(told, func(m store.Message) bool {
		return m.Kind == store.MessageJobCancelled && strings.Contains(m.Body, "stay held")
	}) {
		t.Errorf("Coordination's leader was told an assignment stays held: %+v", told)
	}

	// Once its push is confirmed it ends as cancelled, nothing is left
	// held, and the job's local view goes while its branches stay on origin.
	c.stops(t)
	for _, a := range c.assignments {
		if a, err := c.s.Assignment(a.ID); err != nil || a.State != store.AssignmentCancelled {
			t.Errorf("assignment %d is %s (%v), want it cancelled", a.ID, a.State, err)
		}
	}
	snapshot, err := c.d.dashboardSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Jobs) != 1 || snapshot.Jobs[0].Assignments != (AssignmentCounts{Cancelled: 2}) {
		t.Errorf("the Jobs tab shows %+v, want both assignments cancelled and none needing reconciliation", snapshot.Jobs)
	}
	if done, err := c.s.LocalRemoved(c.job.Number); err != nil || !done {
		t.Errorf("the cancelled job's local view removed = %v (%v), want it removed", done, err)
	}
	for i, a := range c.assignments {
		if tip := git(t, c.origin, "rev-parse", "refs/heads/"+a.Workspace.Branch); tip != c.commits[i] {
			t.Errorf("origin's %s is at %s, want it kept at %s", a.Workspace.Branch, tip, c.commits[i])
		}
	}
}

func TestARestartDuringAJobCancelStartsTheWorkersAgainForTheirCancellationAndHoldsNone(t *testing.T) {
	c := newCancellableJob(t, newTwoResults(t), true)
	resp, err := c.cancel("coordination", c.job.Number)
	if err != nil {
		t.Fatal(err)
	}
	// Each worker fetched its cancellation, and the factory stopped: the
	// first's dispatch at a boundary, in a session the provider reported no
	// native session of, so it cannot be resumed; the second's while it was
	// at work, so what it did is not known.
	at := time.Now()
	for i, a := range c.assignments {
		j := slices.IndexFunc(resp.Cancellation.Dispatches, func(d store.Dispatch) bool { return d.Agent == a.Worker })
		if j < 0 {
			t.Fatalf("%s was not told of the cancellation: %+v", a.Worker, resp.Cancellation.Dispatches)
		}
		dispatch := resp.Cancellation.Dispatches[j].ID
		if err := c.s.ChangeDispatch(dispatch, 1, store.DispatchCreated, store.DispatchNudged, "", at); err != nil {
			t.Fatal(err)
		}
		if _, err := c.s.Inbox(a.Worker, dispatch, dispatch, at); err != nil {
			t.Fatal(err)
		}
		if err := c.s.WorkFetchedDispatch(dispatch, a.Worker, 1, at); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if stopped, err := c.s.StopDispatchIfWorking(dispatch, 1, "", at); err != nil || !stopped {
				t.Fatalf("stopping %s's cancellation: %v, %v", a.Worker, stopped, err)
			}
		}
	}

	// The factory starts again.
	out := c.sessionsEnded(t)
	c.d.recoverWork(true)
	c.d.restoreLeaders()

	for _, a := range c.assignments {
		if state := assignmentState(t, c.s, a.ID); state != store.AssignmentCancelling {
			t.Errorf("assignment %d is %s after the restart, want it still cancelling", a.ID, state)
		}
		c.startedForCancellation(t, a, out)
	}
	snapshot, err := c.d.dashboardSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Jobs) != 1 || snapshot.Jobs[0].Assignments != (AssignmentCounts{Cancelling: 2}) {
		t.Errorf("the Jobs tab shows %+v, want nothing of the cancelled job needing reconciliation", snapshot.Jobs)
	}

	// Once their pushes are confirmed they end as cancelled, and the job's
	// local view goes.
	c.stops(t)
	for _, a := range c.assignments {
		if state := assignmentState(t, c.s, a.ID); state != store.AssignmentCancelled {
			t.Errorf("assignment %d is %s, want it cancelled", a.ID, state)
		}
	}
	if done, err := c.s.LocalRemoved(c.job.Number); err != nil || !done {
		t.Errorf("the cancelled job's local view removed = %v (%v), want it removed", done, err)
	}
}

// stopsInCancellation has the factory stop during the job cancel resp did,
// with the worker of a left as leave says: its cancellation unfetched, at
// work, or stopped at a boundary. With a provider, the worker's session ran
// on it, and the provider identified it with transcript.
func (c cancellableJob) stopsInCancellation(t *testing.T, resp Response, a store.Assignment, leave, provider, transcript string) {
	t.Helper()
	at := time.Now()
	j := slices.IndexFunc(resp.Cancellation.Dispatches, func(d store.Dispatch) bool { return d.Agent == a.Worker })
	if j < 0 {
		t.Fatalf("%s was not told of the cancellation: %+v", a.Worker, resp.Cancellation.Dispatches)
	}
	dispatch, generation := resp.Cancellation.Dispatches[j].ID, 1
	if provider != "" {
		g, err := c.s.SessionStarted(store.SessionStart{Agent: a.Worker, Role: roles.Engineering, Provider: provider, Version: "2.1.292", Model: "opus", Executable: "/p", Args: []string{}, Dir: a.Workspace.Path, PID: 7, At: at, Assignment: a.ID})
		if err != nil {
			t.Fatal(err)
		}
		generation = g
		if err := c.s.SessionIdentified(a.Worker, g, "native-"+a.Worker, transcript, at); err != nil {
			t.Fatal(err)
		}
	}
	if leave == store.DispatchCreated {
		return
	}
	if err := c.s.ChangeDispatch(dispatch, generation, store.DispatchCreated, store.DispatchNudged, "", at); err != nil {
		t.Fatal(err)
	}
	if _, err := c.s.Inbox(a.Worker, dispatch, dispatch, at); err != nil {
		t.Fatal(err)
	}
	if err := c.s.WorkFetchedDispatch(dispatch, a.Worker, generation, at); err != nil {
		t.Fatal(err)
	}
	if leave == store.DispatchStopped {
		if stopped, err := c.s.StopDispatchIfWorking(dispatch, generation, "", at); err != nil || !stopped {
			t.Fatalf("stopping %s's cancellation: %v, %v", a.Worker, stopped, err)
		}
	}
}

func TestARestartDuringAJobCancelEndsAnAssignmentWhoseWorkspaceIsGoneAsCancelledWithNoPush(t *testing.T) {
	transcript := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(transcript, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, leave, provider string
	}{
		// The session could be resumed, but not in a workspace that is gone.
		{"resumable session", store.DispatchStopped, providers.ClaudeCode},
		{"no native session", store.DispatchStopped, ""},
		{"cut off", store.DispatchWorking, providers.ClaudeCode},
		{"never fetched", store.DispatchCreated, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCancellableJob(t, newTwoResults(t), true)
			resp, err := c.cancel("coordination", c.job.Number)
			if err != nil {
				t.Fatal(err)
			}
			for _, a := range c.assignments {
				c.stopsInCancellation(t, resp, a, tc.leave, tc.provider, transcript)
				if err := os.RemoveAll(a.Workspace.Path); err != nil {
					t.Fatal(err)
				}
			}

			// The factory starts again.
			out := c.sessionsEnded(t)
			c.d.recoverWork(true)
			c.d.restoreLeaders()

			for i, a := range c.assignments {
				c.endedWithoutPush(t, a, c.commits[i], out)
			}
			if pending, err := c.s.PendingLeaders(); err != nil || slices.ContainsFunc(pending, func(p string) bool { return strings.HasPrefix(p, "worker") }) {
				t.Errorf("the agents with a message waiting are %v (%v), want no worker of the cancelled job", pending, err)
			}
			snapshot, err := c.d.dashboardSnapshot()
			if err != nil {
				t.Fatal(err)
			}
			if len(snapshot.Jobs) != 1 || snapshot.Jobs[0].Assignments != (AssignmentCounts{Cancelled: 2}) {
				t.Errorf("the Jobs tab shows %+v, want both assignments cancelled and none needing reconciliation", snapshot.Jobs)
			}
			c.localViewRemoved(t)
		})
	}
}

func TestARestartDuringAJobCancelStartsAgainAWorkerWhoseTranscriptIsGoneOrThatRanOnAnotherProvider(t *testing.T) {
	c := newCancellableJob(t, newTwoResults(t), true)
	resp, err := c.cancel("coordination", c.job.Number)
	if err != nil {
		t.Fatal(err)
	}
	c.stopsInCancellation(t, resp, c.assignments[0], store.DispatchStopped, providers.ClaudeCode, filepath.Join(t.TempDir(), "gone.jsonl"))
	c.stopsInCancellation(t, resp, c.assignments[1], store.DispatchStopped, "codex", "")

	out := c.sessionsEnded(t)
	c.d.recoverWork(true)
	c.d.restoreLeaders()

	for _, a := range c.assignments {
		if state := assignmentState(t, c.s, a.ID); state != store.AssignmentCancelling {
			t.Errorf("assignment %d is %s after the restart, want it still cancelling", a.ID, state)
		}
		c.startedForCancellation(t, a, out)
	}
	c.stops(t)
	for _, a := range c.assignments {
		if state := assignmentState(t, c.s, a.ID); state != store.AssignmentCancelled {
			t.Errorf("assignment %d is %s, want it cancelled", a.ID, state)
		}
	}
	c.localViewRemoved(t)
}

// A worker whose resumed session ended before it came up has its cancelled
// job's assignment's cancellation started again in a new session, or, its
// workspace gone, the assignment ends as cancelled with no push.
func TestACancelledJobsWorkerThatCouldNotResumeIsStartedAgainOrEndsWithNoWorkspace(t *testing.T) {
	c := newCancellableJob(t, newTwoResults(t), true)
	resp, err := c.cancel("coordination", c.job.Number)
	if err != nil {
		t.Fatal(err)
	}
	resumes, gone := c.assignments[0], c.assignments[1]
	for _, a := range c.assignments {
		c.stopsInCancellation(t, resp, a, store.DispatchStopped, providers.ClaudeCode, "")
	}
	if err := os.RemoveAll(gone.Workspace.Path); err != nil {
		t.Fatal(err)
	}
	out := c.sessionsEnded(t)
	c.d.mu.Lock()
	for _, a := range c.assignments {
		l := c.d.leaders[a.Worker]
		l.assignment = a.ID
		c.d.cannotResume(l, "exit status 1")
	}
	c.d.mu.Unlock()
	// The workspace-gone assignment ended as cancelled within cannotResume;
	// d.agents also counts the restarted worker's running session, so it is
	// not waited on here, and the fixture's cleanup waits on it once the
	// sessions have ended.

	c.startedForCancellation(t, resumes, out)
	c.endedWithoutPush(t, gone, c.commits[1], out)
	if state := assignmentState(t, c.s, resumes.ID); state != store.AssignmentCancelling {
		t.Errorf("assignment %d is %s, want it still cancelling", resumes.ID, state)
	}
	c.stops(t)
	if state := assignmentState(t, c.s, resumes.ID); state != store.AssignmentCancelled {
		t.Errorf("assignment %d is %s, want it cancelled", resumes.ID, state)
	}
	c.localViewRemoved(t)
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
	inbox, err := c.s.Inbox("leader@coordination", 0, 0, time.Now())
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

func TestAWitnessWithNoWordsIsRefusedBeforeThePullRequestIsClosed(t *testing.T) {
	c := newCancellableJob(t, newTwoResults(t), true)
	blank := c
	blank.witness = witnessed(t, c.s, " \n\t ")
	before, err := c.s.Journal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := blank.cancel("coordination", c.job.Number); err == nil || !strings.Contains(err.Error(), "has no words to give as the reason") {
		t.Errorf("job cancel citing a blank message returned %v, want it refused for having no words", err)
	}
	if after, err := c.s.Journal(); err != nil || len(after) != len(before) {
		t.Errorf("a refused cancellation journaled %d entries (%v)", len(after)-len(before), err)
	}
	if state, _ := os.ReadFile(filepath.Join(c.testDir, "pr-state")); strings.TrimSpace(string(state)) != "OPEN" {
		t.Errorf("the pull request is %s, want it left open", state)
	}
	if _, err := os.Stat(filepath.Join(c.testDir, "close-comment")); !os.IsNotExist(err) {
		t.Errorf("gh closed the pull request (%v), want no write to GitHub", err)
	}
	if j, err := c.s.Job(c.job.Number); err != nil || j.State != store.JobOpen {
		t.Errorf("after the refusal the job is %+v (%v), want it open", j, err)
	}

	// The user's own words then cancel it.
	resp, err := c.cancel("coordination", c.job.Number)
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Cancellation; got.Job.State != store.JobCancelled || got.Job.CancelReason != c.words || got.Closed == nil {
		t.Errorf("the cancellation is %+v, want the job cancelled with the user's words and its pull request closed", got)
	}
}

func TestARerunAfterTheStoreFailedRecordsThePullRequestTheDaemonClosed(t *testing.T) {
	c := newCancellableJob(t, newTwoResults(t), true)
	db, err := sql.Open("sqlite3", "file:"+c.d.cfg.Paths.Store+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER fail_cancel BEFORE UPDATE OF state ON jobs WHEN NEW.state = 'cancelled' BEGIN SELECT RAISE(ABORT, 'the disk is full'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.cancel("coordination", c.job.Number); err == nil || !strings.Contains(err.Error(), "pull request #7") || !strings.Contains(err.Error(), "the job was not cancelled") {
		t.Fatalf("job cancel with the store failing returned %v, want the closed pull request reported", err)
	}
	if state, _ := os.ReadFile(filepath.Join(c.testDir, "pr-state")); strings.TrimSpace(string(state)) != "CLOSED" {
		t.Fatalf("the pull request is %s, want gh to have closed it", state)
	}
	if _, err := db.Exec(`DROP TRIGGER fail_cancel`); err != nil {
		t.Fatal(err)
	}

	resp, err := c.cancel("coordination", c.job.Number)
	if err != nil {
		t.Fatal(err)
	}
	got := resp.Cancellation
	if got.Job.State != store.JobCancelled || got.Closed == nil || got.Closed.Agent != store.Daemon || got.Closed.Kind != store.EffectPullRequestClosed || got.Closed.Ref != "https://github.com/talvor/asmai-fixture/pull/7" {
		t.Errorf("the re-run's cancellation is %+v, want the daemon's closing of #7 recorded", got)
	}
	inbox, err := c.s.Inbox("leader@coordination", 0, 0, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(inbox, func(m store.Message) bool {
		return m.Kind == store.MessageJobCancelled && strings.Contains(m.Body, "pull request #7") && strings.Contains(m.Body, "is closed with a comment") && !strings.Contains(m.Body, "no open pull request")
	}) {
		t.Errorf("Coordination's leader was not told the pull request was closed: %+v", inbox)
	}
}

func TestAPullRequestClosedByAnotherHandIsNotTheDaemonsEffect(t *testing.T) {
	c := newCancellableJob(t, newTwoResults(t), true)
	if err := os.WriteFile(filepath.Join(c.testDir, "pr-state"), []byte("CLOSED"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.testDir, "close-comment"), []byte("Closing: superseded by #8"), 0o600); err != nil {
		t.Fatal(err)
	}
	resp, err := c.cancel("coordination", c.job.Number)
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Cancellation; got.Job.State != store.JobCancelled || got.Closed != nil {
		t.Errorf("the cancellation is %+v, want the job cancelled with nothing closed by the daemon", got)
	}
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
