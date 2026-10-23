// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/talvor/asmai/internal/githooks"
	"github.com/talvor/asmai/internal/providers"
	"github.com/talvor/asmai/internal/repos"
	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/store"
)

// writeTimeout is how long a write the daemon makes for an agent may take,
// which includes the repository's own hooks, such as a pre-commit that runs
// its checks.
const writeTimeout = 15 * time.Minute

// Written is what a worker's commit or merge made in its workspace.
type Written struct {
	// Branch is the assignment branch the workspace is on, and Commit the
	// commit it is at afterwards, or empty when a conflict left the merge
	// unfinished.
	Branch string `json:"branch"`
	Commit string `json:"commit,omitempty"`
	// Effect is the effect the daemon recorded for the commit it made, and
	// Dispatch the dispatch it is tagged with. A merge that made none records
	// none.
	Effect   int64 `json:"effect,omitempty"`
	Dispatch int64 `json:"dispatch"`
	// UpToDate says a merge found the ref already in the branch.
	UpToDate bool `json:"up_to_date,omitempty"`
	// Conflicts are the files a merge left in conflict, which the worker
	// resolves and commits, and Aborted says a merge was ended.
	Conflicts []string `json:"conflicts,omitempty"`
	Aborted   bool     `json:"aborted,omitempty"`
}

// writeCommand serves the commands that write for an agent: a worker's commit
// and merge, and the delivery owner's pull request open and update.
func (d *daemon) writeCommand(conn *net.UnixConn, req Request) {
	conn.SetDeadline(time.Now().Add(writeTimeout + 2*ghTimeout))
	var resp Response
	var err error
	switch req.Command {
	case CommandCommit:
		resp.Written, err = d.commit(req)
	case CommandMerge:
		resp.Written, err = d.merge(req)
	case CommandPullRequestOpen:
		resp.PullRequest, resp.Effect, err = d.openPullRequest(req)
	case CommandPullRequestUpdate:
		resp.PullRequest, resp.Effect, err = d.updatePullRequest(req)
	}
	if err != nil {
		if !errors.As(err, new(*userError)) {
			d.log.Error("a write for an agent failed", "command", req.Command, "job", req.Job, "error", err.Error())
		}
		resp = Response{Error: err.Error()}
	}
	reply(conn, resp)
}

// workerWrite is a worker making a write in its workspace: who asks, the
// dispatch it works on, its assignment and that assignment's repository, and
// the environment the daemon runs git in for it.
type workerWrite struct {
	ref        sessionRef
	dispatch   int64
	assignment store.Assignment
	repository store.Repository
	env        []string
}

// workerWrite returns the write req's worker makes, or why it may not: only
// a worker, working on a dispatch of an assignment it carries that is not
// handed over yet, in a workspace that is a checkout of a repository.
func (d *daemon) workerWrite(req Request) (workerWrite, error) {
	d.mu.Lock()
	ref, l, err := d.caller(req)
	dispatch := int64(0)
	if err == nil {
		dispatch = l.currentDispatch
	}
	d.mu.Unlock()
	if err != nil {
		return workerWrite{}, err
	}
	if ref.address.Name == roles.Leader {
		return workerWrite{}, refuse("%s changes no workspace: a leader's work goes through its workers", ref.address)
	}
	if dispatch == 0 {
		return workerWrite{}, refuse("%s has no current dispatch to tag the write with; write while working on the assignment you fetched with `asmai inbox`", ref.address)
	}
	a, err := d.store.DispatchAssignment(dispatch)
	if errors.Is(err, sql.ErrNoRows) {
		return workerWrite{}, refuse("dispatch %d is about no assignment, so there is no workspace to write in", dispatch)
	}
	if err != nil {
		return workerWrite{}, err
	}
	switch {
	case a.Worker != ref.address.String():
		return workerWrite{}, refuse("assignment %d belongs to %s, not %s", a.ID, a.Worker, ref.address)
	case a.ReadOnly:
		return workerWrite{}, refuse("assignment %d is a validation, which changes nothing", a.ID)
	case a.Scratch:
		return workerWrite{}, refuse("assignment %d is in a scratch workspace, which is no repository: there is nothing to commit, merge or push", a.ID)
	case a.State != store.AssignmentActive && a.State != store.AssignmentCancelling:
		return workerWrite{}, refuse("assignment %d is %s, so its workspace is not written: it is the owning leader's to accept or reject", a.ID, a.State)
	}
	job, err := d.store.Job(a.Job)
	if err != nil {
		return workerWrite{}, err
	}
	repository, err := d.store.Repository(job.Repository)
	if errors.Is(err, sql.ErrNoRows) {
		return workerWrite{}, notRegistered(job.Repository)
	}
	if err != nil {
		return workerWrite{}, err
	}
	return workerWrite{ref: ref, dispatch: dispatch, assignment: a, repository: repository, env: d.workerGitEnv(a, req.Session)}, nil
}

// workerGitEnv returns the environment the daemon runs a worker's git in:
// the one the worker's own session has, so that the repository's hooks and
// AsmAI's, which add the trailers for the worker's session, run as they did
// when the worker ran git itself.
func (d *daemon) workerGitEnv(a store.Assignment, credential string) []string {
	set := map[string]string{
		SessionCredential: credential,
		SlotVariable:      strconv.Itoa(a.Workspace.Slot),
		"TMPDIR":          a.Workspace.Tmp,
	}
	maps.Copy(set, githooks.Environment(os.Environ(), d.cfg.Paths.GitHooks))
	return providers.SessionEnv(os.Environ(), d.cfg.Paths.Bin, set)
}

// effectOf records the effect kind of ref as the worker w's own, in its
// dispatch, and returns its ID.
func (d *daemon) effectOf(w workerWrite, kind, ref string) (int64, error) {
	e, err := d.store.EffectRecorded(w.ref.address.String(), w.ref.generation, w.dispatch, kind, ref, time.Now())
	if err != nil {
		return 0, refuse("%v", err)
	}
	return e.ID, nil
}

// commit commits the changes of req's worker in its workspace, as the user,
// with the repository's hooks and the trailers AsmAI adds, and records the
// commit as the worker's effect. With paths, only those files are taken.
func (d *daemon) commit(req Request) (*Written, error) {
	w, err := d.workerWrite(req)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(d.work, writeTimeout)
	defer cancel()
	commit, err := repos.CommitWorkspace(ctx, w.assignment.Workspace.Path, w.env, req.Message, req.Paths)
	if err != nil {
		return nil, refuse("%v", err)
	}
	effect, err := d.effectOf(w, "commit", commit)
	if err != nil {
		return nil, err
	}
	d.log.Info("worker commit made", "assignment", w.assignment.ID, "agent", w.ref.address.String(), "commit", commit, "effect", effect)
	return &Written{Branch: w.assignment.Workspace.Branch, Commit: commit, Effect: effect, Dispatch: w.dispatch}, nil
}

// mergeable returns the refs a worker may merge into its assignment branch:
// the job branch, which the assignment names, origin's copy of it, which an
// outside change on origin is taken in from, origin's copy of the
// repository's default branch, which a base moved on is taken in from, and
// origin's copy of the worker's own assignment branch.
func (w workerWrite) mergeable() []string {
	return []string{
		w.assignment.JobBranch,
		"origin/" + w.assignment.JobBranch,
		"origin/" + w.repository.DefaultBranch,
		"origin/" + w.assignment.Workspace.Branch,
	}
}

// merge merges the ref of req into the assignment branch of its worker in its
// workspace, or with Abort ends the merge in progress. The ref is one of
// mergeable; an origin ref is fetched first. A merge that makes a commit
// records it as the worker's effect. A conflict is not an error: the files in
// conflict are named and left for the worker to resolve and commit.
func (d *daemon) merge(req Request) (*Written, error) {
	w, err := d.workerWrite(req)
	if err != nil {
		return nil, err
	}
	branch := w.assignment.Workspace.Branch
	ctx, cancel := context.WithTimeout(d.work, writeTimeout)
	defer cancel()
	if req.Abort {
		if err := repos.AbortMerge(ctx, w.assignment.Workspace.Path, w.env); err != nil {
			return nil, refuse("%v", err)
		}
		return &Written{Branch: branch, Dispatch: w.dispatch, Aborted: true}, nil
	}
	allowed := w.mergeable()
	if !contains(allowed, req.Ref) {
		return nil, refuse("a worker merges only the job branch or origin's copy of it, origin's copy of the repository's default branch or origin's copy of its own assignment branch: one of %s, not %q", strings.Join(allowed, ", "), req.Ref)
	}
	if strings.HasPrefix(req.Ref, "origin/") {
		if err := repos.Fetch(ctx, w.repository.Clone); err != nil {
			return nil, err
		}
	}
	merged, err := repos.MergeIntoWorkspace(ctx, w.assignment.Workspace.Path, w.env, req.Ref)
	if err != nil {
		return nil, refuse("%v", err)
	}
	out := &Written{Branch: branch, Commit: merged.Commit, Dispatch: w.dispatch, UpToDate: merged.UpToDate, Conflicts: merged.Conflicts}
	if merged.Commit != "" {
		if out.Effect, err = d.effectOf(w, "commit", merged.Commit); err != nil {
			return nil, err
		}
	}
	d.log.Info("worker merge made", "assignment", w.assignment.ID, "agent", w.ref.address.String(), "ref", req.Ref, "commit", merged.Commit, "conflicts", len(merged.Conflicts))
	return out, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// pushAssignmentBranch pushes the assignment branch of req's worker from its
// workspace to origin with a plain push, never forced, and records the push as
// the worker's effect. Origin's refusal, for a branch that has commits the
// worker's does not, names the merge that takes them in.
func (d *daemon) pushAssignmentBranch(req Request) (store.Push, error) {
	w, err := d.workerWrite(req)
	if err != nil {
		return store.Push{}, err
	}
	if req.Job != 0 {
		return store.Push{}, refuse("a worker pushes its own assignment branch %s, and no job: run asmai push with no job number", w.assignment.Workspace.Branch)
	}
	branch := w.assignment.Workspace.Branch
	ctx, cancel := context.WithTimeout(d.work, writeTimeout)
	defer cancel()
	commit, err := repos.Commit(ctx, w.assignment.Workspace.Path, "HEAD")
	if err != nil {
		return store.Push{}, fmt.Errorf("reading the commit of %s: %w", w.assignment.Workspace.Path, err)
	}
	if err := repos.PushWorkspaceBranch(ctx, w.assignment.Workspace.Path, w.env, branch); err != nil {
		return store.Push{}, refuse("%v; nothing is recorded as pushed. If origin's branch has commits you do not have, take them in with `asmai merge origin/%s`, commit, and push again", err, branch)
	}
	effect, err := d.effectOf(w, "push", "origin/"+branch+"@"+commit)
	if err != nil {
		return store.Push{}, err
	}
	d.log.Info("assignment branch pushed", "assignment", w.assignment.ID, "agent", w.ref.address.String(), "branch", branch, "commit", commit, "effect", effect)
	return store.Push{Job: w.assignment.Job, Branch: branch, Commit: commit, Result: store.PushPushed, Leader: w.ref.address.String(), Dispatch: w.dispatch, Effect: effect, At: time.Now().UTC()}, nil
}
