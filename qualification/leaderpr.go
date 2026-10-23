// SPDX-License-Identifier: Apache-2.0

package main

import (
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

// c50 qualifies the pull request writes the daemon makes for a delivery
// owner's `asmai pr open` and `asmai pr update` (ADR 0018). No agent has a
// gh write rule, so the daemon runs `gh pr create` and `gh pr edit` itself, as
// the user, with the user's own gh sign-in and network and no sandbox, in the
// job's clone. The case runs the code the daemon runs for them
// (github.Repository, github.CreatePullRequest, github.EditBody and
// github.ViewPullRequest) in a clone of the fixture, on a scratch job branch
// pushed with the daemon's push: it opens a draft pull request with the body
// on gh's standard input, finds it a draft from the branch at its commit,
// updates its body and finds the body changed and the pull request still a
// draft with its title unchanged. The pull request is closed and the scratch
// branch deleted afterwards. The case does not run `asmai pr open` itself:
// that needs a job and a delivery owner's session, which the development
// tests drive through the daemon with the fake provider, and C42 covers a
// refused draft.
func (h *Harness) c50(ctx context.Context, f *Factory, r *Result) (err error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	realGH, err := exec.LookPath("gh")
	if err != nil {
		return errors.New("gh is not on the PATH: C50 opens a pull request with it")
	}
	scratch, err := os.MkdirTemp("", "asmai-qualify-leaderpr")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	clone := filepath.Join(scratch, "clone")
	base, err := repos.Clone(ctx, pushFixture, clone, "")
	if err != nil {
		return err
	}
	token, err := randomToken("c50")
	if err != nil {
		return err
	}
	branch := "asmai/qualification-" + token
	start, err := repos.OriginTip(ctx, clone, base)
	if err != nil {
		return err
	}
	work, err := gitAs(ctx, clone, harnessAuthor, "commit-tree", start+"^{tree}", "-p", start, "-m", "C50: the job branch's work")
	if err != nil {
		return err
	}
	work = strings.TrimSpace(work)
	if err := repos.CreateBranch(ctx, clone, branch, work); err != nil {
		return err
	}
	var opened string
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		if opened != "" {
			if _, closeErr := runGH(cleanup, realGH, clone, "pr", "close", opened); closeErr != nil {
				err = errors.Join(err, fmt.Errorf("closing %s: %w", opened, closeErr))
			}
		}
		if _, deleteErr := git(cleanup, clone, "push", "--quiet", "origin", "--delete", branch); deleteErr != nil {
			err = errors.Join(err, fmt.Errorf("deleting the scratch branch %s from %s: %w", branch, pushFixture, deleteErr))
		}
	}()
	if outside, err := repos.PushBranch(ctx, clone, branch); err != nil || len(outside) != 0 {
		return fmt.Errorf("the push of %s found %v (%v)", branch, outside, err)
	}

	repository, err := github.Repository(ctx, clone)
	if err != nil {
		return fmt.Errorf("looking up the repository of the clone with gh, as the daemon does before it opens a pull request: %w", err)
	}
	title := "C50-" + token
	url, err := github.CreatePullRequest(ctx, clone, github.Create{
		Repository: repository, Base: base, Head: branch, Title: title, Draft: true,
		Body: "A pull request AsmAI's qualification case C50 opens and closes. Do not merge it.\n",
	})
	if err != nil {
		return fmt.Errorf("opening a draft pull request from %s as the daemon does for `asmai pr open`: %w", branch, err)
	}
	opened = url
	p, err := github.ViewPullRequest(ctx, clone, url)
	if err != nil {
		return err
	}
	if !p.Draft || p.IsNotReady() || p.HeadBranch != branch || p.HeadCommit != work || p.Title != title {
		return fmt.Errorf("the pull request %s was found %+v, want a draft titled %q from %s at %s", url, p, title, branch, work)
	}
	r.observe("the daemon's `gh pr create --draft`, run as the user in a clone of %s with the body on its standard input, opened %s; its lookup found pull request #%d a draft from %s at %s", repository, url, p.Number, branch, p.HeadCommit)

	marker := "C50 updated the body " + token
	if err := github.EditBody(ctx, clone, repository, p.Number, marker+"\n"); err != nil {
		return fmt.Errorf("updating the body of %s as the daemon does for `asmai pr update`: %w", url, err)
	}
	out, err := runGH(ctx, realGH, clone, "pr", "view", url, "--json", "body", "--jq", ".body")
	if err != nil {
		return err
	}
	if !strings.Contains(out, marker) {
		return fmt.Errorf("after the daemon's body update the body of %s is %q, want it to hold %q", url, strings.TrimSpace(out), marker)
	}
	if p, err = github.ViewPullRequest(ctx, clone, url); err != nil {
		return err
	}
	if !p.Draft || p.Title != title {
		return fmt.Errorf("after the body update the pull request %s was found %+v, want it still a draft titled %q: the update changes the body and nothing else", url, p, title)
	}
	r.observe("the daemon's `gh pr edit --body-file -` then updated the body of %s, leaving it a draft with its title", url)
	return nil
}
