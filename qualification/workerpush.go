// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/providers"
	"github.com/talvor/asmai/internal/repos"
)

// c51 qualifies the git writes the daemon makes in a worker's workspace for
// its `asmai commit`, `asmai merge` and `asmai push` (ADR 0018; #151 and #182).
// No agent runs a git write, with the managed policy or without it, so the
// daemon runs git itself, as the user, with the worker's own environment and
// no sandbox. The workspace is a linked checkout of a clone of the fixture on
// GitHub, on a scratch assignment branch, as a real worker's is, with a new
// file in it. The case runs the code the daemon runs, in the environment the
// daemon gives the worker (providers.SessionEnv with the session credential,
// slot and temporary directory): repos.CommitWorkspace takes the new file in a
// commit, which a second call with nothing changed refuses; repos.MergeIntoWorkspace
// finds origin's base already in the branch; and repos.PushWorkspaceBranch
// pushes the branch plainly, so that origin's branch is at the commit. The
// scratch branch is deleted afterwards. The case does not run `asmai commit`
// itself, which needs a job and a worker's session: the development tests
// drive those through the daemon with the fake provider, and C37 qualifies
// that a worker's own session is denied every git write.
func (h *Harness) c51(ctx context.Context, f *Factory, r *Result) (err error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	scratch, err := os.MkdirTemp("", "asmai-qualify-workerpush")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	if scratch, err = filepath.EvalSymlinks(scratch); err != nil {
		return err
	}
	clone := filepath.Join(scratch, "clone")
	workspace := filepath.Join(scratch, "workspace")
	tmp := filepath.Join(scratch, "tmp")
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	base, err := repos.Clone(ctx, pushFixture, clone, "")
	if err != nil {
		return err
	}
	token, err := randomToken("c51")
	if err != nil {
		return err
	}
	branch := "asmai/qualification-" + token
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		if out, _ := git(cleanup, clone, "ls-remote", "--heads", "origin", branch); strings.TrimSpace(out) == "" {
			return
		}
		if _, deleteErr := git(cleanup, clone, "push", "--quiet", "origin", "--delete", branch); deleteErr != nil {
			err = errors.Join(err, fmt.Errorf("deleting the scratch branch %s from %s: %w", branch, pushFixture, deleteErr))
		}
	}()
	// The worker commits as the user does, with the identity git already has.
	for _, args := range [][]string{
		{"config", "--local", "user.name", harnessAuthor.name},
		{"config", "--local", "user.email", harnessAuthor.email},
		{"config", "--local", "commit.gpgsign", "false"},
		{"worktree", "add", "-q", "-b", branch, workspace, "origin/" + base},
	} {
		if _, err := git(ctx, clone, args...); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(workspace, "c51.txt"), []byte("C51 "+token+"\n"), 0o644); err != nil {
		return err
	}

	env := providers.SessionEnv(f.env, filepath.Dir(f.asmai), map[string]string{
		daemon.SessionCredential: "qualification-" + token,
		"TMPDIR":                 tmp,
	})
	commit, err := repos.CommitWorkspace(ctx, workspace, env, "C51: a worker's commit, made by the daemon", nil)
	if err != nil {
		return fmt.Errorf("the daemon's commit of the worker's new file: %w", err)
	}
	if head, _ := git(ctx, workspace, "rev-parse", "HEAD"); strings.TrimSpace(head) != commit {
		return fmt.Errorf("the commit the daemon reported, %s, is not the workspace's HEAD %q", commit, strings.TrimSpace(head))
	}
	if tracked, _ := git(ctx, workspace, "show", "--name-only", "--format=", "HEAD"); strings.TrimSpace(tracked) != "c51.txt" {
		return fmt.Errorf("the daemon's commit holds %q, want the worker's c51.txt alone", strings.TrimSpace(tracked))
	}
	if _, err := repos.CommitWorkspace(ctx, workspace, env, "C51: nothing changed", nil); err == nil || !strings.Contains(err.Error(), "nothing to commit") {
		return fmt.Errorf("a second daemon commit with nothing changed returned %v, want it to say there is nothing to commit", err)
	}
	r.observe("the daemon's commit, run as the user with the worker's environment and no sandbox, took the worker's new file in %s in a linked checkout of a clone of %s, and refused a second commit with nothing changed", commit, pushFixture)

	merged, err := repos.MergeIntoWorkspace(ctx, workspace, env, "origin/"+base)
	if err != nil {
		return fmt.Errorf("the daemon's merge of origin/%s into the worker's branch: %w", base, err)
	}
	if !merged.UpToDate && merged.Commit == "" {
		return fmt.Errorf("the daemon's merge of origin/%s made no commit and was not up to date: %+v", base, merged)
	}
	if merged.UpToDate {
		r.observe("the daemon's merge of origin/%s found it already in the worker's branch", base)
	} else {
		r.observe("the daemon's merge of origin/%s into the worker's branch made %s", base, merged.Commit)
		if commit, err = repos.Commit(ctx, workspace, "HEAD"); err != nil {
			return err
		}
	}

	if err := repos.PushWorkspaceBranch(ctx, workspace, env, branch); err != nil {
		return fmt.Errorf("the daemon's push of the worker's branch: %w", err)
	}
	out, err := git(ctx, clone, "ls-remote", "--heads", "origin", branch)
	if err != nil {
		return err
	}
	if got, _, _ := strings.Cut(strings.TrimSpace(out), "\t"); got != commit {
		return fmt.Errorf("after the daemon's push of %s origin's branch is at %q, want the worker's commit %s", branch, got, commit)
	}
	r.observe("the daemon's plain push of %s reached origin at %s", branch, commit)
	return nil
}
