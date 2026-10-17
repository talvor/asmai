// SPDX-License-Identifier: Apache-2.0

package store

import (
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
