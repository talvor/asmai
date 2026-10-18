// SPDX-License-Identifier: Apache-2.0

package store

import (
	"fmt"
	"path/filepath"
	"testing"
)

func TestAgentWorksReadTheLatestDispatchAndTheAssignmentsNeedingReconciliation(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	job := jobWithBranch(t, s)
	a, d := newAssignment(t, s, job)
	startWorker(t, s, a.ID, "")
	working(t, s, d)

	works, err := s.AgentWorks()
	if err != nil {
		t.Fatal(err)
	}
	w := works[a.Worker]
	if w.Dispatch != DispatchWorking || w.Job != job || w.Assignment != a.ID || w.Outcome != "Add greeting.txt" || w.Kind != MessageAssignment || w.Reconciling != 0 {
		t.Errorf("a working worker's work is %+v", w)
	}
	if owner := works[a.Owner]; owner.Dispatch == "" && owner.Reconciling != 0 {
		t.Errorf("the owner is reconciling %+v", owner)
	}

	if _, err := s.AssignmentNeedsReconciliation(a.ID, d.ID, "its turn was cut off", "Reconcile it.", assignmentsAt); err != nil {
		t.Fatal(err)
	}
	works, err = s.AgentWorks()
	if err != nil {
		t.Fatal(err)
	}
	w = works[a.Worker]
	if w.Reconciling != a.ID || w.ReconcilingJob != job || !w.ReconcilingAt.Equal(assignmentsAt) || w.Dispatch != DispatchUnknown {
		t.Errorf("a held worker's work is %+v, want assignment %d reconciling with an unknown dispatch", w, a.ID)
	}
}

func TestJobWorksCountAssignmentsByStateAndNamePullRequestAndTitle(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	job := jobWithBranch(t, s)
	a, _ := newAssignment(t, s, job)

	works, err := s.JobWorks()
	if err != nil {
		t.Fatal(err)
	}
	if len(works) != 1 || works[0].Number != job || works[0].Ended || works[0].Assignments[AssignmentActive] != 1 || works[0].PullRequest != 0 || works[0].Validating || works[0].NeedsYou != 0 {
		t.Fatalf("the job's work is %+v, want one active assignment of an open job with no pull request (assignment %d)", works, a.ID)
	}
}

func TestJobWorksDeliveryEvidenceFollowsTheHeadAndWritingWork(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	job, dispatch := leaderWorking(t, s)
	const branch = "asmai/job-1-add-a-greeting"
	if _, err := s.JobBranchPushed(Push{Job: job, Branch: branch, Commit: otherCommit, Leader: owner, Dispatch: dispatch.ID}, 1, nil, assignmentsAt); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.PullRequestRecorded(PullRequest{Job: job, Number: 12, URL: "https://example.test/pull/12", Title: "Add a greeting", Branch: branch, Head: otherCommit, State: "OPEN", Leader: owner, Dispatch: dispatch.ID}, 1, "https://example.test/pull/12", assignmentsAt); err != nil {
		t.Fatal(err)
	}
	read := func() JobWork {
		t.Helper()
		works, err := s.JobWorks()
		if err != nil || len(works) != 1 {
			t.Fatalf("reading the job's work returned %+v, %v", works, err)
		}
		return works[0]
	}
	if w := read(); !w.Pushed || !w.PRCurrent || w.Writing || w.PullRequest != 12 || w.Title != "Add a greeting" {
		t.Fatalf("the current head's delivery work is %+v", w)
	}
	if _, err := s.db.Exec(`UPDATE job_branches SET tip = ? WHERE job = ?`, tipCommit, job); err != nil {
		t.Fatal(err)
	}
	if w := read(); w.Pushed || w.PRCurrent || w.PullRequest != 12 {
		t.Errorf("the old head's evidence is %+v", w)
	}
	if _, err := s.db.Exec(`UPDATE job_branches SET tip = ? WHERE job = ?`, otherCommit, job); err != nil {
		t.Fatal(err)
	}
	id, err := s.NextAssignmentID()
	if err != nil {
		t.Fatal(err)
	}
	number, slot, err := s.Allocate("engineering")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AssignmentCreated(NewAssignment{
		ID: id, Job: job, Owner: owner, Worker: fmt.Sprintf("worker%d@engineering", number),
		Outcome: "Fix the red checks", Criteria: []string{"checks pass"},
		Workspace: Workspace{Slot: slot, Path: "/ws/rework", Tmp: "/ws/rework-tmp", Branch: fmt.Sprintf("asmai/job-1/%d", id), Base: otherCommit},
	}, assignmentsAt); err != nil {
		t.Fatal(err)
	}
	if !read().Writing {
		t.Error("a new writing assignment did not interrupt delivery")
	}
}

func TestJobWorksValidationFollowsTheHead(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	job := jobWithBranch(t, s)
	a, d := newValidation(t, s, job)
	if _, _, err := s.ReportSubmitted(validationOf(a, d, tipCommit, ValidationInput{Findings: []FindingInput{}}), assignmentsAt); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ValidationAccepted(Acceptance{Assignment: a.ID, Leader: qualityOwner, Reasons: []string{"passed"}}, owner, assignmentsAt); err != nil {
		t.Fatal(err)
	}
	works, err := s.JobWorks()
	if err != nil || len(works) != 1 || !works[0].Validated {
		t.Fatalf("the accepted validation is not current: %+v, %v", works, err)
	}
	if _, err := s.db.Exec(`UPDATE job_branches SET tip = ? WHERE job = ?`, otherCommit, job); err != nil {
		t.Fatal(err)
	}
	works, err = s.JobWorks()
	if err != nil || len(works) != 1 || works[0].Validated {
		t.Errorf("the old validation still covers the new head: %+v, %v", works, err)
	}
}
