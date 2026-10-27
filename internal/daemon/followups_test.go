// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/store"
)

// fakeViewGH is a gh on the PATH that shows one pull request, #7 of job 1's
// branch, in the state the test's file holds: OPEN, MERGED or CLOSED. It
// refuses every other call, and `gh pr view` as long as the state file is
// missing.
const fakeViewGH = `#!/bin/sh
[ "$1 $2" = "pr view" ] || { echo "the fake gh does not do: $*" >&2; exit 1; }
state=$(cat "$ASMAI_TEST_DIR/pr-state" 2>/dev/null) || { echo "no pull requests found" >&2; exit 1; }
printf '{"number":7,"url":"https://github.com/talvor/asmai-fixture/pull/7","title":"Add a greeting","isDraft":false,"headRefName":"%s","headRefOid":"abc","state":"%s"}\n' "$(cat "$ASMAI_TEST_DIR/pr-branch")" "$state"
`

// deliveredFirstJob is a fixture whose first job has an accepted result and
// an open pull request, so that a follow-up can continue it.
type deliveredFirstJob struct {
	twoResults
	testDir string
	branch  string
}

func newDeliveredFirstJob(t *testing.T) deliveredFirstJob {
	return newFirstJobWithPullRequest(t, true, false)
}

func newFirstJobWithPullRequest(t *testing.T, delivered, keepSecond bool) deliveredFirstJob {
	t.Helper()
	f := newTwoResultsOf(t, "planning")
	testDir := t.TempDir()
	bin := filepath.Join(testDir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(fakeViewGH), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ASMAI_TEST_DIR", testDir)
	branch := JobBranchName(f.job)
	if err := os.WriteFile(filepath.Join(testDir, "pr-branch"), []byte(branch), 0o600); err != nil {
		t.Fatal(err)
	}

	// The first result is accepted; the second can remain live for handover.
	if _, err := f.decide(CommandAccept, f.assignments[0].ID, "file-1.txt is there"); err != nil {
		t.Fatal(err)
	}
	if !keepSecond {
		if _, err := f.decide(CommandCancel, f.assignments[1].ID, "not needed"); err != nil {
			t.Fatal(err)
		}
		if _, err := f.s.AssignmentCancelled(f.assignments[1].ID, store.Cancellation{Commit: f.commits[1], Pushed: true}, time.Now()); err != nil {
			t.Fatal(err)
		}
	}

	// The daemon pushes the job branch, and the delivery owner opens the pull
	// request and records it, in the dispatch that brought it the result.
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
	if delivered {
		if _, _, err := f.s.JobDelivered(store.Delivery{Job: f.job.Number, Number: 7, URL: pr.URL, Head: tip, NoCI: true}, "delivered", at); err != nil {
			t.Fatal(err)
		}
	}
	return deliveredFirstJob{twoResults: f, testDir: testDir, branch: branch}
}

// followUp opens a new job in the fixture's repository, with the job branch
// the daemon makes as a job opens, and links it to job.
func (f deliveredFirstJob) followUp(t *testing.T, follows int64) (store.Job, Response, error) {
	t.Helper()
	j, err := f.s.JobOpened(store.Job{Repository: "fixture", Reading: "Greet by name too", Mandate: store.MandateTestedPR, Criteria: []string{"done"}}, true, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	repository, err := f.s.Repository("fixture")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.d.ensureJobBranch(context.Background(), j, repository); err != nil {
		t.Fatal(err)
	}
	j, err = f.s.JobLinked(j.Number, follows, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var resp Response
	err = f.d.settleFollowUp(j, &resp)
	return j, resp, err
}

func TestAFollowUpToAJobWithAnOpenPullRequestContinuesItsBranchAndPullRequestFromOriginsCopy(t *testing.T) {
	f := newDeliveredFirstJob(t)
	if err := os.WriteFile(filepath.Join(f.testDir, "pr-state"), []byte("OPEN"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The branch on origin has a commit the clone's copy lacks, as a review
	// fix would: the follow-up starts from origin's copy.
	other := filepath.Join(t.TempDir(), "other")
	git(t, filepath.Dir(other), "clone", "--branch", f.branch, f.origin, other)
	if err := os.WriteFile(filepath.Join(other, "review.txt"), []byte("fix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, other, "add", "review.txt")
	git(t, other, "commit", "-m", "review fix")
	git(t, other, "push", "origin", f.branch)
	originTip := git(t, other, "rev-parse", "HEAD")

	j, resp, err := f.followUp(t, f.job.Number)
	if err != nil {
		t.Fatal(err)
	}
	if j.Follows != f.job.Number {
		t.Errorf("the follow-up follows job %d, want %d", j.Follows, f.job.Number)
	}
	b, err := f.s.JobBranch(j.Number)
	if err != nil {
		t.Fatal(err)
	}
	if b.Name != f.branch || b.Continues != f.job.Number || b.StartedFrom != originTip || b.Tip != originTip {
		t.Errorf("the follow-up's job branch is %+v, want %s continued from origin's copy %s", b, f.branch, originTip)
	}
	if resp.Continues == nil || resp.Continues.Number != 7 || resp.JobBranch == nil || resp.JobBranch.Name != f.branch {
		t.Errorf("the link answered %+v and %+v, want it to continue pull request #7 on %s", resp.Continues, resp.JobBranch, f.branch)
	}
	if got := git(t, f.clone, "rev-parse", "refs/heads/"+f.branch); got != originTip {
		t.Errorf("the clone's branch is at %s, want origin's copy %s", got, originTip)
	}
	if out := git(t, f.clone, "branch", "--list", JobBranchName(j)); out != "" {
		t.Errorf("the branch the daemon made as the follow-up opened is still there: %s", out)
	}
	if got := git(t, filepath.Join(f.d.cfg.Paths.Views, fmt.Sprintf("job-%d", j.Number)), "rev-parse", "HEAD"); got != originTip {
		t.Errorf("the follow-up's view is at %s, want %s", got, originTip)
	}

	// The brief points to the pull request it continues.
	var continued []store.PullRequest
	if resp.Continues != nil {
		continued = []store.PullRequest{*resp.Continues}
	}
	brief := brief(j, store.Repository{Origin: f.origin, Clone: f.clone, DefaultBranch: "trunk"}, nil, nil, &b, continued, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, "")
	for _, want := range []string{"Follow-up to job 1", "continues job 1's job branch", "Continues job 1's pull request #7 https://github.com/talvor/asmai-fixture/pull/7, still open"} {
		if !strings.Contains(brief, want) {
			t.Errorf("the brief lacks %q:\n%s", want, brief)
		}
	}

	// A job branch belongs to one open job at a time: a second follow-up to
	// the same job starts afresh while the first holds the branch, and the
	// store refuses to give it the branch.
	second, resp2, err := f.followUp(t, f.job.Number)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := f.s.JobBranch(second.Number)
	if err != nil {
		t.Fatal(err)
	}
	if b2.Continues != 0 || b2.Name != JobBranchName(second) || !strings.Contains(resp2.Starts, "open job 2 holds the job branch") {
		t.Errorf("the second follow-up's branch is %+v and it says %q, want a new branch and the reason", b2, resp2.Starts)
	}
	if _, err := f.s.JobBranchMade(store.JobBranch{Job: 99, Repository: "fixture", Name: f.branch, StartedFrom: originTip, View: "v"}, time.Now()); err == nil {
		t.Errorf("a branch an open job holds was given to another job")
	}
}

func TestAFollowUpReplacesAnOpenJobWithAnOpenPullRequest(t *testing.T) {
	f := newFirstJobWithPullRequest(t, false, false)
	if err := os.WriteFile(filepath.Join(f.testDir, "pr-state"), []byte("OPEN"), 0o600); err != nil {
		t.Fatal(err)
	}
	j, resp, err := f.followUp(t, f.job.Number)
	if err != nil {
		t.Fatal(err)
	}
	branch, err := f.s.JobBranch(j.Number)
	if err != nil {
		t.Fatal(err)
	}
	if branch.Name != f.branch || branch.Continues != f.job.Number || resp.Continues == nil || resp.Continues.Number != 7 {
		t.Errorf("follow-up branch = %+v, continuation = %+v", branch, resp.Continues)
	}
	predecessor, err := f.s.Job(f.job.Number)
	if err != nil {
		t.Fatal(err)
	}
	if predecessor.State != store.JobEnded {
		t.Errorf("predecessor state = %s, want ended", predecessor.State)
	}
	if holder, err := f.s.BranchHeldByOpenJob("fixture", f.branch, f.job.Number); err != nil || holder != j.Number {
		t.Errorf("branch holder = %d (%v), want job %d", holder, err, j.Number)
	}
	entries, err := f.s.JobEntries(f.job.Number)
	if err != nil {
		t.Fatal(err)
	}
	var replaced bool
	for _, entry := range entries {
		if entry.Kind == store.KindJobEnded {
			var end struct {
				Job        int64  `json:"job"`
				By         string `json:"by"`
				ReplacedBy int64  `json:"replaced_by"`
			}
			if err := json.Unmarshal(entry.Data, &end); err != nil {
				t.Fatal(err)
			}
			replaced = end.Job == f.job.Number && end.By == "replaced" && end.ReplacedBy == j.Number
		}
	}
	if !replaced {
		t.Errorf("predecessor has no journaled replacement: %+v", entries)
	}
}

func TestAFollowUpContinuesAnInheritedPullRequest(t *testing.T) {
	f := newDeliveredFirstJob(t)
	if err := os.WriteFile(filepath.Join(f.testDir, "pr-state"), []byte("OPEN"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, _, err := f.followUp(t, f.job.Number)
	if err != nil {
		t.Fatal(err)
	}
	if prs, err := f.s.PullRequests(b.Number); err != nil || len(prs) != 0 {
		t.Fatalf("job %d's direct pull request records = %+v (%v), want none", b.Number, prs, err)
	}
	c, resp, err := f.followUp(t, b.Number)
	if err != nil {
		t.Fatal(err)
	}
	branch, err := f.s.JobBranch(c.Number)
	if err != nil {
		t.Fatal(err)
	}
	if branch.Name != f.branch || branch.Continues != b.Number || resp.Continues == nil || resp.Continues.URL != "https://github.com/talvor/asmai-fixture/pull/7" {
		t.Errorf("chained branch = %+v, pull request = %+v", branch, resp.Continues)
	}
	if earlier, err := f.s.Job(b.Number); err != nil || earlier.State != store.JobEnded {
		t.Errorf("intermediate job = %+v (%v), want ended", earlier, err)
	}
	shown := callWith(t, f.d.jobCommand, Request{Command: CommandBrief, Job: c.Number})
	if shown.Error != "" || !strings.Contains(shown.Brief, "Continues job 2's pull request #7 https://github.com/talvor/asmai-fixture/pull/7") {
		t.Errorf("chained brief = %+v", shown)
	}
}

func TestAFollowUpKeepsItsOwnBranchUntilLiveWorkIsCommittedAndPushed(t *testing.T) {
	f := newFirstJobWithPullRequest(t, false, true)
	if err := os.WriteFile(filepath.Join(f.testDir, "pr-state"), []byte("OPEN"), 0o600); err != nil {
		t.Fatal(err)
	}
	live := f.assignments[1]
	file := filepath.Join(live.Workspace.Path, "unfinished.txt")
	if err := os.WriteFile(file, []byte("unfinished\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	j, resp, err := f.followUp(t, f.job.Number)
	if err != nil {
		t.Fatal(err)
	}
	assertFresh := func(want string) {
		t.Helper()
		branch, err := f.s.JobBranch(j.Number)
		if err != nil {
			t.Fatal(err)
		}
		if branch.Name != JobBranchName(j) || branch.Continues != 0 || resp.Continues != nil || !strings.Contains(resp.Starts, want) {
			t.Errorf("unsafe handover: branch = %+v, response = %+v, want %q", branch, resp, want)
		}
		if predecessor, err := f.s.Job(f.job.Number); err != nil || predecessor.State != store.JobOpen {
			t.Errorf("predecessor = %+v (%v), want open", predecessor, err)
		}
		if assignment, err := f.s.Assignment(live.ID); err != nil || assignment.State != store.AssignmentSubmitted {
			t.Errorf("assignment = %+v (%v), want submitted", assignment, err)
		}
	}
	assertFresh("uncommitted work")
	if _, err := f.s.JobBranchDropped(j.Number, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.JobBranchMade(store.JobBranch{Job: j.Number, Repository: "fixture", Name: f.branch, StartedFrom: f.started, View: "view", Continues: f.job.Number}, time.Now()); err == nil {
		t.Error("the store transferred the branch without checked live work")
	}
	linked, err := f.s.JobLinked(j.Number, f.job.Number, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	resp = Response{}
	if err := f.d.settleFollowUp(linked, &resp); err != nil {
		t.Fatal(err)
	}
	assertFresh("uncommitted work")
	git(t, live.Workspace.Path, "add", "unfinished.txt")
	git(t, live.Workspace.Path, "commit", "-m", "finish pending work")
	resp = Response{}
	if err := f.d.settleFollowUp(linked, &resp); err != nil {
		t.Fatal(err)
	}
	assertFresh("not pushed")
	git(t, live.Workspace.Path, "push", "origin", live.Workspace.Branch)
	resp = Response{}
	if err := f.d.settleFollowUp(linked, &resp); err != nil {
		t.Fatal(err)
	}
	branch, err := f.s.JobBranch(j.Number)
	if err != nil {
		t.Fatal(err)
	}
	if branch.Name != f.branch || branch.Continues != f.job.Number || resp.Continues == nil || resp.Continues.Number != 7 {
		t.Errorf("safe handover: branch = %+v, response = %+v", branch, resp)
	}
	if predecessor, err := f.s.Job(f.job.Number); err != nil || predecessor.State != store.JobEnded {
		t.Errorf("predecessor = %+v (%v), want ended", predecessor, err)
	}
	if assignment, err := f.s.Assignment(live.ID); err != nil || assignment.State != store.AssignmentCancelled {
		t.Errorf("assignment = %+v (%v), want cancelled", assignment, err)
	}
}

func TestAFollowUpReplacesAnOpenJobAndItsLiveAssignment(t *testing.T) {
	for _, state := range []string{store.AssignmentSubmitted, store.AssignmentActive, store.AssignmentCancelling} {
		t.Run(state, func(t *testing.T) {
			f := newFirstJobWithPullRequest(t, false, true)
			live := f.assignments[1]
			switch state {
			case store.AssignmentActive:
				if _, _, err := f.s.ResultRejected(live.ID, live.Owner, []string{"revise the result"}, time.Now()); err != nil {
					t.Fatal(err)
				}
			case store.AssignmentCancelling:
				if _, _, err := f.s.CancellationRequested(live.ID, live.Owner, []string{"no longer needed"}, time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(f.testDir, "pr-state"), []byte("OPEN"), 0o600); err != nil {
				t.Fatal(err)
			}
			j, resp, err := f.followUp(t, f.job.Number)
			if err != nil {
				t.Fatal(err)
			}
			if resp.Continues == nil || resp.Continues.Number != 7 {
				t.Errorf("continuation = %+v, want open pull request #7", resp.Continues)
			}
			if got, err := f.s.Assignment(live.ID); err != nil || got.State != store.AssignmentCancelled {
				t.Errorf("replaced assignment = %+v (%v), want cancelled", got, err)
			}
			if got, err := f.s.Job(f.job.Number); err != nil || got.State != store.JobEnded {
				t.Errorf("predecessor = %+v (%v), want ended", got, err)
			}
			if holder, err := f.s.BranchHeldByOpenJob("fixture", f.branch, f.job.Number); err != nil || holder != j.Number {
				t.Errorf("branch holder = %d (%v), want job %d", holder, err, j.Number)
			}
			if _, err := f.s.LiveAssignmentOf(live.Worker); !errors.Is(err, sql.ErrNoRows) {
				t.Errorf("replaced worker still has a live assignment: %v", err)
			}
			linked, err := f.s.JobLinked(j.Number, f.job.Number, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			var again Response
			if err := f.d.settleFollowUp(linked, &again); err != nil || again.Continues == nil || again.Continues.Number != 7 {
				t.Errorf("repeated link = %+v (%v), want the same continuation", again, err)
			}
			entries, err := f.s.JobEntries(f.job.Number)
			if err != nil {
				t.Fatal(err)
			}
			var cancelled, ended int
			for _, entry := range entries {
				switch entry.Kind {
				case store.KindAssignmentCancelled:
					var event struct {
						Assignment int64  `json:"assignment"`
						By         string `json:"by"`
						ReplacedBy int64  `json:"replaced_by"`
					}
					if err := json.Unmarshal(entry.Data, &event); err != nil {
						t.Fatal(err)
					}
					if event.Assignment == live.ID && event.By == "replacement" && event.ReplacedBy == j.Number {
						cancelled++
					}
				case store.KindJobEnded:
					var event struct {
						By         string `json:"by"`
						ReplacedBy int64  `json:"replaced_by"`
					}
					if err := json.Unmarshal(entry.Data, &event); err != nil {
						t.Fatal(err)
					}
					if event.By == "replaced" && event.ReplacedBy == j.Number {
						ended++
					}
				}
			}
			if cancelled != 1 || ended != 1 {
				t.Errorf("journaled replacement: assignment %d times, job %d times", cancelled, ended)
			}
		})
	}
}

func TestAFollowUpStartsAfreshOnceThePullRequestIsMergedOrClosedOrMissing(t *testing.T) {
	for _, state := range []string{"MERGED", "CLOSED"} {
		t.Run(state, func(t *testing.T) {
			f := newDeliveredFirstJob(t)
			if err := os.WriteFile(filepath.Join(f.testDir, "pr-state"), []byte(state), 0o600); err != nil {
				t.Fatal(err)
			}
			j, resp, err := f.followUp(t, f.job.Number)
			if err != nil {
				t.Fatal(err)
			}
			b, err := f.s.JobBranch(j.Number)
			if err != nil {
				t.Fatal(err)
			}
			if b.Continues != 0 || b.Name != JobBranchName(j) || resp.Continues != nil || !strings.Contains(resp.Starts, state) {
				t.Errorf("the follow-up's branch is %+v and the link answered %+v and %q, want a new branch, with %s as the reason", b, resp.Continues, resp.Starts, state)
			}
			if b.StartedFrom == f.commits[0] {
				t.Errorf("the follow-up started from the earlier job's work %s, not the base", b.StartedFrom)
			}
		})
	}
	t.Run("the earlier job is open", func(t *testing.T) {
		f := newTwoResultsOf(t, "planning")
		other, err := f.s.JobOpened(store.Job{Repository: "fixture", Reading: "Greet by name too", Mandate: store.MandateTestedPR, Criteria: []string{"done"}}, true, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		repository, _ := f.s.Repository("fixture")
		if _, err := f.d.ensureJobBranch(context.Background(), other, repository); err != nil {
			t.Fatal(err)
		}
		other, err = f.s.JobLinked(other.Number, f.job.Number, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		var resp Response
		if err := f.d.settleFollowUp(other, &resp); err != nil {
			t.Fatal(err)
		}
		if b, _ := f.s.JobBranch(other.Number); b.Continues != 0 || !strings.Contains(resp.Starts, "job 1 has no pull request") {
			t.Errorf("the follow-up's branch is %+v and it says %q", b, resp.Starts)
		}
	})
}
