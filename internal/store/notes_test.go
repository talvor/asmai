// SPDX-License-Identifier: Apache-2.0

package store

import (
	"path/filepath"
	"strings"
	"testing"
)

// notesPushedAndRecorded is pushedAndRecorded for a job with the mandate
// mandate: a notes job when it is planning-only or research-only.
func notesPushedAndRecorded(t *testing.T, s *Store, mandate string) int64 {
	t.Helper()
	job, _ := pushedAndRecorded(t, s)
	if _, err := s.db.Exec(`UPDATE jobs SET mandate = ? WHERE number = ?`, mandate, job); err != nil {
		t.Fatal(err)
	}
	return job
}

func TestOnlyAJobInARepositoryWithANotesMandateIsANotesJob(t *testing.T) {
	for _, tt := range []struct {
		j    Job
		want bool
	}{
		{Job{Repository: "fixture", Mandate: MandateResearchOnly}, true},
		{Job{Repository: "fixture", Mandate: MandatePlanningOnly}, true},
		{Job{Repository: "fixture", Mandate: MandateTestedPR}, false},
		{Job{Mandate: MandateResearchOnly}, false},
	} {
		if got := tt.j.Notes(); got != tt.want {
			t.Errorf("%+v is a notes job: %v, want %v", tt.j, got, tt.want)
		}
	}
}

func TestANotesJobsPushedHeadIsDeliverableWithNoQualityValidation(t *testing.T) {
	green := []CheckState{{Name: "CI / test", Status: "COMPLETED", Conclusion: "SUCCESS", State: "passed"}}
	for _, mandate := range []string{MandateResearchOnly, MandatePlanningOnly} {
		t.Run(mandate, func(t *testing.T) {
			s := open(t, filepath.Join(t.TempDir(), "store.db"))
			job := notesPushedAndRecorded(t, s, mandate)
			delivery := Delivery{Job: job, Number: 7, URL: "https://github.com/talvor/asmai-fixture/pull/7", Head: tipCommit, Checks: green}

			// Quality validated nothing, and nothing asks it to.
			if err := s.TestedPR(job, tipCommit); err == nil || !strings.Contains(err.Error(), "Quality has not validated") {
				t.Errorf("a notes job's unvalidated head is a tested pull request: %v", err)
			}
			if err := s.Deliverable(job, tipCommit); err != nil {
				t.Errorf("a notes job's pushed head is not deliverable: %v", err)
			}
			if err := s.Deliverable(job, otherCommit); err == nil || !strings.Contains(err.Error(), "not pushed at "+otherCommit) {
				t.Errorf("a head the daemon did not push is deliverable: %v", err)
			}
			a, _ := newAssignment(t, s, job)
			if err := s.Deliverable(job, tipCommit); err == nil || !strings.Contains(err.Error(), "assignment") || !strings.Contains(err.Error(), "has not ended") {
				t.Errorf("a head with a live assignment is deliverable: %v (assignment %d)", err, a.ID)
			}
			if _, err := s.db.Exec(`UPDATE assignments SET state = ? WHERE id = ?`, AssignmentCancelled, a.ID); err != nil {
				t.Fatal(err)
			}

			for name, d := range map[string]Delivery{
				"with a check failed":     {Job: job, Number: 7, URL: delivery.URL, Head: tipCommit, Checks: []CheckState{{Name: "CI / test", State: "failed"}}},
				"with no checks":          {Job: job, Number: 7, URL: delivery.URL, Head: tipCommit, Checks: []CheckState{}},
				"of another pull request": {Job: job, Number: 8, URL: delivery.URL, Head: tipCommit, Checks: green},
			} {
				if _, _, err := s.JobDelivered(d, "delivered", assignmentsAt); err == nil {
					t.Errorf("a notes pull request %s was delivered", name)
				}
			}
			got, dispatch, err := s.JobDelivered(delivery, "Job 1 is delivered", assignmentsAt)
			if err != nil {
				t.Fatal(err)
			}
			if got.NoCI || got.Number != 7 || dispatch.Agent != "leader@coordination" {
				t.Errorf("the delivery is %+v, told in %+v", got, dispatch)
			}
			if j, err := s.Job(job); err != nil || j.State != JobEnded {
				t.Errorf("the delivered notes job is %+v (%v)", j, err)
			}
			if read, err := s.JobDelivery(job); err != nil || read.NoCI || len(read.Checks) != 1 {
				t.Errorf("the delivery reads back as %+v (%v)", read, err)
			}
		})
	}
}

func TestARepositoryWithNoCIDeliversItsNotesPullRequestWithNoChecks(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	job := notesPushedAndRecorded(t, s, MandateResearchOnly)
	url := "https://github.com/talvor/asmai-fixture/pull/7"

	for name, d := range map[string]Delivery{
		"with a check that is not passed": {Job: job, Number: 7, URL: url, Head: tipCommit, NoCI: true, Checks: []CheckState{{Name: "lint", State: "pending"}}},
		"of a head it did not push":       {Job: job, Number: 7, URL: url, Head: otherCommit, NoCI: true, Checks: []CheckState{}},
		"of another pull request":         {Job: job, Number: 8, URL: url, Head: tipCommit, NoCI: true, Checks: []CheckState{}},
	} {
		if _, _, err := s.JobDelivered(d, "delivered", assignmentsAt); err == nil {
			t.Errorf("a no-CI notes pull request %s was delivered", name)
		}
	}
	got, dispatch, err := s.JobDelivered(Delivery{Job: job, Number: 7, URL: url, Head: tipCommit, NoCI: true, Checks: []CheckState{}}, "Job 1 is delivered", assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	if !got.NoCI || dispatch.Agent != "leader@coordination" {
		t.Errorf("the delivery is %+v, told in %+v", got, dispatch)
	}
	if read, err := s.JobDelivery(job); err != nil || !read.NoCI || len(read.Checks) != 0 {
		t.Errorf("the no-CI delivery reads back as %+v (%v)", read, err)
	}
	if j, err := s.Job(job); err != nil || j.State != JobEnded {
		t.Errorf("the delivered notes job is %+v (%v)", j, err)
	}
}

func TestNoCIDeliversOnlyANotesPullRequestAndANotesJobsHeadWithoutItIsNotDelivered(t *testing.T) {
	// A code job's pull request waits for green checks whatever the
	// repository declares: its no-CI delivery is not this one's.
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	job, _ := pushedAndRecorded(t, s)
	a, d := newValidation(t, s, job)
	if _, _, err := s.ReportSubmitted(validationOf(a, d, tipCommit, ValidationInput{}), assignmentsAt); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ValidationAccepted(Acceptance{Assignment: a.ID, Leader: qualityOwner, Reasons: []string{"passed"}}, owner, assignmentsAt); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.JobDelivered(Delivery{Job: job, Number: 7, URL: "https://github.com/talvor/asmai-fixture/pull/7", Head: tipCommit, NoCI: true, Checks: []CheckState{}}, "delivered", assignmentsAt); err == nil || !strings.Contains(err.Error(), "notes") {
		t.Errorf("a code job's pull request was delivered with no CI: %v", err)
	}
}

// submittedAssignment gives s's job an assignment of Engineering whose result
// is submitted at otherCommit, pushed to its assignment branch, and returns it.
func submittedAssignment(t *testing.T, s *Store, job int64) Assignment {
	t.Helper()
	a, d := newAssignment(t, s, job)
	working(t, s, d)
	if _, err := s.EffectRecorded(a.Worker, 1, d.ID, "push", "origin/"+a.Workspace.Branch+"@"+otherCommit, assignmentsAt); err != nil {
		t.Fatal(err)
	}
	sub := submission(d, ReportResult)
	sub.Agent = a.Worker
	if _, _, err := s.ReportSubmitted(sub, assignmentsAt); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestAPrototypeStaysOnItsAssignmentBranchAndItsAcceptanceMovesNoJobBranch(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	job := jobWithBranch(t, s)
	live, _ := newAssignment(t, s, job)
	reasons := []string{"the prototype answers the question"}
	if _, _, err := s.PrototypeKept(Acceptance{Assignment: live.ID, Leader: owner, Reasons: reasons}, assignmentsAt); err == nil || !strings.Contains(err.Error(), "no result to accept") {
		t.Errorf("an assignment with no result stayed on its branch: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE assignments SET state = ? WHERE id = ?`, AssignmentCancelled, live.ID); err != nil {
		t.Fatal(err)
	}

	a := submittedAssignment(t, s, job)
	for name, in := range map[string]Acceptance{
		"by another leader": {Assignment: a.ID, Leader: "leader@planning", Reasons: reasons},
		"with no reasons":   {Assignment: a.ID, Leader: owner},
	} {
		if _, _, err := s.PrototypeKept(in, assignmentsAt); err == nil {
			t.Errorf("a prototype was kept %s", name)
		}
	}
	d, p, err := s.PrototypeKept(Acceptance{Assignment: a.ID, Leader: owner, Reasons: reasons}, assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind != DecisionAccepted || d.Commit != otherCommit || p.Assignment != a.ID || p.Job != job || p.Branch != a.Workspace.Branch || p.Commit != otherCommit || p.Decision != d.ID || p.At.IsZero() {
		t.Errorf("the prototype is %+v, decided by %+v", p, d)
	}
	if got, err := s.Assignment(a.ID); err != nil || got.State != AssignmentAccepted {
		t.Errorf("the prototype's assignment is %+v (%v), want it accepted", got, err)
	}
	if b, err := s.JobBranch(job); err != nil || b.Tip != tipCommit {
		t.Errorf("the job branch is %+v (%v), want it still at %s: a prototype stays on its own branch", b, err, tipCommit)
	}
	if kept, err := s.PrototypesKept(job); err != nil || len(kept) != 1 || kept[0] != p {
		t.Errorf("the prototypes kept are %+v (%v), want %+v", kept, err, p)
	}
	if kept, err := s.PrototypesKept(job + 1); err != nil || len(kept) != 0 {
		t.Errorf("another job's prototypes are %+v (%v)", kept, err)
	}
	if n := len(journaledKinds(t, s, KindPrototypeKept)); n != 1 {
		t.Errorf("the prototype is journaled %d times, want 1", n)
	}
	if n := len(journaledKinds(t, s, KindJobBranchMoved)); n != 0 {
		t.Errorf("the job branch moved %d times for a prototype", n)
	}
	if _, _, err := s.PrototypeKept(Acceptance{Assignment: a.ID, Leader: owner, Reasons: reasons}, assignmentsAt); err == nil {
		t.Error("a prototype's acceptance was recorded twice")
	}
	// The verdict note's assignment still fast-forwards the job branch from
	// the tip the prototype left alone.
	note := submittedAssignment(t, s, job)
	if _, err := s.ResultAccepted(Acceptance{Assignment: note.ID, Leader: owner, Reasons: []string{"the verdict note names the prototype's branch"}, FromTip: tipCommit}, assignmentsAt); err != nil {
		t.Errorf("the verdict note's acceptance after the prototype's was refused: %v", err)
	}
}

func TestAPrototypeOfAScratchOrReadOnlyAssignmentIsRefused(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	job := jobWithBranch(t, s)
	a, d := newValidation(t, s, job)
	if _, _, err := s.ReportSubmitted(validationOf(a, d, tipCommit, ValidationInput{}), assignmentsAt); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.PrototypeKept(Acceptance{Assignment: a.ID, Leader: qualityOwner, Reasons: []string{"x"}}, assignmentsAt); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Errorf("a validation stayed on its branch: %v", err)
	}
}
