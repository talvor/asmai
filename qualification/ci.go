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

// How long C43 and C45 wait for the fixture's CI to be green on a head, and
// how often they look, as the daemon's CI watch does at its slowest. A test
// shortens both.
var (
	ciWait = 10 * time.Minute
	ciPoll = 10 * time.Second
)

// scratchPullRequests is a scratch job branch of AsmAI's clone of the
// fixture, pushed with the daemon's push, from which a case opens pull
// requests with the harness user's own gh. close closes every pull request
// it opened that is still open and deletes the branch from GitHub.
type scratchPullRequests struct {
	gh, clone, base, branch, head, token string
	opened                               []string
}

// newScratchPullRequests makes AsmAI's clone of the fixture in scratch, a
// scratch job branch asmai/qualification-<token> with one commit on the
// default branch, and pushes it with the daemon's push.
func newScratchPullRequests(ctx context.Context, scratch, prefix string) (*scratchPullRequests, error) {
	gh, err := exec.LookPath("gh")
	if err != nil {
		return nil, fmt.Errorf("gh is not on the PATH: %s opens pull requests with it", strings.ToUpper(prefix))
	}
	s := &scratchPullRequests{gh: gh, clone: filepath.Join(scratch, "clone")}
	if s.base, err = repos.Clone(ctx, pushFixture, s.clone); err != nil {
		return nil, err
	}
	if s.token, err = randomToken(prefix); err != nil {
		return nil, err
	}
	s.branch = "asmai/qualification-" + s.token
	start, err := repos.OriginTip(ctx, s.clone, s.base)
	if err != nil {
		return nil, err
	}
	work, err := git(ctx, s.clone, "-c", "user.name=AsmAI Qualification", "-c", "user.email=qualification@example.invalid", "-c", "commit.gpgsign=false", "commit-tree", start+"^{tree}", "-p", start, "-m", strings.ToUpper(prefix)+": the job branch's work")
	if err != nil {
		return nil, err
	}
	s.head = strings.TrimSpace(work)
	if err := repos.CreateBranch(ctx, s.clone, s.branch, s.head); err != nil {
		return nil, err
	}
	if outside, err := repos.PushBranch(ctx, s.clone, s.branch); err != nil || len(outside) != 0 {
		return s, fmt.Errorf("the push of %s found %v (%v)", s.branch, outside, err)
	}
	return s, nil
}

// open opens a pull request from the scratch branch, as a draft when draft
// is set, and returns its number and URL.
func (s *scratchPullRequests) open(ctx context.Context, draft bool, title string) (int, string, error) {
	args := []string{"pr", "create", "--base", s.base, "--head", s.branch, "--title", title, "--body", "A pull request an AsmAI qualification case opens and closes. Do not merge it."}
	if draft {
		args = append(args, "--draft")
	}
	out, err := runGH(ctx, s.gh, s.clone, args...)
	if err != nil {
		return 0, "", err
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return 0, "", errors.New("gh pr create printed no URL")
	}
	url := fields[len(fields)-1]
	s.opened = append(s.opened, url)
	p, err := github.ViewPullRequest(ctx, s.clone, url)
	if err != nil {
		return 0, url, err
	}
	return p.Number, url, nil
}

// closePullRequest closes url, which it no longer closes at the end.
func (s *scratchPullRequests) closePullRequest(ctx context.Context, url string) error {
	if _, err := runGH(ctx, s.gh, s.clone, "pr", "close", url); err != nil {
		return fmt.Errorf("closing %s: %w", url, err)
	}
	for i, u := range s.opened {
		if u == url {
			s.opened = append(s.opened[:i], s.opened[i+1:]...)
			break
		}
	}
	return nil
}

func (s *scratchPullRequests) close(ctx context.Context) error {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	var err error
	for _, url := range s.opened {
		if _, closeErr := runGH(cleanup, s.gh, s.clone, "pr", "close", url); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("closing %s: %w", url, closeErr))
		}
	}
	if s.branch != "" {
		if _, deleteErr := git(cleanup, s.clone, "push", "--quiet", "origin", "--delete", s.branch); deleteErr != nil {
			err = errors.Join(err, fmt.Errorf("deleting the scratch branch %s from %s: %w", s.branch, pushFixture, deleteErr))
		}
	}
	return err
}

// untilGreen reads the checks on pull request number's head with the code
// the daemon's CI watch runs (github.ViewChecks and its verdict) until they
// are green on head, which it requires the pull request's head to be, and
// returns what it saw each time it changed. A head with no checks is never
// green, so it keeps looking until the checks appear. A red verdict, or no
// green within ciWait, fails.
func untilGreen(ctx context.Context, clone string, number int, head string) ([]string, github.Checks, error) {
	deadline := time.Now().Add(ciWait)
	var seen []string
	for {
		c, err := github.ViewChecks(ctx, clone, number)
		if err != nil {
			return seen, github.Checks{}, err
		}
		if c.HeadCommit != head {
			return seen, c, fmt.Errorf("pull request #%d's head is %s, not the pushed %s", number, c.HeadCommit, head)
		}
		verdict := c.Verdict()
		saw := verdict + ": " + describeChecks(c.Checks)
		if len(seen) == 0 || seen[len(seen)-1] != saw {
			seen = append(seen, saw)
		}
		switch verdict {
		case github.Green:
			return seen, c, nil
		case github.Red:
			return seen, c, fmt.Errorf("the fixture's CI is red on %s: %s", head, describeChecks(c.Checks))
		}
		if time.Now().After(deadline) {
			return seen, c, fmt.Errorf("the checks on %s were not green within %s: %s", head, ciWait, strings.Join(seen, "; "))
		}
		select {
		case <-ctx.Done():
			return seen, c, ctx.Err()
		case <-time.After(ciPoll):
		}
	}
}

func describeChecks(checks []github.Check) string {
	if len(checks) == 0 {
		return "no checks"
	}
	var parts []string
	for _, c := range checks {
		parts = append(parts, c.Name+" "+c.State)
	}
	return strings.Join(parts, ", ")
}

// c43 is the daemon watching CI through gh on the pushed head, on its green
// path, against the fixture repository on GitHub with the harness user's own
// gh. It pushes a scratch job branch with the daemon's push, opens a draft
// pull request from it, and reads the checks on its head with the code the
// daemon's watch runs until they are green, requiring the pull request's head
// to be the pushed commit and each check's state to be read. A head with no
// checks is never green: it keeps looking until the fixture's CI reports. The
// red path arrives later. It marks nothing ready and merges nothing; it closes
// the pull request and deletes the scratch branch from GitHub afterwards.
func (h *Harness) c43(ctx context.Context, f *Factory, r *Result) (err error) {
	ctx, cancel := context.WithTimeout(ctx, ciWait+5*time.Minute)
	defer cancel()
	scratch, err := os.MkdirTemp("", "asmai-qualify-ci")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	s, err := newScratchPullRequests(ctx, scratch, "c43")
	if s != nil {
		defer func() { err = errors.Join(err, s.close(ctx)) }()
	}
	if err != nil {
		return err
	}
	number, url, err := s.open(ctx, true, "C43: CI on the pushed head "+s.token)
	if err != nil {
		return fmt.Errorf("opening a draft pull request from %s: %w", s.branch, err)
	}
	seen, c, err := untilGreen(ctx, s.clone, number, s.head)
	if err != nil {
		return err
	}
	r.observe("the daemon's watch read the checks on %s, the pushed head of %s, as: %s", s.head, url, strings.Join(seen, "; "))
	if !c.Draft {
		return fmt.Errorf("watching CI changed %s: it is no longer a draft", url)
	}
	r.observe("every check on the pushed head was green (%s), and the pull request is still a draft: watching changes nothing", describeChecks(c.Checks))
	return nil
}

// c45 is the daemon marking the pull request ready when CI is green, against
// the fixture repository on GitHub with the harness user's own gh. It pushes
// a scratch job branch with the daemon's push and opens a draft pull request
// from it; once the daemon's watch reads every check green on the pushed
// head, the daemon's code marks it ready (github.MarkReady), and its lookup
// must find it not a draft. Then it opens one normally with "[not ready]" at
// the start of its title, as when GitHub refuses a draft, and once green the
// daemon's code must take "[not ready]" out of its title. Neither is merged:
// ready is not merged. Both are closed and the scratch branch deleted from
// GitHub afterwards.
func (h *Harness) c45(ctx context.Context, f *Factory, r *Result) (err error) {
	ctx, cancel := context.WithTimeout(ctx, 2*ciWait+5*time.Minute)
	defer cancel()
	scratch, err := os.MkdirTemp("", "asmai-qualify-ready")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	s, err := newScratchPullRequests(ctx, scratch, "c45")
	if s != nil {
		defer func() { err = errors.Join(err, s.close(ctx)) }()
	}
	if err != nil {
		return err
	}
	ready := func(draft bool, title string) (github.PullRequest, string, error) {
		number, url, err := s.open(ctx, draft, title)
		if err != nil {
			return github.PullRequest{}, url, fmt.Errorf("opening a pull request from %s: %w", s.branch, err)
		}
		seen, c, err := untilGreen(ctx, s.clone, number, s.head)
		if err != nil {
			return github.PullRequest{}, url, err
		}
		r.observe("the daemon's watch read the checks on %s, the pushed head of %s, as: %s", s.head, url, strings.Join(seen, "; "))
		if err := github.MarkReady(ctx, s.clone, c.PullRequest); err != nil {
			return github.PullRequest{}, url, fmt.Errorf("marking %s ready: %w", url, err)
		}
		p, err := github.ViewPullRequest(ctx, s.clone, url)
		if err != nil {
			return github.PullRequest{}, url, err
		}
		if p.State != "OPEN" {
			return p, url, fmt.Errorf("%s is %s after it was marked ready, not open: ready is not merged", url, p.State)
		}
		return p, url, nil
	}

	p, url, err := ready(true, "C45: a draft pull request "+s.token)
	if err != nil {
		return err
	}
	if p.Draft || p.IsNotReady() {
		return fmt.Errorf("the draft pull request %s was found %+v after the daemon marked it ready, want no longer a draft", url, p)
	}
	r.observe("once green, the daemon's code marked the draft %s ready, and its lookup found pull request #%d not a draft and open, not merged", url, p.Number)
	if err := s.closePullRequest(ctx, url); err != nil {
		return err
	}

	title := "C45: a pull request whose draft was refused " + s.token
	p, url, err = ready(false, github.NotReady+" "+title)
	if err != nil {
		return err
	}
	if p.Draft || p.IsNotReady() || p.Title != title {
		return fmt.Errorf("the pull request %s was found %+v after the daemon marked it ready, want %q at the start of its title taken out", url, p, github.NotReady)
	}
	r.observe("once green, the daemon's code took %q out of the title of %s, and its lookup found pull request #%d titled %q, open, not merged", github.NotReady, url, p.Number, p.Title)
	return nil
}
