// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/talvor/asmai/internal/github"
	"github.com/talvor/asmai/internal/repos"
	"github.com/talvor/asmai/internal/store"
)

// continuation is the job branch and pull request a follow-up continues: its
// predecessor's, which is still open on GitHub (05 rule 16).
type continuation struct {
	// Predecessor is the job the follow-up follows, Branch the job branch
	// the follow-up keeps and Tip origin's copy of it, which the follow-up
	// starts from.
	Predecessor int64
	Branch      string
	Tip         string
	// PullRequest is the predecessor's latest recorded pull request.
	PullRequest store.PullRequest
}

func (d *daemon) continuedPullRequest(job int64) (store.PullRequest, error) {
	for {
		prs, err := d.store.PullRequests(job)
		if err != nil {
			return store.PullRequest{}, err
		}
		if len(prs) != 0 {
			return prs[len(prs)-1], nil
		}
		branch, err := d.store.JobBranch(job)
		if errors.Is(err, sql.ErrNoRows) {
			return store.PullRequest{}, sql.ErrNoRows
		}
		if err != nil {
			return store.PullRequest{}, err
		}
		if branch.Continues == 0 {
			return store.PullRequest{}, sql.ErrNoRows
		}
		job = branch.Continues
	}
}

func (d *daemon) replaceableAssignments(ctx context.Context, job int64) (string, error) {
	assignments, err := d.store.AssignmentsForJob(job)
	if err != nil {
		return "", err
	}
	for _, a := range assignments {
		if a.State == store.AssignmentAccepted || a.State == store.AssignmentCancelled {
			continue
		}
		uncommitted, err := repos.Uncommitted(ctx, a.Workspace.Path)
		if err != nil {
			return fmt.Sprintf("job %d's assignment %d workspace could not be checked: %v; retry `asmai job link` after its work is safe on origin", job, a.ID, err), nil
		}
		if len(uncommitted) != 0 {
			return fmt.Sprintf("job %d's assignment %d has uncommitted work; commit and push it before retrying `asmai job link`", job, a.ID), nil
		}
		if a.ReadOnly {
			continue
		}
		commit, err := repos.Commit(ctx, a.Workspace.Path, "HEAD")
		if err != nil {
			return fmt.Sprintf("job %d's assignment %d commit could not be checked: %v; retry `asmai job link` after its work is safe on origin", job, a.ID, err), nil
		}
		if !d.pushed(ctx, a, commit) {
			return fmt.Sprintf("job %d's assignment %d has work not pushed to origin/%s; push it before retrying `asmai job link`", job, a.ID, a.Workspace.Branch), nil
		}
	}
	return "", nil
}

// continuationOf decides how the follow-up j starts: it continues the job
// branch and the pull request of the job it follows when that pull request is
// still open and no unrelated open job holds the branch, and otherwise starts
// from the base with a new job branch and pull request. It returns the
// continuation, or nil with the reason j starts afresh. It looks the pull
// request up on GitHub, as the daemon recorded it open and it may have been
// merged or closed since, and fetches origin, so that the branch is read as
// it is now. An error is a failed look-up, and decides nothing. assignMu is
// held.
func (d *daemon) continuationOf(ctx context.Context, j store.Job, repository store.Repository) (*continuation, string, error) {
	pred, err := d.store.Job(j.Follows)
	if err != nil {
		return nil, "", err
	}
	branch, err := d.store.JobBranch(pred.Number)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Sprintf("job %d made no job branch", pred.Number), nil
	}
	if err != nil {
		return nil, "", err
	}
	recorded, err := d.continuedPullRequest(pred.Number)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Sprintf("job %d has no pull request", pred.Number), nil
	}
	if err != nil {
		return nil, "", err
	}
	ghCtx, cancel := context.WithTimeout(ctx, ghTimeout)
	found, err := github.ViewPullRequest(ghCtx, repository.Clone, recorded.URL)
	cancel()
	if err != nil {
		return nil, "", fmt.Errorf("looking up job %d's pull request %s with gh: %w", pred.Number, recorded.URL, err)
	}
	if found.State != "OPEN" {
		return nil, fmt.Sprintf("job %d's pull request #%d is %s, no longer open", pred.Number, found.Number, found.State), nil
	}
	if found.HeadBranch != branch.Name {
		return nil, fmt.Sprintf("job %d's pull request #%d is from %s, not its job branch %s", pred.Number, found.Number, found.HeadBranch, branch.Name), nil
	}
	if holder, err := d.store.BranchHeldByOpenJob(repository.Name, branch.Name, j.Number); err != nil {
		return nil, "", err
	} else if holder != 0 && holder != pred.Number {
		return nil, fmt.Sprintf("open job %d holds the job branch %s", holder, branch.Name), nil
	}
	if !pred.Ended() {
		checkCtx, cancel := context.WithTimeout(ctx, gitTimeout)
		why, err := d.replaceableAssignments(checkCtx, pred.Number)
		cancel()
		if err != nil || why != "" {
			return nil, why, err
		}
	}
	gitCtx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	if err := repos.Fetch(gitCtx, repository.Clone); err != nil {
		return nil, "", err
	}
	tip, err := repos.OriginTip(gitCtx, repository.Clone, branch.Name)
	if err != nil {
		return nil, "", err
	}
	recorded.State, recorded.Head, recorded.Draft, recorded.NotReady = found.State, found.HeadCommit, found.Draft, found.IsNotReady()
	return &continuation{Predecessor: pred.Number, Branch: branch.Name, Tip: tip, PullRequest: recorded}, "", nil
}

// settleFollowUp makes the job branch of the follow-up j, which job link has
// just linked, from its predecessor's where the predecessor's pull request is
// still open, and otherwise leaves the branch it has or makes one from the
// base. The job branch the daemon made as the job opened is dropped first,
// while no work is on it. It reports in resp how the follow-up starts.
func (d *daemon) settleFollowUp(j store.Job, resp *Response) error {
	repository, err := d.store.Repository(j.Repository)
	if errors.Is(err, sql.ErrNoRows) {
		return notRegistered(j.Repository)
	}
	if err != nil {
		return err
	}
	d.assignMu.Lock()
	defer d.assignMu.Unlock()
	ctx, cancel := context.WithTimeout(d.work, 2*(2*gitTimeout+ghTimeout))
	defer cancel()
	if branch, err := d.store.JobBranch(j.Number); err == nil {
		if branch.Continues != 0 {
			return d.reportStart(j, branch, resp)
		}
		c, why, err := d.continuationOf(ctx, j, repository)
		if err != nil {
			return refuse("job %d now follows job %d, but whether its pull request is still open was not read: %v. Run `asmai job link` again", j.Number, j.Follows, err)
		}
		if c == nil {
			resp.Starts = why
			resp.JobBranch = &branch
			return nil
		}
		dropped, err := d.store.JobBranchDropped(j.Number, time.Now())
		if err != nil {
			return refuse("%v", err)
		}
		if _, err := repos.DeleteBranch(ctx, repository.Clone, dropped.Name); err != nil {
			d.log.Warn("deleting the job branch made for a follow-up before it continued", "job", j.Number, "branch", dropped.Name, "error", err.Error())
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	} else {
		c, why, err := d.continuationOf(ctx, j, repository)
		if err != nil {
			return refuse("job %d now follows job %d, but whether its pull request is still open was not read: %v. Run `asmai job link` again", j.Number, j.Follows, err)
		}
		if c == nil {
			resp.Starts = why
		}
	}
	branch, err := d.ensureJobBranch(ctx, j, repository)
	if err != nil {
		return refuse("job %d now follows job %d, but its job branch was not made: %v. Run `asmai job link` again, or give it an assignment, which makes it", j.Number, j.Follows, err)
	}
	return d.reportStart(j, branch, resp)
}

// reportStart says in resp how the follow-up j starts: on the job branch and
// pull request it continues, or afresh.
func (d *daemon) reportStart(j store.Job, branch store.JobBranch, resp *Response) error {
	resp.JobBranch = &branch
	if branch.Continues == 0 {
		if resp.Starts == "" {
			resp.Starts = fmt.Sprintf("job %d's pull request is not open for it to continue, so it starts from the base", j.Follows)
		}
		return nil
	}
	pr, err := d.continuedPullRequest(branch.Continues)
	if err != nil {
		return err
	}
	resp.Continues = &pr
	return nil
}
