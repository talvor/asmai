// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestRedispatchSupersedesEarlierUnfetchedWork(t *testing.T) {
	for _, state := range []string{DispatchCreated, DispatchNudged, DispatchUnknown} {
		t.Run(state, func(t *testing.T) {
			s := open(t, filepath.Join(t.TempDir(), "store.db"))
			a, first := newAssignment(t, s, jobWithBranch(t, s))
			if state != DispatchCreated {
				if err := s.ChangeDispatch(first.ID, 1, DispatchCreated, state, "", assignmentsAt); err != nil {
					t.Fatal(err)
				}
			}
			latest, err := s.AssignmentRedispatched(a.ID, a.Owner, []string{"try again"}, assignmentsAt)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Inbox(a.Worker, first.ID, assignmentsAt); err == nil {
				t.Fatal("the superseded dispatch remained deliverable")
			}
			if next, err := s.NextDispatch(a.Worker); err != nil || next.ID != latest.ID {
				t.Fatalf("next dispatch is %+v (%v), want %d", next, err, latest.ID)
			}
			if messages, err := s.Inbox(a.Worker, 0, assignmentsAt); err != nil || len(messages) != 1 || messages[0].Dispatch != latest.ID {
				t.Fatalf("worker inbox is %+v (%v), want only the latest dispatch", messages, err)
			}
		})
	}
}

func TestRedispatchKeepsWorkingDispatchAndSupersedesTheNextQueuedOne(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	a, first := newAssignment(t, s, jobWithBranch(t, s))
	working(t, s, first)
	queued, err := s.AssignmentRedispatched(a.ID, a.Owner, []string{"first nudge"}, assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	latest, err := s.AssignmentRedispatched(a.ID, a.Owner, []string{"second nudge"}, assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	var state string
	if err := s.db.QueryRow(`SELECT state FROM dispatches WHERE id = ?`, first.ID).Scan(&state); err != nil || state != DispatchWorking {
		t.Fatalf("the working dispatch is %s (%v)", state, err)
	}
	if _, err := s.Inbox(a.Worker, queued.ID, assignmentsAt); err == nil {
		t.Fatal("the first redispatch remained deliverable")
	}
	if next, err := s.NextDispatch(a.Worker); err != nil || next.ID != latest.ID {
		t.Fatalf("next dispatch is %+v (%v), want %d", next, err, latest.ID)
	}
}

func TestStallNoticeRequiresTheStoppedDispatchToRemainLatest(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	a, stopped := stoppedAssignment(t, s)
	if _, err := s.AssignmentRedispatched(a.ID, a.Owner, []string{"continue"}, assignmentsAt); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WorkerStalled(a.ID, stopped.ID, "old turn", assignmentsAt); err == nil {
		t.Fatal("the old dispatch generated a stall notice after redispatch")
	}
	if _, err := s.NextDispatch(a.Owner); err != sql.ErrNoRows {
		t.Fatalf("the owner has a stale dispatch (%v)", err)
	}
}

func TestAnActiveAssignmentIsSentToItsWorkerAgainInANewDispatchOnlyByItsOwner(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	a, d := stoppedAssignment(t, s)

	for name, call := range map[string]func() error{
		"no reason": func() error {
			_, err := s.AssignmentRedispatched(a.ID, "leader@engineering", nil, assignmentsAt)
			return err
		},
		"a blank one": func() error {
			_, err := s.AssignmentRedispatched(a.ID, "leader@engineering", []string{" "}, assignmentsAt)
			return err
		},
		"another lead": func() error {
			_, err := s.AssignmentRedispatched(a.ID, "leader@quality", []string{"x"}, assignmentsAt)
			return err
		},
		"the worker": func() error {
			_, err := s.AssignmentRedispatched(a.ID, "worker1@engineering", []string{"x"}, assignmentsAt)
			return err
		},
		"no such one": func() error {
			_, err := s.AssignmentRedispatched(99, "leader@engineering", []string{"x"}, assignmentsAt)
			return err
		},
	} {
		if call() == nil {
			t.Errorf("an assignment was sent again with %s", name)
		}
	}

	again, err := s.AssignmentRedispatched(a.ID, "leader@engineering", []string{"the push works now"}, assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	if again.ID == d.ID || again.Agent != "worker1@engineering" || again.State != DispatchCreated {
		t.Errorf("the new dispatch is %+v, want a created one of its own for the worker, not dispatch %d", again, d.ID)
	}
	if got, _ := s.Assignment(a.ID); got.State != AssignmentActive {
		t.Errorf("the assignment is %s, want it active", got.State)
	}
	var state string
	if err := s.db.QueryRow(`SELECT state FROM dispatches WHERE id = ?`, d.ID).Scan(&state); err != nil || state != DispatchStopped {
		t.Errorf("the earlier dispatch is %s (%v), want it left stopped", state, err)
	}
	messages, err := s.Inbox("worker1@engineering", again.ID, assignmentsAt)
	if err != nil || len(messages) != 1 || messages[0].Kind != MessageRedispatch || messages[0].Sender != "leader@engineering" ||
		!strings.Contains(messages[0].Body, "the push works now") || messages[0].AssignmentData == nil {
		t.Fatalf("the worker's inbox holds %+v (%v)", messages, err)
	}
	if !strings.Contains(strings.Join(kinds(t, s), ","), KindAssignmentRedispatched) {
		t.Errorf("the journal holds %v, want the redispatch", kinds(t, s))
	}

	// The worker reports on the new dispatch, as on any dispatch that gives
	// it its assignment.
	if err := s.db.QueryRow(`SELECT state FROM dispatches WHERE id = ?`, again.ID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if err := s.ChangeDispatch(again.ID, 1, state, DispatchWorking, "", assignmentsAt); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EffectRecorded("worker1@engineering", 1, again.ID, "push", "origin/asmai/job-1/1", assignmentsAt); err != nil {
		t.Fatal(err)
	}
	sub := submission(again, ReportResult)
	if _, _, err := s.ReportSubmitted(sub, assignmentsAt); err != nil {
		t.Errorf("a result on the new dispatch returned %v", err)
	}
	if _, err := s.AssignmentRedispatched(a.ID, "leader@engineering", []string{"x"}, assignmentsAt); err == nil || !strings.Contains(err.Error(), "submitted") {
		t.Errorf("a submitted assignment was sent again: %v", err)
	}
}

func TestAnAssignmentThatNeedsReconciliationIsActiveAgainWhenItsOwnerSendsItAgain(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	a, d := stoppedAssignment(t, s)
	if _, err := s.AssignmentNeedsReconciliation(a.ID, d.ID, "its session cannot be resumed", "Reconcile it.", assignmentsAt); err != nil {
		t.Fatal(err)
	}

	again, err := s.AssignmentRedispatched(a.ID, "leader@engineering", []string{"I looked: the branch is on origin"}, assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Assignment(a.ID); got.State != AssignmentActive {
		t.Errorf("the assignment is %s, want it active again", got.State)
	}
	if again.ID == d.ID || again.Agent != "worker1@engineering" {
		t.Errorf("the new dispatch is %+v, want one of its own for the worker", again)
	}
}

func TestAnAssignmentThatHasEndedIsNotSentToItsWorkerAgain(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	a, _ := stoppedAssignment(t, s)
	if _, _, err := s.CancellationRequested(a.ID, "leader@engineering", []string{"not wanted"}, assignmentsAt); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AssignmentRedispatched(a.ID, "leader@engineering", []string{"x"}, assignmentsAt); err == nil || !strings.Contains(err.Error(), "cancelling") {
		t.Errorf("a cancelling assignment was sent again: %v", err)
	}
}

func TestAWorkerThatStoppedWithoutAReportIsReportedToItsOwnerWithoutChangingTheAssignment(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	a, d := stoppedAssignment(t, s)

	told, err := s.WorkerStalled(a.ID, d.ID, "worker1 stopped.", assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	if told.Agent != "leader@engineering" || told.State != DispatchCreated {
		t.Errorf("the owner's dispatch is %+v, want a created one for the owning leader", told)
	}
	if got, _ := s.Assignment(a.ID); got.State != AssignmentActive {
		t.Errorf("the assignment is %s, want it still active", got.State)
	}
	messages, err := s.Inbox("leader@engineering", told.ID, assignmentsAt)
	if err != nil || len(messages) != 1 || messages[0].Kind != MessageStalled || messages[0].Sender != Daemon || messages[0].Body != "worker1 stopped." || messages[0].AssignmentData == nil {
		t.Fatalf("the owner's inbox holds %+v (%v)", messages, err)
	}

	// An assignment that is no longer active has no worker to report.
	if _, _, err := s.CancellationRequested(a.ID, "leader@engineering", []string{"x"}, assignmentsAt); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WorkerStalled(a.ID, d.ID, "again", assignmentsAt); err == nil {
		t.Error("a cancelling assignment's worker was reported as stalled")
	}
}

func TestABlockedReportNeedsNoRecordedPushButAResultDoes(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	job := jobWithBranch(t, s)
	a, d := newAssignment(t, s, job)
	working(t, s, d)

	if _, _, err := s.ReportSubmitted(submission(d, ReportResult), assignmentsAt); err == nil || !strings.Contains(err.Error(), "push") {
		t.Errorf("a result without a recorded push returned %v", err)
	}
	r, told, err := s.ReportSubmitted(submission(d, ReportBlocked), assignmentsAt)
	if err != nil {
		t.Fatalf("a blocked report without a recorded push returned %v", err)
	}
	if r.Kind != ReportBlocked || len(r.Effects) != 0 || told.Agent != "leader@engineering" {
		t.Errorf("the blocked report is %+v for %+v", r, told)
	}
	if got, _ := s.Assignment(a.ID); got.State != AssignmentActive {
		t.Errorf("the assignment is %s, want it still active", got.State)
	}
}
