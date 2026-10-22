// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/repos"
	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/session"
	"github.com/talvor/asmai/internal/store"
)

// twoResults is a daemon, with real git, for a repository job whose two
// assignments were made from the job branch's tip and each submitted a result
// of one commit.
type twoResults struct {
	d       *daemon
	s       *store.Store
	clone   string
	origin  string
	job     store.Job
	started string
	// assignments and commits are indexed from zero by assignment.
	assignments [2]store.Assignment
	commits     [2]string
	// reported are the dispatches that brought each result to the owning
	// leader.
	reported [2]store.Dispatch
}

func newTwoResults(t *testing.T) twoResults {
	t.Helper()
	return newTwoResultsOf(t, roles.Engineering)
}

// newTwoResultsOf is newTwoResults for the writing role role, whose leader
// owns the assignments and whose workers carry them, in a job whose mandate
// is the one the role's work needs.
func newTwoResultsOf(t *testing.T, role string) twoResults {
	t.Helper()
	paths := stateDir(t)
	origin := remote(t)
	if role == roles.Coordination {
		// A repository whose setup job is open has no setup yet.
		origin = remoteWithoutSetup(t)
	}
	clone := filepath.Join(paths.Repositories, "fixture")
	if err := os.MkdirAll(filepath.Dir(clone), 0o700); err != nil {
		t.Fatal(err)
	}
	git(t, filepath.Dir(clone), "clone", origin, clone)
	s, err := store.Open(paths.Store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	at := time.Now()
	repository := store.Repository{Name: "fixture", Origin: origin, DefaultBranch: "trunk", Clone: clone, AddedAt: at}
	if err := s.ConversationEntered("leader@coordination", roles.Coordination, 1, "/dev/pts/test", at); err != nil {
		t.Fatal(err)
	}
	var job store.Job
	if role == roles.Coordination {
		// Coordination's own workers carry the setup of a repository, whose
		// job the daemon opens as the repository is registered.
		if job, _, err = s.RepositoryAddedWithSetup(repository, store.SetupOpening{Words: "asmai repo add " + origin, Reading: "Set fixture up for the skills", Criteria: []string{"done"}, Body: "setup"}); err != nil {
			t.Fatal(err)
		}
	} else {
		if err := s.RepositoryAdded(repository); err != nil {
			t.Fatal(err)
		}
		_, witness, err := s.ObservedWitnessed(store.Witnessed{Agent: "leader@coordination", Role: roles.Coordination, Generation: 1, Terminal: "/dev/pts/test", Text: "add a greeting"}, "UserPromptSubmit", json.RawMessage(`{"prompt":"add a greeting"}`), at)
		if err != nil {
			t.Fatal(err)
		}
		mandate := store.MandateTestedPR
		switch role {
		case roles.Planning:
			mandate = store.MandatePlanningOnly
		case roles.Research:
			mandate = store.MandateResearchOnly
		}
		if job, err = s.JobOpened(store.Job{Repository: "fixture", Witness: witness, Reading: "Add a greeting", Mandate: mandate, Criteria: []string{"done"}}, false, at); err != nil {
			t.Fatal(err)
		}
	}
	owner := roles.LeaderOf(role)
	d := &daemon{
		cfg: Config{Paths: paths}, work: context.Background(), store: s,
		leaders:  map[string]*leader{owner.String(): {address: owner, session: &session.Session{}, generation: 1}},
		sessions: map[string]sessionRef{"credential": {address: owner, generation: 1}},
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	repository, err = s.Repository("fixture")
	if err != nil {
		t.Fatal(err)
	}
	branch, err := d.ensureJobBranch(context.Background(), job, repository)
	if err != nil {
		t.Fatal(err)
	}
	f := twoResults{d: d, s: s, clone: clone, origin: origin, job: job, started: branch.Tip}
	for i := range f.assignments {
		id, err := s.NextAssignmentID()
		if err != nil {
			t.Fatal(err)
		}
		number, slot, err := s.Allocate(role)
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(paths.Workspaces, "job-1", "assignment-"+string(rune('0'+id)))
		w := store.Workspace{Slot: slot, Path: filepath.Join(dir, "repo"), Tmp: filepath.Join(dir, "tmp"), Branch: "asmai/job-1/" + string(rune('0'+id)), Base: branch.Tip}
		if err := repos.AddWorkspace(context.Background(), clone, w.Path, w.Branch, w.Base); err != nil {
			t.Fatal(err)
		}
		worker := roles.WorkerOf(role, number)
		a, dispatch, err := s.AssignmentCreated(store.NewAssignment{ID: id, Job: job.Number, Owner: owner.String(), Worker: worker.String(), Outcome: "outcome", Criteria: []string{"done"}, Workspace: w}, at)
		if err != nil {
			t.Fatal(err)
		}
		// The worker commits a file of its own, pushes its branch and
		// submits.
		file := "file-" + string(rune('0'+id)) + ".txt"
		if err := os.WriteFile(filepath.Join(w.Path, file), []byte("work\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, w.Path, "add", file)
		git(t, w.Path, "commit", "-m", "work of assignment "+string(rune('0'+id)))
		git(t, w.Path, "push", "origin", w.Branch)
		commit := git(t, w.Path, "rev-parse", "HEAD")
		if err := s.ChangeDispatch(dispatch.ID, 1, store.DispatchCreated, store.DispatchWorking, "", at); err != nil {
			t.Fatal(err)
		}
		if _, err := s.EffectRecorded(worker.String(), 1, dispatch.ID, "push", "origin/"+w.Branch+"@"+commit, at); err != nil {
			t.Fatal(err)
		}
		_, reported, err := s.ReportSubmitted(store.Submission{
			Agent: worker.String(), Generation: 1, Dispatch: dispatch.ID, Kind: store.ReportResult, Commit: commit, JobTip: branch.Tip, TookInTip: true,
			Input: store.ReportInput{Evidence: []string{"it works"}, PRSection: "Adds " + file},
		}, at)
		if err != nil {
			t.Fatal(err)
		}
		f.assignments[i], f.commits[i], f.reported[i] = a, commit, reported
	}
	return f
}

func (f twoResults) decide(command string, assignment int64, reasons ...string) (Response, error) {
	resp, _, err := f.d.decide(Request{Session: "credential", Command: command, Assignment: assignment, Reasons: reasons})
	return resp, err
}

func TestAResultSubmittedAgainstAnOldTipGoesBackToItsWorkerWithoutBeingRejected(t *testing.T) {
	f := newTwoResults(t)
	view := filepath.Join(f.d.cfg.Paths.Views, "job-1")
	branchRef := "refs/heads/" + JobBranchName(f.job)

	// The first result is a fast-forward of the tip: the branch moves to its
	// commit, the leaders' view follows, and the workspace is removed.
	resp, err := f.decide(CommandAccept, f.assignments[0].ID, "file-1.txt is there")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Assignment.State != store.AssignmentAccepted || resp.Decision.Commit != f.commits[0] || resp.JobBranch.Tip != f.commits[0] {
		t.Errorf("the acceptance answered %+v, %+v and %+v", resp.Assignment, resp.Decision, resp.JobBranch)
	}
	if got := git(t, f.clone, "rev-parse", branchRef); got != f.commits[0] {
		t.Errorf("the job branch is at %s, want the accepted commit %s", got, f.commits[0])
	}
	if got := git(t, view, "rev-parse", "HEAD"); got != f.commits[0] {
		t.Errorf("the view is at %s, want %s", got, f.commits[0])
	}
	if _, err := os.Stat(filepath.Join(view, "file-1.txt")); err != nil {
		t.Errorf("the view lacks the accepted work: %v", err)
	}
	if _, err := os.Stat(f.assignments[0].Workspace.Path); !os.IsNotExist(err) {
		t.Errorf("the accepted assignment's workspace is still there (%v)", err)
	}
	if out := git(t, f.clone, "branch", "--list", f.assignments[0].Workspace.Branch); out != "" {
		t.Errorf("the clone still has the accepted assignment's branch: %s", out)
	}
	if got := git(t, f.origin, "rev-parse", "refs/heads/"+f.assignments[0].Workspace.Branch); got != f.commits[0] {
		t.Errorf("origin's assignment branch is at %s, want it kept at %s", got, f.commits[0])
	}
	if out := git(t, f.origin, "branch", "--list", "asmai/job-1-*"); out != "" {
		t.Errorf("the job branch was pushed: %s", out)
	}

	// The second was made from the old tip: its commit does not contain the
	// first's, so the job branch moved after the result was submitted. The
	// acceptance accepts nothing and rejects nothing: the work returns to its
	// worker.
	second := f.assignments[1]
	resp, err = f.decide(CommandAccept, second.ID, "file-2.txt is there")
	if err != nil {
		t.Fatalf("accepting a result submitted against an old tip returned %v", err)
	}
	if !resp.Returned || resp.Decision != nil || resp.Assignment.State != store.AssignmentActive || resp.Dispatch == nil || resp.Dispatch.Agent != second.Worker {
		t.Fatalf("the acceptance answered returned=%t, decision %+v, assignment %+v and dispatch %+v, want the work returned to %s", resp.Returned, resp.Decision, resp.Assignment, resp.Dispatch, second.Worker)
	}
	kind, about, err := f.s.DispatchKind(resp.Dispatch.ID)
	if err != nil || kind != store.MessageMovedTip || about != second.ID {
		t.Errorf("the worker's dispatch is of kind %q about assignment %d (%v), want %q about %d", kind, about, err, store.MessageMovedTip, second.ID)
	}
	if got := git(t, f.clone, "rev-parse", branchRef); got != f.commits[0] {
		t.Errorf("the returned work moved the job branch to %s", got)
	}
	if got := git(t, view, "rev-parse", "HEAD"); got != f.commits[0] {
		t.Errorf("the returned work moved the view to %s", got)
	}
	if b, _ := f.s.JobBranch(f.job.Number); b.Tip != f.commits[0] {
		t.Errorf("the returned work moved the recorded tip to %s", b.Tip)
	}
	if _, err := os.Stat(second.Workspace.Path); err != nil {
		t.Errorf("returning the work removed the workspace: %v", err)
	}
	// It is not a rejection: no decision is recorded and the journal has no
	// rejection, only the return.
	if decisions, _ := f.s.Decisions(second.ID); len(decisions) != 0 {
		t.Errorf("returning the work recorded a decision: %+v", decisions)
	}
	entries, err := f.s.JobEntries(f.job.Number)
	if err != nil {
		t.Fatal(err)
	}
	returned := 0
	for _, e := range entries {
		switch e.Kind {
		case store.KindResultRejected:
			t.Errorf("the journal records a rejection: %s", e.Data)
		case store.KindResultReturned:
			returned++
		}
	}
	if returned != 1 {
		t.Errorf("the journal records %d returns, want 1", returned)
	}

	// The same worker takes in the new tip and submits again, and the leader
	// accepts it.
	at := time.Now()
	git(t, second.Workspace.Path, "merge", "--no-edit", f.commits[0])
	git(t, second.Workspace.Path, "push", "origin", second.Workspace.Branch)
	merged := git(t, second.Workspace.Path, "rev-parse", "HEAD")
	if err := f.s.ChangeDispatch(resp.Dispatch.ID, 1, store.DispatchCreated, store.DispatchWorking, "", at); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.EffectRecorded(second.Worker, 1, resp.Dispatch.ID, "push", "origin/"+second.Workspace.Branch+"@"+merged, at); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.s.ReportSubmitted(store.Submission{
		Agent: second.Worker, Generation: 1, Dispatch: resp.Dispatch.ID, Kind: store.ReportResult, Commit: merged, JobTip: f.commits[0], TookInTip: true,
		Input: store.ReportInput{Evidence: []string{"it works with the new tip"}, PRSection: "Adds file-2.txt"},
	}, at); err != nil {
		t.Fatalf("the worker's second result after the return: %v", err)
	}
	resp, err = f.decide(CommandAccept, second.ID, "file-2.txt is there on the new tip")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Returned || resp.Decision == nil || resp.JobBranch.Tip != merged {
		t.Errorf("the second acceptance answered returned=%t, %+v and %+v, want the job branch at %s", resp.Returned, resp.Decision, resp.JobBranch, merged)
	}
}

func TestOnlyTheOwningLeaderDecidesOnAResult(t *testing.T) {
	f := newTwoResults(t)
	id := f.assignments[0].ID
	branchRef := "refs/heads/" + JobBranchName(f.job)

	// A session that is not Engineering's leader is refused, and so is a
	// request with no reasons.
	other := roles.WorkerOf(roles.Engineering, 1)
	f.d.leaders[other.String()] = &leader{address: other, session: &session.Session{}, generation: 1}
	f.d.sessions["worker"] = sessionRef{address: other, generation: 1}
	for _, command := range []string{CommandAccept, CommandReject, CommandCancel} {
		if _, _, err := f.d.decide(Request{Session: "worker", Command: command, Assignment: id, Reasons: []string{"x"}}); err == nil || !strings.Contains(err.Error(), "only leader@engineering") {
			t.Errorf("%s by a worker returned %v", command, err)
		}
		if _, err := f.decide(command, id); err == nil || !strings.Contains(err.Error(), "needs an assignment and its reasons") {
			t.Errorf("%s with no reasons returned %v", command, err)
		}
		if _, err := f.decide(command, 99, "x"); err == nil || !strings.Contains(err.Error(), "does not exist") {
			t.Errorf("%s of an assignment that does not exist returned %v", command, err)
		}
	}
	if got := git(t, f.clone, "rev-parse", branchRef); got != f.started {
		t.Errorf("a refused decision moved the job branch to %s", got)
	}

	// Rejecting returns the assignment to its worker and moves nothing.
	resp, err := f.decide(CommandReject, id, "no test covers the empty case")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Assignment.State != store.AssignmentActive || resp.Dispatch.Agent != f.assignments[0].Worker || resp.Decision.Kind != store.DecisionRejected {
		t.Errorf("the rejection answered %+v, %+v and %+v", resp.Assignment, resp.Dispatch, resp.Decision)
	}
	if got := git(t, f.clone, "rev-parse", branchRef); got != f.started {
		t.Errorf("a rejection moved the job branch to %s", got)
	}
	if _, err := os.Stat(f.assignments[0].Workspace.Path); err != nil {
		t.Errorf("a rejection removed the workspace: %v", err)
	}
	if _, err := f.decide(CommandAccept, id, "x"); err == nil || !strings.Contains(err.Error(), "active") {
		t.Errorf("accepting an active assignment returned %v", err)
	}
}
