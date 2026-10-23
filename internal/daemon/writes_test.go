// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/githooks"
	"github.com/talvor/asmai/internal/repos"
	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/session"
	"github.com/talvor/asmai/internal/store"
)

// activeWorker is an Engineering worker of f's job that works on a dispatch
// of its assignment, which it has not submitted.
type activeWorker struct {
	assignment store.Assignment
	dispatch   int64
	address    roles.Address
	credential string
}

// gitIdentity gives the daemon's git, which has no configuration of its own
// in a test, the identity the user's would give it.
func gitIdentity(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	t.Setenv("GIT_AUTHOR_NAME", "T")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.test")
	t.Setenv("GIT_COMMITTER_NAME", "T")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.test")
}

// newActiveWorker gives f's job a writing assignment that is still active,
// with a worker whose session is "worker".
func (f *twoResults) newActiveWorker(t *testing.T) activeWorker {
	t.Helper()
	branch, err := f.s.JobBranch(f.job.Number)
	if err != nil {
		t.Fatal(err)
	}
	id, err := f.s.NextAssignmentID()
	if err != nil {
		t.Fatal(err)
	}
	number, slot, err := f.s.Allocate(roles.Engineering)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(f.d.cfg.Paths.Workspaces, "job-1", "assignment-"+string(rune('0'+id)))
	w := store.Workspace{Slot: slot, Path: filepath.Join(dir, "repo"), Tmp: filepath.Join(dir, "tmp"), Branch: "asmai/job-1/" + string(rune('0'+id)), Base: branch.Tip}
	if err := repos.AddWorkspace(context.Background(), f.clone, w.Path, w.Branch, w.Base); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(w.Tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	worker := roles.WorkerOf(roles.Engineering, number)
	at := time.Now()
	a, dispatch, err := f.s.AssignmentCreated(store.NewAssignment{ID: id, Job: f.job.Number, Owner: "leader@engineering", Worker: worker.String(), Outcome: "outcome", Criteria: []string{"done"}, Workspace: w}, at)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.ChangeDispatch(dispatch.ID, 1, store.DispatchCreated, store.DispatchWorking, "", at); err != nil {
		t.Fatal(err)
	}
	f.d.leaders[worker.String()] = &leader{address: worker, session: &session.Session{}, generation: 1, currentDispatch: dispatch.ID, assignment: a.ID}
	f.d.sessions["worker"] = sessionRef{address: worker, generation: 1}
	return activeWorker{assignment: a, dispatch: dispatch.ID, address: worker, credential: "worker"}
}

func (w activeWorker) write(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(w.assignment.Workspace.Path, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// installHooks gives the daemon AsmAI's hooks directory, whose hooks run a
// stand-in for asmai that adds a trailer naming the session the commit was
// made for, as `asmai git-hook` adds the real ones.
func (f *twoResults) installHooks(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "asmai")
	body := "#!/bin/sh\n[ \"$2\" = prepare-commit-msg ] && printf '\\nAsmAI-Session: %s\\n' \"$ASMAI_SESSION\" >> \"$3\"\nexit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	f.d.cfg.Paths.GitHooks = filepath.Join(dir, "hooks")
	if err := githooks.Install(f.d.cfg.Paths.GitHooks, script); err != nil {
		t.Fatal(err)
	}
}

func TestAWorkersCommitIsMadeByTheDaemonWithTheWorkersHooksAndRecordedAsItsEffect(t *testing.T) {
	gitIdentity(t)
	f := newTwoResults(t)
	f.installHooks(t)
	w := f.newActiveWorker(t)
	path := w.assignment.Workspace.Path
	commit := Request{Session: w.credential, Command: CommandCommit, Message: "Add the greeting"}

	// Nothing changed, so there is nothing to commit.
	if _, err := f.d.commit(commit); err == nil || !strings.Contains(err.Error(), "nothing to commit") {
		t.Errorf("a commit with no change returned %v", err)
	}
	// A leader commits nothing: its work goes through its workers.
	if _, err := f.d.commit(Request{Session: "credential", Command: CommandCommit, Message: "x"}); err == nil || !strings.Contains(err.Error(), "leader") {
		t.Errorf("a leader's commit returned %v", err)
	}

	w.write(t, "greeting.txt", "hello\n")
	w.write(t, "scratch.txt", "scratch\n")
	commit.Paths = []string{"greeting.txt"}
	written, err := f.d.commit(commit)
	if err != nil {
		t.Fatal(err)
	}
	head := git(t, path, "rev-parse", "HEAD")
	if written.Commit != head || written.Branch != w.assignment.Workspace.Branch || written.Effect == 0 || written.Dispatch != w.dispatch {
		t.Errorf("the commit is %+v, the workspace is at %s", written, head)
	}
	// The hook that ran is the one AsmAI gave the worker's git, for the
	// worker's own session.
	if got := git(t, path, "show", "--name-only", "--format=%B", "HEAD"); !strings.Contains(got, "Add the greeting") || !strings.Contains(got, "AsmAI-Session: worker") || !strings.Contains(got, "greeting.txt") || strings.Contains(got, "scratch.txt") {
		t.Errorf("the commit is %q", got)
	}
	// The daemon records it, as the worker's effect in its dispatch.
	effects, err := f.s.DispatchEffects(w.dispatch)
	if err != nil {
		t.Fatal(err)
	}
	if len(effects) != 1 || effects[0].Kind != "commit" || effects[0].Ref != head || effects[0].Agent != w.address.String() {
		t.Errorf("the effects are %+v, want the worker's commit of %s", effects, head)
	}
}

func TestAWorkerMergesOnlyTheJobBranchOrOriginsBaseOrItsOwnBranchAndAConflictWaitsForItsCommit(t *testing.T) {
	gitIdentity(t)
	f := newTwoResults(t)
	w := f.newActiveWorker(t)
	path := w.assignment.Workspace.Path
	jobBranch := JobBranchName(f.job)

	for _, ref := range []string{"main", "origin/some-other", "--abort", "HEAD~1", "asmai/job-1/9"} {
		if _, err := f.d.merge(Request{Session: w.credential, Command: CommandMerge, Ref: ref}); err == nil || !strings.Contains(err.Error(), jobBranch) {
			t.Errorf("merging %s returned %v, want a refusal that names what a worker may merge", ref, err)
		}
	}
	if _, err := f.d.merge(Request{Session: "credential", Command: CommandMerge, Ref: jobBranch}); err == nil || !strings.Contains(err.Error(), "leader") {
		t.Errorf("a leader's merge returned %v", err)
	}

	// The job branch is already in the workspace's history.
	got, err := f.d.merge(Request{Session: w.credential, Command: CommandMerge, Ref: jobBranch})
	if err != nil || !got.UpToDate || got.Effect != 0 {
		t.Fatalf("merging the job branch returned %+v, %v, want up to date", got, err)
	}

	// Origin's base moves, and the worker takes it in: the daemon fetches
	// first, and the merge commit is the worker's effect.
	other := filepath.Join(t.TempDir(), "other")
	git(t, filepath.Dir(other), "clone", f.origin, other)
	if err := os.WriteFile(filepath.Join(other, "base.txt"), []byte("moved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, other, "add", "base.txt")
	git(t, other, "commit", "-m", "Move the base")
	git(t, other, "push", "origin", "trunk")
	got, err = f.d.merge(Request{Session: w.credential, Command: CommandMerge, Ref: "origin/trunk"})
	if err != nil {
		t.Fatal(err)
	}
	if head := git(t, path, "rev-parse", "HEAD"); got.Commit != head || got.Effect == 0 || got.UpToDate {
		t.Errorf("the merge is %+v, the workspace is at %s", got, head)
	}
	if _, err := os.Stat(filepath.Join(path, "base.txt")); err != nil {
		t.Errorf("the base is not merged in: %v", err)
	}

	// A conflict is left in the workspace for the worker to resolve and commit.
	w.write(t, "conflict.txt", "mine\n")
	if _, err := f.d.commit(Request{Session: w.credential, Command: CommandCommit, Message: "Mine"}); err != nil {
		t.Fatal(err)
	}
	git(t, other, "pull", "--quiet", "origin", "trunk")
	if err := os.WriteFile(filepath.Join(other, "conflict.txt"), []byte("theirs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, other, "add", "conflict.txt")
	git(t, other, "commit", "-m", "Theirs")
	git(t, other, "push", "origin", "trunk")
	got, err = f.d.merge(Request{Session: w.credential, Command: CommandMerge, Ref: "origin/trunk"})
	if err != nil || len(got.Conflicts) != 1 || got.Conflicts[0] != "conflict.txt" || got.Commit != "" || got.Effect != 0 {
		t.Fatalf("the conflicting merge returned %+v, %v", got, err)
	}
	w.write(t, "conflict.txt", "both\n")
	done, err := f.d.commit(Request{Session: w.credential, Command: CommandCommit, Message: "Merge origin's base"})
	if err != nil {
		t.Fatal(err)
	}
	if parents := strings.Fields(git(t, path, "rev-list", "--parents", "-n1", done.Commit)); len(parents) != 3 {
		t.Errorf("the commit is %v, want a merge", parents)
	}

	// Abort undoes a merge in progress.
	git(t, other, "pull", "--quiet", "origin", "trunk")
	if err := os.WriteFile(filepath.Join(other, "conflict.txt"), []byte("again\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, other, "commit", "-am", "Again")
	git(t, other, "push", "origin", "trunk")
	w.write(t, "conflict.txt", "mine again\n")
	if _, err := f.d.commit(Request{Session: w.credential, Command: CommandCommit, Message: "Mine again"}); err != nil {
		t.Fatal(err)
	}
	if got, err = f.d.merge(Request{Session: w.credential, Command: CommandMerge, Ref: "origin/trunk"}); err != nil || len(got.Conflicts) == 0 {
		t.Fatalf("the second conflicting merge returned %+v, %v", got, err)
	}
	if got, err = f.d.merge(Request{Session: w.credential, Command: CommandMerge, Abort: true}); err != nil || !got.Aborted {
		t.Fatalf("aborting returned %+v, %v", got, err)
	}
	if out := git(t, path, "status", "--porcelain"); out != "" {
		t.Errorf("after the abort the workspace has %q", out)
	}
}

func TestAWorkersPushSendsItsAssignmentBranchPlainlyAndTheDaemonRecordsIt(t *testing.T) {
	gitIdentity(t)
	f := newTwoResults(t)
	w := f.newActiveWorker(t)
	branch := w.assignment.Workspace.Branch
	push := Request{Session: w.credential, Command: CommandPush}

	w.write(t, "greeting.txt", "hello\n")
	committed, err := f.d.commit(Request{Session: w.credential, Command: CommandCommit, Message: "Add the greeting"})
	if err != nil {
		t.Fatal(err)
	}
	// A worker pushes its own branch, never a job's.
	if _, err := f.d.push(Request{Session: w.credential, Command: CommandPush, Job: f.job.Number}); err == nil || !strings.Contains(err.Error(), "own assignment branch") {
		t.Errorf("a worker's push of a job returned %v", err)
	}
	p, err := f.d.push(push)
	if err != nil {
		t.Fatal(err)
	}
	if p.Branch != branch || p.Commit != committed.Commit || p.Effect == 0 || p.Dispatch != w.dispatch {
		t.Errorf("the push is %+v, want %s at %s", p, branch, committed.Commit)
	}
	if got := git(t, f.origin, "rev-parse", "refs/heads/"+branch); got != committed.Commit {
		t.Errorf("origin's branch is at %s, want %s", got, committed.Commit)
	}
	effects, err := f.s.DispatchEffects(w.dispatch)
	if err != nil {
		t.Fatal(err)
	}
	if last := effects[len(effects)-1]; last.Kind != "push" || last.Ref != "origin/"+branch+"@"+committed.Commit || last.Agent != w.address.String() {
		t.Errorf("the last effect is %+v, want the worker's push", last)
	}

	// A branch that moved on origin is never overwritten.
	other := filepath.Join(t.TempDir(), "other")
	git(t, filepath.Dir(other), "clone", f.origin, other)
	git(t, other, "checkout", branch)
	git(t, other, "commit", "--allow-empty", "-m", "outside")
	git(t, other, "push", "origin", branch)
	w.write(t, "more.txt", "more\n")
	if _, err := f.d.commit(Request{Session: w.credential, Command: CommandCommit, Message: "More"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.d.push(push); err == nil || !strings.Contains(err.Error(), "merge origin/"+branch) {
		t.Errorf("a push over a commit on origin returned %v, want a refusal that says to merge it", err)
	}
	if _, err := f.d.merge(Request{Session: w.credential, Command: CommandMerge, Ref: "origin/" + branch}); err != nil {
		t.Fatalf("merging the worker's own branch on origin: %v", err)
	}
	if _, err := f.d.push(push); err != nil {
		t.Errorf("the push after the merge returned %v", err)
	}
}

func TestOnlyAWorkerWithALiveWritingAssignmentWritesToItsWorkspace(t *testing.T) {
	gitIdentity(t)
	f := newTwoResults(t)
	w := f.newActiveWorker(t)
	w.write(t, "greeting.txt", "hello\n")

	// Without a dispatch it fetched, a worker is tagged with nothing.
	f.d.leaders[w.address.String()].currentDispatch = 0
	if _, err := f.d.commit(Request{Session: w.credential, Command: CommandCommit, Message: "x"}); err == nil || !strings.Contains(err.Error(), "no current dispatch") {
		t.Errorf("a commit outside a dispatch returned %v", err)
	}
	f.d.leaders[w.address.String()].currentDispatch = w.dispatch

	// A worker whose result is submitted has handed its workspace over.
	if _, err := f.s.EffectRecorded(w.address.String(), 1, w.dispatch, "push", "origin/x@y", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.s.ReportSubmitted(store.Submission{Agent: w.address.String(), Generation: 1, Dispatch: w.dispatch, Kind: store.ReportResult, Commit: git(t, w.assignment.Workspace.Path, "rev-parse", "HEAD"), JobTip: f.started, TookInTip: true, Input: store.ReportInput{Evidence: []string{"e"}, PRSection: "p"}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.d.commit(Request{Session: w.credential, Command: CommandCommit, Message: "x"}); err == nil || !strings.Contains(err.Error(), "submitted") {
		t.Errorf("a commit after the result returned %v, want a refusal that the assignment is submitted", err)
	}
}
