// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
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

// writingResult makes a writing assignment of f's job from the job branch's
// tip, lets work change its workspace, and has its worker commit, push and
// submit a result. It returns the assignment.
func (f *twoResults) writingResult(t *testing.T, work func(dir string)) store.Assignment {
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
	worker := roles.WorkerOf(roles.Engineering, number)
	at := time.Now()
	a, dispatch, err := f.s.AssignmentCreated(store.NewAssignment{ID: id, Job: f.job.Number, Owner: "leader@engineering", Worker: worker.String(), Outcome: "outcome", Criteria: []string{"done"}, Workspace: w}, at)
	if err != nil {
		t.Fatal(err)
	}
	work(w.Path)
	git(t, w.Path, "push", "origin", w.Branch)
	commit := git(t, w.Path, "rev-parse", "HEAD")
	if err := f.s.ChangeDispatch(dispatch.ID, 1, store.DispatchCreated, store.DispatchWorking, "", at); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.EffectRecorded(worker.String(), 1, dispatch.ID, "push", "origin/"+w.Branch+"@"+commit, at); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.s.ReportSubmitted(store.Submission{
		Agent: worker.String(), Generation: 1, Dispatch: dispatch.ID, Kind: store.ReportResult, Commit: commit, JobTip: branch.Tip, TookInTip: true,
		Input: store.ReportInput{Evidence: []string{"it works"}, PRSection: "Adds work"},
	}, at); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestValidationIsHandedToQualityOnlyOnceTheJobBranchHasTakenInTheLatestBase(t *testing.T) {
	f := newTwoResults(t)
	handoff := Request{Session: "credential", Command: CommandHandoffSend, Job: f.job.Number, Agent: "leader@quality"}
	if _, err := f.decide(CommandAccept, f.assignments[0].ID, "file-1.txt is there"); err != nil {
		t.Fatal(err)
	}
	head, err := f.d.validationHead(handoff)
	if err != nil || head != f.commits[0] {
		t.Fatalf("with the base taken in, the head to validate is %q (%v), want the tip %s", head, err, f.commits[0])
	}

	// Someone else moves the base on origin: the job branch no longer has
	// taken in the latest base.
	other := filepath.Join(t.TempDir(), "other")
	git(t, filepath.Dir(other), "clone", f.origin, other)
	if err := os.WriteFile(filepath.Join(other, "base.txt"), []byte("moved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, other, "add", "base.txt")
	git(t, other, "commit", "-m", "Move the base")
	git(t, other, "push", "origin", "trunk")
	moved := git(t, other, "rev-parse", "HEAD")
	_, err = f.d.validationHead(handoff)
	if err == nil || !strings.Contains(err.Error(), "has not taken in the latest base") || !strings.Contains(err.Error(), moved) || !strings.Contains(err.Error(), "merge origin/trunk") {
		t.Fatalf("with the base moved, handing validation over returned %v", err)
	}

	// Engineering assigns a worker to merge it in, and accepts the result by
	// the normal path: the branch fast-forwards to a commit that has it.
	merge := f.writingResult(t, func(dir string) {
		git(t, dir, "fetch", "origin")
		git(t, dir, "merge", "--no-edit", "origin/trunk")
	})
	resp, err := f.decide(CommandAccept, merge.ID, "the base is merged in and the checks pass")
	if err != nil {
		t.Fatal(err)
	}
	head, err = f.d.validationHead(handoff)
	if err != nil || head != resp.JobBranch.Tip || head == f.commits[0] {
		t.Fatalf("after the merge, the head to validate is %q (%v), want the new tip %s", head, err, resp.JobBranch.Tip)
	}
	if ok, err := repos.Contains(context.Background(), f.clone, "refs/heads/"+JobBranchName(f.job), moved); err != nil || !ok {
		t.Errorf("the job branch has the moved base in its history: %v (%v)", ok, err)
	}

	// Only Engineering's leader hands a job branch over.
	quality := roles.LeaderOf(roles.Quality)
	f.d.leaders[quality.String()] = &leader{address: quality, session: &session.Session{}, generation: 1}
	f.d.sessions["quality"] = sessionRef{address: quality, generation: 1}
	handoff.Session = "quality"
	if _, err := f.d.validationHead(handoff); err == nil || !strings.Contains(err.Error(), "only leader@engineering") {
		t.Errorf("Quality's leader handing a branch over returned %v", err)
	}
	handoff.Session, handoff.Job = "credential", 99
	if _, err := f.d.validationHead(handoff); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("a job that does not exist returned %v", err)
	}
}

// validationFixture is f with Quality's leader and a Quality worker's session
// that can be called as, and a validation assigned at the job branch's tip.
type validationFixture struct {
	*twoResults
	tip        string
	assignment store.Assignment
	dispatch   store.Dispatch
}

func newValidationFixture(t *testing.T) validationFixture {
	t.Helper()
	f := newTwoResults(t)
	if _, err := f.decide(CommandAccept, f.assignments[0].ID, "file-1.txt is there"); err != nil {
		t.Fatal(err)
	}
	quality := roles.LeaderOf(roles.Quality)
	f.d.leaders[quality.String()] = &leader{address: quality, session: &session.Session{}, generation: 1}
	f.d.sessions["quality"] = sessionRef{address: quality, generation: 1}
	v := validationFixture{twoResults: &f, tip: f.commits[0]}

	// The worker cannot start with no provider installed, which assign
	// reports after it has recorded the assignment.
	resp, _, err := f.d.assign(Request{Session: "quality", Command: CommandAssign, Job: f.job.Number, ReadOnly: true, Commit: v.tip, Outcome: "Validate the job branch", Criteria: []string{"checks re-run at the commit"}})
	if err == nil || !strings.Contains(err.Error(), "starting worker worker1@quality") {
		t.Fatalf("assigning a validation returned %+v, %v, want only the worker's startup to fail", resp, err)
	}
	assignments, err := f.s.AssignmentsForJob(f.job.Number)
	if err != nil {
		t.Fatal(err)
	}
	v.assignment = assignments[len(assignments)-1]
	if v.dispatch, err = f.s.NextDispatch("worker1@quality"); err != nil {
		t.Fatalf("the Quality worker has no assignment dispatch: %v", err)
	}
	return v
}

func TestAValidationIsAssignedReadOnlyAtTheExactHeadInACleanWorkspaceWithNoBranch(t *testing.T) {
	v := newValidationFixture(t)
	a := v.assignment
	if !a.ReadOnly || a.Commit != v.tip || a.Owner != "leader@quality" || a.Worker != "worker1@quality" || a.Workspace.Branch != "" || a.State != store.AssignmentActive {
		t.Fatalf("the validation is %+v", a)
	}
	if a.Intent == nil || a.Intent.Reading != "Add a greeting" || a.Intent.Mandate != store.MandateTestedPR || strings.Join(a.Intent.Criteria, "|") != "done" {
		t.Errorf("the validation is given the intent %+v", a.Intent)
	}

	// The workspace is a checkout of AsmAI's clone at the exact head commit,
	// detached, so that it has no branch to push, and clean.
	if got := git(t, a.Workspace.Path, "rev-parse", "HEAD"); got != v.tip {
		t.Errorf("the workspace is at %s, want the head %s", got, v.tip)
	}
	if out := git(t, a.Workspace.Path, "branch", "--show-current"); out != "" {
		t.Errorf("the workspace is on branch %q, want none", out)
	}
	if out := git(t, a.Workspace.Path, "status", "--porcelain"); out != "" {
		t.Errorf("the workspace is not clean: %s", out)
	}
	if common := git(t, a.Workspace.Path, "rev-parse", "--git-common-dir"); !strings.Contains(common, "fixture") {
		t.Errorf("the workspace belongs to %s, want AsmAI's clone", common)
	}
	if out := git(t, v.clone, "branch", "--list", "asmai/job-1/*"); strings.Contains(out, "asmai/job-1/"+string(rune('0'+a.ID))) {
		t.Errorf("the clone has a branch for the validation: %s", out)
	}

	// What it refuses: a head that is not the tip, a writing assignment by
	// Quality, a validation by Engineering, and a second at once.
	for name, tt := range map[string]struct {
		session string
		req     Request
		want    string
	}{
		"a commit that is not the tip": {"quality", Request{Job: 1, ReadOnly: true, Commit: v.commits[1]}, "exact head"},
		"no commit":                    {"quality", Request{Job: 1, ReadOnly: true}, "give --commit"},
		"a writing assignment":         {"quality", Request{Job: 1}, "only leader@engineering may assign writing work"},
		"a validation by Engineering":  {"credential", Request{Job: 1, ReadOnly: true, Commit: v.tip}, "only leader@quality may assign read-only validation"},
		"a commit for writing work":    {"credential", Request{Job: 1, Commit: v.tip}, "takes no --commit"},
	} {
		req := tt.req
		req.Session, req.Command, req.Outcome, req.Criteria = tt.session, CommandAssign, "outcome", []string{"criterion"}
		if _, _, err := v.d.assign(req); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: assign returned %v, want it to say %q", name, err, tt.want)
		}
	}
	// With the first validation still running, another is refused, and its
	// workspace is undone.
	count := len(v.entriesOf(t))
	if _, _, err := v.d.assign(Request{Session: "quality", Command: CommandAssign, Job: 1, ReadOnly: true, Commit: v.tip, Outcome: "again", Criteria: []string{"c"}}); err == nil || !strings.Contains(err.Error(), "already has validation") {
		t.Errorf("a second validation returned %v", err)
	}
	if got := len(v.entriesOf(t)); got != count {
		t.Errorf("the refused validation left %d assignments, want %d", got, count)
	}
	if out := git(t, v.clone, "worktree", "list"); strings.Count(out, "assignment-") != 2 {
		// The job's first assignment's workspace is gone, and the second
		// result's and the validation's remain.
		t.Errorf("the clone's checkouts are\n%s\nwant the second result's workspace and the validation's only", out)
	}
}

func (v validationFixture) entriesOf(t *testing.T) []store.Assignment {
	t.Helper()
	assignments, err := v.s.AssignmentsForJob(v.job.Number)
	if err != nil {
		t.Fatal(err)
	}
	return assignments
}

func TestAValidationReportIsAtItsCommitAndAcceptingItDeliversItWithoutMovingTheJobBranch(t *testing.T) {
	v := newValidationFixture(t)
	a := v.assignment
	worker := roles.WorkerOf(roles.Quality, 1)
	l := &leader{address: worker, session: &session.Session{}, generation: 1, assignment: a.ID, currentDispatch: v.dispatch.ID}
	v.d.leaders[worker.String()] = l
	v.d.sessions["worker"] = sessionRef{address: worker, generation: 1}
	if err := v.s.ChangeDispatch(v.dispatch.ID, 1, store.DispatchCreated, store.DispatchWorking, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	input := store.ReportInput{
		Evidence: []string{"./check.sh -> passed"}, Checks: []store.Check{{Command: "./check.sh", Outcome: "passed"}}, Gaps: []string{},
		Validation: &store.ValidationInput{Findings: []store.FindingInput{{Kind: store.FindingBlocking, Text: "file-1.txt says work, not hello"}}},
	}
	report := func() (store.Report, error) {
		r, _, err := v.d.report(Request{Session: "worker", Command: CommandResult, Report: &input})
		return r, err
	}

	// A worker that changed the work, however it did it, is not at the commit
	// it was fixed at, and its report is refused.
	git(t, a.Workspace.Path, "commit", "--allow-empty", "-m", "a fix, which Quality never makes")
	if _, err := report(); err == nil || !strings.Contains(err.Error(), "validation never changes the work") {
		t.Fatalf("a report from a workspace that moved returned %v", err)
	}
	git(t, a.Workspace.Path, "reset", "--hard", v.tip)

	r, err := report()
	if err != nil {
		t.Fatal(err)
	}
	if r.Commit != v.tip || r.Validation == nil || len(r.Validation.Findings) != 1 || !strings.Contains(strings.Join(r.Artifacts, "\n"), "validated commit "+v.tip) {
		t.Errorf("the validation report is %+v, want it at %s naming it in its evidence", r, v.tip)
	}
	if got, _ := v.s.Assignment(a.ID); got.State != store.AssignmentSubmitted {
		t.Errorf("the validation is %s, want submitted", got.State)
	}

	// Only Quality's leader, who owns it, decides on it.
	if _, _, err := v.d.decide(Request{Session: "credential", Command: CommandAccept, Assignment: a.ID, Reasons: []string{"x"}}); err == nil || !strings.Contains(err.Error(), "owned by leader@quality") {
		t.Errorf("Engineering's leader accepting Quality's validation returned %v", err)
	}
	l.session = nil // the worker is stopped by the acceptance, which this fake session cannot
	resp, notify, err := v.d.decide(Request{Session: "quality", Command: CommandAccept, Assignment: a.ID, Reasons: []string{"the checks re-ran at the head"}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Assignment.State != store.AssignmentAccepted || resp.Dispatch == nil || resp.Dispatch.Agent != "leader@engineering" || notify != "leader@engineering" || resp.JobBranch != nil {
		t.Errorf("the acceptance answered %+v, %+v and told %q", resp.Assignment, resp.Dispatch, notify)
	}
	if got := git(t, v.clone, "rev-parse", "refs/heads/"+JobBranchName(v.job)); got != v.tip {
		t.Errorf("the job branch is at %s, want it unmoved at %s", got, v.tip)
	}
	if _, err := os.Stat(a.Workspace.Path); !os.IsNotExist(err) {
		t.Errorf("the read-only workspace is still there when its assignment ended (%v)", err)
	}
	if out := git(t, v.clone, "worktree", "list"); strings.Contains(out, a.Workspace.Path) {
		t.Errorf("the clone still records the read-only workspace:\n%s", out)
	}
	open, err := v.s.OpenFindings(v.job.Number)
	if err != nil || len(open) != 1 || open[0].Text != "file-1.txt says work, not hello" || open[0].Commit != v.tip {
		t.Errorf("the open blocking findings are %+v (%v)", open, err)
	}
	removed := 0
	entries, err := v.s.Journal()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Kind == store.KindReadOnlyWorkspaceRemoved {
			removed++
		}
	}
	if removed != 1 {
		t.Errorf("the journal holds %d read-only workspace removals, want 1", removed)
	}
}

func TestAValidationReportCanBeRejectedBackToItsWorkerAndRecordsNoFindingUntilAccepted(t *testing.T) {
	v := newValidationFixture(t)
	a := v.assignment
	worker := roles.WorkerOf(roles.Quality, 1)
	v.d.leaders[worker.String()] = &leader{address: worker, session: &session.Session{}, generation: 1, assignment: a.ID, currentDispatch: v.dispatch.ID}
	v.d.sessions["worker"] = sessionRef{address: worker, generation: 1}
	if err := v.s.ChangeDispatch(v.dispatch.ID, 1, store.DispatchCreated, store.DispatchWorking, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	input := store.ReportInput{
		Evidence: []string{"read the diff"}, Checks: []store.Check{}, Gaps: []string{"the checks were not re-run"},
		Validation: &store.ValidationInput{Findings: []store.FindingInput{{Kind: store.FindingAdvisory, Text: "naming"}}},
	}
	if _, _, err := v.d.report(Request{Session: "worker", Command: CommandResult, Report: &input}); err != nil {
		t.Fatal(err)
	}
	resp, notify, err := v.d.decide(Request{Session: "quality", Command: CommandReject, Assignment: a.ID, Reasons: []string{"the repository's checks were not re-run"}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Assignment.State != store.AssignmentActive || resp.Dispatch == nil || resp.Dispatch.Agent != worker.String() || notify != worker.String() {
		t.Errorf("the rejection answered %+v and %+v, told %q, want the assignment active again with its worker", resp.Assignment, resp.Dispatch, notify)
	}
	if findings, err := v.s.Findings(v.job.Number); err != nil || len(findings) != 0 {
		t.Errorf("a rejected validation recorded findings %+v (%v)", findings, err)
	}
	if _, err := os.Stat(a.Workspace.Path); err != nil {
		t.Errorf("a rejection removed the read-only workspace: %v", err)
	}
}

func TestCancellingAValidationEndsItWithNoPushAndRemovesItsWorkspace(t *testing.T) {
	v := newValidationFixture(t)
	a := v.assignment
	resp, notify, err := v.d.decide(Request{Session: "quality", Command: CommandCancel, Assignment: a.ID, Reasons: []string{"the job was withdrawn"}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Assignment.State != store.AssignmentCancelling || notify != "worker1@quality" {
		t.Fatalf("the cancellation answered %+v, told %q, want the assignment cancelling with its worker told", resp.Assignment, notify)
	}
	if _, err := os.Stat(a.Workspace.Path); err != nil {
		t.Errorf("the workspace was removed before the worker stopped: %v", err)
	}

	// Once the worker has stopped there is nothing to wait for: it has no
	// branch to push.
	v.d.cancelled(sessionRef{address: roles.WorkerOf(roles.Quality, 1), generation: 1}, a.ID)
	got, err := v.s.Assignment(a.ID)
	if err != nil || got.State != store.AssignmentCancelled {
		t.Fatalf("the validation is %+v (%v), want it cancelled", got, err)
	}
	if _, err := os.Stat(a.Workspace.Path); !os.IsNotExist(err) {
		t.Errorf("the cancelled validation's workspace is still there (%v)", err)
	}
	// Quality's next validation of the job can be assigned at once.
	if live, err := v.s.LiveValidation(v.job.Number); err != nil || live != 0 {
		t.Errorf("the job still has validation %d live (%v)", live, err)
	}
}
