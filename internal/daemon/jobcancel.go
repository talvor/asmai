// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/talvor/asmai/internal/github"
	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/store"
)

// cancelJob cancels the job req names at the user's word, for Coordination's
// leader only: the user's own words in the witnessed message req cites are
// the reason. It refuses a job that does not exist, has ended or is already
// cancelled, and then nothing changes. Otherwise it first closes the job's
// open pull request with a comment giving the reason, through gh: if that
// fails, nothing is cancelled and the failure is the refusal. Then, in one
// step, the job ends as cancelled, its live assignments and those that
// needed reconciliation are cancelling, its unreviewed results are
// discarded, its pending decision requests are withdrawn and the closing is
// recorded as the daemon's effect. The workers are started to push their
// assignment branches and stop, and each assignment ends as cancelled once
// the daemon has confirmed its push; one whose workspace is gone has nothing
// to push from, so it ends as cancelled at once, with no push and no worker
// started. Every branch stays on origin.
func (d *daemon) cancelJob(req Request) (store.JobCancellation, error) {
	d.mu.Lock()
	ref, _, err := d.caller(req)
	d.mu.Unlock()
	if err != nil {
		return store.JobCancellation{}, err
	}
	if ref.address != roles.LeaderOf(roles.Coordination) {
		return store.JobCancellation{}, refuse("only %s may cancel a job, at the user's own word: %s cancels its own workers' assignments with asmai cancel", roles.LeaderOf(roles.Coordination), ref.address)
	}
	if req.Job <= 0 || (req.Witness <= 0 && !req.LatestWitness) {
		return store.JobCancellation{}, refuse("job cancel needs the job and the user's own words: --message latest or --message <witnessed-id>")
	}

	// Holding assignMu, no acceptance moves the job branch and no
	// assignment starts while the job is cancelled.
	d.assignMu.Lock()
	j, err := d.store.CheckJobCancellable(req.Job)
	if err != nil {
		d.assignMu.Unlock()
		return store.JobCancellation{}, refuse("%v", err)
	}
	// Every check JobCancelled refuses on is made before gh writes, so a
	// refusal changes nothing, on GitHub either.
	witness, reason, err := d.store.CancelReason(req.Witness, req.LatestWitness)
	if err != nil {
		d.assignMu.Unlock()
		return store.JobCancellation{}, refuse("%v", err)
	}
	closed, err := d.closeCancelledPullRequest(j, witness, reason)
	if err != nil {
		d.assignMu.Unlock()
		return store.JobCancellation{}, err
	}

	d.mu.Lock()
	gone, err := d.goneWorkspaces(j.Number)
	if err != nil {
		d.mu.Unlock()
		d.assignMu.Unlock()
		if closed != nil {
			return store.JobCancellation{}, refuse("pull request #%d %s is closed, but the job was not cancelled: %v; run asmai job cancel again", closed.Number, closed.URL, err)
		}
		return store.JobCancellation{}, refuse("%v", err)
	}
	c, err := d.store.JobCancelled(store.JobCancel{Job: j.Number, Witness: witness, Closed: closed, WorkspaceGone: gone}, time.Now())
	if err == nil {
		for i, a := range c.Cancelling {
			if d.cancelledWithoutWorkspace(a) {
				c.Cancelling[i].State = store.AssignmentCancelled
				continue
			}
			if worker, err := roles.ParseAddress(a.Worker); err == nil {
				if err := d.ensureAgent(worker); err != nil {
					d.log.Error("starting the worker of a cancelled job's assignment", "agent", a.Worker, "assignment", a.ID, "error", err.Error())
				}
			}
		}
	}
	d.mu.Unlock()
	d.assignMu.Unlock()
	if err != nil {
		if closed != nil {
			return store.JobCancellation{}, refuse("pull request #%d %s is closed, but the job was not cancelled: %v; run asmai job cancel again", closed.Number, closed.URL, err)
		}
		return store.JobCancellation{}, refuse("%v", err)
	}
	d.log.Info("job cancelled at the user's word", "job", j.Number, "witness", witness, "cancelling", len(c.Cancelling), "discarded", len(c.Discarded), "withdrawn", len(c.Withdrawn), "pull_request_closed", closed != nil)
	d.cancelledJobEnded(j.Number)
	return c, nil
}

// goneWorkspaces returns the assignments of job that a cancellation makes
// cancelling whose workspaces are gone, so that the messages of the
// cancellation say each ends as cancelled with no push.
func (d *daemon) goneWorkspaces(job int64) ([]int64, error) {
	assignments, err := d.store.AssignmentsForJob(job)
	if err != nil {
		return nil, err
	}
	var gone []int64
	for _, a := range assignments {
		switch a.State {
		case store.AssignmentActive, store.AssignmentSubmitted, store.AssignmentNeedsReconciliation:
		default:
			continue
		}
		if _, err := os.Stat(a.Workspace.Path); errors.Is(err, fs.ErrNotExist) {
			gone = append(gone, a.ID)
		}
	}
	return gone, nil
}

// closeCancelledPullRequest closes job j's latest recorded pull request, if
// GitHub still has it open, with a comment giving the user's words reason,
// and returns it; it returns nil when the job has none open. One an earlier
// attempt closed, whose cancellation then failed, is found closed with
// AsmAI's comment for the job, and is returned as closed. A failure to
// look it up or close it is returned as a refusal that says nothing was
// cancelled. assignMu is held.
func (d *daemon) closeCancelledPullRequest(j store.Job, witness int64, reason string) (*store.PullRequest, error) {
	prs, err := d.store.PullRequests(j.Number)
	if err != nil || len(prs) == 0 {
		return nil, err
	}
	p := prs[len(prs)-1]
	repository, err := d.store.Repository(j.Repository)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notRegistered(j.Repository)
	}
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(d.work, 2*ghTimeout)
	defer cancel()
	found, err := github.ViewPullRequest(ctx, repository.Clone, p.URL)
	if err != nil {
		return nil, refuse("nothing is cancelled: looking up job %d's pull request #%d %s with gh failed: %v; run asmai job cancel again once gh can reach GitHub", j.Number, p.Number, p.URL, err)
	}
	if found.State == "CLOSED" {
		// A cancellation whose step failed after gh closed the pull request
		// left AsmAI's comment on it: the closing is still the daemon's.
		comments, err := github.PullRequestComments(ctx, repository.Clone, found.Number)
		if err != nil {
			return nil, refuse("nothing is cancelled: reading the comments on job %d's closed pull request #%d %s with gh failed: %v; run asmai job cancel again once gh can reach GitHub", j.Number, found.Number, p.URL, err)
		}
		if closedForCancellation(comments, j.Number) {
			d.log.Info("a cancelled job's pull request was already closed by an earlier attempt", "job", j.Number, "number", found.Number, "url", p.URL)
			return &p, nil
		}
	}
	if found.State != "OPEN" {
		d.log.Info("a cancelled job's pull request is no longer open, so it is not closed", "job", j.Number, "number", found.Number, "state", found.State)
		return nil, nil
	}
	comment := fmt.Sprintf("%s (witnessed message %d):\n\n> %s\n\nThe job branch and every assignment branch stay on origin.", cancelCommentLead(j.Number), witness, strings.ReplaceAll(reason, "\n", "\n> "))
	if err := github.ClosePullRequest(ctx, repository.Clone, found.Number, comment); err != nil {
		return nil, refuse("nothing is cancelled: closing job %d's pull request #%d %s with gh failed: %v; run asmai job cancel again once gh can close it", j.Number, found.Number, p.URL, err)
	}
	d.log.Info("a cancelled job's pull request closed with the reason", "job", j.Number, "number", found.Number, "url", p.URL)
	return &p, nil
}

// cancelCommentLead is how the comment AsmAI closes a cancelled job's pull
// request with starts.
func cancelCommentLead(job int64) string {
	return fmt.Sprintf("AsmAI closed this pull request: job %d was cancelled at the user's word", job)
}

// closedForCancellation reports whether comments, the bodies gh printed, hold
// the comment AsmAI closed job's pull request with.
func closedForCancellation(comments string, job int64) bool {
	lead := cancelCommentLead(job) + " "
	for line := range strings.Lines(comments) {
		if strings.HasPrefix(line, lead) {
			return true
		}
	}
	return false
}

// cancelledJobEnded removes what is local to job once it is cancelled and
// none of its assignments still holds its workspace: its view, workspaces and
// branches in AsmAI's clone. Its branches stay on origin.
func (d *daemon) cancelledJobEnded(job int64) {
	j, err := d.store.Job(job)
	if err != nil || j.State != store.JobCancelled {
		return
	}
	d.removeJobLocal(job)
}

// holdsWorkspace reports whether an assignment of job still holds its
// workspace: one that is live, cancelling or held for reconciliation.
func (d *daemon) holdsWorkspace(job int64) (bool, error) {
	assignments, err := d.store.AssignmentsForJob(job)
	if err != nil {
		return false, err
	}
	for _, a := range assignments {
		if a.State != store.AssignmentAccepted && a.State != store.AssignmentCancelled {
			return true, nil
		}
	}
	return false, nil
}
