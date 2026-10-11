// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/talvor/asmai/internal/repos"
)

// pushFixture is the repository C41 pushes to: talvor/asmai-fixture, which
// the harness user can push to with their own gh sign-in. A test replaces it
// with a local bare repository.
var pushFixture = "https://github.com/talvor/asmai-fixture.git"

// c41 is the daemon's push, and its refusal when origin holds commits AsmAI
// did not make, against the fixture repository on GitHub with the harness
// user's own git and credentials. It runs the push the daemon runs,
// repos.PushBranch, on a scratch job branch of AsmAI's own clone of the
// fixture: the first push makes the branch; then a commit is pushed to it
// from another clone, so the next push fetches it, pushes nothing and names
// it; once it is merged in, as a worker would, the push keeps it. The scratch
// branch is deleted from GitHub afterwards.
func (h *Harness) c41(ctx context.Context, f *Factory, r *Result) (err error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	scratch, err := os.MkdirTemp("", "asmai-qualify-push")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	clone := filepath.Join(scratch, "clone")
	base, err := repos.Clone(ctx, pushFixture, clone)
	if err != nil {
		return err
	}
	token, err := randomToken("c41")
	if err != nil {
		return err
	}
	branch := "asmai/qualification-" + token
	start, err := repos.OriginTip(ctx, clone, base)
	if err != nil {
		return err
	}
	if err := repos.CreateBranch(ctx, clone, branch, start); err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		if _, deleteErr := git(cleanup, clone, "push", "--quiet", "origin", "--delete", branch); deleteErr != nil {
			err = errors.Join(err, fmt.Errorf("deleting the scratch branch %s from %s: %w", branch, pushFixture, deleteErr))
		}
	}()
	commit := func(message string) (string, error) {
		tip, err := repos.Commit(ctx, clone, "refs/heads/"+branch)
		if err != nil {
			return "", err
		}
		next, err := git(ctx, clone, "-c", "user.name=AsmAI Qualification", "-c", "user.email=qualification@example.invalid", "-c", "commit.gpgsign=false", "commit-tree", tip+"^{tree}", "-p", tip, "-m", message)
		if err != nil {
			return "", err
		}
		next = strings.TrimSpace(next)
		return next, repos.MoveBranch(ctx, clone, branch, next, tip)
	}
	onOrigin := func() (string, error) {
		out, err := git(ctx, clone, "ls-remote", "origin", "refs/heads/"+branch)
		if err != nil {
			return "", err
		}
		sha, _, _ := strings.Cut(strings.TrimSpace(out), "\t")
		return sha, nil
	}

	// The first push makes the job branch on origin.
	work, err := commit("C41: the job branch's work")
	if err != nil {
		return err
	}
	outside, err := repos.PushBranch(ctx, clone, branch)
	if err != nil {
		return fmt.Errorf("the first push of %s: %w", branch, err)
	}
	if len(outside) != 0 {
		return fmt.Errorf("the first push of a new branch found commits AsmAI did not make: %v", outside)
	}
	if got, err := onOrigin(); err != nil || got != work {
		return fmt.Errorf("after the push, origin's %s is at %q (%v), want %s", branch, got, err, work)
	}
	r.observe("the push of the scratch job branch %s to %s with a plain push reached origin at %s", branch, pushFixture, work)

	// Someone else pushes a commit to the branch, and the clone moves on.
	other := filepath.Join(scratch, "other")
	if _, err := git(ctx, scratch, "clone", "--quiet", "--branch", branch, pushFixture, other); err != nil {
		return err
	}
	if _, err := git(ctx, other, "-c", "user.name=Someone Else", "-c", "user.email=someone@example.invalid", "-c", "commit.gpgsign=false", "commit", "--quiet", "--allow-empty", "-m", "C41: a commit AsmAI did not make"); err != nil {
		return err
	}
	if _, err := git(ctx, other, "push", "--quiet", "origin", branch); err != nil {
		return err
	}
	stranger, err := repos.Commit(ctx, other, "HEAD")
	if err != nil {
		return err
	}
	mine, err := commit("C41: more work")
	if err != nil {
		return err
	}
	outside, err = repos.PushBranch(ctx, clone, branch)
	if err != nil {
		return fmt.Errorf("the push over an outside commit: %w", err)
	}
	if !slices.Equal(outside, []string{stranger}) {
		return fmt.Errorf("the push over the outside commit %s found %v as commits AsmAI did not make", stranger, outside)
	}
	if got, err := onOrigin(); err != nil || got != stranger {
		return fmt.Errorf("after the refused push, origin's %s is at %q (%v), want the outside commit %s: it was pushed over", branch, got, err, stranger)
	}
	r.observe("with a commit AsmAI did not make on origin's branch, the push fetched it, pushed nothing and named it: %s", stranger)

	// It is merged in, as a worker would, and the push keeps it.
	merge := filepath.Join(scratch, "merge")
	if _, err := git(ctx, clone, "worktree", "add", "--quiet", "--detach", merge, mine); err != nil {
		return err
	}
	if _, err := git(ctx, merge, "-c", "user.name=AsmAI Qualification", "-c", "user.email=qualification@example.invalid", "-c", "commit.gpgsign=false", "merge", "--quiet", "--no-edit", "origin/"+branch); err != nil {
		return err
	}
	merged, err := repos.Commit(ctx, merge, "HEAD")
	if err != nil {
		return err
	}
	if err := repos.MoveBranch(ctx, clone, branch, merged, mine); err != nil {
		return err
	}
	if outside, err = repos.PushBranch(ctx, clone, branch); err != nil || len(outside) != 0 {
		return fmt.Errorf("the push after the merge found %v (%v)", outside, err)
	}
	if got, err := onOrigin(); err != nil || got != merged {
		return fmt.Errorf("after the merge, origin's %s is at %q (%v), want the merge %s", branch, got, err, merged)
	}
	if kept, err := repos.Contains(ctx, clone, "refs/remotes/origin/"+branch, stranger); err != nil || !kept {
		return fmt.Errorf("after the merge, origin's %s does not have the outside commit %s (%v)", branch, stranger, err)
	}
	r.observe("once a merge took it in, the push reached origin at %s, keeping the outside commit", merged)
	return nil
}
