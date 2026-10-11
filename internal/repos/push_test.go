// SPDX-License-Identifier: Apache-2.0

package repos

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestAJobBranchIsPushedPlainlyAndOutsideCommitsOnOriginAreFoundNotOverwritten(t *testing.T) {
	ctx := context.Background()
	clone, remote, first := cloned(t)
	const branch = "asmai/job-1-greet"
	if err := CreateBranch(ctx, clone, branch, first); err != nil {
		t.Fatal(err)
	}
	commit := func(message string) string {
		t.Helper()
		tip := run(t, clone, "rev-parse", "refs/heads/"+branch)
		next := run(t, clone, "commit-tree", tip+"^{tree}", "-p", tip, "-m", message)
		run(t, clone, "update-ref", "refs/heads/"+branch, next, tip)
		return next
	}
	work := commit("work")

	// Origin has no job branch yet: nothing on it is outside, and the push
	// makes it.
	if outside, err := PushBranch(ctx, clone, branch); err != nil || len(outside) != 0 {
		t.Fatalf("the first push found the outside commits %v (%v), want none", outside, err)
	}
	if got := run(t, remote, "rev-parse", "refs/heads/"+branch); got != work {
		t.Fatalf("origin's job branch is at %s, want %s", got, work)
	}

	// Someone else pushes two commits to the job branch on origin, and the
	// clone moves on too.
	other := filepath.Join(realPath(t, t.TempDir()), "other")
	run(t, filepath.Dir(other), "clone", "--quiet", remote, other)
	run(t, other, "checkout", "--quiet", branch)
	run(t, other, "commit", "--allow-empty", "-m", "outside 1")
	run(t, other, "commit", "--allow-empty", "-m", "outside 2")
	run(t, other, "push", "--quiet", "origin", branch)
	outsideTip := run(t, other, "rev-parse", "HEAD")
	want := strings.Split(run(t, other, "rev-list", work+"..HEAD"), "\n")
	mine := commit("more work")

	// The push fetches them, newest first, and pushes nothing over them.
	outside, err := PushBranch(ctx, clone, branch)
	if err != nil || !slices.Equal(outside, want) {
		t.Fatalf("the push found the outside commits %v (%v), want %v", outside, err, want)
	}
	if got := run(t, remote, "rev-parse", "refs/heads/"+branch); got != outsideTip {
		t.Fatalf("origin's job branch is at %s after a refused push, want the outside tip %s", got, outsideTip)
	}

	// Once they are merged in, nothing is outside and the push keeps them.
	run(t, clone, "branch", "--quiet", "merging", mine)
	merge := filepath.Join(realPath(t, t.TempDir()), "merge")
	run(t, clone, "worktree", "add", "--quiet", merge, "merging")
	run(t, merge, "merge", "--quiet", "--no-edit", "origin/"+branch)
	merged := run(t, merge, "rev-parse", "HEAD")
	if err := MoveBranch(ctx, clone, branch, merged, mine); err != nil {
		t.Fatal(err)
	}
	if outside, err := PushBranch(ctx, clone, branch); err != nil || len(outside) != 0 {
		t.Fatalf("after the merge, the push found the outside commits %v (%v), want none", outside, err)
	}
	if got := run(t, remote, "rev-parse", "refs/heads/"+branch); got != merged {
		t.Errorf("origin's job branch is at %s, want the merge %s", got, merged)
	}
	if ok, err := Contains(ctx, remote, "refs/heads/"+branch, outsideTip); err != nil || !ok {
		t.Errorf("origin's job branch lost the outside commits: %v (%v)", ok, err)
	}
}
