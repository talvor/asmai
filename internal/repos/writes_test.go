// SPDX-License-Identifier: Apache-2.0

package repos

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// identity is the environment the daemon runs a worker's git in, with an
// identity standing in for the user's own configuration.
func identity() []string {
	env := os.Environ()
	return append(env,
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@example.test", "GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@example.test")
}

// workspaceOf makes a workspace of a clone of a remote, on branch, and
// returns it with the clone, the remote and the commit it starts at.
func workspaceOf(t *testing.T, branch string) (workspace, clone, remote, first string) {
	t.Helper()
	clone, remote, first = cloned(t)
	workspace = filepath.Join(realPath(t, t.TempDir()), "ws", "repo")
	if err := AddWorkspace(context.Background(), clone, workspace, branch, first); err != nil {
		t.Fatal(err)
	}
	return workspace, clone, remote, first
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestACommitStagesEverythingTheWorkspaceChangedOrOnlyTheNamedPathsAndRefusesWhenNothingChanged(t *testing.T) {
	ctx := context.Background()
	workspace, _, _, first := workspaceOf(t, "asmai/job-1/1")

	if _, err := CommitWorkspace(ctx, workspace, identity(), "nothing", nil); err == nil || !strings.Contains(err.Error(), "nothing to commit") {
		t.Errorf("committing with no change returned %v, want a refusal that there is nothing to commit", err)
	}

	write(t, workspace, "a.txt", "a\n")
	write(t, workspace, "b.txt", "b\n")
	commit, err := CommitWorkspace(ctx, workspace, identity(), "Add a", []string{"a.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if got := run(t, workspace, "rev-parse", "HEAD"); got != commit || commit == first {
		t.Fatalf("the commit is %s, the workspace is at %s", commit, got)
	}
	if got := run(t, workspace, "show", "--name-only", "--format=%s", "HEAD"); got != "Add a\n\na.txt" {
		t.Errorf("the commit holds %q, want only a.txt under its message", got)
	}
	if got := run(t, workspace, "status", "--porcelain"); got != "?? b.txt" {
		t.Errorf("the workspace has %q left, want b.txt untouched", got)
	}

	// With no paths it takes everything, deletions and new files included.
	if err := os.Remove(filepath.Join(workspace, "a.txt")); err != nil {
		t.Fatal(err)
	}
	write(t, workspace, "c.txt", "c\n")
	if _, err := CommitWorkspace(ctx, workspace, identity(), "Everything", nil); err != nil {
		t.Fatal(err)
	}
	if got := run(t, workspace, "status", "--porcelain"); got != "" {
		t.Errorf("the workspace has %q left after committing everything", got)
	}
	if got := run(t, workspace, "show", "--name-status", "--format=", "HEAD"); !strings.Contains(got, "D\ta.txt") || !strings.Contains(got, "A\tb.txt") || !strings.Contains(got, "A\tc.txt") {
		t.Errorf("the commit holds %q", got)
	}
}

func TestACommitRunsTheRepositorysHooksAndASkippedHookIsNotAnOption(t *testing.T) {
	ctx := context.Background()
	workspace, clone, _, _ := workspaceOf(t, "asmai/job-1/1")
	hook := filepath.Join(clone, ".git", "hooks", "pre-commit")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho 'lint failed' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, workspace, "a.txt", "a\n")
	_, err := CommitWorkspace(ctx, workspace, identity(), "Add a", nil)
	if err == nil || !strings.Contains(err.Error(), "lint failed") {
		t.Fatalf("a commit the pre-commit hook refuses returned %v, want its message", err)
	}
	if got := run(t, workspace, "rev-list", "--count", "HEAD"); got != "1" {
		t.Errorf("the refused commit left %s commits", got)
	}
}

func TestAMessageThatLooksLikeAnOptionIsACommitMessage(t *testing.T) {
	ctx := context.Background()
	workspace, _, _, _ := workspaceOf(t, "asmai/job-1/1")
	write(t, workspace, "a.txt", "a\n")
	if _, err := CommitWorkspace(ctx, workspace, identity(), "--amend", nil); err != nil {
		t.Fatal(err)
	}
	if got := run(t, workspace, "log", "-1", "--format=%s"); got != "--amend" {
		t.Errorf("the message is %q", got)
	}
	if _, err := CommitWorkspace(ctx, workspace, identity(), "  ", nil); err == nil {
		t.Error("a blank message was accepted")
	}
}

func TestAMergeTakesARefInAndAConflictIsLeftForTheWorkerToResolveAndCommit(t *testing.T) {
	ctx := context.Background()
	clone, _, _ := cloned(t)
	write(t, clone, "shared.txt", "base\n")
	run(t, clone, "add", "shared.txt")
	run(t, clone, "commit", "-m", "base")
	workspace := filepath.Join(realPath(t, t.TempDir()), "ws", "repo")
	if err := AddWorkspace(ctx, clone, workspace, "asmai/job-1/1", run(t, clone, "rev-parse", "HEAD")); err != nil {
		t.Fatal(err)
	}

	// The job branch, main here, and the workspace both change shared.txt.
	write(t, clone, "shared.txt", "theirs\n")
	run(t, clone, "commit", "-am", "theirs")
	write(t, workspace, "shared.txt", "mine\n")
	run(t, workspace, "commit", "-am", "mine")

	result, err := MergeIntoWorkspace(ctx, workspace, identity(), "main")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(result.Conflicts, []string{"shared.txt"}) || result.Commit != "" || result.UpToDate {
		t.Fatalf("the merge is %+v, want a conflict in shared.txt and no commit", result)
	}

	// The worker resolves it, and committing completes the merge.
	write(t, workspace, "shared.txt", "both\n")
	commit, err := CommitWorkspace(ctx, workspace, identity(), "Merge the job branch", nil)
	if err != nil {
		t.Fatal(err)
	}
	if parents := strings.Fields(run(t, workspace, "rev-list", "--parents", "-n1", commit)); len(parents) != 3 {
		t.Errorf("the commit is %v, want a merge of two parents", parents)
	}
	if got := run(t, workspace, "status", "--porcelain"); got != "" {
		t.Errorf("the workspace has %q left", got)
	}

	// Taking in what is already in is a no-op, and abort undoes a conflict.
	again, err := MergeIntoWorkspace(ctx, workspace, identity(), "main")
	if err != nil || again.Commit != "" || len(again.Conflicts) != 0 || !again.UpToDate {
		t.Errorf("merging what is already in returned %+v, %v", again, err)
	}
	write(t, clone, "shared.txt", "later\n")
	run(t, clone, "commit", "-am", "later")
	write(t, workspace, "shared.txt", "mine again\n")
	run(t, workspace, "commit", "-am", "mine again")
	if result, err = MergeIntoWorkspace(ctx, workspace, identity(), "main"); err != nil || len(result.Conflicts) == 0 {
		t.Fatalf("the second merge is %+v, %v, want a conflict", result, err)
	}
	if err := AbortMerge(ctx, workspace, identity()); err != nil {
		t.Fatal(err)
	}
	if got := run(t, workspace, "status", "--porcelain"); got != "" {
		t.Errorf("after the abort the workspace has %q", got)
	}
}

func TestACleanMergeOfAnOriginBranchIsCommittedByTheMergeItself(t *testing.T) {
	ctx := context.Background()
	workspace, clone, remote, _ := workspaceOf(t, "asmai/job-1/1")
	write(t, workspace, "mine.txt", "mine\n")
	if _, err := CommitWorkspace(ctx, workspace, identity(), "Mine", nil); err != nil {
		t.Fatal(err)
	}

	other := filepath.Join(realPath(t, t.TempDir()), "other")
	run(t, filepath.Dir(other), "clone", "--quiet", remote, other)
	write(t, other, "theirs.txt", "theirs\n")
	run(t, other, "add", "theirs.txt")
	run(t, other, "commit", "-m", "Theirs")
	run(t, other, "push", "--quiet", "origin", "main")

	if err := Fetch(ctx, clone); err != nil {
		t.Fatal(err)
	}
	result, err := MergeIntoWorkspace(ctx, workspace, identity(), "origin/main")
	if err != nil {
		t.Fatal(err)
	}
	if result.Commit == "" || result.Commit != run(t, workspace, "rev-parse", "HEAD") || len(result.Conflicts) != 0 {
		t.Fatalf("the merge is %+v", result)
	}
	if got := run(t, workspace, "log", "--format=%s", "-3"); !strings.Contains(got, "Merge") {
		t.Errorf("the log is %q, want a merge commit", got)
	}
}

func TestABranchIsPushedPlainlyFromTheWorkspaceAndANonFastForwardPushIsRefused(t *testing.T) {
	ctx := context.Background()
	workspace, _, remote, _ := workspaceOf(t, "asmai/job-1/1")
	write(t, workspace, "a.txt", "a\n")
	commit, err := CommitWorkspace(ctx, workspace, identity(), "Add a", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := PushWorkspaceBranch(ctx, workspace, identity(), "asmai/job-1/1"); err != nil {
		t.Fatal(err)
	}
	if got := run(t, remote, "rev-parse", "refs/heads/asmai/job-1/1"); got != commit {
		t.Fatalf("origin's branch is at %s, want %s", got, commit)
	}

	// Somebody else moves origin's branch: a plain push never overwrites it.
	other := filepath.Join(realPath(t, t.TempDir()), "other")
	run(t, filepath.Dir(other), "clone", "--quiet", remote, other)
	run(t, other, "checkout", "--quiet", "asmai/job-1/1")
	run(t, other, "commit", "--allow-empty", "-m", "outside")
	run(t, other, "push", "--quiet", "origin", "asmai/job-1/1")
	outside := run(t, other, "rev-parse", "HEAD")
	write(t, workspace, "b.txt", "b\n")
	if _, err := CommitWorkspace(ctx, workspace, identity(), "Add b", nil); err != nil {
		t.Fatal(err)
	}
	if err := PushWorkspaceBranch(ctx, workspace, identity(), "asmai/job-1/1"); err == nil {
		t.Fatal("a push over a commit on origin was accepted")
	}
	if got := run(t, remote, "rev-parse", "refs/heads/asmai/job-1/1"); got != outside {
		t.Errorf("origin's branch is at %s after the refusal, want %s", got, outside)
	}
}
