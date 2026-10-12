// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// The states of a check, and the verdict on a head's checks.
const (
	// CheckPending is a check that has not finished, or whose state is
	// unknown.
	CheckPending = "pending"
	// CheckPassed is a check that finished successfully, or neutral or
	// skipped, which GitHub counts as passing.
	CheckPassed = "passed"
	// CheckFailed is a check that finished any other way.
	CheckFailed = "failed"
)

// The verdicts on a head's checks.
const (
	// Green is every check passed, with at least one check.
	Green = "green"
	// Red is a check failed.
	Red = "red"
	// Waiting is a check not finished, or no check at all: a head with no
	// checks is never green.
	Waiting = "waiting"
)

// Check is one check on a pull request's head, as GitHub has it: a check run
// of a workflow or another app, or a commit status.
type Check struct {
	// Name is the check run's name, with its workflow's before it when it
	// has one, as "CI / test", or the commit status's context.
	Name string `json:"name"`
	// Status and Conclusion are GitHub's for a check run, such as COMPLETED
	// and SUCCESS; a commit status has its state as its Conclusion.
	Status     string `json:"status,omitempty"`
	Conclusion string `json:"conclusion,omitempty"`
	// State is what AsmAI makes of it: CheckPending, CheckPassed or
	// CheckFailed.
	State string `json:"state"`
	URL   string `json:"url,omitempty"`
}

// Checks is a pull request as GitHub has it now, with the checks on its head
// commit.
type Checks struct {
	PullRequest
	Checks []Check `json:"checks"`
}

// Verdict is Green when every check passed and there is at least one, Red
// when one failed, and otherwise Waiting.
func (c Checks) Verdict() string {
	return Verdict(c.Checks)
}

// Verdict is Green when every check passed and there is at least one, Red
// when one failed, and otherwise Waiting. No checks is Waiting, never Green.
func Verdict(checks []Check) string {
	if len(checks) == 0 {
		return Waiting
	}
	verdict := Green
	for _, c := range checks {
		switch c.State {
		case CheckFailed:
			return Red
		case CheckPassed:
		default:
			verdict = Waiting
		}
	}
	return verdict
}

// rollupEntry is an entry of the statusCheckRollup gh pr view prints: a
// check run or a commit status.
type rollupEntry struct {
	Type         string `json:"__typename"`
	Name         string `json:"name"`
	WorkflowName string `json:"workflowName"`
	Status       string `json:"status"`
	Conclusion   string `json:"conclusion"`
	DetailsURL   string `json:"detailsUrl"`
	Context      string `json:"context"`
	State        string `json:"state"`
	TargetURL    string `json:"targetUrl"`
}

func (e rollupEntry) check() Check {
	if e.Type == "StatusContext" {
		c := Check{Name: e.Context, Conclusion: e.State, URL: e.TargetURL, State: CheckPending}
		switch e.State {
		case "SUCCESS":
			c.State = CheckPassed
		case "FAILURE", "ERROR":
			c.State = CheckFailed
		}
		return c
	}
	name := e.Name
	if e.WorkflowName != "" {
		name = e.WorkflowName + " / " + e.Name
	}
	c := Check{Name: name, Status: e.Status, Conclusion: e.Conclusion, URL: e.DetailsURL, State: CheckPending}
	if e.Status == "COMPLETED" {
		switch e.Conclusion {
		case "SUCCESS", "NEUTRAL", "SKIPPED":
			c.State = CheckPassed
		case "":
		default:
			c.State = CheckFailed
		}
	}
	return c
}

// ViewChecks looks up the pull request number with `gh pr view` run in dir,
// whose origin is its repository, with the checks on its head commit. The
// head and its checks come from the same call, so the checks are the head's.
func ViewChecks(ctx context.Context, dir string, number int) (Checks, error) {
	if number <= 0 {
		return Checks{}, fmt.Errorf("%d is not a pull request number", number)
	}
	ref := strconv.Itoa(number)
	out, err := gh(ctx, dir, "pr", "view", ref, "--json", "number,url,title,isDraft,headRefName,headRefOid,isCrossRepository,state,statusCheckRollup")
	if err != nil {
		return Checks{}, err
	}
	var v struct {
		PullRequest
		Rollup []rollupEntry `json:"statusCheckRollup"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return Checks{}, fmt.Errorf("reading what gh pr view printed: %w", err)
	}
	if v.Number != number || v.HeadCommit == "" {
		return Checks{}, fmt.Errorf("gh pr view %s printed no pull request #%d with a head commit", ref, number)
	}
	c := Checks{PullRequest: v.PullRequest, Checks: []Check{}}
	for _, e := range v.Rollup {
		c.Checks = append(c.Checks, e.check())
	}
	return c, nil
}

// MarkReady marks the pull request p ready for review, with gh run in dir:
// a draft with `gh pr ready`, and one with NotReady at the start of its title
// by taking it out with `gh pr edit`. It only ever changes those two: it never
// merges, and ready never means merged.
func MarkReady(ctx context.Context, dir string, p PullRequest) error {
	ref := strconv.Itoa(p.Number)
	if p.Draft {
		if _, err := gh(ctx, dir, "pr", "ready", ref); err != nil {
			return err
		}
	}
	if p.IsNotReady() {
		if _, err := gh(ctx, dir, "pr", "edit", ref, "--title", ReadyTitle(p.Title)); err != nil {
			return err
		}
	}
	return nil
}

// ReadyTitle is title without NotReady at its start.
func ReadyTitle(title string) string {
	if !strings.HasPrefix(title, NotReady) {
		return title
	}
	return strings.TrimSpace(strings.TrimPrefix(title, NotReady))
}

// FailedRuns returns the workflow runs of the failed checks among checks,
// each once, as GitHub Actions numbers them in a check run's details URL,
// .../actions/runs/<run>/job/<job>. A failed commit status, or a check run of
// another app, has no workflow run to re-run, and is not among them.
func FailedRuns(checks []Check) []int64 {
	var runs []int64
	for _, c := range checks {
		if c.State != CheckFailed {
			continue
		}
		_, after, ok := strings.Cut(c.URL, "/actions/runs/")
		if !ok {
			continue
		}
		id, _, _ := strings.Cut(after, "/")
		run, err := strconv.ParseInt(id, 10, 64)
		if err != nil || run <= 0 || slices.Contains(runs, run) {
			continue
		}
		runs = append(runs, run)
	}
	return runs
}

// RerunFailed re-runs the failed jobs of each of runs with `gh run rerun
// <run> --failed`, run in dir, whose origin is the runs' repository. It stops
// at the first run gh does not re-run, and returns the runs it re-ran.
func RerunFailed(ctx context.Context, dir string, runs []int64) ([]int64, error) {
	if len(runs) == 0 {
		return nil, errors.New("there is no workflow run to re-run")
	}
	var rerun []int64
	for _, run := range runs {
		if _, err := gh(ctx, dir, "run", "rerun", strconv.FormatInt(run, 10), "--failed"); err != nil {
			return rerun, err
		}
		rerun = append(rerun, run)
	}
	return rerun, nil
}
