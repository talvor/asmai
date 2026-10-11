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
	"os"
	"os/exec"
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
	out, err := gh(ctx, dir, "pr", "view", ref, "--json", "number,url,title,isDraft,headRefName,headRefOid,state")
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

// gh runs gh in dir, never prompting, and returns what it printed on stdout.
func gh(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Dir = dir
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
