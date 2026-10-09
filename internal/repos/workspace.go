// SPDX-License-Identifier: Apache-2.0

package repos

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Fetch brings AsmAI's clone up to date with its origin, so that the origin's
// branches are read as they are now.
func Fetch(ctx context.Context, clone string) error {
	if _, err := git(ctx, clone, "fetch", "--quiet", "--prune", "origin"); err != nil {
		return fmt.Errorf("fetching origin into %s: %w", clone, err)
	}
	return nil
}

// OriginTip returns the commit AsmAI's clone has for branch of its origin, as
// the last Fetch left it.
func OriginTip(ctx context.Context, clone, branch string) (string, error) {
	commit, err := Commit(ctx, clone, "refs/remotes/origin/"+branch)
	if err != nil {
		return "", fmt.Errorf("origin has no branch %s in %s: %w", branch, clone, err)
	}
	return commit, nil
}

// Commit returns the commit rev names in the repository at dir.
func Commit(ctx context.Context, dir, rev string) (string, error) {
	return git(ctx, dir, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
}

// CreateBranch makes the branch name in the repository at clone, at commit.
// It refuses a name that is taken.
func CreateBranch(ctx context.Context, clone, name, commit string) error {
	if _, err := git(ctx, clone, "branch", "--no-track", name, commit); err != nil {
		return fmt.Errorf("making branch %s: %w", name, err)
	}
	return nil
}

// AddWorkspace makes dir a checkout of AsmAI's clone on a new branch, at
// commit. The checkout shares the clone's objects and its configuration, so
// that the branch can be pushed to the clone's origin. Whatever an earlier
// attempt left at dir or under the branch's name, which no record points at,
// is replaced.
func AddWorkspace(ctx context.Context, clone, dir, branch, commit string) error {
	if err := clearWorktree(ctx, clone, dir); err != nil {
		return err
	}
	if _, err := git(ctx, clone, "worktree", "add", "--quiet", "--no-track", "-B", branch, dir, commit); err != nil {
		return fmt.Errorf("making the workspace %s: %w", dir, err)
	}
	return nil
}

// RemoveWorkspace removes the checkout at dir, which AddWorkspace made, and
// the branch it was made on, from the clone. Its branch on the origin stays.
func RemoveWorkspace(ctx context.Context, clone, dir, branch string) error {
	err := clearWorktree(ctx, clone, dir)
	if _, deleteErr := git(ctx, clone, "branch", "-D", branch); deleteErr != nil && err == nil {
		err = fmt.Errorf("deleting branch %s: %w", branch, deleteErr)
	}
	return err
}

// clearWorktree removes the checkout at dir, if there is one, and the clone's
// record of it.
func clearWorktree(ctx context.Context, clone, dir string) error {
	if _, err := os.Lstat(dir); err == nil {
		makeWritable(dir)
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("clearing %s: %w", dir, err)
		}
	}
	if _, err := git(ctx, clone, "worktree", "prune"); err != nil {
		return fmt.Errorf("pruning the clone's checkouts: %w", err)
	}
	return nil
}

// EnsureView makes dir a read-only checkout of AsmAI's clone at commit,
// detached, and returns once it is: it makes the view if it is missing and
// moves it to commit if it is elsewhere. The view's files are made read-only,
// so that nobody edits them by mistake; it is a guard, not a boundary.
func EnsureView(ctx context.Context, clone, dir, commit string) error {
	if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
		head, err := Commit(ctx, dir, "HEAD")
		if err == nil && head == commit {
			return makeReadOnly(dir)
		}
		if err == nil {
			makeWritable(dir)
			if _, err := git(ctx, dir, "checkout", "--quiet", "--force", "--detach", commit); err != nil {
				return fmt.Errorf("moving the view %s to %s: %w", dir, commit, err)
			}
			return makeReadOnly(dir)
		}
	}
	if err := clearWorktree(ctx, clone, dir); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return err
	}
	if _, err := git(ctx, clone, "worktree", "add", "--quiet", "--detach", dir, commit); err != nil {
		return fmt.Errorf("making the view %s: %w", dir, err)
	}
	return makeReadOnly(dir)
}

// makeReadOnly takes the write permission from every file under dir but git's
// own.
func makeReadOnly(dir string) error {
	return filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Name() == ".git" {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() && !entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		return os.Chmod(path, info.Mode().Perm()&^0o222)
	})
}

// makeWritable gives the owner write permission on every file and directory
// under dir, so that a read-only view can be removed.
func makeWritable(dir string) {
	filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && !entry.Type().IsRegular() && !entry.IsDir() {
			return nil
		}
		if info, statErr := os.Lstat(path); statErr == nil && (info.Mode().IsRegular() || info.IsDir()) {
			os.Chmod(path, info.Mode().Perm()|0o200)
		}
		return nil
	})
}

// Uncommitted returns what the checkout at dir has changed and not committed,
// one status line each, untracked files included.
func Uncommitted(ctx context.Context, dir string) ([]string, error) {
	out, err := git(ctx, dir, "status", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("reading the status of %s: %w", dir, err)
	}
	if out == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

// Contains reports whether commit is in the history of rev in the repository
// at dir: whether rev has taken commit in.
func Contains(ctx context.Context, dir, rev, commit string) (bool, error) {
	_, exit, err := gitStatus(ctx, dir, "merge-base", "--is-ancestor", commit, rev)
	switch {
	case err == nil:
		return true, nil
	case exit == 1:
		return false, nil
	}
	return false, fmt.Errorf("comparing %s with %s in %s: %w", commit, rev, dir, err)
}
