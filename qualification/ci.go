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
	"slices"
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
	gh, clone, base, start, branch, head, token string
	opened                                      []string
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
	if s.start, err = repos.OriginTip(ctx, s.clone, s.base); err != nil {
		return nil, err
	}
	work, err := gitAs(ctx, s.clone, harnessAuthor, "commit-tree", s.start+"^{tree}", "-p", s.start, "-m", strings.ToUpper(prefix)+": the job branch's work")
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

// pushNewer commits to the scratch branch, on its head, the default branch's
// tree with the fixture's .ci-mode set to mode, or as the default branch has
// it when mode is empty, and pushes it with the daemon's push: a newer head
// of every pull request from the branch.
func (s *scratchPullRequests) pushNewer(ctx context.Context, scratch, mode, message string) error {
	tree := s.start + "^{tree}"
	if mode != "" {
		file := filepath.Join(scratch, "ci-mode")
		if err := os.WriteFile(file, []byte(mode+"\n"), 0o600); err != nil {
			return err
		}
		blob, err := git(ctx, s.clone, "hash-object", "-w", file)
		if err != nil {
			return err
		}
		index := []string{"GIT_INDEX_FILE=" + filepath.Join(scratch, "ci-mode.index")}
		if _, err := gitWith(ctx, s.clone, index, "read-tree", s.start); err != nil {
			return err
		}
		if _, err := gitWith(ctx, s.clone, index, "update-index", "--add", "--cacheinfo", "100644,"+strings.TrimSpace(blob)+",.ci-mode"); err != nil {
			return err
		}
		written, err := gitWith(ctx, s.clone, index, "write-tree")
		if err != nil {
			return err
		}
		tree = strings.TrimSpace(written)
	}
	next, err := gitAs(ctx, s.clone, harnessAuthor, "commit-tree", tree, "-p", s.head, "-m", message)
	if err != nil {
		return err
	}
	next = strings.TrimSpace(next)
	if err := repos.MoveBranch(ctx, s.clone, s.branch, next, s.head); err != nil {
		return err
	}
	if outside, err := repos.PushBranch(ctx, s.clone, s.branch); err != nil || len(outside) != 0 {
		return fmt.Errorf("the push of %s found %v (%v)", s.branch, outside, err)
	}
	s.head = next
	return nil
}

// gitWith runs git in dir with env added to its environment.
func gitWith(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
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
// are green on head, which it requires the pull request's head to be once
// GitHub moved it from previous, the head pushed before head, if any, and
// returns what it saw each time it changed. A head with no checks is never
// green, so it keeps looking until the checks appear. A red verdict, or no
// green within ciWait, fails.
func untilGreen(ctx context.Context, clone string, number int, previous, head string) ([]string, github.Checks, error) {
	return watchUntil(ctx, clone, number, previous, head, "green", func(c github.Checks) (bool, error) {
		switch c.Verdict() {
		case github.Green:
			return true, nil
		case github.Red:
			return false, fmt.Errorf("the fixture's CI is red on %s: %s", head, describeChecks(c.Checks))
		}
		return false, nil
	})
}

// untilRed is untilGreen for the fixture's CI forced red: it reads the checks
// on head until a check failed, and fails if they are green.
func untilRed(ctx context.Context, clone string, number int, previous, head string) ([]string, github.Checks, error) {
	return watchUntil(ctx, clone, number, previous, head, "red", func(c github.Checks) (bool, error) {
		switch c.Verdict() {
		case github.Red:
			return true, nil
		case github.Green:
			return false, fmt.Errorf("the fixture's CI is green on %s, which its .ci-mode forces red: %s", head, describeChecks(c.Checks))
		}
		return false, nil
	})
}

// watchUntil reads the checks on pull request number's head with the code
// the daemon's CI watch runs until done says they are what it waits for,
// requiring the pull request's head to be head, and returns what it saw each
// time it changed. Until GitHub moves the pull request to head, its head may
// still be previous, the head pushed before head, if any: those checks are
// not head's, and are skipped. Nothing within ciWait fails, saying what it
// waited for.
func watchUntil(ctx context.Context, clone string, number int, previous, head, what string, done func(github.Checks) (bool, error)) ([]string, github.Checks, error) {
	deadline := time.Now().Add(ciWait)
	var seen []string
	for {
		c, err := github.ViewChecks(ctx, clone, number)
		if err != nil {
			return seen, github.Checks{}, err
		}
		switch {
		case c.HeadCommit == head:
			previous = ""
			saw := c.Verdict() + ": " + describeChecks(c.Checks)
			if len(seen) == 0 || seen[len(seen)-1] != saw {
				seen = append(seen, saw)
			}
			ok, err := done(c)
			if err != nil || ok {
				return seen, c, err
			}
		case previous == "" || c.HeadCommit != previous:
			return seen, c, fmt.Errorf("pull request #%d's head is %s, not the pushed %s", number, c.HeadCommit, head)
		}
		if time.Now().After(deadline) {
			if c.HeadCommit != head {
				return seen, c, fmt.Errorf("pull request #%d's head is still %s, not the pushed %s, after %s", number, c.HeadCommit, head, ciWait)
			}
			return seen, c, fmt.Errorf("the checks on %s were not %s within %s: %s", head, what, ciWait, strings.Join(seen, "; "))
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

// c43 is the daemon watching CI through gh on the pushed head, against the
// fixture repository on GitHub with the harness user's own gh: green; red
// with one judged re-run; and a newer push replacing the wait. It pushes a
// scratch job branch with the daemon's push, opens a draft pull request from
// it, and reads the checks on its head with the code the daemon's watch runs
// until they are green, requiring the pull request's head to be the pushed
// commit and each check's state to be read. A head with no checks is never
// green: it keeps looking until the fixture's CI reports. Then it pushes a
// newer head whose .ci-mode forces the fixture's CI red, whose checks
// replace the older head's, and once they are red re-runs their failed jobs
// once with the daemon's code (github.FailedRuns and github.RerunFailed),
// waiting for the re-run's own result, red again. Then it pushes a fix, a
// newer head again, and waits for it to be green, requiring the pull
// request's head to be the fix. It marks nothing ready and merges nothing; it
// closes the pull request and deletes the scratch branch from GitHub
// afterwards.
func (h *Harness) c43(ctx context.Context, f *Factory, r *Result) (err error) {
	ctx, cancel := context.WithTimeout(ctx, 4*ciWait+5*time.Minute)
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
	seen, c, err := untilGreen(ctx, s.clone, number, "", s.head)
	if err != nil {
		return err
	}
	r.observe("the daemon's watch read the checks on %s, the pushed head of %s, as: %s", s.head, url, strings.Join(seen, "; "))
	if !c.Draft {
		return fmt.Errorf("watching CI changed %s: it is no longer a draft", url)
	}
	r.observe("every check on the pushed head was green (%s), and the pull request is still a draft: watching changes nothing", describeChecks(c.Checks))

	// Red: a newer head whose CI is forced red replaces the wait, and its
	// failed jobs are re-run once.
	green := s.head
	if err := s.pushNewer(ctx, scratch, "red", "C43: force the fixture's CI red"); err != nil {
		return err
	}
	seen, c, err = untilRed(ctx, s.clone, number, green, s.head)
	if err != nil {
		return err
	}
	r.observe("a newer push of %s replaced the wait on %s: the daemon's watch read the checks on the new head as: %s", s.head, green, strings.Join(seen, "; "))
	failed := failedOf(c.Checks)
	runs := github.FailedRuns(c.Checks)
	if len(runs) == 0 {
		return fmt.Errorf("none of the failed checks on %s (%s) is a GitHub Actions run to re-run", s.head, describeChecks(failed))
	}
	rerun, err := github.RerunFailed(ctx, s.clone, runs)
	if err != nil {
		return fmt.Errorf("re-running the failed jobs of runs %v: %w", runs, err)
	}
	r.observe("the daemon's code re-ran the failed jobs of the red checks %s once, in workflow runs %v", describeChecks(failed), rerun)
	seen, c, err = watchUntil(ctx, s.clone, number, "", s.head, "re-run", func(c github.Checks) (bool, error) {
		switch c.Verdict() {
		case github.Green:
			return false, fmt.Errorf("the re-run of the checks on %s, which its .ci-mode forces red, is green: %s", s.head, describeChecks(c.Checks))
		case github.Red:
			// The re-run is a new check run, at a URL of its own.
			return !sameChecks(failedOf(c.Checks), failed), nil
		}
		return false, nil
	})
	if err != nil {
		return err
	}
	r.observe("the re-run reported on %s: %s; it is the head's one re-run, so a fix follows", s.head, strings.Join(seen, "; "))

	// A fix: a newer head replaces the wait on the red one.
	red := s.head
	if err := s.pushNewer(ctx, scratch, "", "C43: fix the fixture's CI"); err != nil {
		return err
	}
	seen, c, err = untilGreen(ctx, s.clone, number, red, s.head)
	if err != nil {
		return err
	}
	if c.HeadCommit != s.head || !c.Draft {
		return fmt.Errorf("after the fix, %s is at %s, draft %v, want at the fix %s and still a draft", url, c.HeadCommit, c.Draft, s.head)
	}
	r.observe("the fix's newer push of %s replaced the wait on %s: the daemon's watch read the checks on the new head as: %s, and the pull request is still a draft", s.head, red, strings.Join(seen, "; "))
	return nil
}

// failedOf returns the checks that failed among checks.
func failedOf(checks []github.Check) []github.Check {
	var failed []github.Check
	for _, c := range checks {
		if c.State == github.CheckFailed {
			failed = append(failed, c)
		}
	}
	return failed
}

// sameChecks reports whether a and b are the same check runs, by name and
// URL.
func sameChecks(a, b []github.Check) bool {
	return slices.EqualFunc(a, b, func(x, y github.Check) bool { return x.Name == y.Name && x.URL == y.URL })
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
		seen, c, err := untilGreen(ctx, s.clone, number, "", s.head)
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
