// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/talvor/asmai/internal/github"
	"github.com/talvor/asmai/internal/repos"
)

// draftRefused is what gh prints when GitHub refuses a draft pull request,
// which C42's gh wrapper prints in its place.
const draftRefused = "pull request create failed: GraphQL: Draft pull requests are not supported in this repository. (createPullRequest)"

// c42 is a draft pull request, and the "[not ready]" fallback when GitHub
// refuses a draft, on the fixture repository on GitHub with the harness
// user's own gh. It pushes a scratch job branch of AsmAI's clone of the
// fixture with the daemon's push, opens a draft pull request from it with gh,
// as Engineering's leader does, and looks it up with the code the daemon runs
// for the leader's `asmai effect pull-request` (github.ViewPullRequest), which
// must find it a draft. The fixture accepts drafts, so the refusal is
// simulated at the gh boundary: a gh that refuses `gh pr create --draft`, as
// GitHub does for a repository without draft pull requests, and otherwise runs
// the real gh. Through it, the draft is refused, and the pull request opened
// normally with "[not ready]" at the start of its title must be found not a
// draft and not ready. Both pull requests are closed and the scratch branch
// deleted from GitHub afterwards.
func (h *Harness) c42(ctx context.Context, f *Factory, r *Result) (err error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	realGH, err := exec.LookPath("gh")
	if err != nil {
		return errors.New("gh is not on the PATH: C42 opens pull requests with it")
	}
	scratch, err := os.MkdirTemp("", "asmai-qualify-pr")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	clone := filepath.Join(scratch, "clone")
	base, err := repos.Clone(ctx, pushFixture, clone)
	if err != nil {
		return err
	}
	token, err := randomToken("c42")
	if err != nil {
		return err
	}
	branch := "asmai/qualification-" + token
	start, err := repos.OriginTip(ctx, clone, base)
	if err != nil {
		return err
	}
	work, err := git(ctx, clone, "-c", "user.name=AsmAI Qualification", "-c", "user.email=qualification@example.invalid", "-c", "commit.gpgsign=false", "commit-tree", start+"^{tree}", "-p", start, "-m", "C42: the job branch's work")
	if err != nil {
		return err
	}
	work = strings.TrimSpace(work)
	if err := repos.CreateBranch(ctx, clone, branch, work); err != nil {
		return err
	}
	var opened []string
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		for _, url := range opened {
			if _, closeErr := runGH(cleanup, realGH, clone, "pr", "close", url); closeErr != nil {
				err = errors.Join(err, fmt.Errorf("closing %s: %w", url, closeErr))
			}
		}
		if _, deleteErr := git(cleanup, clone, "push", "--quiet", "origin", "--delete", branch); deleteErr != nil {
			err = errors.Join(err, fmt.Errorf("deleting the scratch branch %s from %s: %w", branch, pushFixture, deleteErr))
		}
	}()
	if outside, err := repos.PushBranch(ctx, clone, branch); err != nil || len(outside) != 0 {
		return fmt.Errorf("the push of %s found %v (%v)", branch, outside, err)
	}
	create := func(gh string, draft bool, title string) (string, error) {
		args := []string{"pr", "create", "--base", base, "--head", branch, "--title", title, "--body", "A pull request AsmAI's qualification case C42 opens and closes. Do not merge it."}
		if draft {
			args = append(args, "--draft")
		}
		out, err := runGH(ctx, gh, clone, args...)
		if err != nil {
			return "", err
		}
		lines := strings.Fields(out)
		if len(lines) == 0 {
			return "", fmt.Errorf("gh pr create printed no URL")
		}
		url := lines[len(lines)-1]
		opened = append(opened, url)
		return url, nil
	}

	// The fixture accepts a draft, and the daemon's lookup finds it one.
	url, err := create(realGH, true, "C42: a draft pull request "+token)
	if err != nil {
		return fmt.Errorf("opening a draft pull request from %s: %w", branch, err)
	}
	p, err := github.ViewPullRequest(ctx, clone, url)
	if err != nil {
		return err
	}
	if !p.Draft || p.IsNotReady() || p.HeadBranch != branch || p.HeadCommit != work {
		return fmt.Errorf("the draft pull request %s was found %+v, want a draft from %s at %s", url, p, branch, work)
	}
	r.observe("gh opened %s from %s as a draft, and the daemon's lookup found pull request #%d a draft at %s", url, branch, p.Number, p.HeadCommit)
	if _, err := runGH(ctx, realGH, clone, "pr", "close", url); err != nil {
		return fmt.Errorf("closing %s: %w", url, err)
	}
	opened = opened[:0]

	// GitHub's refusal of a draft is simulated at the gh boundary.
	wrapper := filepath.Join(scratch, "gh")
	script := fmt.Sprintf("#!/bin/sh\nif [ \"$1 $2\" = \"pr create\" ]; then\n\tfor arg in \"$@\"; do\n\t\tif [ \"$arg\" = --draft ]; then\n\t\t\techo %q >&2\n\t\t\texit 1\n\t\tfi\n\tdone\nfi\nexec %q \"$@\"\n", draftRefused, realGH)
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		return err
	}
	if _, err := create(wrapper, true, "C42: a refused draft "+token); err == nil || !strings.Contains(err.Error(), "Draft pull requests are not supported") {
		return fmt.Errorf("the simulated refusal of a draft did not refuse it: %v", err)
	}
	url, err = create(wrapper, false, github.NotReady+" C42: a pull request whose draft was refused "+token)
	if err != nil {
		return fmt.Errorf("opening the pull request normally after the refused draft: %w", err)
	}
	if p, err = github.ViewPullRequest(ctx, clone, url); err != nil {
		return err
	}
	if p.Draft || !p.IsNotReady() || p.HeadBranch != branch {
		return fmt.Errorf("the pull request %s opened after the refused draft was found %+v, want not a draft, with %q at the start of its title", url, p, github.NotReady)
	}
	r.observe("with the draft refused at the gh boundary (simulated: the fixture accepts drafts), gh opened %s normally, and the daemon's lookup found pull request #%d not a draft, titled %q", url, p.Number, p.Title)
	return nil
}

// runGH runs gh, the executable at path, in dir, never prompting, and returns
// what it printed on stdout.
func runGH(ctx context.Context, path, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1", "NO_COLOR=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("gh %s: %w: %s", strings.Join(args[:2], " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
