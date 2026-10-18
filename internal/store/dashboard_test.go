// SPDX-License-Identifier: Apache-2.0

package store

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
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

func TestJobDetailCarriesTheTimeEachItemLastChanged(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	job := jobWithBranch(t, s)
	h := Handoff{Job: job, Sender: "leader@coordination", Receiver: "leader@engineering", Outcome: "Add a greeting", Decisions: "none", Evidence: "none", Constraints: "none", Permissions: "none", Criteria: []string{"c"}}
	sent, _, err := s.SendHandoff(h, assignmentsAt.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	v, d := newValidation(t, s, job)
	if _, _, err := s.ReportSubmitted(validationOf(v, d, tipCommit, ValidationInput{Findings: []FindingInput{
		{Kind: FindingAdvisory, Text: "no doc line"},
		{Kind: FindingNeedsYou, Text: "greet whom?"},
	}}), assignmentsAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	later := assignmentsAt.Add(2 * time.Minute)
	if _, _, err := s.ValidationAccepted(Acceptance{Assignment: v.ID, Leader: qualityOwner, Reasons: []string{"passed"}}, owner, later); err != nil {
		t.Fatal(err)
	}
	a, _ := newAssignment(t, s, job)

	_, got, err := s.JobWorkDetail(job)
	if err != nil {
		t.Fatal(err)
	}
	if got.Branch != "asmai/job-1-add-a-greeting" || got.Head != tipCommit || got.PullRequest != nil || got.Checks != nil {
		t.Errorf("the header is %+v", got)
	}
	if len(got.Handoffs) != 1 || got.Handoffs[0].ID != sent.ID || !got.Handoffs[0].At.Equal(assignmentsAt.Add(-time.Hour)) {
		t.Errorf("the handoffs are %+v, want the one sent an hour before", got.Handoffs)
	}
	if len(got.Findings) != 2 || !got.Findings[0].At.Equal(later) || got.Findings[1].Kind != FindingNeedsYou {
		t.Errorf("the findings are %+v, want both recorded when the validation was accepted", got.Findings)
	}
	if len(got.Assignments) != 2 {
		t.Fatalf("the assignments are %+v, want the validation and the new one", got.Assignments)
	}
	if v := got.Assignments[0]; v.ID != 1 || v.State != AssignmentAccepted || !v.At.Equal(later) || !v.ReadOnly {
		t.Errorf("the validation is %+v, want it accepted when its result was", v)
	}
	if n := got.Assignments[1]; n.ID != a.ID || n.State != AssignmentActive || !n.At.Equal(assignmentsAt) || n.Worker != a.Worker || n.Dispatch != DispatchCreated {
		t.Errorf("the new assignment is %+v, want it active since it was made, with its dispatch created", n)
	}
	if _, _, err := s.JobWorkDetail(job + 1); err == nil {
		t.Error("the detail of a job that does not exist was read")
	}
}

func TestJobDetailChecksFollowTheCurrentPullRequestAndHead(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	job, dispatch := pushedAndRecorded(t, s)
	green := []CheckState{{Name: "CI / test", State: "passed"}}
	read := func() *ChecksObservation {
		t.Helper()
		_, detail, err := s.JobWorkDetail(job)
		if err != nil {
			t.Fatal(err)
		}
		return detail.Checks
	}
	if got := read(); got != nil {
		t.Fatalf("an unobserved head has checks %+v", got)
	}
	if _, _, err := s.ChecksObserved(ChecksObservation{Job: job, Number: 7, Head: tipCommit, Verdict: "green", Checks: green}, assignmentsAt); err != nil {
		t.Fatal(err)
	}
	if got := read(); got == nil || got.Verdict != "green" {
		t.Fatalf("the observed head has checks %+v", got)
	}
	if _, err := s.db.Exec(`UPDATE job_branches SET tip = ? WHERE job = ?`, otherCommit, job); err != nil {
		t.Fatal(err)
	}
	if got := read(); got != nil {
		t.Errorf("checks for the old branch head are %+v", got)
	}
	p := PullRequest{Job: job, Number: 7, URL: "https://github.com/talvor/asmai-fixture/pull/7", Title: "Add a greeting", Branch: "asmai/job-1-add-a-greeting", Head: otherCommit, State: "OPEN", Leader: owner, Dispatch: dispatch.ID}
	if _, _, err := s.PullRequestRecorded(p, 1, p.URL, assignmentsAt); err != nil {
		t.Fatal(err)
	}
	if got := read(); got != nil {
		t.Errorf("checks for the pull request's old head are %+v", got)
	}
	if _, _, err := s.ChecksObserved(ChecksObservation{Job: job, Number: 7, Head: otherCommit, Verdict: "waiting"}, assignmentsAt); err != nil {
		t.Fatal(err)
	}
	if got := read(); got == nil || got.Head != otherCommit || got.Verdict != "waiting" {
		t.Errorf("the new head has checks %+v", got)
	}
	p.Number, p.URL = 8, "https://github.com/talvor/asmai-fixture/pull/8"
	if _, _, err := s.PullRequestRecorded(p, 1, p.URL, assignmentsAt); err != nil {
		t.Fatal(err)
	}
	if got := read(); got != nil {
		t.Errorf("checks for another pull request are %+v", got)
	}
	if _, _, err := s.ChecksObserved(ChecksObservation{Job: job, Number: 8, Head: otherCommit, Verdict: "green", Checks: green}, assignmentsAt); err != nil {
		t.Fatal(err)
	}
	if got := read(); got == nil || got.Number != 8 || got.Verdict != "green" {
		t.Errorf("the current pull request has checks %+v", got)
	}
}

func TestJobDetailAssignmentAgeFollowsItsWorkerDispatch(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	job := jobWithBranch(t, s)
	a, dispatch := newAssignment(t, s, job)
	later := assignmentsAt.Add(500 * time.Millisecond)
	if err := s.ChangeDispatch(dispatch.ID, 1, DispatchCreated, DispatchWorking, "", later); err != nil {
		t.Fatal(err)
	}
	_, detail, err := s.JobWorkDetail(job)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Assignments) != 1 || detail.Assignments[0].ID != a.ID || detail.Assignments[0].Dispatch != DispatchWorking || !detail.Assignments[0].At.Equal(later) {
		t.Errorf("the working assignment is %+v", detail.Assignments)
	}
}

func TestJobDetailAssignmentDispatchIsAddressedToItsWorker(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	job := jobWithBranch(t, s)
	a, dispatch := newValidation(t, s, job)
	later := assignmentsAt.Add(500 * time.Millisecond)
	if _, _, err := s.ReportSubmitted(validationOf(a, dispatch, tipCommit, ValidationInput{Findings: []FindingInput{}}), later); err != nil {
		t.Fatal(err)
	}
	_, detail, err := s.JobWorkDetail(job)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Assignments) != 1 || detail.Assignments[0].ID != a.ID || detail.Assignments[0].Dispatch != DispatchWorking || !detail.Assignments[0].At.Equal(later) {
		t.Errorf("the submitted assignment's worker dispatch is %+v", detail.Assignments)
	}
}

func TestJobWorkDetailReturnsMatchingHeaderAndWork(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	job, dispatch := pushedAndRecorded(t, s)
	if _, _, err := s.ChecksObserved(ChecksObservation{Job: job, Number: 7, Head: tipCommit, Verdict: "green", Checks: []CheckState{{Name: "CI", State: "passed"}}}, assignmentsAt); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE job_branches SET tip = ? WHERE job = ?`, otherCommit, job); err != nil {
		t.Fatal(err)
	}
	p := PullRequest{Job: job, Number: 8, URL: "https://github.com/talvor/asmai-fixture/pull/8", Title: "Updated greeting", Branch: "asmai/job-1-add-a-greeting", Head: otherCommit, State: "OPEN", Leader: owner, Dispatch: dispatch.ID}
	if _, _, err := s.PullRequestRecorded(p, 1, p.URL, assignmentsAt); err != nil {
		t.Fatal(err)
	}
	work, detail, err := s.JobWorkDetail(job)
	if err != nil {
		t.Fatal(err)
	}
	if detail.PullRequest == nil || work.Number != job || work.PullRequest != detail.PullRequest.Number || work.Title != detail.PullRequest.Title || detail.Head != otherCommit || !work.PRCurrent || detail.Checks != nil {
		t.Errorf("the current job's stage inputs and detail disagree: %+v, %+v", work, detail)
	}
	if _, _, err := s.ChecksObserved(ChecksObservation{Job: job, Number: 8, Head: otherCommit, Verdict: "waiting"}, assignmentsAt); err != nil {
		t.Fatal(err)
	}
	work, detail, err = s.JobWorkDetail(job)
	if err != nil {
		t.Fatal(err)
	}
	if work.PullRequest != 8 || detail.Checks == nil || detail.Checks.Number != 8 || detail.Checks.Head != detail.Head {
		t.Errorf("the current pull request's checks and detail disagree: %+v, %+v", work, detail)
	}
	if _, _, err := s.JobWorkDetail(job + 1); err == nil {
		t.Error("a nonexistent job returned stage inputs and detail")
	}
}
