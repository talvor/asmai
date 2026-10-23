// SPDX-License-Identifier: Apache-2.0

package repos

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// The writes a worker makes in its workspace. Only the daemon runs them: a
// worker asks for each with an `asmai` command, as no git write of an agent
// is approved without a person (ADR 0018). The daemon runs git as the user,
// with the worker's own environment in env, so that the repository's hooks
// run as they did when the worker ran git, and the hooks directory of AsmAI's
// adds the trailers to the commit. A hook is never skipped.

// CommitWorkspace commits what the workspace at dir changed and returns the
// commit it is at. With no paths it stages everything the workspace changed,
// deletions and new files included; with paths it stages only those, which
// are relative to the workspace. Where a merge is in progress it completes
// it with the message, whether or not anything else changed. With nothing to
// commit, it commits nothing and says so.
func CommitWorkspace(ctx context.Context, dir string, env []string, message string, paths []string) (string, error) {
	message = strings.TrimSpace(message)
	if message == "" {
		return "", errors.New("a commit needs a message")
	}
	add := []string{"add", "-A", "--"}
	if len(paths) == 0 {
		add = append(add, ".")
	} else {
		add = append(add, paths...)
	}
	if _, _, err := gitIn(ctx, dir, env, true, add...); err != nil {
		return "", fmt.Errorf("staging the changes of %s: %w", dir, err)
	}
	merging := mergeInProgress(ctx, dir, env)
	if _, exit, err := gitIn(ctx, dir, env, false, "diff", "--cached", "--quiet"); err == nil && !merging {
		return "", errors.New("there is nothing to commit: the workspace has no change to commit")
	} else if err != nil && exit != 1 {
		return "", fmt.Errorf("reading what is staged in %s: %w", dir, err)
	}
	if _, _, err := gitIn(ctx, dir, env, true, "commit", "--quiet", "--message="+message); err != nil {
		return "", fmt.Errorf("the commit was refused: %w", err)
	}
	return Commit(ctx, dir, "HEAD")
}

// mergeInProgress reports whether the workspace at dir has a merge begun and
// not yet committed.
func mergeInProgress(ctx context.Context, dir string, env []string) bool {
	_, _, err := gitIn(ctx, dir, env, false, "rev-parse", "--quiet", "--verify", "MERGE_HEAD")
	return err == nil
}

// Merged is what merging a ref into a workspace came to: UpToDate when the
// ref was already in the branch, the Commit the workspace is at when the
// merge made one or moved the branch, and otherwise the Conflicts it left in
// the workspace's files, for the worker to resolve and commit.
type Merged struct {
	UpToDate  bool
	Commit    string
	Conflicts []string
}

// MergeIntoWorkspace merges ref into the branch of the workspace at dir,
// letting git make the merge commit with its own message. A conflict is not
// an error: the merge is left in progress, with the conflicted files named,
// and CommitWorkspace completes it once they are resolved.
func MergeIntoWorkspace(ctx context.Context, dir string, env []string, ref string) (Merged, error) {
	if ref == "" || strings.HasPrefix(ref, "-") {
		return Merged{}, fmt.Errorf("%q does not name a branch to merge", ref)
	}
	before, err := Commit(ctx, dir, "HEAD")
	if err != nil {
		return Merged{}, fmt.Errorf("reading the commit of %s: %w", dir, err)
	}
	if _, _, mergeErr := gitIn(ctx, dir, env, true, "merge", "--quiet", "--no-edit", ref); mergeErr != nil {
		out, _, err := gitIn(ctx, dir, env, false, "diff", "--name-only", "--diff-filter=U")
		if err == nil && out != "" && mergeInProgress(ctx, dir, env) {
			return Merged{Conflicts: strings.Split(out, "\n")}, nil
		}
		return Merged{}, fmt.Errorf("merging %s into %s: %w", ref, dir, mergeErr)
	}
	after, err := Commit(ctx, dir, "HEAD")
	if err != nil {
		return Merged{}, fmt.Errorf("reading the commit of %s: %w", dir, err)
	}
	if after == before {
		return Merged{UpToDate: true}, nil
	}
	return Merged{Commit: after}, nil
}

// AbortMerge ends the merge in progress in the workspace at dir, putting its
// files back as they were before it.
func AbortMerge(ctx context.Context, dir string, env []string) error {
	if _, _, err := gitIn(ctx, dir, env, true, "merge", "--abort"); err != nil {
		return fmt.Errorf("there is no merge to abort in %s: %w", dir, err)
	}
	return nil
}

// PushWorkspaceBranch pushes branch from the workspace at dir to the same
// branch of its origin with a plain push, never forced, which origin refuses
// unless it is a fast-forward, so that no commit on origin is ever discarded.
func PushWorkspaceBranch(ctx context.Context, dir string, env []string, branch string) error {
	ref := "refs/heads/" + branch
	if _, _, err := gitIn(ctx, dir, env, true, "push", "--quiet", "origin", ref+":"+ref); err != nil {
		return fmt.Errorf("pushing %s to origin: %w", branch, err)
	}
	return nil
}
