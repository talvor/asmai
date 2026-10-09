// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"time"

	"github.com/talvor/asmai/internal/repos"
	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/store"
)

// gitTimeout is how long making a job branch, a view or a workspace may
// take: a fetch from origin is the most of it.
const gitTimeout = 5 * time.Minute

// assignmentCommand serves assign, effect, result and blocked.
func (d *daemon) assignmentCommand(conn *net.UnixConn, req Request) {
	var resp Response
	var err error
	var notify string
	switch req.Command {
	case CommandAssign:
		conn.SetDeadline(time.Now().Add(gitTimeout + time.Minute))
		var a store.Assignment
		var dispatch store.Dispatch
		if a, dispatch, err = d.assign(req); err == nil {
			resp.Assignment, resp.Dispatch = &a, &dispatch
		}
	case CommandEffect:
		var e store.Effect
		if e, err = d.effect(req); err == nil {
			resp.Effect = &e
		}
	case CommandResult, CommandBlocked:
		conn.SetDeadline(time.Now().Add(gitTimeout + time.Minute))
		var r store.Report
		var dispatch store.Dispatch
		if r, dispatch, err = d.report(req); err == nil {
			resp.Report, resp.Dispatch = &r, &dispatch
			notify = dispatch.Agent
		}
	}
	if err != nil {
		if !errors.As(err, new(*userError)) {
			d.log.Error("an assignment command failed", "command", req.Command, "error", err.Error())
		}
		resp.Error = err.Error()
	}
	reply(conn, resp)
	if notify != "" {
		d.nudge(notify)
	}
}

// caller returns the session that made req and its agent's slot, or why it
// is not the agent's current session. d.mu is held.
func (d *daemon) caller(req Request) (sessionRef, *leader, error) {
	ref, ok := d.sessions[req.Session]
	l := d.leaders[ref.address.String()]
	if !ok || l == nil || l.session == nil || l.generation != ref.generation || d.stopping {
		return sessionRef{}, nil, refuse("unknown or superseded agent session credential")
	}
	return ref, l, nil
}

// assign gives the writing assignment req describes to a worker of
// Engineering. It makes the job's branch if it has none, takes the lowest
// free worker number and slot, makes the workspace of AsmAI's clone on the
// assignment's own branch from the job branch's tip, records the assignment
// with the dispatch that delivers it, and starts the worker in the workspace.
// The worker is nudged at its first input boundary.
func (d *daemon) assign(req Request) (store.Assignment, store.Dispatch, error) {
	engineering := roles.LeaderOf(roles.Engineering)
	d.mu.Lock()
	ref, _, err := d.caller(req)
	d.mu.Unlock()
	if err != nil {
		return store.Assignment{}, store.Dispatch{}, err
	}
	if ref.address != engineering {
		return store.Assignment{}, store.Dispatch{}, refuse("only %s may assign writing work", engineering)
	}
	if req.Job <= 0 || strings.TrimSpace(req.Outcome) == "" || len(req.Criteria) == 0 {
		return store.Assignment{}, store.Dispatch{}, refuse("an assignment needs a job, an outcome and acceptance criteria")
	}

	d.assignMu.Lock()
	defer d.assignMu.Unlock()
	j, err := d.store.Job(req.Job)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Assignment{}, store.Dispatch{}, refuse("job %d does not exist; `asmai jobs` lists the jobs", req.Job)
	}
	if err != nil {
		return store.Assignment{}, store.Dispatch{}, err
	}
	if j.State != store.JobOpen {
		return store.Assignment{}, store.Dispatch{}, refuse("job %d is %s, so it takes no new assignment", j.Number, j.State)
	}
	if j.Repository == "" {
		return store.Assignment{}, store.Dispatch{}, refuse("job %d has no repository, so it has no writing assignment", j.Number)
	}
	repository, err := d.store.Repository(j.Repository)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Assignment{}, store.Dispatch{}, notRegistered(j.Repository)
	}
	if err != nil {
		return store.Assignment{}, store.Dispatch{}, err
	}

	ctx, cancel := context.WithTimeout(d.work, gitTimeout)
	defer cancel()
	branch, err := d.ensureJobBranch(ctx, j, repository)
	if err != nil {
		return store.Assignment{}, store.Dispatch{}, err
	}
	id, err := d.store.NextAssignmentID()
	if err != nil {
		return store.Assignment{}, store.Dispatch{}, err
	}
	number, slot, err := d.store.Allocate(roles.Engineering)
	if err != nil {
		return store.Assignment{}, store.Dispatch{}, err
	}
	worker := roles.WorkerOf(roles.Engineering, number)
	dir := filepath.Join(d.cfg.Paths.Workspaces, fmt.Sprintf("job-%d", j.Number), fmt.Sprintf("assignment-%d", id))
	workspace := store.Workspace{
		Slot:   slot,
		Path:   filepath.Join(dir, "repo"),
		Tmp:    filepath.Join(dir, "tmp"),
		Branch: fmt.Sprintf("asmai/job-%d/%d", j.Number, id),
		Base:   branch.Tip,
	}
	if err := repos.AddWorkspace(ctx, repository.Clone, workspace.Path, workspace.Branch, workspace.Base); err != nil {
		return store.Assignment{}, store.Dispatch{}, err
	}
	undo := func() {
		if err := repos.RemoveWorkspace(context.WithoutCancel(ctx), repository.Clone, workspace.Path, workspace.Branch); err != nil {
			d.log.Error("undoing an assignment's workspace", "assignment", id, "error", err.Error())
		}
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	// The credential is checked again, as the checks and git took time.
	if _, _, err := d.caller(req); err != nil {
		undo()
		return store.Assignment{}, store.Dispatch{}, err
	}
	a, dispatch, err := d.store.AssignmentCreated(store.NewAssignment{
		ID: id, Job: j.Number, Owner: ref.address.String(), Worker: worker.String(),
		Outcome: req.Outcome, Criteria: req.Criteria, Workspace: workspace,
	}, time.Now())
	if err != nil {
		undo()
		return store.Assignment{}, store.Dispatch{}, fmt.Errorf("recording the assignment: %w", err)
	}
	d.log.Info("assignment created", "assignment", a.ID, "job", j.Number, "worker", a.Worker, "slot", slot, "branch", workspace.Branch, "workspace", workspace.Path)
	if err := d.ensureAgent(worker); err != nil {
		// The assignment is recorded and its dispatch waits in the worker's
		// inbox; the worker starts when the factory can run it.
		d.log.Error("starting the worker", "agent", worker.String(), "assignment", a.ID, "dispatch", dispatch.ID, "error", err)
	}
	return a, dispatch, nil
}

// ensureJobBranch returns job's branch, making it if the job has none: from
// the default branch as AsmAI's clone fetches it from origin, recording the
// commit it started from. It also keeps the leaders' read-only view of the
// job at the branch's tip. assignMu is held.
func (d *daemon) ensureJobBranch(ctx context.Context, j store.Job, repository store.Repository) (store.JobBranch, error) {
	branch, err := d.store.JobBranch(j.Number)
	switch {
	case err == nil:
		if err := repos.EnsureView(ctx, repository.Clone, branch.View, branch.Tip); err != nil {
			return store.JobBranch{}, err
		}
		return branch, nil
	case !errors.Is(err, sql.ErrNoRows):
		return store.JobBranch{}, err
	}
	if err := repos.Fetch(ctx, repository.Clone); err != nil {
		return store.JobBranch{}, err
	}
	tip, err := repos.OriginTip(ctx, repository.Clone, repository.DefaultBranch)
	if err != nil {
		return store.JobBranch{}, err
	}
	name := JobBranchName(j)
	startedFrom, err := repos.Commit(ctx, repository.Clone, "refs/heads/"+name)
	if err != nil {
		startedFrom = tip
		if err := repos.CreateBranch(ctx, repository.Clone, name, startedFrom); err != nil {
			return store.JobBranch{}, err
		}
	}
	view := filepath.Join(d.cfg.Paths.Views, fmt.Sprintf("job-%d", j.Number))
	if err := repos.EnsureView(ctx, repository.Clone, view, startedFrom); err != nil {
		return store.JobBranch{}, err
	}
	branch, err = d.store.JobBranchMade(store.JobBranch{Job: j.Number, Repository: repository.Name, Name: name, StartedFrom: startedFrom, View: view}, time.Now())
	if err != nil {
		return store.JobBranch{}, fmt.Errorf("recording the job branch: %w", err)
	}
	d.log.Info("job branch made", "job", j.Number, "branch", name, "started_from", startedFrom, "view", view)
	return branch, nil
}

// JobBranchName names job's branch asmai/job-<n>-<slug>, with the slug taken
// from the job's title, which is Coordination's reading of the user's words.
func JobBranchName(j store.Job) string {
	return fmt.Sprintf("asmai/job-%d-%s", j.Number, slug(j.Reading))
}

// maxSlug is the most a branch name's slug takes of a job's title.
const maxSlug = 40

// slug makes text into lower-case words of letters and digits joined by
// hyphens, at most maxSlug long, or "job" when nothing is left.
func slug(text string) string {
	var b strings.Builder
	hyphen := false
	for _, r := range strings.ToLower(text) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			if hyphen && b.Len() > 0 {
				b.WriteByte('-')
			}
			hyphen = false
			b.WriteRune(r)
		default:
			hyphen = true
		}
		if b.Len() >= maxSlug {
			break
		}
	}
	if b.Len() == 0 {
		return "job"
	}
	return strings.TrimRight(b.String()[:min(b.Len(), maxSlug)], "-")
}

// effect records an effect req's session made in its current dispatch.
func (d *daemon) effect(req Request) (store.Effect, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	ref, l, err := d.caller(req)
	if err != nil {
		return store.Effect{}, err
	}
	if l.currentDispatch == 0 {
		return store.Effect{}, refuse("%s has no current dispatch to tag the effect with; record effects while working on a dispatch you fetched with `asmai inbox`", ref.address)
	}
	e, err := d.store.EffectRecorded(ref.address.String(), ref.generation, l.currentDispatch, req.Kind, req.Ref, time.Now())
	if err != nil {
		return store.Effect{}, refuse("%v", err)
	}
	return e, nil
}

// report records the result or the blocked report of the assignment req's
// worker carries in its current dispatch. A result submits the assignment.
// The report goes to the owning leader, which is started if it is not
// running; the returned dispatch is the leader's, to nudge.
func (d *daemon) report(req Request) (store.Report, store.Dispatch, error) {
	if req.Report == nil {
		return store.Report{}, store.Dispatch{}, refuse("asmai %s needs a report", req.Command)
	}
	kind := store.ReportResult
	if req.Command == CommandBlocked {
		kind = store.ReportBlocked
	}
	d.mu.Lock()
	ref, l, err := d.caller(req)
	current := int64(0)
	if err == nil {
		current = l.currentDispatch
	}
	d.mu.Unlock()
	if err != nil {
		return store.Report{}, store.Dispatch{}, err
	}
	if current == 0 {
		return store.Report{}, store.Dispatch{}, refuse("%s has no current dispatch to report on; report on the assignment you fetched with `asmai inbox`", ref.address)
	}
	a, err := d.store.DispatchAssignment(current)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Report{}, store.Dispatch{}, refuse("dispatch %d is about no assignment, so there is nothing to report", current)
	}
	if err != nil {
		return store.Report{}, store.Dispatch{}, err
	}
	if a.Worker != ref.address.String() {
		return store.Report{}, store.Dispatch{}, refuse("assignment %d belongs to %s, not %s", a.ID, a.Worker, ref.address)
	}
	branch, err := d.store.JobBranch(a.Job)
	if err != nil {
		return store.Report{}, store.Dispatch{}, err
	}

	// What the daemon sees of the workspace is the commit the report is at.
	ctx, cancel := context.WithTimeout(d.work, gitTimeout)
	defer cancel()
	commit, err := repos.Commit(ctx, a.Workspace.Path, "HEAD")
	if err != nil {
		return store.Report{}, store.Dispatch{}, fmt.Errorf("reading the commit of %s: %w", a.Workspace.Path, err)
	}
	uncommitted, err := repos.Uncommitted(ctx, a.Workspace.Path)
	if err != nil {
		return store.Report{}, store.Dispatch{}, err
	}
	if len(uncommitted) > 0 {
		return store.Report{}, store.Dispatch{}, refuse("the workspace has changes that are not committed, so the report would not be at its commit: commit them, push %s, record the push with `asmai effect`, and submit again\n%s", a.Workspace.Branch, strings.Join(uncommitted, "\n"))
	}
	tookIn, err := repos.Contains(ctx, a.Workspace.Path, "HEAD", branch.Tip)
	if err != nil {
		return store.Report{}, store.Dispatch{}, err
	}
	if !tookIn {
		return store.Report{}, store.Dispatch{}, refuse("the assignment branch does not contain the job branch's tip %s; take it in and submit again", branch.Tip)
	}
	job, err := d.store.Job(a.Job)
	if err != nil {
		return store.Report{}, store.Dispatch{}, err
	}
	repository, err := d.store.Repository(job.Repository)
	if err != nil {
		return store.Report{}, store.Dispatch{}, err
	}
	if err := repos.Fetch(ctx, repository.Clone); err != nil {
		return store.Report{}, store.Dispatch{}, err
	}
	remoteBranch := "refs/remotes/origin/" + a.Workspace.Branch
	if _, err := repos.OriginTip(ctx, repository.Clone, a.Workspace.Branch); err != nil {
		return store.Report{}, store.Dispatch{}, refuse("origin does not have the assignment branch %s; push it and submit again", a.Workspace.Branch)
	}
	pushedCommit, err := repos.Contains(ctx, repository.Clone, remoteBranch, commit)
	if err != nil {
		return store.Report{}, store.Dispatch{}, err
	}
	if !pushedCommit {
		return store.Report{}, store.Dispatch{}, refuse("origin's assignment branch %s does not contain reported commit %s; push it and submit again", a.Workspace.Branch, commit)
	}
	input := *req.Report
	input.Artifacts = append([]string{fmt.Sprintf("branch %s at %s", a.Workspace.Branch, commit)}, input.Artifacts...)

	d.mu.Lock()
	defer d.mu.Unlock()
	if _, l, err := d.caller(req); err != nil {
		return store.Report{}, store.Dispatch{}, err
	} else if l.currentDispatch != current {
		return store.Report{}, store.Dispatch{}, refuse("dispatch %d is no longer %s's current dispatch", current, ref.address)
	}
	r, dispatch, err := d.store.ReportSubmitted(store.Submission{
		Agent: ref.address.String(), Generation: ref.generation, Dispatch: current, Kind: kind,
		Commit: commit, JobTip: branch.Tip, TookInTip: tookIn, Input: input,
	}, time.Now())
	if err != nil {
		return store.Report{}, store.Dispatch{}, refuse("%v", err)
	}
	d.log.Info("report submitted", "kind", kind, "assignment", a.ID, "job", a.Job, "worker", ref.address.String(), "commit", commit, "dispatch", dispatch.ID)
	if owner, err := roles.ParseAddress(dispatch.Agent); err == nil {
		if err := d.ensureAgent(owner); err != nil {
			d.log.Error("starting the owning leader", "agent", dispatch.Agent, "dispatch", dispatch.ID, "error", err)
		}
	}
	return r, dispatch, nil
}

// ensureAgent starts the session of the agent at address unless it is
// running: a role's leader, or a worker in the workspace of the assignment it
// carries. d.mu is held.
func (d *daemon) ensureAgent(address roles.Address) error {
	if address.Name == roles.Leader {
		return d.ensureRoleLeader(address.Role)
	}
	return d.ensureWorker(address)
}

func (d *daemon) ensureWorker(address roles.Address) error {
	if _, ok := address.WorkerNumber(); !ok {
		return fmt.Errorf("%s is neither a leader nor a worker", address)
	}
	a, err := d.store.LiveAssignmentOf(address.String())
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%s carries no assignment, so there is nothing to start it for", address)
	}
	if err != nil {
		return err
	}
	l := d.agent(address)
	if l.session != nil {
		if l.assignment != a.ID {
			return fmt.Errorf("%s is still running assignment %d, not assignment %d", address, l.assignment, a.ID)
		}
		return nil
	}
	checks, cfg, install := d.checks()
	if failed(checks) {
		return fmt.Errorf("cannot start %s: factory checks failed", address)
	}
	return d.startWorker(l, cfg.Roles[address.Role], install, a)
}
