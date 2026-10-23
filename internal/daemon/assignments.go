// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/talvor/asmai/internal/config"
	"github.com/talvor/asmai/internal/repos"
	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/skills"
	"github.com/talvor/asmai/internal/store"
)

// gitTimeout is how long making a job branch, a view or a workspace may
// take: a fetch from origin is the most of it.
const gitTimeout = 5 * time.Minute

// assignmentCommand serves assign, effect, result, blocked, accept, reject and
// cancel.
func (d *daemon) assignmentCommand(conn *net.UnixConn, req Request) {
	var resp Response
	var err error
	var notify string
	switch req.Command {
	case CommandAssign:
		conn.SetDeadline(time.Now().Add(3*gitTimeout + ghTimeout + time.Minute))
		var a store.Assignment
		var dispatch store.Dispatch
		if a, dispatch, err = d.assign(req); err == nil {
			d.listSkills(&a)
			resp.Assignment, resp.Dispatch = &a, &dispatch
		}
	case CommandEffect:
		var e store.Effect
		var pr *store.PullRequest
		if req.Kind == store.EffectPullRequest {
			conn.SetDeadline(time.Now().Add(ghTimeout + time.Minute))
			e, pr, err = d.pullRequestEffect(req)
		} else {
			e, err = d.effect(req)
		}
		if err == nil {
			resp.Effect, resp.PullRequest = &e, pr
		}
	case CommandAccept, CommandReject, CommandCancel:
		conn.SetDeadline(time.Now().Add(gitTimeout + stopWait + time.Minute))
		resp, notify, err = d.decide(req)
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

// writingRoles are the roles whose workers carry writing assignments:
// Engineering's change code, Planning's and Research's write notes, and
// Coordination's write a repository's setup files.
var writingRoles = []string{roles.Engineering, roles.Planning, roles.Research, roles.Coordination}

// assign gives the assignment req describes to a worker of the role of the
// leader that makes it: a writing assignment to one of Engineering, Planning,
// Research or, for a repository's setup, Coordination, or with req.ReadOnly a
// validation to one of Quality. It makes
// the job's branch if it has none, takes the lowest free worker number and
// slot, makes the worker's workspace of AsmAI's clone, which is on the
// assignment's own branch from the job branch's tip for a writing assignment
// and a clean checkout fixed at the tip's commit, with no branch, for a
// validation, records the assignment with the dispatch that delivers it, and
// starts the worker in the workspace. A job with no repository has neither
// branch nor clone: the workspace is an empty scratch directory. The worker is
// nudged at its first input boundary.
func (d *daemon) assign(req Request) (store.Assignment, store.Dispatch, error) {
	d.mu.Lock()
	ref, _, err := d.caller(req)
	d.mu.Unlock()
	if err != nil {
		return store.Assignment{}, store.Dispatch{}, err
	}
	role := ref.address.Role
	switch {
	case ref.address.Name != roles.Leader:
		return store.Assignment{}, store.Dispatch{}, refuse("only a role's leader may assign: a worker carries its own assignment")
	case req.ReadOnly && ref.address != roles.LeaderOf(roles.Quality):
		return store.Assignment{}, store.Dispatch{}, refuse("only %s may assign read-only validation", roles.LeaderOf(roles.Quality))
	case !req.ReadOnly && !slices.Contains(writingRoles, role):
		return store.Assignment{}, store.Dispatch{}, refuse("only %s, %s, %s or %s may assign writing work: Quality's validation is read-only, and never changes the work", roles.LeaderOf(roles.Engineering), roles.LeaderOf(roles.Planning), roles.LeaderOf(roles.Research), roles.LeaderOf(roles.Coordination))
	}
	if req.Job <= 0 || strings.TrimSpace(req.Outcome) == "" || len(req.Criteria) == 0 {
		return store.Assignment{}, store.Dispatch{}, refuse("an assignment needs a job, an outcome and acceptance criteria")
	}
	if req.ReadOnly && strings.TrimSpace(req.Commit) == "" {
		return store.Assignment{}, store.Dispatch{}, refuse("a validation is fixed at the job branch's exact head: give --commit")
	}
	if !req.ReadOnly && req.Commit != "" {
		return store.Assignment{}, store.Dispatch{}, refuse("a writing assignment is made from the job branch's tip, so it takes no --commit")
	}

	// The worker's skill list is its role's in the configuration, with its
	// added skills, or the narrower list the assignment gives, and the skill
	// the assignment names is on it.
	d.configMu.Lock()
	cfg := d.applied
	d.configMu.Unlock()
	roleList, err := workerSkills(cfg, role)
	if err != nil {
		return store.Assignment{}, store.Dispatch{}, refuse("%v", err)
	}
	skillList, err := skills.Narrow(role, skills.NamesOf(roleList), req.Skills, req.Skill)
	if err != nil {
		return store.Assignment{}, store.Dispatch{}, refuse("%v", err)
	}
	if len(req.Skills) == 0 {
		skillList = nil
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
	// A job of a notes mandate is authorized only to produce its own outcome,
	// so only its own role's leader gives it writing work (01 rule 17).
	if only, ok := onlyRole[j.Mandate]; ok && !req.ReadOnly && role != only {
		return store.Assignment{}, store.Dispatch{}, refuse("job %d has the %s mandate, which authorizes only its %s outcome: only %s may give its writing work", j.Number, j.Mandate, strings.TrimSuffix(j.Mandate, "-only"), roles.LeaderOf(only))
	}
	// Coordination's workers write only a repository's setup.
	if role == roles.Coordination && j.Mandate != store.MandateSetup {
		return store.Assignment{}, store.Dispatch{}, refuse("job %d has the %s mandate: only %s may give its writing work, and %s gives writing work only for the setup of a repository", j.Number, j.Mandate, roles.LeaderOf(roles.Engineering), roles.LeaderOf(roles.Coordination))
	}
	scratch := j.Repository == ""
	if scratch && req.ReadOnly {
		return store.Assignment{}, store.Dispatch{}, refuse("job %d has no repository, so it has no commit for Quality to validate", j.Number)
	}
	// A skill that starts only on request starts only when the mandate calls
	// for it or the user's own decision does, which goes to the worker too.
	approval, err := d.checkInvocation(j, roleList, req.Skill, req.Approved)
	if err != nil {
		return store.Assignment{}, store.Dispatch{}, err
	}
	records := slices.Clone(req.Records)
	started := ""
	if approval != nil {
		started = req.Skill
		if !slices.Contains(records, approval.ID) {
			records = append(records, approval.ID)
		}
	}
	if err := checkNarrowing(j, roleList, req.Skills, started); err != nil {
		return store.Assignment{}, store.Dispatch{}, err
	}
	if err := d.store.CheckRecords(j.Number, records); err != nil {
		return store.Assignment{}, store.Dispatch{}, refuse("%v", err)
	}
	if scratch {
		return d.assignScratch(ref, req, j, role, skillList, records)
	}
	repository, err := d.store.Repository(j.Repository)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Assignment{}, store.Dispatch{}, notRegistered(j.Repository)
	}
	if err != nil {
		return store.Assignment{}, store.Dispatch{}, err
	}

	ctx, cancel := context.WithTimeout(d.work, 3*gitTimeout+ghTimeout)
	defer cancel()
	if req.ReadOnly {
		if _, err := d.store.JobBranch(j.Number); errors.Is(err, sql.ErrNoRows) {
			return store.Assignment{}, store.Dispatch{}, refuse("job %d has no job branch, so there is nothing to validate", j.Number)
		} else if err != nil {
			return store.Assignment{}, store.Dispatch{}, err
		}
	}
	branch, err := d.ensureJobBranch(ctx, j, repository)
	if err != nil {
		return store.Assignment{}, store.Dispatch{}, err
	}
	// A validation is fixed at the job branch's exact head, and how it goes
	// follows from what changed since Quality last validated the branch. An
	// early review is fixed at the result of the assignment a leader asked
	// Quality to review, and is answered to that leader.
	base, review, since, requester := branch.Tip, "", "", ""
	if req.ReadOnly && req.Assignment != 0 {
		h, err := d.store.EarlyReviewRequest(j.Number, req.Assignment)
		if errors.Is(err, sql.ErrNoRows) {
			return store.Assignment{}, store.Dispatch{}, refuse("no leader has asked Quality, in a handoff you accepted, for an earlier review of assignment %d of job %d: `asmai handoff send --assignment %d` asks for it", req.Assignment, j.Number, req.Assignment)
		}
		if err != nil {
			return store.Assignment{}, store.Dispatch{}, err
		}
		if req.Commit != h.Head {
			return store.Assignment{}, store.Dispatch{}, refuse("the early review of assignment %d is fixed at %s, the exact commit of its result the handoff names, not %s", req.Assignment, h.Head, req.Commit)
		}
		if err := repos.Fetch(ctx, repository.Clone); err != nil {
			return store.Assignment{}, store.Dispatch{}, err
		}
		base, review, requester = h.Head, store.ReviewEarly, h.Sender
	} else if req.ReadOnly {
		if req.Commit != branch.Tip {
			return store.Assignment{}, store.Dispatch{}, refuse("job %d's job branch is at %s, not %s: validation is fixed at the exact head, so assign it at the branch's tip", j.Number, branch.Tip, req.Commit)
		}
		if review, since, err = d.reviewPlan(ctx, j, repository, branch.Tip); err != nil {
			return store.Assignment{}, store.Dispatch{}, refuse("%v", err)
		}
		if review == store.ReviewChecks && req.Skill != "" {
			return store.Assignment{}, store.Dispatch{}, refuse("the changes since %s are a clean merge of the moved base, which needs only the repository's checks re-run at %s: no skill is named for a review the validation does not make", since, branch.Tip)
		}
	} else if req.Assignment != 0 {
		return store.Assignment{}, store.Dispatch{}, refuse("--of names the assignment an early review is of, which only a read-only assignment has")
	}
	if req.ReadOnly {
		if live, err := d.store.LiveValidation(j.Number); err != nil {
			return store.Assignment{}, store.Dispatch{}, err
		} else if live != 0 {
			return store.Assignment{}, store.Dispatch{}, refuse("job %d already has validation %d in progress; it ends before another starts", j.Number, live)
		}
	}
	id, err := d.store.NextAssignmentID()
	if err != nil {
		return store.Assignment{}, store.Dispatch{}, err
	}
	number, slot, err := d.store.Allocate(role)
	if err != nil {
		return store.Assignment{}, store.Dispatch{}, err
	}
	worker := roles.WorkerOf(role, number)
	dir := filepath.Join(d.cfg.Paths.Workspaces, fmt.Sprintf("job-%d", j.Number), fmt.Sprintf("assignment-%d", id))
	workspace := store.Workspace{
		Slot: slot,
		Path: filepath.Join(dir, "repo"),
		Tmp:  filepath.Join(dir, "tmp"),
		Base: base,
	}
	undo := func() {
		if err := repos.RemoveWorkspace(context.WithoutCancel(ctx), repository.Clone, workspace.Path, workspace.Branch); err != nil {
			d.log.Error("undoing an assignment's workspace", "assignment", id, "error", err.Error())
		}
	}
	if req.ReadOnly {
		if err := repos.AddReadOnlyWorkspace(ctx, repository.Clone, workspace.Path, workspace.Base); err != nil {
			return store.Assignment{}, store.Dispatch{}, err
		}
		undo = func() {
			if err := repos.RemoveReadOnlyWorkspace(context.WithoutCancel(ctx), repository.Clone, workspace.Path); err != nil {
				d.log.Error("undoing an assignment's workspace", "assignment", id, "error", err.Error())
			}
		}
	} else {
		workspace.Branch = fmt.Sprintf("asmai/job-%d/%d", j.Number, id)
		if err := repos.AddWorkspace(ctx, repository.Clone, workspace.Path, workspace.Branch, workspace.Base); err != nil {
			return store.Assignment{}, store.Dispatch{}, err
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
		Outcome: req.Outcome, Criteria: req.Criteria, Workspace: workspace, ReadOnly: req.ReadOnly, Skill: req.Skill, Skills: skillList, Records: records,
		Review: review, Since: since, Reviews: req.Assignment, Requester: requester,
	}, time.Now())
	if err != nil {
		undo()
		return store.Assignment{}, store.Dispatch{}, fmt.Errorf("recording the assignment: %w", err)
	}
	d.log.Info("assignment created", "assignment", a.ID, "job", j.Number, "worker", a.Worker, "slot", slot, "branch", workspace.Branch, "workspace", workspace.Path, "read_only", a.ReadOnly, "commit", workspace.Base)
	if err := d.ensureAgent(worker); err != nil {
		return store.Assignment{}, store.Dispatch{}, fmt.Errorf("starting worker %s for assignment %d: %w", worker, a.ID, err)
	}
	return a, dispatch, nil
}

// scratchName is the name of an assignment's scratch workspace within the
// assignment's directory.
const scratchName = "scratch"

// assignScratch gives the writing assignment req describes, of the job j that
// has no repository, to the lowest free worker of role: an empty scratch
// workspace of its own, with no clone, no job branch and no assignment branch
// to make it from (05 rule 8). It records the assignment with its dispatch and
// starts the worker in the workspace. skillList is the worker's narrowed
// list, if any, and records the decision records it is given. assignMu is
// held.
func (d *daemon) assignScratch(ref sessionRef, req Request, j store.Job, role string, skillList []string, records []int64) (store.Assignment, store.Dispatch, error) {
	id, err := d.store.NextAssignmentID()
	if err != nil {
		return store.Assignment{}, store.Dispatch{}, err
	}
	number, slot, err := d.store.Allocate(role)
	if err != nil {
		return store.Assignment{}, store.Dispatch{}, err
	}
	worker := roles.WorkerOf(role, number)
	dir := filepath.Join(d.cfg.Paths.Workspaces, fmt.Sprintf("job-%d", j.Number), fmt.Sprintf("assignment-%d", id))
	workspace := store.Workspace{Slot: slot, Path: filepath.Join(dir, scratchName), Tmp: filepath.Join(dir, "tmp")}
	if err := os.MkdirAll(workspace.Path, 0o700); err != nil {
		return store.Assignment{}, store.Dispatch{}, fmt.Errorf("making the scratch workspace: %w", err)
	}
	undo := func() {
		if err := os.RemoveAll(dir); err != nil {
			d.log.Error("undoing an assignment's scratch workspace", "assignment", id, "error", err.Error())
		}
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	// The credential is checked again, as the checks took time.
	if _, _, err := d.caller(req); err != nil {
		undo()
		return store.Assignment{}, store.Dispatch{}, err
	}
	a, dispatch, err := d.store.AssignmentCreated(store.NewAssignment{
		ID: id, Job: j.Number, Owner: ref.address.String(), Worker: worker.String(),
		Outcome: req.Outcome, Criteria: req.Criteria, Workspace: workspace, Skill: req.Skill, Skills: skillList, Records: records,
	}, time.Now())
	if err != nil {
		undo()
		return store.Assignment{}, store.Dispatch{}, fmt.Errorf("recording the assignment: %w", err)
	}
	d.log.Info("assignment created", "assignment", a.ID, "job", j.Number, "worker", a.Worker, "slot", slot, "workspace", workspace.Path, "scratch", true)
	if err := d.ensureAgent(worker); err != nil {
		return store.Assignment{}, store.Dispatch{}, fmt.Errorf("starting worker %s for assignment %d: %w", worker, a.ID, err)
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
	name := JobBranchName(j)
	var continues int64
	var startedFrom string
	// A follow-up to a job whose pull request is still open continues its
	// job branch, starting from origin's copy of it (05 rule 16).
	if j.Follows != 0 {
		c, why, err := d.continuationOf(ctx, j, repository)
		if err != nil {
			return store.JobBranch{}, err
		}
		if c != nil {
			name, startedFrom, continues = c.Branch, c.Tip, c.Predecessor
			if err := repos.ResetBranch(ctx, repository.Clone, name, startedFrom); err != nil {
				return store.JobBranch{}, err
			}
		} else {
			d.log.Info("a follow-up starts from the base with a new job branch", "job", j.Number, "follows", j.Follows, "why", why)
		}
	}
	if continues == 0 {
		if err := repos.Fetch(ctx, repository.Clone); err != nil {
			return store.JobBranch{}, err
		}
		tip, err := repos.OriginTip(ctx, repository.Clone, repository.DefaultBranch)
		if err != nil {
			return store.JobBranch{}, err
		}
		startedFrom, err = repos.Commit(ctx, repository.Clone, "refs/heads/"+name)
		if err != nil {
			startedFrom = tip
			if err := repos.CreateBranch(ctx, repository.Clone, name, startedFrom); err != nil {
				return store.JobBranch{}, err
			}
		}
	}
	view := filepath.Join(d.cfg.Paths.Views, fmt.Sprintf("job-%d", j.Number))
	if err := repos.EnsureView(ctx, repository.Clone, view, startedFrom); err != nil {
		return store.JobBranch{}, err
	}
	var replaced []store.Assignment
	var checked []int64
	if continues != 0 {
		predecessor, err := d.store.Job(continues)
		if err != nil {
			return store.JobBranch{}, err
		}
		if predecessor.State != store.JobEnded {
			assignments, err := d.store.AssignmentsForJob(continues)
			if err != nil {
				return store.JobBranch{}, err
			}
			for _, a := range assignments {
				if a.State != store.AssignmentAccepted && a.State != store.AssignmentCancelled {
					replaced = append(replaced, a)
					checked = append(checked, a.ID)
				}
			}
		}
	}
	branch, err = d.store.JobBranchMade(store.JobBranch{Job: j.Number, Repository: repository.Name, Name: name, StartedFrom: startedFrom, View: view, Continues: continues}, time.Now(), checked...)
	if err != nil {
		return store.JobBranch{}, fmt.Errorf("recording the job branch: %w", err)
	}
	for _, a := range replaced {
		d.stopWorker(a)
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
	authorized, err := d.grantedEffect(ref, req.Kind, l.currentDispatch)
	if err != nil {
		return store.Effect{}, err
	}
	e, err := d.store.EffectRecorded(ref.address.String(), ref.generation, l.currentDispatch, req.Kind, req.Ref, time.Now())
	if err != nil {
		return store.Effect{}, refuse("%v", err)
	}
	if authorized.ID != 0 {
		if err := d.store.AuthorizationUsed(authorized.ID, e.ID); err != nil {
			return store.Effect{}, err
		}
	}
	return e, nil
}

// repositoryOptions returns the settings in cfg of the repository job
// targets, or the defaults of a repository whose entry cfg lacks.
func (d *daemon) repositoryOptions(cfg config.Config, job int64) config.RepositoryOptions {
	if j, err := d.store.Job(job); err == nil {
		if options, ok := cfg.Options[j.Repository]; ok {
			return options
		}
	}
	return config.RepositoryOptions{CommitTrailers: true}
}

// The trailers every commit of a worker carries.
const (
	TrailerJob      = "AsmAI-Job"
	TrailerAgent    = "AsmAI-Agent"
	TrailerDispatch = "AsmAI-Dispatch"
)

// trailers returns the trailers a commit of req's worker carries: the job of
// the assignment in its current dispatch, the worker and the dispatch. Only a
// worker commits, and only while it works on a dispatch it fetched. It returns
// none for a worker of a repository that has the trailers switched off.
func (d *daemon) trailers(req Request) ([]string, error) {
	d.mu.Lock()
	ref, l, err := d.caller(req)
	current := int64(0)
	if err == nil {
		current = l.currentDispatch
	}
	d.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if ref.address.Name == roles.Leader {
		return nil, refuse("%s commits nothing: a leader's work goes through its workers", ref.address)
	}
	if current == 0 {
		return nil, refuse("%s has no current dispatch to name in the commit; commit while working on the assignment you fetched with `asmai inbox`", ref.address)
	}
	a, err := d.store.DispatchAssignment(current)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, refuse("dispatch %d is about no assignment, so there is no job to name in the commit", current)
	}
	if err != nil {
		return nil, err
	}
	d.configMu.Lock()
	cfg := d.applied
	d.configMu.Unlock()
	if !d.repositoryOptions(cfg, a.Job).CommitTrailers {
		return nil, nil
	}
	return []string{
		fmt.Sprintf("%s: %d", TrailerJob, a.Job),
		fmt.Sprintf("%s: %s", TrailerAgent, ref.address),
		fmt.Sprintf("%s: %d", TrailerDispatch, current),
	}, nil
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
	if err := checkRan(req.Report.SkillsRun, a); err != nil {
		return store.Report{}, store.Dispatch{}, err
	}
	input := *req.Report
	var branch store.JobBranch
	var commit string
	var tookIn bool
	ctx, cancel := context.WithTimeout(d.work, gitTimeout)
	defer cancel()
	if a.Scratch {
		// A scratch workspace is no repository: there is no commit to see,
		// no job branch to take in and no assignment branch to push.
		input.Artifacts = append([]string{"scratch workspace " + a.Workspace.Path}, input.Artifacts...)
	} else {
		if branch, err = d.store.JobBranch(a.Job); err != nil {
			return store.Report{}, store.Dispatch{}, err
		}
		// What the daemon sees of the workspace is the commit the report is at.
		if commit, err = repos.Commit(ctx, a.Workspace.Path, "HEAD"); err != nil {
			return store.Report{}, store.Dispatch{}, fmt.Errorf("reading the commit of %s: %w", a.Workspace.Path, err)
		}
	}
	switch {
	case a.ReadOnly:
		// A validation is fixed at one commit, and its evidence names it.
		// It has no branch to push, so only the commit it is at is checked.
		if commit != a.Commit {
			return store.Report{}, store.Dispatch{}, refuse("the workspace is at %s, but assignment %d is fixed at %s: validation never changes the work, so check out %s again and report at it", commit, a.ID, a.Commit, a.Commit)
		}
		if tookIn, err = repos.Contains(ctx, a.Workspace.Path, "HEAD", branch.Tip); err != nil {
			return store.Report{}, store.Dispatch{}, err
		}
		input.Artifacts = append([]string{fmt.Sprintf("validated commit %s of job branch %s, in a read-only workspace", commit, branch.Name)}, input.Artifacts...)
	case !a.Scratch:
		if tookIn, err = d.checkWritingReport(ctx, a, branch, commit); err != nil {
			return store.Report{}, store.Dispatch{}, err
		}
		input.Artifacts = append([]string{fmt.Sprintf("branch %s at %s", a.Workspace.Branch, commit)}, input.Artifacts...)
	}

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

// checkWritingReport checks that a writing assignment's workspace is at a
// commit that can be reported: everything is committed, the job branch's tip
// is taken in, and origin's assignment branch holds the commit. It returns
// whether the commit has the tip in its history.
func (d *daemon) checkWritingReport(ctx context.Context, a store.Assignment, branch store.JobBranch, commit string) (bool, error) {
	uncommitted, err := repos.Uncommitted(ctx, a.Workspace.Path)
	if err != nil {
		return false, err
	}
	if len(uncommitted) > 0 {
		return false, refuse("the workspace has changes that are not committed, so the report would not be at its commit: commit them, push %s, record the push with `asmai effect`, and submit again\n%s", a.Workspace.Branch, strings.Join(uncommitted, "\n"))
	}
	tookIn, err := repos.Contains(ctx, a.Workspace.Path, "HEAD", branch.Tip)
	if err != nil {
		return false, err
	}
	if !tookIn {
		return false, refuse("the assignment branch does not contain the job branch's tip %s; take it in and submit again", branch.Tip)
	}
	job, err := d.store.Job(a.Job)
	if err != nil {
		return false, err
	}
	repository, err := d.store.Repository(job.Repository)
	if err != nil {
		return false, err
	}
	if err := repos.Fetch(ctx, repository.Clone); err != nil {
		return false, err
	}
	remoteBranch := "refs/remotes/origin/" + a.Workspace.Branch
	if _, err := repos.OriginTip(ctx, repository.Clone, a.Workspace.Branch); err != nil {
		return false, refuse("origin does not have the assignment branch %s; push it with `asmai push` and submit again", a.Workspace.Branch)
	}
	pushedCommit, err := repos.Contains(ctx, repository.Clone, remoteBranch, commit)
	if err != nil {
		return false, err
	}
	if !pushedCommit {
		return false, refuse("origin's assignment branch %s does not contain reported commit %s; commit what is uncommitted with `asmai commit`, push it with `asmai push` and submit again", a.Workspace.Branch, commit)
	}
	return true, nil
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
	if a.State == store.AssignmentNeedsReconciliation {
		return fmt.Errorf("assignment %d needs reconciliation, so %s is not started for it", a.ID, address)
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
		return d.held(address.String(), checks)
	}
	// A worker whose assignment continues after a restart resumes the
	// provider's session it was in, where the start recorded it.
	resume := ""
	if w, err := d.store.WorkerDispatch(a.ID); err == nil && w.Kind == store.MessageResumption && !w.Fetched {
		resume = w.Dispatch.NativeSession
	}
	return d.startWorker(l, cfg, install, a, resume)
}
