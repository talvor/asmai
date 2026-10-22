// SPDX-License-Identifier: Apache-2.0

package store

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// validationAt records a read-only assignment of job for the next free worker
// of Quality, fixed at commit and going the way review says.
func validationAt(t *testing.T, s *Store, job int64, n NewAssignment) (Assignment, error) {
	a, _, err := validationWith(t, s, job, n)
	return a, err
}

// validationWith is validationAt, which also returns the validation's dispatch.
func validationWith(t *testing.T, s *Store, job int64, n NewAssignment) (Assignment, Dispatch, error) {
	t.Helper()
	id, err := s.NextAssignmentID()
	if err != nil {
		t.Fatal(err)
	}
	number, slot, err := s.Allocate("quality")
	if err != nil {
		t.Fatal(err)
	}
	n.ID, n.Job, n.Owner, n.ReadOnly = id, job, qualityOwner, true
	n.Worker = "worker" + string(rune('0'+number)) + "@quality"
	n.Outcome, n.Criteria = "Validate", []string{"checks re-run"}
	n.Workspace.Slot, n.Workspace.Path, n.Workspace.Tmp = slot, "/ws/validation", "/ws/validation-tmp"
	a, d, err := s.AssignmentCreated(n, assignmentsAt)
	if err == nil {
		working(t, s, d)
	}
	return a, d, err
}

func TestAValidationGoesAsAFullReviewTheChangesOrOnlyTheChecksSinceTheLastValidatedCommit(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	job := jobWithBranch(t, s)
	base := func(review, since string) NewAssignment {
		return NewAssignment{Review: review, Since: since, Workspace: Workspace{Base: tipCommit}}
	}
	for name, n := range map[string]NewAssignment{
		"a review of the changes with no commit to look from": base(ReviewChanges, ""),
		"a checks-only review with no commit to look from":    base(ReviewChecks, ""),
		"a full review from a commit":                         base(ReviewFull, otherCommit),
		"a review of no known kind":                           base("skim", ""),
		"an early review of no assignment":                    {Review: ReviewEarly, Requester: "leader@engineering", Workspace: Workspace{Base: tipCommit}},
	} {
		if _, err := validationAt(t, s, job, n); err == nil {
			t.Errorf("%s was recorded", name)
		}
	}
	// A validation with no review goes as a full one.
	a, ad, err := validationWith(t, s, job, base("", ""))
	if err != nil || a.Review != ReviewFull || a.Since != "" {
		t.Fatalf("a validation that says nothing of its review is %+v (%v), want a full one", a, err)
	}
	if got, err := s.Assignment(a.ID); err != nil || got.Review != ReviewFull {
		t.Errorf("the validation reads back as %+v (%v)", got, err)
	}
	if _, _, err := s.ReportSubmitted(validationOf(a, ad, tipCommit, ValidationInput{Findings: []FindingInput{}}), assignmentsAt); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ValidationAccepted(Acceptance{Assignment: a.ID, Leader: qualityOwner, Reasons: []string{"x"}}, owner, assignmentsAt); err != nil {
		t.Fatal(err)
	}

	for name, review := range map[string]string{"the changes": ReviewChanges, "only the checks": ReviewChecks} {
		next, err := validationAt(t, s, job, base(review, tipCommit))
		if err != nil || next.Review != review || next.Since != tipCommit {
			t.Fatalf("a review of %s is %+v (%v)", name, next, err)
		}
		if _, _, err := s.CancellationRequested(next.ID, qualityOwner, []string{"again"}, assignmentsAt); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AssignmentCancelled(next.ID, Cancellation{Commit: tipCommit}, assignmentsAt); err != nil {
			t.Fatal(err)
		}
	}
	// A writing assignment is no review at all.
	writing := NewAssignment{ID: 99, Job: job, Owner: owner, Worker: "worker2@engineering", Outcome: "o", Criteria: []string{"c"}, Review: ReviewFull,
		Workspace: Workspace{Slot: 9, Path: "/p", Tmp: "/t", Branch: "asmai/job-1/x", Base: tipCommit}}
	if _, _, err := s.AssignmentCreated(writing, assignmentsAt); err == nil {
		t.Error("a writing assignment was given a review")
	}
}

func TestOnlyAValidationOfTheFinishedJobBranchValidatesAHeadAndAnEarlyReviewOfAnAssignmentDoesNot(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	a := submitted(t, s) // assignment 1, with a result at otherCommit; the job branch is at tipCommit
	job := a.Job
	if err := s.ValidatedHead(job, tipCommit); err == nil || !strings.Contains(err.Error(), "has not validated "+tipCommit) {
		t.Errorf("a head nobody validated reads as validated: %v", err)
	}

	// An early review of the assignment is fixed at its result, not at the
	// job branch's tip, and its handoff names that result.
	early := NewAssignment{Review: ReviewEarly, Reviews: a.ID, Requester: "leader@coordination", Workspace: Workspace{Base: otherCommit}}
	wrong := early
	wrong.Workspace.Base = tipCommit
	if _, err := validationAt(t, s, job, wrong); err == nil || !strings.Contains(err.Error(), "is at "+otherCommit) {
		t.Errorf("an early review at the job branch's tip returned %v", err)
	}
	unasked := early
	unasked.Requester = ""
	if _, err := validationAt(t, s, job, unasked); err == nil {
		t.Error("an early review that no leader asked for was recorded")
	}
	missing := early
	missing.Reviews = 99
	if _, err := validationAt(t, s, job, missing); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("an early review of an assignment that does not exist returned %v", err)
	}
	v, vd, err := validationWith(t, s, job, early)
	if err != nil || v.Review != ReviewEarly || v.Reviews != a.ID || v.Requester != "leader@coordination" || v.Commit != otherCommit {
		t.Fatalf("the early review is %+v (%v)", v, err)
	}
	if _, err := validationAt(t, s, job, NewAssignment{Review: ReviewEarly, Reviews: v.ID, Requester: owner, Workspace: Workspace{Base: otherCommit}}); err == nil {
		t.Error("a validation was reviewed early")
	}

	// It is an early review: an unaccounted earlier finding is no reason to
	// refuse its report, and it validates no head.
	if _, _, err := s.ReportSubmitted(validationOf(v, vd, otherCommit, ValidationInput{Findings: []FindingInput{{Kind: FindingBlocking, Text: "risky"}}}), assignmentsAt); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ValidationAccepted(Acceptance{Assignment: v.ID, Leader: qualityOwner, Reasons: []string{"x"}}, "leader@coordination", assignmentsAt); err != nil {
		t.Fatal(err)
	}
	if commit, err := s.LastValidated(job); err != nil || commit != "" {
		t.Errorf("an early review validated %q (%v)", commit, err)
	}
	if err := s.ValidatedHead(job, otherCommit); err == nil {
		t.Error("the commit of an early review reads as a validated head")
	}
	// The finding it found stays open for the next validation to confirm.
	next, nd, err := validationWith(t, s, job, NewAssignment{Review: ReviewFull, Workspace: Workspace{Base: tipCommit}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ReportSubmitted(validationOf(next, nd, tipCommit, ValidationInput{Findings: []FindingInput{}}), assignmentsAt); err == nil || !strings.Contains(err.Error(), "risky") {
		t.Errorf("a validation that does not account for the early review's finding returned %v", err)
	}
}

func TestAnEarlyReviewVerdictDoesNotClearAJobFinding(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	writing := submitted(t, s)
	job := writing.Job
	first, fd, err := validationWith(t, s, job, NewAssignment{Review: ReviewFull, Workspace: Workspace{Base: tipCommit}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ReportSubmitted(validationOf(first, fd, tipCommit, ValidationInput{Findings: []FindingInput{{Kind: FindingBlocking, Text: "the change is unsafe"}}}), assignmentsAt); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ValidationAccepted(Acceptance{Assignment: first.ID, Leader: qualityOwner, Reasons: []string{"confirmed"}}, owner, assignmentsAt); err != nil {
		t.Fatal(err)
	}
	open, err := s.OpenFindings(job)
	if err != nil || len(open) != 1 {
		t.Fatalf("open findings are %+v (%v)", open, err)
	}
	id := open[0].ID

	early, ed, err := validationWith(t, s, job, NewAssignment{Review: ReviewEarly, Reviews: writing.ID, Requester: "leader@coordination", Workspace: Workspace{Base: otherCommit}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ReportSubmitted(validationOf(early, ed, otherCommit, ValidationInput{Findings: []FindingInput{}, Resolved: []int64{id}}), assignmentsAt); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ValidationAccepted(Acceptance{Assignment: early.ID, Leader: qualityOwner, Reasons: []string{"confirmed at the assignment commit"}}, "leader@coordination", assignmentsAt); err != nil {
		t.Fatal(err)
	}
	reports, err := s.Reports(early.ID)
	if err != nil || len(reports) != 1 || reports[0].Validation == nil || len(reports[0].Validation.Resolved) != 1 || reports[0].Validation.Resolved[0] != id {
		t.Fatalf("the early verdict is %+v (%v)", reports, err)
	}
	open, err = s.OpenFindings(job)
	if err != nil || len(open) != 1 || open[0].ID != id || open[0].ClearedBy != 0 {
		t.Errorf("the early verdict cleared the job finding: %+v (%v)", open, err)
	}
	if err := s.ValidatedHead(job, tipCommit); err == nil {
		t.Error("the previously validated job head became deliverable after an early review")
	}
	entries, err := s.Journal()
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Kind != KindValidationAccepted {
			continue
		}
		var data struct {
			Report  int64   `json:"report"`
			Cleared []int64 `json:"cleared"`
		}
		if err := json.Unmarshal(entry.Data, &data); err != nil {
			t.Fatal(err)
		}
		if data.Report == reports[0].ID && len(data.Cleared) != 0 {
			t.Errorf("the early review journaled cleared findings %v", data.Cleared)
		}
	}
	messages, err := s.Inbox("leader@coordination", 0, assignmentsAt)
	if err != nil || len(messages) != 1 || !strings.Contains(messages[0].Body, "0 earlier blocking finding(s) cleared") {
		t.Errorf("the early review summary is %+v (%v)", messages, err)
	}

	finished, finishedDispatch, err := validationWith(t, s, job, NewAssignment{Review: ReviewFull, Workspace: Workspace{Base: tipCommit}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ReportSubmitted(validationOf(finished, finishedDispatch, tipCommit, ValidationInput{Findings: []FindingInput{}, Resolved: []int64{id}}), assignmentsAt); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ValidationAccepted(Acceptance{Assignment: finished.ID, Leader: qualityOwner, Reasons: []string{"confirmed on the job branch"}}, owner, assignmentsAt); err != nil {
		t.Fatal(err)
	}
	if err := s.ValidatedHead(job, tipCommit); err != nil {
		t.Errorf("the finished-branch validation did not clear the finding: %v", err)
	}
}
