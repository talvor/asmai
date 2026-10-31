// SPDX-License-Identifier: Apache-2.0

package repos

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

var ErrNoOriginBranch = errors.New("origin has no branch")

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
	commit, exit, err := gitStatus(ctx, clone, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+branch+"^{commit}")
	if err != nil {
		if exit == 1 {
			return "", fmt.Errorf("%w %s in %s: %v", ErrNoOriginBranch, branch, clone, err)
		}
		return "", fmt.Errorf("reading origin's branch %s in %s: %w", branch, clone, err)
	}
	return commit, nil
}

// OriginHasBranch asks the origin of the repository at clone, over the
// network, whether it has the branch name.
func OriginHasBranch(ctx context.Context, clone, name string) (bool, error) {
	exact := []string{name}
	for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
		exact = append(exact, parent)
	}
	conflict, err := originBranchConflict(ctx, clone, exact, []string{name + "/"})
	if err != nil {
		return false, fmt.Errorf("asking origin for branch %s in %s: %w", name, clone, err)
	}
	return conflict != "", nil
}

func OriginJobBranchConflict(ctx context.Context, clone, prefix string, number int64) (string, error) {
	base := "asmai/" + prefix + "/job-" + strconv.FormatInt(number, 10)
	conflict, err := originBranchConflict(ctx, clone, []string{"asmai", "asmai/" + prefix, base}, []string{base + "-", base + "/"})
	if err != nil {
		return "", fmt.Errorf("asking origin for job %d branches in %s: %w", number, clone, err)
	}
	return conflict, nil
}

func originBranchConflict(ctx context.Context, clone string, exact, prefixes []string) (string, error) {
	args := []string{"ls-remote", "--heads", "origin"}
	for _, name := range exact {
		args = append(args, "refs/heads/"+name)
	}
	for _, prefix := range prefixes {
		args = append(args, "refs/heads/"+prefix+"*")
	}
	out, err := git(ctx, clone, args...)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		_, ref, ok := strings.Cut(line, "\t")
		if !ok || !strings.HasPrefix(ref, "refs/heads/") {
			continue
		}
		branch := strings.TrimPrefix(ref, "refs/heads/")
		for _, name := range exact {
			if branch == name {
				return branch, nil
			}
		}
		for _, prefix := range prefixes {
			if strings.HasPrefix(branch, prefix) {
				return branch, nil
			}
		}
	}
	return "", nil
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

// ResetBranch makes the branch name in the repository at clone point at
// commit, making it if it is missing and moving it if it is elsewhere, so that
// the branch is a copy of that commit whatever it held before. No checkout of
// the clone has it checked out.
func ResetBranch(ctx context.Context, clone, name, commit string) error {
	if _, err := git(ctx, clone, "branch", "--force", "--no-track", name, commit); err != nil {
		return fmt.Errorf("resetting branch %s to %s: %w", name, commit, err)
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

// AddReadOnlyWorkspace makes dir a clean checkout of AsmAI's clone fixed at
// commit, detached: it has no branch, so there is nothing in it to push. Its
// files are writable, as running a repository's checks writes build output,
// but nothing the checks write is part of the commit. Whatever an earlier
// attempt left at dir, which no record points at, is replaced.
func AddReadOnlyWorkspace(ctx context.Context, clone, dir, commit string) error {
	if err := clearWorktree(ctx, clone, dir); err != nil {
		return err
	}
	if _, err := git(ctx, clone, "worktree", "add", "--quiet", "--detach", dir, commit); err != nil {
		return fmt.Errorf("making the read-only workspace %s: %w", dir, err)
	}
	return nil
}

// RemoveReadOnlyWorkspace removes the checkout at dir, which
// AddReadOnlyWorkspace made, and the clone's record of it.
func RemoveReadOnlyWorkspace(ctx context.Context, clone, dir string) error {
	return clearWorktree(ctx, clone, dir)
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

// RemoveTree removes the tree at dir, which may hold read-only files and
// directories, as a copy of a view does: it gives the owner write permission
// on each of them first, since a file cannot be removed from a directory that
// is not writable. It does not follow symbolic links, so it changes nothing
// outside the tree, and it removes nothing if dir is missing.
func RemoveTree(dir string) error {
	makeWritable(dir)
	return os.RemoveAll(dir)
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

// MoveBranch moves the branch name of the clone from commit from to commit
// to, and only if the branch is still at from, so that nothing else's move is
// overwritten. The branch is not checked out in any checkout of the clone.
func MoveBranch(ctx context.Context, clone, name, to, from string) error {
	if _, err := git(ctx, clone, "update-ref", "-m", "asmai: move "+name, "refs/heads/"+name, to, from); err != nil {
		return fmt.Errorf("moving branch %s from %s to %s: %w", name, from, to, err)
	}
	return nil
}

// PushBranch pushes the clone's branch name to the same branch of its
// origin, unless origin's branch has commits AsmAI did not make, since only
// the daemon pushes a job branch. It fetches origin first, so that origin's
// branch is read as it is now. With commits on origin the clone's branch does
// not have, it pushes nothing and returns them, newest first, for a worker to
// merge in. Otherwise it pushes with a plain push, never forced, which origin
// refuses unless it is a fast-forward, so that no commit on origin is ever
// discarded.
func PushBranch(ctx context.Context, clone, name string) (outside []string, err error) {
	if err := Fetch(ctx, clone); err != nil {
		return nil, err
	}
	if outside, err = outsideCommits(ctx, clone, name); err != nil || len(outside) > 0 {
		return outside, err
	}
	ref := "refs/heads/" + name
	if _, err := git(ctx, clone, "push", "--quiet", "origin", ref+":"+ref); err != nil {
		return nil, fmt.Errorf("pushing %s to origin: %w", name, err)
	}
	return nil, nil
}

// outsideCommits returns the commits origin's branch name has that the
// clone's branch name does not, newest first, as the last Fetch left them. It
// returns none when origin has no such branch.
func outsideCommits(ctx context.Context, clone, name string) ([]string, error) {
	remote := "refs/remotes/origin/" + name
	found, err := git(ctx, clone, "for-each-ref", "--format=%(objectname)", remote)
	if err != nil {
		return nil, fmt.Errorf("reading origin's branch %s in %s: %w", name, clone, err)
	}
	if found == "" {
		return nil, nil
	}
	out, err := git(ctx, clone, "rev-list", "refs/heads/"+name+".."+remote)
	if err != nil {
		return nil, fmt.Errorf("comparing origin's branch %s with the clone's: %w", name, err)
	}
	if out == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

// RemoveView removes the read-only view at dir, which EnsureView made, and
// the clone's record of it.
func RemoveView(ctx context.Context, clone, dir string) error {
	return clearWorktree(ctx, clone, dir)
}

// DeleteBranch deletes the branch name from the clone, if it has it. The
// branch on the origin stays.
func DeleteBranch(ctx context.Context, clone, name string) (deleted bool, err error) {
	found, err := git(ctx, clone, "for-each-ref", "--format=%(refname)", "refs/heads/"+name)
	if err != nil || found == "" {
		return false, err
	}
	if _, err := git(ctx, clone, "branch", "-D", name); err != nil {
		return false, fmt.Errorf("deleting branch %s: %w", name, err)
	}
	return true, nil
}

// Step is one commit on the path from an earlier commit to a branch's tip,
// with its parents, the first of which is the commit before it on the path.
type Step struct {
	Commit  string
	Parents []string
}

// Steps returns the commits on the first-parent path from since, which tip
// has in its history, to tip, oldest first, each with its parents. It follows
// only first parents, so a commit a merge brings in is not a step of its own.
func Steps(ctx context.Context, dir, since, tip string) ([]Step, error) {
	out, err := git(ctx, dir, "rev-list", "--first-parent", "--reverse", "--parents", since+".."+tip)
	if err != nil {
		return nil, fmt.Errorf("listing the commits from %s to %s in %s: %w", since, tip, dir, err)
	}
	var steps []Step
	for _, line := range strings.Split(out, "\n") {
		if fields := strings.Fields(line); len(fields) > 0 {
			steps = append(steps, Step{Commit: fields[0], Parents: fields[1:]})
		}
	}
	return steps, nil
}

// CleanMerge reports whether step is a merge of two commits that git merges
// without a conflict, and whose tree is exactly what git's merge makes: no
// conflict was resolved by hand and nothing else was changed in it. A git that
// cannot do the merge without a checkout answers false, so that the merge is
// reviewed.
func CleanMerge(ctx context.Context, dir string, step Step) (bool, error) {
	if len(step.Parents) != 2 {
		return false, nil
	}
	out, exit, err := gitStatus(ctx, dir, "merge-tree", "--write-tree", "--no-messages", step.Parents[0], step.Parents[1])
	if err != nil {
		if exit > 0 {
			return false, nil
		}
		return false, fmt.Errorf("merging %s and %s in %s: %w", step.Parents[0], step.Parents[1], dir, err)
	}
	tree, _, _ := strings.Cut(out, "\n")
	have, err := git(ctx, dir, "rev-parse", "--verify", "--quiet", step.Commit+"^{tree}")
	if err != nil {
		return false, fmt.Errorf("reading the tree of %s in %s: %w", step.Commit, dir, err)
	}
	return strings.TrimSpace(tree) == have, nil
}
