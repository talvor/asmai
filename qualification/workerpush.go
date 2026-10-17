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

	"github.com/talvor/asmai/internal/providers"
	"github.com/talvor/asmai/internal/repos"
	"github.com/talvor/asmai/internal/roles"
)

// c51 qualifies that an Engineering worker can commit and push its assignment
// branch in its real session and sandbox (dontAsk, with the settings and
// instructions the code under test gives a worker), the stall of #151. The
// workspace is a linked checkout of a clone of the fixture on GitHub, on a
// scratch assignment branch, as a real worker's is, with a new file in it.
// The session runs `git add`, `git commit` and `git push origin <branch>`,
// each as its own Bash call, as its instructions say. None may be denied,
// fail, be stopped by the sandbox or be retried outside it
// (dangerouslyDisableSandbox), the session's hooks must report no
// PermissionRequest, and origin's branch must be at the worker's commit. The
// scratch branch is deleted afterwards. The case does not run `asmai result`
// or `asmai blocked`, which need a job: the development tests drive those
// through the daemon.
func (h *Harness) c51(ctx context.Context, f *Factory, r *Result) (err error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	if err := checkGuardDependencies(); err != nil {
		return err
	}
	provider, err := h.ready(ctx, f)
	if err != nil {
		return err
	}
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

	instructions, err := roles.WorkerInstructions(roles.Engineering)
	if err != nil {
		return err
	}
	hooks := filepath.Join(scratch, "hooks.jsonl")
	perms := providers.Permissions{Unattended: true, Editable: []string{workspace, tmp}}
	args := providers.ClaudeCodeArgs(hookLog(hooks), guardModel, instructions, "", providers.WriteGuard{Writable: []string{tmp}}, perms)
	commands := []string{
		"git add c51.txt",
		"git commit -q -m 'C51: a worker commits'",
		"git push origin " + branch,
	}
	var list strings.Builder
	for i, c := range commands {
		fmt.Fprintf(&list, "%d. `%s`\n", i+1, c)
	}
	turn, err := h.Provider.Ask(ctx, f, provider, AskSpec{
		Dir:  workspace,
		Args: args,
		Prompt: "You are being qualified, as an Engineering worker, in your own session. Run each of these commands as its own single Bash tool call, in order, exactly as written, never with dangerouslyDisableSandbox. " +
			"If one is refused or fails, do not retry it, change it or work around it: go on to the next. When you have tried them all, reply with the single word done.\n" + list.String(),
	})
	if err != nil {
		return err
	}
	for _, command := range commands {
		if err := workerCommand(turn, command); err != nil {
			return err
		}
	}
	want, err := git(ctx, workspace, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if subject, _ := git(ctx, workspace, "log", "-1", "--format=%s"); strings.TrimSpace(subject) != "C51: a worker commits" {
		return fmt.Errorf("the worker's commit was not made in the workspace (HEAD is %q)", strings.TrimSpace(subject))
	}
	out, err := git(ctx, clone, "ls-remote", "--heads", "origin", branch)
	if err != nil {
		return err
	}
	if got, _, _ := strings.Cut(strings.TrimSpace(out), "\t"); got != strings.TrimSpace(want) {
		return fmt.Errorf("after `git push origin %s` origin's branch is at %q, want the worker's commit %s", branch, got, strings.TrimSpace(want))
	}
	r.observe("an Engineering worker, in its real session in dontAsk mode and its sandbox, ran `git add`, `git commit` and `git push origin %s` in a linked checkout of a clone of %s, each as its own Bash call and none denied, stopped by the sandbox or retried outside it, and origin's branch is at its commit %s", branch, pushFixture, strings.TrimSpace(want))

	asked, err := permissionRequests(hooks)
	if err != nil {
		return err
	}
	if len(asked) > 0 {
		return fmt.Errorf("the worker's session raised a native permission prompt, which nobody can answer in its terminal, for %s", strings.Join(asked, ", "))
	}
	if !exists(hooks) {
		return errors.New("inconclusive: the session's hooks reported nothing, so the harness cannot show that it raised no native prompt")
	}
	r.observe("the session's hooks reported no PermissionRequest")
	return nil
}

// workerCommand returns the command of turn that ran exactly as asked, or the
// reason the case cannot rely on it: never run, retried outside the sandbox,
// denied or failed.
func workerCommand(turn Turn, command string) error {
	tried := turn.Tried(command)
	if len(tried) == 0 {
		return fmt.Errorf("inconclusive: the worker never ran `%s` as one Bash call, so the harness cannot show how its session treats it", command)
	}
	for _, c := range tried {
		switch {
		case c.Unsandboxed:
			return fmt.Errorf("`%s` was retried outside the sandbox (dangerouslyDisableSandbox), so the sandbox stopped it: %s", command, strings.TrimSpace(c.Output))
		case c.Denied || c.Failed:
			return fmt.Errorf("`%s` was denied or failed in the worker's session: %s", command, strings.TrimSpace(c.Output))
		}
	}
	return nil
}
