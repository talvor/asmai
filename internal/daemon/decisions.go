// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/talvor/asmai/internal/repos"
	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/session"
	"github.com/talvor/asmai/internal/store"
)

// stopWait is how long the daemon waits for a stopped worker's session to be
// cleared from its slot, so that the worker's number can be given out again.
const stopWait = 10 * time.Second

// decide serves accept, reject and cancel: the owning leader's decision on
// an assignment. It returns the answer, and the agent to nudge.
func (d *daemon) decide(req Request) (resp Response, notify string, err error) {
	d.mu.Lock()
	ref, _, err := d.caller(req)
	d.mu.Unlock()
	if err != nil {
		return Response{}, "", err
	}
	if ref.address != roles.LeaderOf(roles.Engineering) {
		return Response{}, "", refuse("only %s may run asmai %s", roles.LeaderOf(roles.Engineering), req.Command)
	}
	if req.Assignment <= 0 || len(req.Reasons) == 0 {
		return Response{}, "", refuse("asmai %s needs an assignment and its reasons", req.Command)
	}
	a, err := d.store.Assignment(req.Assignment)
	if errors.Is(err, sql.ErrNoRows) {
		return Response{}, "", refuse("assignment %d does not exist", req.Assignment)
	}
	if err != nil {
		return Response{}, "", err
	}
	if a.Owner != ref.address.String() {
		return Response{}, "", refuse("assignment %d is owned by %s, not %s", a.ID, a.Owner, ref.address)
	}
	switch req.Command {
	case CommandAccept:
		resp, err = d.accept(req, ref)
	default:
		resp, err = d.returnOrCancel(req, ref)
		if resp.Dispatch != nil {
			notify = resp.Dispatch.Agent
		}
	}
	return resp, notify, err
}

// accept records the acceptance of the result of the assignment req names and,
// in the same step, fast-forwards the job branch to the accepted commit and
// refreshes the leaders' view of the job. It refuses a commit that is not a
// fast-forward of the branch's tip, and then nothing moves. Once the work is
// on the job branch the assignment's worker is stopped and its workspace
// removed.
func (d *daemon) accept(req Request, ref sessionRef) (Response, error) {
	d.assignMu.Lock()
	defer d.assignMu.Unlock()
	a, err := d.store.Assignment(req.Assignment)
	if err != nil {
		return Response{}, err
	}
	if a.State != store.AssignmentSubmitted {
		return Response{}, refuse("assignment %d is %s, so it has no result to accept", a.ID, a.State)
	}
	result, err := d.store.LatestResult(a.ID)
	if err != nil {
		return Response{}, refuse("%v", err)
	}
	branch, err := d.store.JobBranch(a.Job)
	if err != nil {
		return Response{}, err
	}
	j, err := d.store.Job(a.Job)
	if err != nil {
		return Response{}, err
	}
	repository, err := d.store.Repository(j.Repository)
	if errors.Is(err, sql.ErrNoRows) {
		return Response{}, notRegistered(j.Repository)
	}
	if err != nil {
		return Response{}, err
	}

	ctx, cancel := context.WithTimeout(d.work, gitTimeout)
	defer cancel()
	clone := repository.Clone
	current, err := repos.Commit(ctx, clone, "refs/heads/"+branch.Name)
	if err != nil {
		return Response{}, errors.New("reading the job branch: " + err.Error())
	}
	moved := false
	switch current {
	case branch.Tip:
		fastForward, err := repos.Contains(ctx, clone, result.Commit, branch.Tip)
		if err != nil {
			return Response{}, err
		}
		if !fastForward {
			return Response{}, refuse("result commit %s is not a fast-forward of the job branch's tip %s, so the acceptance is refused and nothing moved", result.Commit, branch.Tip)
		}
		if err := repos.MoveBranch(ctx, clone, branch.Name, result.Commit, branch.Tip); err != nil {
			return Response{}, err
		}
		moved = true
	case result.Commit:
		// An earlier acceptance moved the branch and did not get as far as
		// recording it; recording it now completes that step.
	default:
		return Response{}, refuse("the job branch %s is at %s, not at its recorded tip %s, so nothing moved", branch.Name, current, branch.Tip)
	}
	undo := func() {
		if !moved {
			return
		}
		ctx := context.WithoutCancel(ctx)
		if err := repos.MoveBranch(ctx, clone, branch.Name, branch.Tip, result.Commit); err != nil {
			d.log.Error("undoing the move of a job branch", "branch", branch.Name, "error", err.Error())
		}
		if err := repos.EnsureView(ctx, clone, branch.View, branch.Tip); err != nil {
			d.log.Error("undoing the refresh of a job's view", "job", a.Job, "error", err.Error())
		}
	}
	if err := repos.EnsureView(ctx, clone, branch.View, result.Commit); err != nil {
		undo()
		return Response{}, err
	}

	d.mu.Lock()
	if _, _, err := d.caller(req); err != nil {
		d.mu.Unlock()
		undo()
		return Response{}, err
	}
	decision, err := d.store.ResultAccepted(store.Acceptance{Assignment: a.ID, Leader: ref.address.String(), Reasons: req.Reasons, FromTip: branch.Tip}, time.Now())
	d.mu.Unlock()
	if err != nil {
		undo()
		return Response{}, refuse("%v", err)
	}
	d.log.Info("result accepted; job branch fast-forwarded", "assignment", a.ID, "job", a.Job, "branch", branch.Name, "from", branch.Tip, "to", result.Commit, "leader", ref.address.String())

	d.stopWorker(a)
	if err := d.removeWorkspace(ctx, a, repository); err != nil {
		return Response{}, err
	}
	a, err = d.store.Assignment(a.ID)
	if err != nil {
		return Response{}, err
	}
	branch, err = d.store.JobBranch(a.Job)
	if err != nil {
		return Response{}, err
	}
	return Response{Assignment: &a, Decision: &decision, JobBranch: &branch}, nil
}

// returnOrCancel serves reject and cancel: both are recorded with their
// reasons and sent to the assignment's worker, which is started if it is not
// running. The returned dispatch is the worker's, to nudge.
func (d *daemon) returnOrCancel(req Request, ref sessionRef) (Response, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, _, err := d.caller(req); err != nil {
		return Response{}, err
	}
	var decision store.Decision
	var dispatch store.Dispatch
	var err error
	if req.Command == CommandReject {
		decision, dispatch, err = d.store.ResultRejected(req.Assignment, ref.address.String(), req.Reasons, time.Now())
	} else {
		decision, dispatch, err = d.store.CancellationRequested(req.Assignment, ref.address.String(), req.Reasons, time.Now())
	}
	if err != nil {
		return Response{}, refuse("%v", err)
	}
	a, err := d.store.Assignment(req.Assignment)
	if err != nil {
		return Response{}, err
	}
	d.log.Info("assignment decided", "decision", decision.Kind, "assignment", a.ID, "job", a.Job, "worker", a.Worker, "leader", ref.address.String(), "dispatch", dispatch.ID)
	if worker, err := roles.ParseAddress(a.Worker); err == nil {
		if err := d.ensureAgent(worker); err != nil {
			d.log.Error("starting the assignment's worker", "agent", a.Worker, "dispatch", dispatch.ID, "error", err.Error())
		}
	}
	return Response{Assignment: &a, Decision: &decision, Dispatch: &dispatch}, nil
}

// stopWorker stops the session of the worker that carried a, which has ended,
// and returns once it is cleared, so that the worker's number can be given
// out again.
func (d *daemon) stopWorker(a store.Assignment) {
	d.mu.Lock()
	l := d.leaders[a.Worker]
	var running *session.Session
	if l != nil && l.assignment == a.ID {
		running = l.session
	}
	d.mu.Unlock()
	if running == nil {
		return
	}
	running.Stop(stopGrace)
	deadline := time.Now().Add(stopWait)
	for time.Now().Before(deadline) {
		d.mu.Lock()
		gone := l.session != running
		d.mu.Unlock()
		if gone {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	d.log.Warn("a stopped worker's session was not cleared in time", "agent", a.Worker, "assignment", a.ID)
}

// removeWorkspace removes the writing workspace of a, whose work is on the
// job branch, and its temporary directory, and journals it. The assignment
// branch stays on origin.
func (d *daemon) removeWorkspace(ctx context.Context, a store.Assignment, repository store.Repository) error {
	ctx = context.WithoutCancel(ctx)
	if err := repos.RemoveWorkspace(ctx, repository.Clone, a.Workspace.Path, a.Workspace.Branch); err != nil {
		d.log.Error("removing an accepted assignment's workspace", "assignment", a.ID, "workspace", a.Workspace.Path, "error", err.Error())
		return err
	}
	if err := os.RemoveAll(a.Workspace.Tmp); err != nil {
		d.log.Error("removing an accepted assignment's temporary directory", "assignment", a.ID, "tmp", a.Workspace.Tmp, "error", err.Error())
		return err
	}
	// The assignment's directory goes too, once nothing is left in it.
	os.Remove(filepath.Dir(a.Workspace.Path))
	if err := d.store.WorkspaceRemoved(a, time.Now()); err != nil {
		d.log.Error("journaling a workspace's removal", "assignment", a.ID, "error", err.Error())
		return err
	}
	d.log.Info("workspace removed", "assignment", a.ID, "job", a.Job, "workspace", a.Workspace.Path)
	return nil
}

// dispatchStopped is told that the turn ref's session ran for dispatch has
// finished. When the dispatch told a worker its assignment is cancelled, the
// worker has stopped: the assignment ends.
func (d *daemon) dispatchStopped(ref sessionRef, dispatch int64) {
	kind, assignment, err := d.store.DispatchKind(dispatch)
	if err != nil || kind != store.MessageCancellation {
		return
	}
	d.mu.Lock()
	if d.stopping {
		d.mu.Unlock()
		return
	}
	d.agents.Add(1)
	d.mu.Unlock()
	go func() {
		defer d.agents.Done()
		d.cancelled(ref, assignment)
	}()
}

// cancelled ends the cancelling assignment whose worker, ref's session, has
// stopped: it records what the worker left behind, whether origin's
// assignment branch holds the workspace's commit, and stops the worker.
func (d *daemon) cancelled(ref sessionRef, id int64) {
	a, err := d.store.Assignment(id)
	if err != nil || a.State != store.AssignmentCancelling || a.Worker != ref.address.String() {
		return
	}
	ctx, cancel := context.WithTimeout(d.work, gitTimeout)
	defer cancel()
	left := store.Cancellation{}
	if commit, err := repos.Commit(ctx, a.Workspace.Path, "HEAD"); err != nil {
		d.log.Error("reading the commit of a cancelled assignment's workspace", "assignment", id, "error", err.Error())
	} else {
		left.Commit = commit
		left.Pushed = d.pushed(ctx, a, commit)
	}
	if !left.Pushed {
		d.log.Warn("a cancelled assignment's worker stopped without its assignment branch on origin at the workspace's commit", "assignment", id, "worker", a.Worker, "branch", a.Workspace.Branch, "commit", left.Commit)
		return
	}
	d.mu.Lock()
	_, err = d.store.AssignmentCancelled(id, left, time.Now())
	d.mu.Unlock()
	if err != nil {
		d.log.Error("ending a cancelled assignment", "assignment", id, "error", err.Error())
		return
	}
	d.log.Info("assignment cancelled", "assignment", id, "job", a.Job, "worker", a.Worker, "commit", left.Commit, "pushed", left.Pushed)
	d.stopWorker(a)
}

// pushed reports whether origin's branch of a holds commit, as AsmAI's clone
// fetches it now.
func (d *daemon) pushed(ctx context.Context, a store.Assignment, commit string) bool {
	j, err := d.store.Job(a.Job)
	if err != nil {
		return false
	}
	repository, err := d.store.Repository(j.Repository)
	if err != nil {
		return false
	}
	if err := repos.Fetch(ctx, repository.Clone); err != nil {
		d.log.Error("fetching origin for a cancelled assignment", "assignment", a.ID, "error", err.Error())
		return false
	}
	if _, err := repos.OriginTip(ctx, repository.Clone, a.Workspace.Branch); err != nil {
		return false
	}
	held, err := repos.Contains(ctx, repository.Clone, "refs/remotes/origin/"+a.Workspace.Branch, commit)
	return err == nil && held
}
