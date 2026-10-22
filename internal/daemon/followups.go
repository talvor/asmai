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

// continuationOf decides how the follow-up j starts: it continues the job
// branch and the pull request of the job it follows when that pull request is
// still open and no other open job holds the branch, and otherwise starts
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
	if pred.State == store.JobOpen {
		return nil, fmt.Sprintf("job %d is still open and a job branch belongs to one open job at a time", pred.Number), nil
	}
	prs, err := d.store.PullRequests(pred.Number)
	if err != nil {
		return nil, "", err
	}
	if len(prs) == 0 {
		return nil, fmt.Sprintf("job %d has no pull request", pred.Number), nil
	}
	recorded := prs[len(prs)-1]
	ctx, cancel := context.WithTimeout(ctx, ghTimeout)
	defer cancel()
	found, err := github.ViewPullRequest(ctx, repository.Clone, recorded.URL)
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
	} else if holder != 0 {
		return nil, fmt.Sprintf("open job %d holds the job branch %s", holder, branch.Name), nil
	}
	if err := repos.Fetch(ctx, repository.Clone); err != nil {
		return nil, "", err
	}
	tip, err := repos.OriginTip(ctx, repository.Clone, branch.Name)
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
	ctx, cancel := context.WithTimeout(d.work, gitTimeout+ghTimeout)
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
		resp.Starts = fmt.Sprintf("job %d's pull request is not open for it to continue, so it starts from the base", j.Follows)
		return nil
	}
	prs, err := d.store.PullRequests(branch.Continues)
	if err != nil {
		return err
	}
	if len(prs) > 0 {
		resp.Continues = &prs[len(prs)-1]
	}
	return nil
}
