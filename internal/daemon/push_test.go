// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/repos"
	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/session"
	"github.com/talvor/asmai/internal/store"
)

// leaderWorks has Engineering's leader of f work on the next dispatch waiting
// for it, as its fetch does, and returns the dispatch.
func (f *twoResults) leaderWorks(t *testing.T) int64 {
	t.Helper()
	owner := roles.LeaderOf(roles.Engineering).String()
	d, err := f.s.NextDispatch(owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.ChangeDispatch(d.ID, 1, d.State, store.DispatchWorking, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	f.d.leaders[owner].currentDispatch = d.ID
	return d.ID
}

func TestTheDaemonPushesTheJobBranchForItsDeliveryOwnerAndRefusesOverOutsideCommitsUntilAWorkerMergesThem(t *testing.T) {
	f := newTwoResults(t)
	ctx := context.Background()
	branch := JobBranchName(f.job)
	push := Request{Session: "credential", Command: CommandPush, Job: f.job.Number}
	if _, err := f.decide(CommandAccept, f.assignments[0].ID, "file-1.txt is there"); err != nil {
		t.Fatal(err)
	}

	// Only the delivery owner asks, and only while working on a dispatch.
	quality := roles.LeaderOf(roles.Quality)
	f.d.leaders[quality.String()] = &leader{address: quality, session: &session.Session{}, generation: 1}
	f.d.sessions["quality"] = sessionRef{address: quality, generation: 1}
	if _, err := f.d.push(Request{Session: "quality", Command: CommandPush, Job: f.job.Number}); err == nil || !strings.Contains(err.Error(), "only leader@engineering") {
		t.Errorf("Quality's leader asking for the push returned %v", err)
	}
	if _, err := f.d.push(push); err == nil || !strings.Contains(err.Error(), "no current dispatch") {
		t.Errorf("a push asked for outside a dispatch returned %v", err)
	}
	dispatch := f.leaderWorks(t)
	if _, err := f.d.push(Request{Session: "credential", Command: CommandPush, Job: 99}); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("a push of a job that does not exist returned %v", err)
	}

	// With nothing outside on origin, the job branch is pushed plainly, and
	// the push is the daemon's effect, tagged with the leader's dispatch.
	first, err := f.d.push(push)
	if err != nil {
		t.Fatal(err)
	}
	if first.Result != store.PushPushed || first.Commit != f.commits[0] || first.Branch != branch || first.Dispatch != dispatch || first.Effect == 0 {
		t.Errorf("the first push is %+v", first)
	}
	if got := git(t, f.origin, "rev-parse", "refs/heads/"+branch); got != f.commits[0] {
		t.Fatalf("origin's job branch is at %s, want the tip %s", got, f.commits[0])
	}

	// Someone else pushes a commit to the job branch on origin, and the job
	// branch moves on with accepted work.
	other := filepath.Join(t.TempDir(), "other")
	git(t, filepath.Dir(other), "clone", f.origin, other)
	git(t, other, "checkout", branch)
	if err := os.WriteFile(filepath.Join(other, "outside.txt"), []byte("outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, other, "add", "outside.txt")
	git(t, other, "commit", "-m", "An outside commit")
	git(t, other, "push", "origin", branch)
	outside := git(t, other, "rev-parse", "HEAD")
	more := f.writingResult(t, func(dir string) {
		git(t, dir, "commit", "--allow-empty", "-m", "more work")
	})
	resp, err := f.decide(CommandAccept, more.ID, "more work is there")
	if err != nil {
		t.Fatal(err)
	}
	tip := resp.JobBranch.Tip

	// The push is refused and recorded as an outside change, and nothing
	// reaches origin.
	_, err = f.d.push(push)
	if err == nil || !strings.Contains(err.Error(), outside) || !strings.Contains(err.Error(), "AsmAI did not make") || !strings.Contains(err.Error(), "merge origin/"+branch) {
		t.Fatalf("a push over an outside commit returned %v", err)
	}
	if got := git(t, f.origin, "rev-parse", "refs/heads/"+branch); got != outside {
		t.Fatalf("origin's job branch is at %s after the refusal, want the outside commit %s", got, outside)
	}
	pushes, err := f.s.Pushes(f.job.Number)
	if err != nil {
		t.Fatal(err)
	}
	if len(pushes) != 2 || pushes[1].Result != store.PushRefused || pushes[1].Commit != tip || strings.Join(pushes[1].Outside, ",") != outside || pushes[1].Found != "origin/"+branch {
		t.Fatalf("the job's pushes are %+v", pushes)
	}

	// A worker merges them in through a writing assignment, and the next
	// push keeps them and records the outside change taken in.
	merge := f.writingResult(t, func(dir string) {
		git(t, dir, "fetch", "origin")
		git(t, dir, "merge", "--no-edit", "origin/"+branch)
	})
	resp, err = f.decide(CommandAccept, merge.ID, "the outside commit is merged in")
	if err != nil {
		t.Fatal(err)
	}
	last, err := f.d.push(push)
	if err != nil {
		t.Fatal(err)
	}
	if last.Result != store.PushPushed || last.Commit != resp.JobBranch.Tip {
		t.Errorf("the push after the merge is %+v", last)
	}
	if got := git(t, f.origin, "rev-parse", "refs/heads/"+branch); got != resp.JobBranch.Tip {
		t.Errorf("origin's job branch is at %s, want the merge %s", got, resp.JobBranch.Tip)
	}
	if ok, err := repos.Contains(ctx, f.origin, "refs/heads/"+branch, outside); err != nil || !ok {
		t.Errorf("origin's job branch lost the outside commit: %v (%v)", ok, err)
	}
	if pushes, err = f.s.Pushes(f.job.Number); err != nil || len(pushes) != 3 || pushes[1].MergedIn != resp.JobBranch.Tip {
		t.Errorf("the job's pushes are %+v (%v), want the outside change merged in by %s", pushes, err, resp.JobBranch.Tip)
	}
}
