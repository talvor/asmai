// SPDX-License-Identifier: Apache-2.0

// Package github is the daemon's boundary with GitHub: the gh commands it
// runs, with the gh on its PATH and that gh's own sign-in. A test replaces gh
// on the PATH.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// NotReady is what a pull request's title starts with when it opened
// normally because GitHub refused a draft, until it is ready.
const NotReady = "[not ready]"

// PullRequest is a pull request as GitHub has it.
type PullRequest struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
	Title  string `json:"title"`
	Draft  bool   `json:"isDraft"`
	// HeadBranch is the branch the pull request is from, and HeadCommit
	// that branch's commit on GitHub.
	HeadBranch string `json:"headRefName"`
	HeadCommit string `json:"headRefOid"`
	// CrossRepository is whether the head branch is in another repository
	// than the one the pull request is in, a fork's.
	CrossRepository bool `json:"isCrossRepository"`
	// State is GitHub's: OPEN, CLOSED or MERGED.
	State string `json:"state"`
}

// IsNotReady reports whether the pull request's title starts with NotReady.
func (p PullRequest) IsNotReady() bool {
	return strings.HasPrefix(p.Title, NotReady)
}

// ViewPullRequest looks up the pull request ref names, its URL or its
// number, with `gh pr view` run in dir, whose origin is the repository of a
// pull request ref names by number.
func ViewPullRequest(ctx context.Context, dir, ref string) (PullRequest, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" || strings.HasPrefix(ref, "-") {
		return PullRequest{}, fmt.Errorf("%q does not name a pull request: name it by the URL gh printed", ref)
	}
	out, err := gh(ctx, dir, "pr", "view", ref, "--json", "number,url,title,isDraft,headRefName,headRefOid,isCrossRepository,state")
	if err != nil {
		return PullRequest{}, err
	}
	var p PullRequest
	if err := json.Unmarshal(out, &p); err != nil {
		return PullRequest{}, fmt.Errorf("reading what gh pr view printed: %w", err)
	}
	if p.Number <= 0 || p.HeadBranch == "" {
		return PullRequest{}, fmt.Errorf("gh pr view %s printed no pull request number or head branch", ref)
	}
	return p, nil
}

// In reports whether the pull request is in the repository whose URL on
// GitHub is repository, from a branch of that repository.
func (p PullRequest) In(repository string) bool {
	return !p.CrossRepository && strings.HasPrefix(p.URL, strings.TrimSuffix(repository, "/")+"/pull/")
}

// RepositoryURL returns the URL on GitHub of the repository `gh repo view`
// run in dir shows, dir's origin.
func RepositoryURL(ctx context.Context, dir string) (string, error) {
	out, err := gh(ctx, dir, "repo", "view", "--json", "url")
	if err != nil {
		return "", err
	}
	var r struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return "", fmt.Errorf("reading what gh repo view printed: %w", err)
	}
	if r.URL == "" {
		return "", errors.New("gh repo view printed no repository URL")
	}
	return r.URL, nil
}

// DraftsRefused reports whether err is GitHub's refusal of a draft pull
// request, as it answers for a repository that does not support them.
func DraftsRefused(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "draft pull requests are not supported")
}

// Repository names the repository of dir's origin as `gh --repo` takes it,
// OWNER/REPO on github.com and HOST/OWNER/REPO elsewhere, as `gh repo view`
// run in dir shows it.
func Repository(ctx context.Context, dir string) (string, error) {
	url, err := RepositoryURL(ctx, dir)
	if err != nil {
		return "", err
	}
	name := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(url, "https://"), "http://"), "/")
	return strings.TrimPrefix(name, "github.com/"), nil
}

// Existing is a pull request from a branch, in any state.
type Existing struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
	State  string `json:"state"`
}

// PullRequestsFrom lists the pull requests of repository, in any state, whose
// head is branch, with gh run in dir.
func PullRequestsFrom(ctx context.Context, dir, repository, branch string) ([]Existing, error) {
	out, err := gh(ctx, dir, "pr", "list", "--repo", repository, "--head", branch, "--state", "all", "--json", "number,url,state")
	if err != nil {
		return nil, err
	}
	var found []Existing
	if err := json.Unmarshal(out, &found); err != nil {
		return nil, fmt.Errorf("reading what gh pr list printed: %w", err)
	}
	return found, nil
}

// Create is what opening a pull request takes: the branch it is from, the
// branch it goes to, its title and body, and whether it is a draft.
type Create struct {
	Repository string
	Base, Head string
	Title      string
	Body       string
	Draft      bool
}

// CreatePullRequest opens the pull request c describes with `gh pr create`
// run in dir, whose origin is its repository, and returns its URL. The body is
// given on gh's standard input, so that it is never a file anyone else could
// change before gh reads it.
func CreatePullRequest(ctx context.Context, dir string, c Create) (string, error) {
	args := []string{"pr", "create", "--repo", c.Repository, "--base", c.Base, "--head", c.Head, "--title", c.Title, "--body-file", "-"}
	if c.Draft {
		args = append(args, "--draft")
	}
	out, err := ghInput(ctx, dir, strings.NewReader(c.Body), args...)
	if err != nil {
		return "", err
	}
	url := ""
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); strings.HasPrefix(line, "https://") || strings.HasPrefix(line, "http://") {
			url = line
		}
	}
	if url == "" {
		return "", errors.New("gh pr create printed no pull request URL")
	}
	return url, nil
}

// EditBody replaces the body of pull request number of repository with body,
// with `gh pr edit` run in dir. It changes nothing else: never its title or
// its draft state.
func EditBody(ctx context.Context, dir, repository string, number int, body string) error {
	_, err := ghInput(ctx, dir, strings.NewReader(body), "pr", "edit", strconv.Itoa(number), "--repo", repository, "--body-file", "-")
	return err
}

// gh runs gh in dir, never prompting, and returns what it printed on stdout.
func gh(ctx context.Context, dir string, args ...string) ([]byte, error) {
	return ghInput(ctx, dir, nil, args...)
}

// ghInput is gh with stdin as gh's standard input.
func ghInput(ctx context.Context, dir string, stdin io.Reader, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Dir = dir
	cmd.Stdin = stdin
	cmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1", "NO_COLOR=1")
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, errors.New("gh is not installed, or not on the daemon's PATH")
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return nil, fmt.Errorf("gh %s: %s", strings.Join(args[:2], " "), strings.ReplaceAll(message, "\n", "; "))
	}
	return out.Bytes(), nil
}
