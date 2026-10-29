// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/session"
	"github.com/talvor/asmai/internal/store"
)

// qualityLeader makes Quality's leader a session the fixture can call as.
func (f *twoResults) qualityLeader() {
	quality := roles.LeaderOf(roles.Quality)
	f.d.leaders[quality.String()] = &leader{address: quality, session: &session.Session{}, generation: 1}
	f.d.sessions["quality"] = sessionRef{address: quality, generation: 1}
}

// tip is the job branch's tip.
func (f *twoResults) tip(t *testing.T) string {
	t.Helper()
	b, err := f.s.JobBranch(f.job.Number)
	if err != nil {
		t.Fatal(err)
	}
	return b.Tip
}

// assignValidation has Quality's leader assign the read-only validation req
// describes, which fails only in starting a worker no provider is installed
// for, and returns the assignment it recorded, if it recorded one.
func (f *twoResults) assignValidation(t *testing.T, req Request) (store.Assignment, error) {
	t.Helper()
	before, err := f.s.AssignmentsForJob(f.job.Number)
	if err != nil {
		t.Fatal(err)
	}
	req.Session, req.Command, req.Job, req.ReadOnly = "quality", CommandAssign, f.job.Number, true
	req.Outcome, req.Criteria = "Validate", []string{"checks re-run"}
	_, _, err = f.d.assign(req)
	after, listErr := f.s.AssignmentsForJob(f.job.Number)
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(after) == len(before) {
		return store.Assignment{}, err
	}
	if err != nil && !strings.Contains(err.Error(), "starting worker") {
		t.Fatalf("assigning the validation returned %v", err)
	}
	return after[len(after)-1], nil
}

// validate has Quality validate the job branch's tip: it assigns the
// validation, its worker reports input at the tip, and Quality's leader
// accepts the report. It returns the validation.
func (f *twoResults) validate(t *testing.T, input store.ValidationInput) store.Assignment {
	t.Helper()
	f.qualityLeader()
	a, err := f.assignValidation(t, Request{Commit: f.tip(t)})
	if a.ID == 0 {
		t.Fatalf("assigning the validation returned %v", err)
	}
	return f.finishValidation(t, a, input)
}

// finishValidation has the worker of validation a report input and Quality's
// leader accept it.
func (f *twoResults) finishValidation(t *testing.T, a store.Assignment, input store.ValidationInput) store.Assignment {
	t.Helper()
	dispatch, err := f.s.NextDispatch(a.Worker)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := roles.ParseAddress(a.Worker)
	if err != nil {
		t.Fatal(err)
	}
	l := &leader{address: worker, session: &session.Session{}, generation: 1, assignment: a.ID, currentDispatch: dispatch.ID}
	f.d.leaders[worker.String()] = l
	f.d.sessions["worker"] = sessionRef{address: worker, generation: 1}
	if err := f.s.ChangeDispatch(dispatch.ID, 1, store.DispatchCreated, store.DispatchWorking, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if input.Findings == nil {
		input.Findings = []store.FindingInput{}
	}
	report := store.ReportInput{Evidence: []string{"./check.sh -> passed"}, Checks: []store.Check{{Command: "./check.sh", Outcome: "passed"}}, Gaps: []string{}, Validation: &input}
	if _, _, err := f.d.report(Request{Session: "worker", Command: CommandResult, Report: &report}); err != nil {
		t.Fatal(err)
	}
	l.session = nil // the acceptance stops the worker, which this fake session cannot
	if _, _, err := f.d.decide(Request{Session: "quality", Command: CommandAccept, Assignment: a.ID, Reasons: []string{"the report meets its criteria"}}); err != nil {
		t.Fatal(err)
	}
	done, err := f.s.Assignment(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	return done
}

// moveBase has someone else commit file on the repository's base on origin.
func (f *twoResults) moveBase(t *testing.T, file, text string) {
	t.Helper()
	other := filepath.Join(t.TempDir(), "other")
	git(t, filepath.Dir(other), "clone", f.origin, other)
	if err := os.WriteFile(filepath.Join(other, file), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, other, "add", file)
	git(t, other, "commit", "-m", "Move the base")
	git(t, other, "push", "origin", "trunk")
}

// handoffTo has the Engineering leader's session ask for the validation of
// the job branch, and returns what the daemon fixes for it.
func (f *twoResults) handoffTarget(t *testing.T) (validationTarget, error) {
	t.Helper()
	return f.d.validationTarget(Request{Session: "credential", Command: CommandHandoffSend, Job: f.job.Number, Agent: "leader@quality"})
}

func TestAFixAfterABlockingFindingIsReviewedForWhatChangedAndAskedToConfirmTheFinding(t *testing.T) {
	f := newTwoResults(t)
	if _, err := f.decide(CommandAccept, f.assignments[0].ID, "file-1.txt is there"); err != nil {
		t.Fatal(err)
	}
	first, err := f.handoffTarget(t)
	if err != nil || first.review != store.ReviewFull || first.since != "" || first.head != f.commits[0] {
		t.Fatalf("the first validation is %+v (%v), want a full review of %s", first, err, f.commits[0])
	}
	v1 := f.validate(t, store.ValidationInput{Findings: []store.FindingInput{{Kind: store.FindingBlocking, Text: "file-1.txt says work, not hello"}}})
	if v1.Review != store.ReviewFull || v1.Since != "" {
		t.Errorf("the first validation is %+v, want a full review", v1)
	}
	open, err := f.s.OpenFindings(f.job.Number)
	if err != nil || len(open) != 1 {
		t.Fatalf("the open findings are %+v (%v)", open, err)
	}

	// The blocking finding leaves the head validated but not passed, and the
	// head Quality validated is not validated again.
	if _, err := f.handoffTarget(t); err == nil || !strings.Contains(err.Error(), "already validated") {
		t.Errorf("handing over the head Quality already validated returned %v", err)
	}
	if err := f.s.ValidatedHead(f.job.Number, f.commits[0]); err == nil || !strings.Contains(err.Error(), "blocking finding") {
		t.Errorf("a head with a blocking finding open reads as validated: %v", err)
	}

	// Engineering assigns the fix, and accepting it is a later commit: the
	// evidence for the old head is stale for the new one.
	fix := f.writingResult(t, func(dir string) {
		if err := os.WriteFile(filepath.Join(dir, "file-1.txt"), []byte("hello\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, dir, "commit", "-am", "Say hello")
	})
	resp, err := f.decide(CommandAccept, fix.ID, "file-1.txt says hello")
	if err != nil {
		t.Fatal(err)
	}
	fixed := resp.JobBranch.Tip
	if err := f.s.ValidatedHead(f.job.Number, fixed); err == nil || !strings.Contains(err.Error(), "has not validated "+fixed) {
		t.Errorf("a head nobody validated reads as validated: %v", err)
	}
	if err := f.s.TestedPR(f.job.Number, fixed); err == nil {
		t.Error("the fixed head is a tested pull request on the old head's validation")
	}
	target, err := f.handoffTarget(t)
	if err != nil || target.review != store.ReviewChanges || target.since != f.commits[0] || target.head != fixed {
		t.Fatalf("the re-validation is %+v (%v), want the changes since %s", target, err, f.commits[0])
	}

	// The validation says so, and the report must say for each earlier
	// blocking finding whether it is resolved.
	f.qualityLeader()
	a, err := f.assignValidation(t, Request{Commit: fixed, Skill: ""})
	if a.ID == 0 || a.Review != store.ReviewChanges || a.Since != f.commits[0] || a.Commit != fixed {
		t.Fatalf("the re-validation is %+v (%v)", a, err)
	}
	f.finishValidation(t, a, store.ValidationInput{Resolved: []int64{open[0].ID}})
	if open, _ := f.s.OpenFindings(f.job.Number); len(open) != 0 {
		t.Errorf("the re-validation left %+v open", open)
	}
	if err := f.s.TestedPR(f.job.Number, fixed); err == nil || strings.Contains(err.Error(), "validated") || strings.Contains(err.Error(), "finding") {
		t.Errorf("the fixed head, validated with no finding open, is refused for %v, want only its push", err)
	}
}

func TestACleanMergeOfAMovedBaseNeedsOnlyTheChecksAndAResolvedConflictIsReviewed(t *testing.T) {
	f := newTwoResults(t)
	if _, err := f.decide(CommandAccept, f.assignments[0].ID, "file-1.txt is there"); err != nil {
		t.Fatal(err)
	}
	f.validate(t, store.ValidationInput{})

	// The base moves, and a worker merges it in without a conflict.
	f.moveBase(t, "base.txt", "moved\n")
	merge := f.writingResult(t, func(dir string) {
		git(t, dir, "fetch", "origin")
		git(t, dir, "merge", "--no-edit", "origin/trunk")
	})
	resp, err := f.decide(CommandAccept, merge.ID, "the base is merged in")
	if err != nil {
		t.Fatal(err)
	}
	merged := resp.JobBranch.Tip
	target, err := f.handoffTarget(t)
	if err != nil || target.review != store.ReviewChecks || target.since != f.commits[0] || target.head != merged {
		t.Fatalf("the validation after a clean merge is %+v (%v), want only the checks since %s", target, err, f.commits[0])
	}
	// There is no review for a skill to run.
	f.qualityLeader()
	if _, err := f.assignValidation(t, Request{Commit: merged, Skill: "code-review"}); err == nil || !strings.Contains(err.Error(), "needs only the repository's checks") {
		t.Errorf("assigning a review skill for a clean merge returned %v", err)
	}
	a, err := f.assignValidation(t, Request{Commit: merged})
	if a.ID == 0 || a.Review != store.ReviewChecks || a.Since != f.commits[0] {
		t.Fatalf("the checks-only validation is %+v (%v)", a, err)
	}
	f.finishValidation(t, a, store.ValidationInput{})
	if err := f.s.ValidatedHead(f.job.Number, merged); err != nil {
		t.Errorf("a head whose checks Quality re-ran is not validated: %v", err)
	}
	if commit, err := f.s.LastValidated(f.job.Number); err != nil || commit != merged {
		t.Errorf("the last validated commit is %q (%v), want the merge", commit, err)
	}
}

func TestAMergeWhoseConflictAWorkerResolvedIsReviewedLikeAFix(t *testing.T) {
	f := newTwoResults(t)
	if _, err := f.decide(CommandAccept, f.assignments[0].ID, "file-1.txt is there"); err != nil {
		t.Fatal(err)
	}
	f.validate(t, store.ValidationInput{})

	f.moveBase(t, "file-1.txt", "from the base\n")
	merge := f.writingResult(t, func(dir string) {
		git(t, dir, "fetch", "origin")
		cmd := execGit(dir, "merge", "--no-edit", "origin/trunk")
		if err := cmd.Run(); err == nil {
			t.Fatal("the merge did not conflict")
		}
		if err := os.WriteFile(filepath.Join(dir, "file-1.txt"), []byte("resolved by hand\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, dir, "add", "file-1.txt")
		git(t, dir, "commit", "--no-edit")
	})
	if _, err := f.decide(CommandAccept, merge.ID, "the conflict is resolved"); err != nil {
		t.Fatal(err)
	}
	target, err := f.handoffTarget(t)
	if err != nil || target.review != store.ReviewChanges || target.since != f.commits[0] {
		t.Fatalf("the validation after a resolved conflict is %+v (%v), want a review of the changes", target, err)
	}
}

func TestAnOutsideCommitMergedInIsReviewedLikeAFixNotChecked(t *testing.T) {
	f := newTwoResults(t)
	if _, err := f.decide(CommandAccept, f.assignments[0].ID, "file-1.txt is there"); err != nil {
		t.Fatal(err)
	}
	f.validate(t, store.ValidationInput{})
	branch := jobBranchFor(f.job)
	other := filepath.Join(t.TempDir(), "other")
	git(t, filepath.Dir(other), "clone", f.origin, other)
	git(t, other, "checkout", "-b", branch)
	if err := os.WriteFile(filepath.Join(other, "outside.txt"), []byte("outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, other, "add", "outside.txt")
	git(t, other, "commit", "-m", "An outside commit")
	git(t, other, "push", "origin", branch)
	merge := f.writingResult(t, func(dir string) {
		git(t, dir, "fetch", "origin")
		git(t, dir, "merge", "--no-edit", "origin/"+branch)
	})
	if _, err := f.decide(CommandAccept, merge.ID, "the outside commit is merged in"); err != nil {
		t.Fatal(err)
	}
	target, err := f.handoffTarget(t)
	if err != nil || target.review != store.ReviewChanges {
		t.Fatalf("the validation after merging an outside commit is %+v (%v), want a review of the changes", target, err)
	}
}

func TestAnyLeaderMayAskQualityToReviewAnAssignmentEarlierAtItsResult(t *testing.T) {
	f := newTwoResults(t)
	f.qualityLeader()
	coordination := roles.LeaderOf(roles.Coordination)
	f.d.leaders[coordination.String()] = &leader{address: coordination, session: &session.Session{}, generation: 1}
	f.d.sessions["coordination"] = sessionRef{address: coordination, generation: 1}
	if _, err := f.decide(CommandAccept, f.assignments[0].ID, "file-1.txt is there"); err != nil {
		t.Fatal(err)
	}
	risky := f.assignments[1]
	ask := func(session string, assignment int64) (validationTarget, error) {
		return f.d.validationTarget(Request{Session: session, Command: CommandHandoffSend, Job: f.job.Number, Agent: "leader@quality", Assignment: assignment})
	}

	// Any leader but Quality's asks, for an assignment of the job that has a
	// result, at the commit of that result: not at the job branch's tip.
	for _, session := range []string{"credential", "coordination"} {
		target, err := ask(session, risky.ID)
		if err != nil || target.head != f.commits[1] || target.review != store.ReviewEarly || target.assignment != risky.ID || target.since != "" {
			t.Errorf("%s asking for an early review got %+v (%v), want the result at %s", session, target, err, f.commits[1])
		}
	}
	if _, err := ask("quality", risky.ID); err == nil || !strings.Contains(err.Error(), "Quality never asks itself") {
		t.Errorf("Quality asking itself returned %v", err)
	}
	if _, err := ask("credential", 99); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("an assignment that does not exist returned %v", err)
	}
	if _, err := f.d.validationTarget(Request{Session: "coordination", Command: CommandHandoffSend, Job: f.job.Number, Agent: "leader@quality"}); err == nil || !strings.Contains(err.Error(), "only leader@engineering hands a job branch") {
		t.Errorf("Coordination handing the job branch over returned %v", err)
	}

	// The handoff records the review, and Quality's leader accepts it.
	target, _ := ask("coordination", risky.ID)
	h, hd, err := f.s.SendHandoff(store.Handoff{Job: f.job.Number, Sender: coordination.String(), Receiver: "leader@quality", Outcome: "Review the risky assignment", Decisions: "none", Evidence: "none", Constraints: "none", Permissions: "none", Criteria: []string{"reviewed"}, Head: target.head, Review: target.review, Assignment: target.assignment}, time.Now())
	if err != nil || h.Review != store.ReviewEarly || h.Assignment != risky.ID {
		t.Fatalf("the early handoff is %+v (%v)", h, err)
	}
	if _, _, err := f.s.SendHandoff(store.Handoff{Job: f.job.Number, Sender: coordination.String(), Receiver: "leader@quality", Outcome: "o", Decisions: "none", Evidence: "none", Constraints: "none", Permissions: "none", Criteria: []string{"c"}, Head: f.commits[0], Review: store.ReviewEarly, Assignment: risky.ID}, time.Now()); err == nil || !strings.Contains(err.Error(), "is at "+f.commits[1]) {
		t.Errorf("an early handoff at a commit that is not the result returned %v", err)
	}
	if _, err := f.s.EarlyReviewRequest(f.job.Number, risky.ID); err == nil {
		t.Error("a handoff Quality's leader has not accepted is a request to assign")
	}
	if err := f.s.ChangeDispatch(hd.ID, 1, store.DispatchCreated, store.DispatchDelivered, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Inbox("leader@quality", hd.ID, hd.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.s.AnswerHandoff(h.ID, "leader@quality", hd.ID, 1, store.HandoffAccepted, "", time.Now()); err != nil {
		t.Fatal(err)
	}

	// The validation is fixed at that result, answers the leader that asked,
	// and refuses what the handoff does not name.
	if _, err := f.assignValidation(t, Request{Commit: f.commits[0], Assignment: risky.ID}); err == nil || !strings.Contains(err.Error(), "fixed at "+f.commits[1]) {
		t.Errorf("an early review at another commit returned %v", err)
	}
	if _, err := f.assignValidation(t, Request{Commit: f.commits[1], Assignment: f.assignments[0].ID}); err == nil || !strings.Contains(err.Error(), "no leader has asked Quality") {
		t.Errorf("an early review nobody asked for returned %v", err)
	}
	if _, _, err := f.d.assign(Request{Session: "credential", Command: CommandAssign, Job: f.job.Number, Assignment: risky.ID, Outcome: "o", Criteria: []string{"c"}}); err == nil || !strings.Contains(err.Error(), "only a read-only assignment has") {
		t.Errorf("a writing assignment of an early review returned %v", err)
	}
	a, err := f.assignValidation(t, Request{Commit: f.commits[1], Assignment: risky.ID})
	if a.ID == 0 || a.Review != store.ReviewEarly || a.Reviews != risky.ID || a.Requester != coordination.String() || a.Commit != f.commits[1] || a.Since != "" {
		t.Fatalf("the early review is %+v (%v)", a, err)
	}
	if got := git(t, a.Workspace.Path, "rev-parse", "HEAD"); got != f.commits[1] {
		t.Errorf("the early review's workspace is at %s, want the result %s", got, f.commits[1])
	}
	if tip := f.tip(t); tip != f.commits[0] {
		t.Errorf("the job branch is at %s, want it where it was, %s", tip, f.commits[0])
	}

	// Its report goes to the leader that asked, records its findings, and
	// validates no head of the job.
	dispatch, err := f.s.NextDispatch(a.Worker)
	if err != nil {
		t.Fatal(err)
	}
	worker, _ := roles.ParseAddress(a.Worker)
	l := &leader{address: worker, session: &session.Session{}, generation: 1, assignment: a.ID, currentDispatch: dispatch.ID}
	f.d.leaders[worker.String()] = l
	f.d.sessions["worker"] = sessionRef{address: worker, generation: 1}
	if err := f.s.ChangeDispatch(dispatch.ID, 1, store.DispatchCreated, store.DispatchWorking, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	report := store.ReportInput{Evidence: []string{"read the result"}, Checks: []store.Check{{Command: "./check.sh", Outcome: "passed"}}, Gaps: []string{},
		Validation: &store.ValidationInput{Findings: []store.FindingInput{{Kind: store.FindingBlocking, Text: "file-2.txt is risky"}}}}
	if _, _, err := f.d.report(Request{Session: "worker", Command: CommandResult, Report: &report}); err != nil {
		t.Fatal(err)
	}
	l.session = nil
	resp, notify, err := f.d.decide(Request{Session: "quality", Command: CommandAccept, Assignment: a.ID, Reasons: []string{"the report meets its criteria"}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Dispatch == nil || resp.Dispatch.Agent != coordination.String() || notify != coordination.String() {
		t.Errorf("the early review's report went to %+v, told %q, want %s", resp.Dispatch, notify, coordination)
	}
	if open, err := f.s.OpenFindings(f.job.Number); err != nil || len(open) != 1 || open[0].Commit != f.commits[1] {
		t.Errorf("the open findings are %+v (%v)", open, err)
	}
	if commit, err := f.s.LastValidated(f.job.Number); err != nil || commit != "" {
		t.Errorf("an early review validated %q (%v), want no head", commit, err)
	}
	if err := f.s.ValidatedHead(f.job.Number, f.commits[1]); err == nil {
		t.Error("the commit of an early review reads as a validated head")
	}
	// The job branch's own validation is still a full review, and must still
	// say whether the finding the early review found is resolved.
	target, err = f.handoffTarget(t)
	if err != nil || target.review != store.ReviewFull {
		t.Errorf("the validation after an early review is %+v (%v), want a full review", target, err)
	}
}

// execGit is git run in dir for a test that expects it to fail.
func execGit(dir string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@example.test", "GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@example.test")
	return cmd
}
