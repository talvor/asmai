// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	if !ownsWorkers(ref.address) {
		return Response{}, "", refuse("only %s, %s, %s, %s or %s may run asmai %s", roles.LeaderOf(roles.Engineering), roles.LeaderOf(roles.Planning), roles.LeaderOf(roles.Research), roles.LeaderOf(roles.Coordination), roles.LeaderOf(roles.Quality), req.Command)
	}
	if req.EndsJob && req.Command != CommandAccept {
		return Response{}, "", refuse("--ends-job belongs to asmai accept: only an acceptance is the leader accepting the job's outcome")
	}
	if req.Prototype && req.Command != CommandAccept {
		return Response{}, "", refuse("--prototype belongs to asmai accept: only an acceptance can leave a prototype on its own branch")
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
	case CommandRedispatch:
		resp, err = d.redispatch(req)
	default:
		resp, err = d.returnOrCancel(req, ref)
	}
	// A rejection, a cancellation or a redispatch goes to the assignment's
	// worker, and an accepted validation to the delivery owner.
	if err == nil && resp.Dispatch != nil {
		notify = resp.Dispatch.Agent
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
	// Work that depends on a pending decision waits for its answer; the
	// store refuses the acceptance too, but only after the branch has moved.
	if pending, err := d.store.BlockingDecisions(a.ID); err != nil {
		return Response{}, err
	} else if len(pending) > 0 {
		return Response{}, refuse("assignment %d waits for decision request %d (version %d), which is pending, so its result is not accepted yet; independent work goes on", a.ID, pending[0].Request, pending[0].Version)
	}
	if req.Prototype && (a.Scratch || a.ReadOnly) {
		return Response{}, refuse("assignment %d is a validation or works in a scratch workspace, so it has no assignment branch for a prototype to stay on", a.ID)
	}
	if a.Scratch {
		return d.acceptScratch(req, ref, a)
	}
	if req.EndsJob {
		return Response{}, refuse("job %d has a repository, so it does not end on its leader's acceptance: it ends in its delivery", a.Job)
	}
	if a.ReadOnly {
		return d.acceptValidation(req, ref, a)
	}
	if req.Prototype {
		return d.acceptPrototype(req, ref, a)
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
			// The job branch moved after the result was submitted: the work
			// goes back to its worker to take in the new tip, which is not a
			// rejection (02 rule 31, 05 rule 19).
			return d.returnMovedTip(req, ref, a, branch)
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

// returnMovedTip returns the submitted result of the assignment a to its
// worker, which is started if it is not running, because the job branch moved
// to its tip after the result was submitted. Nothing is accepted, no decision
// is recorded and nothing moved. The returned dispatch is the worker's, to
// nudge. assignMu is held.
func (d *daemon) returnMovedTip(req Request, ref sessionRef, a store.Assignment, branch store.JobBranch) (Response, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, _, err := d.caller(req); err != nil {
		return Response{}, err
	}
	dispatch, err := d.store.ResultReturned(a.ID, ref.address.String(), branch.Tip, time.Now())
	if err != nil {
		return Response{}, refuse("%v", err)
	}
	d.log.Info("result returned to its worker: the job branch moved after it was submitted", "assignment", a.ID, "job", a.Job, "worker", a.Worker, "tip", branch.Tip, "leader", ref.address.String(), "dispatch", dispatch.ID)
	if worker, err := roles.ParseAddress(a.Worker); err == nil {
		if err := d.ensureAgent(worker); err != nil {
			d.log.Error("starting the assignment's worker", "agent", a.Worker, "dispatch", dispatch.ID, "error", err.Error())
		}
	}
	a, err = d.store.Assignment(a.ID)
	if err != nil {
		return Response{}, err
	}
	return Response{Assignment: &a, Dispatch: &dispatch, JobBranch: &branch, Returned: true}, nil
}

// acceptPrototype records the acceptance of the result of the writing
// assignment a as a prototype that stays on its own assignment branch on
// origin, which must hold the result's commit: the job branch does not move,
// so the prototype never reaches the job's pull request, and the leader gives
// its verdict as a note on the job branch, with a pointer to that branch,
// through an assignment of its own. The assignment ends, its worker is
// stopped and its workspace removed. assignMu is held.
func (d *daemon) acceptPrototype(req Request, ref sessionRef, a store.Assignment) (Response, error) {
	result, err := d.store.LatestResult(a.ID)
	if err != nil {
		return Response{}, refuse("%v", err)
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
	if err := repos.Fetch(ctx, repository.Clone); err != nil {
		return Response{}, err
	}
	if _, err := repos.OriginTip(ctx, repository.Clone, a.Workspace.Branch); err != nil {
		return Response{}, refuse("origin does not have the assignment branch %s, where a prototype stays; the worker pushes it and submits again, so reject the result", a.Workspace.Branch)
	}
	pushed, err := repos.Contains(ctx, repository.Clone, "refs/remotes/origin/"+a.Workspace.Branch, result.Commit)
	if err != nil {
		return Response{}, err
	}
	if !pushed {
		return Response{}, refuse("origin's assignment branch %s does not contain the result's commit %s, so the prototype is not on its branch yet; the worker pushes it and submits again, so reject the result", a.Workspace.Branch, result.Commit)
	}
	d.mu.Lock()
	if _, _, err := d.caller(req); err != nil {
		d.mu.Unlock()
		return Response{}, err
	}
	decision, prototype, err := d.store.PrototypeKept(store.Acceptance{Assignment: a.ID, Leader: ref.address.String(), Reasons: req.Reasons}, time.Now())
	d.mu.Unlock()
	if err != nil {
		return Response{}, refuse("%v", err)
	}
	d.log.Info("result accepted as a prototype that stays on its assignment branch; the job branch did not move", "assignment", a.ID, "job", a.Job, "branch", prototype.Branch, "commit", prototype.Commit, "leader", ref.address.String())
	d.stopWorker(a)
	if err := d.removeWorkspace(ctx, a, repository); err != nil {
		return Response{}, err
	}
	a, err = d.store.Assignment(a.ID)
	if err != nil {
		return Response{}, err
	}
	branch, err := d.store.JobBranch(a.Job)
	if err != nil {
		return Response{}, err
	}
	return Response{Assignment: &a, Decision: &decision, JobBranch: &branch, Prototype: &prototype}, nil
}

// acceptScratch records the acceptance of the result of the assignment a, which
// works in a scratch workspace of a job with no repository, by its owning
// leader. Nothing moves, as there is no job branch: the assignment ends, its
// worker is stopped and its temporary directory removed, and its scratch
// workspace is kept with the results in it. With req.EndsJob the owning
// leader accepts the job's outcome against its mandate: the job ends, and
// Coordination's leader is told where the results are, with the dispatch the
// response carries. assignMu is held.
func (d *daemon) acceptScratch(req Request, ref sessionRef, a store.Assignment) (Response, error) {
	j, err := d.store.Job(a.Job)
	if err != nil {
		return Response{}, err
	}
	body := ""
	if req.EndsJob {
		if body, err = d.scratchOutcomeBody(j, a, ref.address.String(), req.Reasons); err != nil {
			return Response{}, err
		}
	}
	d.mu.Lock()
	if _, _, err := d.caller(req); err != nil {
		d.mu.Unlock()
		return Response{}, err
	}
	decision, dispatch, err := d.store.ScratchResultAccepted(store.Acceptance{Assignment: a.ID, Leader: ref.address.String(), Reasons: req.Reasons, EndsJob: req.EndsJob}, body, time.Now())
	d.mu.Unlock()
	if err != nil {
		return Response{}, refuse("%v", err)
	}
	d.log.Info("result accepted in a scratch workspace", "assignment", a.ID, "job", a.Job, "workspace", a.Workspace.Path, "leader", ref.address.String(), "ends_job", req.EndsJob)
	d.stopWorker(a)
	if err := os.RemoveAll(a.Workspace.Tmp); err != nil {
		d.log.Error("removing an accepted assignment's temporary directory", "assignment", a.ID, "tmp", a.Workspace.Tmp, "error", err.Error())
		return Response{}, err
	}
	a, err = d.store.Assignment(a.ID)
	if err != nil {
		return Response{}, err
	}
	resp := Response{Assignment: &a, Decision: &decision}
	if dispatch != nil {
		resp.Dispatch = dispatch
		j, err = d.store.Job(a.Job)
		if err != nil {
			return Response{}, err
		}
		resp.Job = &j
		d.log.Info("job ended on its leader's acceptance; its scratch workspaces are kept", "job", j.Number, "leader", ref.address.String(), "dispatch", dispatch.ID)
	}
	return resp, nil
}

// scratchOutcomeBody is what Coordination's leader is told when the owning
// leader accepts the outcome of the job j, which has no repository, with
// the result of the assignment a, and the reasons it gave: where the results
// are, in which scratch workspaces, and how the user removes them (06 rule 39).
func (d *daemon) scratchOutcomeBody(j store.Job, a store.Assignment, leader string, reasons []string) (string, error) {
	assignments, err := d.store.AssignmentsForJob(j.Number)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Job %d has ended: its owning leader, %s, accepted its outcome against its %s mandate (%s). ", j.Number, leader, j.Mandate, strings.Join(reasons, "; "))
	fmt.Fprintf(&b, "It has no repository, so it has no pull request: the results are in the scratch workspaces of its assignments, which stay until the user removes them with `asmai job clean %d`, which is refused while the job is open.\n", j.Number)
	for _, other := range assignments {
		// The assignment a is accepted in the same step that tells
		// Coordination, so it is still submitted here.
		if other.ID != a.ID && other.State != store.AssignmentAccepted {
			continue
		}
		result, err := d.store.LatestResult(other.ID)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "- assignment %d (%s), %s: the results are in %s\n", other.ID, other.Worker, other.Outcome, other.Workspace.Path)
		for _, artifact := range result.Artifacts {
			fmt.Fprintf(&b, "  artifact: %s\n", artifact)
		}
		for _, evidence := range result.Evidence {
			fmt.Fprintf(&b, "  evidence: %s\n", evidence)
		}
		for _, gap := range result.Gaps {
			fmt.Fprintf(&b, "  gap: %s\n", gap)
		}
	}
	fmt.Fprintf(&b, "Tell the user, in a line tagged with job %d, where the results are, and that `asmai job clean %d` removes them.", j.Number, j.Number)
	return b.String(), nil
}

// acceptValidation records the acceptance of the validation report of the
// read-only assignment a by its owning leader, Quality's. The report's
// findings are recorded, and the earlier blocking findings it confirms
// resolved are cleared. The report goes to Engineering's leader, the delivery
// owner, which is started if it is not running, and the returned dispatch is
// its, to nudge. The assignment has ended, so its worker is stopped and its
// read-only workspace removed. Nothing about the job branch changes. assignMu
// is held.
func (d *daemon) acceptValidation(req Request, ref sessionRef, a store.Assignment) (Response, error) {
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
	// The report goes to the delivery owner, or for an early review to the
	// leader that asked for it.
	delivery := roles.LeaderOf(roles.Engineering)
	if a.Requester != "" {
		if delivery, err = roles.ParseAddress(a.Requester); err != nil {
			return Response{}, err
		}
	}
	d.mu.Lock()
	if _, _, err := d.caller(req); err != nil {
		d.mu.Unlock()
		return Response{}, err
	}
	decision, dispatch, err := d.store.ValidationAccepted(store.Acceptance{Assignment: a.ID, Leader: ref.address.String(), Reasons: req.Reasons}, delivery.String(), time.Now())
	if err != nil {
		d.mu.Unlock()
		return Response{}, refuse("%v", err)
	}
	d.log.Info("validation accepted", "assignment", a.ID, "job", a.Job, "commit", decision.Commit, "leader", ref.address.String(), "dispatch", dispatch.ID, "to", delivery.String())
	if err := d.ensureAgent(delivery); err != nil {
		d.log.Error("starting the delivery owner", "agent", delivery.String(), "dispatch", dispatch.ID, "error", err.Error())
	}
	d.mu.Unlock()

	d.stopWorker(a)
	ctx, cancel := context.WithTimeout(d.work, gitTimeout)
	defer cancel()
	if err := d.removeWorkspace(ctx, a, repository); err != nil {
		return Response{}, err
	}
	a, err = d.store.Assignment(a.ID)
	if err != nil {
		return Response{}, err
	}
	return Response{Assignment: &a, Decision: &decision, Dispatch: &dispatch}, nil
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

// redispatch serves redispatch: the owning leader sends its assignment, which
// is active or needs reconciliation, to its worker again in a new dispatch,
// and the worker is started if it is not running. The returned dispatch is the
// worker's, to nudge.
func (d *daemon) redispatch(req Request) (Response, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	ref, _, err := d.caller(req)
	if err != nil {
		return Response{}, err
	}
	dispatch, err := d.store.AssignmentRedispatched(req.Assignment, ref.address.String(), req.Reasons, time.Now())
	if err != nil {
		return Response{}, refuse("%v", err)
	}
	a, err := d.store.Assignment(req.Assignment)
	if err != nil {
		return Response{}, err
	}
	d.log.Info("assignment redispatched", "assignment", a.ID, "job", a.Job, "worker", a.Worker, "leader", ref.address.String(), "dispatch", dispatch.ID)
	if worker, err := roles.ParseAddress(a.Worker); err == nil {
		if err := d.ensureAgent(worker); err != nil {
			d.log.Error("starting the assignment's worker", "agent", a.Worker, "dispatch", dispatch.ID, "error", err.Error())
		}
	}
	return Response{Assignment: &a, Dispatch: &dispatch}, nil
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

// removeWorkspace removes the workspace of a, which has ended, and its
// temporary directory, and journals it: a writing assignment's, whose work is
// on the job branch, with its branch, which stays on origin; or a read-only
// assignment's, a clean checkout with no branch.
func (d *daemon) removeWorkspace(ctx context.Context, a store.Assignment, repository store.Repository) error {
	ctx = context.WithoutCancel(ctx)
	remove := func() error {
		return repos.RemoveWorkspace(ctx, repository.Clone, a.Workspace.Path, a.Workspace.Branch)
	}
	if a.ReadOnly {
		remove = func() error { return repos.RemoveReadOnlyWorkspace(ctx, repository.Clone, a.Workspace.Path) }
	}
	if err := remove(); err != nil {
		d.log.Error("removing an ended assignment's workspace", "assignment", a.ID, "workspace", a.Workspace.Path, "error", err.Error())
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
// finished. When the dispatch told a worker its assignment is cancelled, or
// resumed it after a restart while it was, the worker has stopped: the
// assignment ends. Any other dispatch that gave a worker its assignment may
// have ended with no report, which its owning leader is told.
func (d *daemon) dispatchStopped(ref sessionRef, dispatch int64) {
	kind, assignment, err := d.store.DispatchKind(dispatch)
	if err != nil {
		return
	}
	// A cancellation that was under way when the factory stopped is resumed
	// in a dispatch of its own, and ends the assignment the same way.
	if kind == store.MessageResumption {
		if a, err := d.store.Assignment(assignment); err == nil && a.State == store.AssignmentCancelling {
			kind = store.MessageCancellation
		}
	}
	if kind != store.MessageCancellation {
		d.workerStopped(ref, dispatch, kind, assignment)
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

// workerStopped is told that the turn ref's session, a worker's, ran for
// dispatch, which is of kind kind and about assignment, has finished. If the
// dispatch gave the worker its active assignment and ended with no result and
// no blocked report, and the worker is not waiting for the answer to a
// decision it asked for, the assignment is active but nothing is working on
// it: its owning leader is told in a message, with what origin holds of the
// assignment branch. Nothing is retried.
func (d *daemon) workerStopped(ref sessionRef, dispatch int64, kind string, assignment int64) {
	if ref.address.Name == roles.Leader || assignment == 0 {
		return
	}
	switch kind {
	case store.MessageAssignment, store.MessageRejection, store.MessageMovedTip, store.MessageResumption, store.MessageRedispatch, store.MessageDecisionAnswer:
	default:
		return
	}
	w, err := d.store.WorkerDispatch(assignment)
	if err != nil || w.Dispatch.ID != dispatch || w.Report != "" || w.Assignment.State != store.AssignmentActive || w.Assignment.Worker != ref.address.String() {
		return
	}
	if waiting, err := d.store.PendingRequestsOf(ref.address.String()); err != nil || len(waiting) > 0 {
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
		d.tellStalled(w.Assignment, dispatch)
	}()
}

// tellStalled tells the owning leader of a that its worker's turn for dispatch
// ended with no result and no blocked report, and starts the leader if it is
// not running.
func (d *daemon) tellStalled(a store.Assignment, dispatch int64) {
	ctx, cancel := context.WithTimeout(d.work, gitTimeout)
	defer cancel()
	body := fmt.Sprintf("%s's turn for dispatch %d of assignment %d of job %d ended with no result and no blocked report. The assignment is still active and nothing is retried.\n%s\n"+
		"Look at `asmai brief %d` and the effects %s recorded. If the cause was outside the factory and is fixed, send the assignment to %s again in a new dispatch with `asmai redispatch --assignment %d --reason <why>`; otherwise decide with `asmai reject`, `asmai cancel` or by assigning the work again.",
		a.Worker, dispatch, a.ID, a.Job, d.originHolds(ctx, a), a.Job, a.Worker, a.Worker, a.ID)
	d.mu.Lock()
	if d.stopping {
		d.mu.Unlock()
		return
	}
	told, err := d.store.WorkerStalled(a.ID, dispatch, body, time.Now())
	if err != nil {
		d.mu.Unlock()
		d.log.Error("telling an owning leader that its worker stopped without a report", "assignment", a.ID, "dispatch", dispatch, "error", err.Error())
		return
	}
	d.log.Warn("a worker stopped without a result or a blocked report", "assignment", a.ID, "job", a.Job, "worker", a.Worker, "owner", a.Owner, "dispatch", dispatch, "told", told.ID)
	if owner, err := roles.ParseAddress(a.Owner); err == nil {
		if err := d.ensureAgent(owner); err != nil {
			d.log.Error("starting the owning leader to tell it", "agent", a.Owner, "dispatch", told.ID, "error", err.Error())
		}
	}
	d.mu.Unlock()
	d.nudge(a.Owner)
}

// originHolds says what origin holds of the assignment branch of a, whose
// worker stopped without a report, against the commit its workspace is at.
func (d *daemon) originHolds(ctx context.Context, a store.Assignment) string {
	switch {
	case a.Scratch:
		return fmt.Sprintf("Its workspace %s is a scratch directory with no repository, no branch and no commit.", a.Workspace.Path)
	case a.ReadOnly:
		return fmt.Sprintf("Its workspace %s is a read-only checkout fixed at %s, with no branch on origin.", a.Workspace.Path, a.Commit)
	}
	commit, err := repos.Commit(ctx, a.Workspace.Path, "HEAD")
	if err != nil {
		return fmt.Sprintf("Its workspace %s could not be read (%v), so whether origin holds its work is not known.", a.Workspace.Path, err)
	}
	uncommitted, err := repos.Uncommitted(ctx, a.Workspace.Path)
	left := ""
	if err == nil && len(uncommitted) > 0 {
		left = fmt.Sprintf(" It also has %d uncommitted change(s), which no commit holds.", len(uncommitted))
	}
	j, err := d.store.Job(a.Job)
	if err != nil {
		return fmt.Sprintf("Its workspace is at commit %s; whether origin holds it is not known: %v.%s", commit, err, left)
	}
	repository, err := d.store.Repository(j.Repository)
	if err != nil {
		return fmt.Sprintf("Its workspace is at commit %s; whether origin holds it is not known: %v.%s", commit, err, left)
	}
	if err := repos.Fetch(ctx, repository.Clone); err != nil {
		return fmt.Sprintf("Its workspace is at commit %s; origin could not be read, so whether it holds the commit is not known: %v.%s", commit, err, left)
	}
	tip, err := repos.OriginTip(ctx, repository.Clone, a.Workspace.Branch)
	if err != nil {
		return fmt.Sprintf("Its workspace is at commit %s, and origin has no branch %s: the work is not pushed.%s", commit, a.Workspace.Branch, left)
	}
	if held, err := repos.Contains(ctx, repository.Clone, "refs/remotes/origin/"+a.Workspace.Branch, commit); err != nil || !held {
		return fmt.Sprintf("Its workspace is at commit %s, and origin's branch %s is at %s, which does not hold it: the commit is not pushed.%s", commit, a.Workspace.Branch, tip, left)
	}
	return fmt.Sprintf("Its workspace is at commit %s, and origin's branch %s is at %s, which holds it: the work is pushed.%s", commit, a.Workspace.Branch, tip, left)
}

// cancelled ends the cancelling assignment whose worker, ref's session, has
// stopped: it records what the worker left behind, whether origin's
// assignment branch holds the workspace's commit, and stops the worker.
func (d *daemon) cancelled(ref sessionRef, id int64) {
	a, err := d.store.Assignment(id)
	if err != nil || a.State != store.AssignmentCancelling || a.Worker != ref.address.String() {
		return
	}
	if a.Scratch {
		d.cancelledScratch(a)
		return
	}
	ctx, cancel := context.WithTimeout(d.work, gitTimeout)
	defer cancel()
	if a.ReadOnly {
		d.cancelledValidation(ctx, a)
		return
	}
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
	d.cancelledJobEnded(a.Job)
}

// cancelledScratch ends the cancelling assignment a, of a job with no
// repository, whose worker has stopped. It has no branch to push, so nothing
// is waited for: the assignment ends and the worker is stopped. Whatever the
// worker left in the scratch workspace is kept with the job's results.
func (d *daemon) cancelledScratch(a store.Assignment) {
	d.mu.Lock()
	_, err := d.store.AssignmentCancelled(a.ID, store.Cancellation{}, time.Now())
	d.mu.Unlock()
	if err != nil {
		d.log.Error("ending a cancelled scratch assignment", "assignment", a.ID, "error", err.Error())
		return
	}
	d.log.Info("scratch assignment cancelled", "assignment", a.ID, "job", a.Job, "worker", a.Worker, "workspace", a.Workspace.Path)
	d.stopWorker(a)
	if err := os.RemoveAll(a.Workspace.Tmp); err != nil {
		d.log.Error("removing a cancelled assignment's temporary directory", "assignment", a.ID, "tmp", a.Workspace.Tmp, "error", err.Error())
	}
}

// cancelledValidation ends the cancelling read-only assignment a, whose worker
// has stopped. It has no branch to push, so nothing is waited for: the
// assignment ends, the worker is stopped and the read-only workspace removed.
func (d *daemon) cancelledValidation(ctx context.Context, a store.Assignment) {
	d.mu.Lock()
	_, err := d.store.AssignmentCancelled(a.ID, store.Cancellation{Commit: a.Commit}, time.Now())
	d.mu.Unlock()
	if err != nil {
		d.log.Error("ending a cancelled validation", "assignment", a.ID, "error", err.Error())
		return
	}
	d.log.Info("validation cancelled", "assignment", a.ID, "job", a.Job, "worker", a.Worker, "commit", a.Commit)
	d.stopWorker(a)
	j, err := d.store.Job(a.Job)
	if err != nil {
		d.log.Error("reading the job of a cancelled validation", "assignment", a.ID, "error", err.Error())
		return
	}
	repository, err := d.store.Repository(j.Repository)
	if err != nil {
		d.log.Error("reading the repository of a cancelled validation", "assignment", a.ID, "error", err.Error())
		return
	}
	d.assignMu.Lock()
	d.removeWorkspace(ctx, a, repository)
	d.assignMu.Unlock()
	d.cancelledJobEnded(a.Job)
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
