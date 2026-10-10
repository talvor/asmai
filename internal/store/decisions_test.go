// SPDX-License-Identifier: Apache-2.0

package store

import (
	"path/filepath"
	"strings"
	"testing"
)

const owner = "leader@engineering"

// submitted gives s a job whose one assignment has a result submitted, and
// returns the assignment.
func submitted(t *testing.T, s *Store) Assignment {
	t.Helper()
	job := jobWithBranch(t, s)
	a, d := newAssignment(t, s, job)
	working(t, s, d)
	if _, err := s.EffectRecorded("worker1@engineering", 1, d.ID, "push", "origin/asmai/job-1/1@"+otherCommit, assignmentsAt); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ReportSubmitted(submission(d, ReportResult), assignmentsAt); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestAcceptingAResultEndsTheAssignmentAndMovesTheJobBranchInOneStep(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	a := submitted(t, s)

	for name, in := range map[string]Acceptance{
		"reasons":                   {Assignment: a.ID, Leader: owner, FromTip: tipCommit},
		"reasons with text":         {Assignment: a.ID, Leader: owner, Reasons: []string{" "}, FromTip: tipCommit},
		"its owning leader":         {Assignment: a.ID, Leader: "leader@quality", Reasons: []string{"x"}, FromTip: tipCommit},
		"an assignment that exists": {Assignment: 99, Leader: owner, Reasons: []string{"x"}, FromTip: tipCommit},
		"the branch's recorded tip": {Assignment: a.ID, Leader: owner, Reasons: []string{"x"}, FromTip: otherCommit},
	} {
		if _, err := s.ResultAccepted(in, assignmentsAt); err == nil {
			t.Errorf("an acceptance without %s was recorded", name)
		}
	}
	if b, _ := s.JobBranch(a.Job); b.Tip != tipCommit {
		t.Fatalf("a refused acceptance moved the job branch to %s", b.Tip)
	}
	if got, _ := s.Assignment(a.ID); got.State != AssignmentSubmitted {
		t.Fatalf("a refused acceptance moved the assignment to %s", got.State)
	}

	d, err := s.ResultAccepted(Acceptance{Assignment: a.ID, Leader: owner, Reasons: []string{"greeting.txt says hello: check.sh passed at the commit", "the check passes"}, FromTip: tipCommit}, assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind != DecisionAccepted || d.Leader != owner || d.Commit != otherCommit || d.Report != 1 || len(d.Reasons) != 2 {
		t.Errorf("the decision is %+v", d)
	}
	if got, _ := s.Assignment(a.ID); got.State != AssignmentAccepted {
		t.Errorf("the assignment is %s, want accepted", got.State)
	}
	if b, _ := s.JobBranch(a.Job); b.Tip != otherCommit || b.StartedFrom != tipCommit {
		t.Errorf("the job branch is %+v, want its tip at the accepted commit and the start unchanged", b)
	}
	decisions, err := s.Decisions(a.ID)
	if err != nil || len(decisions) != 1 || decisions[0].Leader != owner || strings.Join(decisions[0].Reasons, "|") != "greeting.txt says hello: check.sh passed at the commit|the check passes" {
		t.Errorf("the decisions are %+v (%v)", decisions, err)
	}
	got := strings.Join(kinds(t, s), ",")
	if !strings.Contains(got, KindJobBranchMoved) || !strings.Contains(got, KindResultAccepted) {
		t.Errorf("the journal holds %s, want the branch's move and the acceptance", got)
	}

	// The assignment has ended: it frees its worker and slot, takes no
	// further decision, and later work is a new assignment.
	if worker, slot, err := s.Allocate("engineering"); err != nil || worker != 1 || slot != 1 {
		t.Errorf("after the acceptance the next worker and slot are %d and %d (%v), want 1 and 1", worker, slot, err)
	}
	if _, err := s.ResultAccepted(Acceptance{Assignment: a.ID, Leader: owner, Reasons: []string{"x"}, FromTip: otherCommit}, assignmentsAt); err == nil || !strings.Contains(err.Error(), "accepted") {
		t.Errorf("a second acceptance returned %v", err)
	}
	if _, _, err := s.ResultRejected(a.ID, owner, []string{"x"}, assignmentsAt); err == nil {
		t.Error("an accepted result was rejected")
	}
	if _, _, err := s.CancellationRequested(a.ID, owner, []string{"x"}, assignmentsAt); err == nil {
		t.Error("an accepted assignment was cancelled")
	}
}

func TestRejectingAResultReturnsTheAssignmentToTheSameWorkerInANewDispatch(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	a := submitted(t, s)

	for name, call := range map[string]func() error{
		"reasons": func() error { _, _, err := s.ResultRejected(a.ID, owner, nil, assignmentsAt); return err },
		"its owning leader": func() error {
			_, _, err := s.ResultRejected(a.ID, "leader@quality", []string{"x"}, assignmentsAt)
			return err
		},
		"an assignment that exists": func() error { _, _, err := s.ResultRejected(99, owner, []string{"x"}, assignmentsAt); return err },
	} {
		if call() == nil {
			t.Errorf("a rejection without %s was recorded", name)
		}
	}
	d, dispatch, err := s.ResultRejected(a.ID, owner, []string{"the check does not cover an empty file"}, assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind != DecisionRejected || d.Leader != owner || d.Commit != otherCommit || d.Report != 1 {
		t.Errorf("the decision is %+v", d)
	}
	got, _ := s.Assignment(a.ID)
	if got.State != AssignmentActive || got.Worker != "worker1@engineering" {
		t.Errorf("the assignment is %s with %s, want it active with worker1@engineering", got.State, got.Worker)
	}
	if dispatch.Agent != "worker1@engineering" || dispatch.State != DispatchCreated {
		t.Errorf("the rejection's dispatch is %+v, want a created one for the same worker", dispatch)
	}
	if live, err := s.LiveAssignmentOf("worker1@engineering"); err != nil || live.ID != a.ID {
		t.Errorf("the worker carries %+v (%v), want assignment %d", live, err, a.ID)
	}
	messages, err := s.Inbox("worker1@engineering", dispatch.ID, assignmentsAt)
	if err != nil || len(messages) != 1 || messages[0].Kind != MessageRejection || messages[0].Body != "the check does not cover an empty file" || messages[0].ReportData == nil || messages[0].AssignmentData == nil {
		t.Fatalf("the worker's inbox holds %+v (%v)", messages, err)
	}
	if b, _ := s.JobBranch(a.Job); b.Tip != tipCommit {
		t.Errorf("a rejection moved the job branch to %s", b.Tip)
	}

	// The worker submits again in the dispatch that returned the work.
	if err := s.ChangeDispatch(dispatch.ID, 1, DispatchDelivered, DispatchWorking, "", assignmentsAt); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EffectRecorded("worker1@engineering", 1, dispatch.ID, "push", "origin/asmai/job-1/1@"+otherCommit, assignmentsAt); err != nil {
		t.Fatal(err)
	}
	sub := submission(dispatch, ReportResult)
	sub.Commit = tipCommit[:39] + "3"
	if _, _, err := s.ReportSubmitted(sub, assignmentsAt); err != nil {
		t.Fatalf("a result in the rejection's dispatch: %v", err)
	}
	if got, _ := s.Assignment(a.ID); got.State != AssignmentSubmitted {
		t.Errorf("the assignment is %s, want submitted again", got.State)
	}
	if latest, err := s.LatestResult(a.ID); err != nil || latest.ID != 2 || latest.Commit != sub.Commit {
		t.Errorf("the latest result is %+v (%v), want the second", latest, err)
	}
	// An active assignment has no result to decide on.
	if _, _, err := s.ResultRejected(a.ID, owner, []string{"x"}, assignmentsAt); err != nil {
		t.Errorf("the second result could not be rejected: %v", err)
	}
	if _, _, err := s.ResultRejected(a.ID, owner, []string{"x"}, assignmentsAt); err == nil || !strings.Contains(err.Error(), "active") {
		t.Errorf("rejecting an active assignment's result returned %v", err)
	}
	if _, err := s.ResultAccepted(Acceptance{Assignment: a.ID, Leader: owner, Reasons: []string{"x"}, FromTip: tipCommit}, assignmentsAt); err == nil {
		t.Error("an active assignment's result was accepted")
	}
}

func TestCancellingAnAssignmentHoldsItsWorkerUntilTheWorkerHasStopped(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	job := jobWithBranch(t, s)
	a, d := newAssignment(t, s, job)
	working(t, s, d)

	if _, _, err := s.CancellationRequested(a.ID, owner, nil, assignmentsAt); err == nil {
		t.Error("a cancellation without reasons was recorded")
	}
	if _, _, err := s.CancellationRequested(a.ID, "leader@quality", []string{"x"}, assignmentsAt); err == nil {
		t.Error("a cancellation by another leader was recorded")
	}
	decision, dispatch, err := s.CancellationRequested(a.ID, owner, []string{"the job changed scope"}, assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Kind != DecisionCancelled || decision.Report != 0 || dispatch.Agent != "worker1@engineering" {
		t.Errorf("the decision is %+v with dispatch %+v", decision, dispatch)
	}
	if got, _ := s.Assignment(a.ID); got.State != AssignmentCancelling {
		t.Errorf("the assignment is %s, want cancelling", got.State)
	}
	// Until the worker has stopped it holds its number, slot and workspace.
	if worker, slot, err := s.Allocate("engineering"); err != nil || worker != 2 || slot != 2 {
		t.Errorf("while cancelling the next worker and slot are %d and %d (%v), want 2 and 2", worker, slot, err)
	}
	if live, err := s.LiveAssignmentOf("worker1@engineering"); err != nil || live.ID != a.ID {
		t.Errorf("the worker carries %+v (%v), want assignment %d to start it for", live, err, a.ID)
	}
	if kind, assignment, err := s.DispatchKind(dispatch.ID); err != nil || kind != MessageCancellation || assignment != a.ID {
		t.Errorf("the dispatch is a %s about assignment %d (%v)", kind, assignment, err)
	}
	// The worker pushes in the cancellation's dispatch, but submits nothing.
	if err := s.ChangeDispatch(dispatch.ID, 1, DispatchCreated, DispatchWorking, "", assignmentsAt); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EffectRecorded("worker1@engineering", 1, dispatch.ID, "push", "origin/asmai/job-1/1@"+otherCommit, assignmentsAt); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ReportSubmitted(submission(dispatch, ReportResult), assignmentsAt); err == nil {
		t.Error("a result was recorded in a cancellation's dispatch")
	}
	if _, _, err := s.CancellationRequested(a.ID, owner, []string{"x"}, assignmentsAt); err == nil {
		t.Error("a cancelling assignment was cancelled again")
	}

	if _, err := s.AssignmentCancelled(a.ID, Cancellation{Commit: otherCommit}, assignmentsAt); err == nil {
		t.Error("an assignment with an unpushed workspace commit was cancelled")
	}
	if got, _ := s.Assignment(a.ID); got.State != AssignmentCancelling {
		t.Errorf("after an unpushed cancellation the assignment is %s, want cancelling", got.State)
	}
	if _, err := s.AssignmentCancelled(a.ID, Cancellation{Commit: otherCommit, Pushed: true}, assignmentsAt); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Assignment(a.ID); got.State != AssignmentCancelled {
		t.Errorf("the assignment is %s, want cancelled", got.State)
	}
	if worker, slot, err := s.Allocate("engineering"); err != nil || worker != 1 || slot != 1 {
		t.Errorf("after the cancellation the next worker and slot are %d and %d (%v), want 1 and 1", worker, slot, err)
	}
	if _, err := s.AssignmentCancelled(a.ID, Cancellation{}, assignmentsAt); err == nil {
		t.Error("a cancelled assignment was cancelled again")
	}
	if b, _ := s.JobBranch(job); b.Tip != tipCommit {
		t.Errorf("a cancellation moved the job branch to %s", b.Tip)
	}
	got := strings.Join(kinds(t, s), ",")
	if !strings.Contains(got, KindCancellationRequested) || !strings.Contains(got, KindAssignmentCancelled) {
		t.Errorf("the journal holds %s, want the cancellation requested and the assignment cancelled", got)
	}
}
