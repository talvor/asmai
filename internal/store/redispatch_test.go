// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestRedispatchRefusesAnUnfetchedAssignment(t *testing.T) {
	for _, state := range []string{DispatchCreated, DispatchNudged, DispatchUnknown} {
		t.Run(state, func(t *testing.T) {
			s := open(t, filepath.Join(t.TempDir(), "store.db"))
			a, first := newAssignment(t, s, jobWithBranch(t, s))
			if state != DispatchCreated {
				if err := s.ChangeDispatch(first.ID, 1, DispatchCreated, state, "", assignmentsAt); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.AssignmentRedispatched(a.ID, a.Owner, []string{"try again"}, assignmentsAt); err == nil || !strings.Contains(err.Error(), "wait for the worker to receive it or cancel") {
				t.Fatalf("redispatch over queued assignment returned %v", err)
			}
			if state != DispatchUnknown {
				if next, err := s.NextDispatch(a.Worker); err != nil || next.ID != first.ID {
					t.Fatalf("next dispatch is %+v (%v), want original assignment %d", next, err, first.ID)
				}
			}
			if messages, err := s.Inbox(a.Worker, first.ID, first.ID, assignmentsAt); err != nil || len(messages) != 1 || messages[0].Kind != MessageAssignment {
				t.Fatalf("original assignment is %+v (%v), want it delivered", messages, err)
			}
		})
	}
}

func TestRedispatchRetiresUnknownWorkAfterReconciliationButRefusesDeliverableWork(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	a, first := stoppedAssignment(t, s)
	unknown, err := s.AssignmentRedispatched(a.ID, a.Owner, []string{"continue"}, assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ChangeDispatch(unknown.ID, 1, DispatchCreated, DispatchUnknown, "", assignmentsAt); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AssignmentNeedsReconciliation(a.ID, unknown.ID, "worker exited", "Reconcile it.", assignmentsAt); err != nil {
		t.Fatal(err)
	}
	replacement, err := s.AssignmentRedispatched(a.ID, a.Owner, []string{"cause fixed"}, assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	if replacement.ID == unknown.ID || replacement.ID == first.ID {
		t.Fatalf("replacement dispatch is %+v", replacement)
	}
	var state string
	if err := s.db.QueryRow(`SELECT state FROM dispatches WHERE id = ?`, unknown.ID).Scan(&state); err != nil || state != DispatchStopped {
		t.Fatalf("unknown dispatch remained %s (%v)", state, err)
	}
	if next, err := s.NextDispatch(a.Worker); err != nil || next.ID != replacement.ID {
		t.Fatalf("next dispatch is %+v (%v), want replacement %d", next, err, replacement.ID)
	}
	if _, err := s.AssignmentRedispatched(a.ID, a.Owner, []string{"again"}, assignmentsAt); err == nil || !strings.Contains(err.Error(), "wait for the worker to receive it or cancel") {
		t.Fatalf("redispatch over deliverable queued work returned %v", err)
	}
}

func TestRedispatchRefusesOtherQueuedWorkMessages(t *testing.T) {
	for _, kind := range []string{MessageRedispatch, MessageRejection, MessageMovedTip, MessageResumption} {
		t.Run(kind, func(t *testing.T) {
			s := open(t, filepath.Join(t.TempDir(), "store.db"))
			var a Assignment
			var queued Dispatch
			var err error
			switch kind {
			case MessageRedispatch:
				a, _ = stoppedAssignment(t, s)
				queued, err = s.AssignmentRedispatched(a.ID, a.Owner, []string{"first nudge"}, assignmentsAt)
			case MessageRejection:
				a = submitted(t, s)
				_, queued, err = s.ResultRejected(a.ID, a.Owner, []string{"revise"}, assignmentsAt)
			case MessageMovedTip:
				a = submitted(t, s)
				queued, err = s.ResultReturned(a.ID, a.Owner, tipCommit, assignmentsAt)
			case MessageResumption:
				var first Dispatch
				a, first = stoppedAssignment(t, s)
				queued, err = s.AssignmentResumed(a.ID, first.ID, nativeID, "Continue.", assignmentsAt)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.AssignmentRedispatched(a.ID, a.Owner, []string{"again"}, assignmentsAt); err == nil || !strings.Contains(err.Error(), "wait for the worker to receive it or cancel") {
				t.Fatalf("redispatch over queued %s returned %v", kind, err)
			}
			if next, err := s.NextDispatch(a.Worker); err != nil || next.ID != queued.ID {
				t.Fatalf("next dispatch is %+v (%v), want queued %s %d", next, err, kind, queued.ID)
			}
		})
	}
}

func TestRedispatchRefusesWorkingDispatchUntilItStops(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	a, first := newAssignment(t, s, jobWithBranch(t, s))
	working(t, s, first)
	if _, err := s.AssignmentRedispatched(a.ID, a.Owner, []string{"first nudge"}, assignmentsAt); err == nil || !strings.Contains(err.Error(), "wait for it to stop") {
		t.Fatalf("redispatch over working dispatch returned %v", err)
	}
	var state string
	if err := s.db.QueryRow(`SELECT state FROM dispatches WHERE id = ?`, first.ID).Scan(&state); err != nil || state != DispatchWorking {
		t.Fatalf("the working dispatch is %s (%v)", state, err)
	}
	if err := s.ChangeDispatch(first.ID, 1, DispatchWorking, DispatchStopped, "", assignmentsAt); err != nil {
		t.Fatal(err)
	}
	queued, err := s.AssignmentRedispatched(a.ID, a.Owner, []string{"cause fixed"}, assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	if next, err := s.NextDispatch(a.Worker); err != nil || next.ID != queued.ID {
		t.Fatalf("next dispatch is %+v (%v), want %d", next, err, queued.ID)
	}
}

func TestRedispatchRefusesDeliveredDispatch(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	a, first := newAssignment(t, s, jobWithBranch(t, s))
	if _, err := s.Inbox(a.Worker, first.ID, first.ID, assignmentsAt); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AssignmentRedispatched(a.ID, a.Owner, []string{"again"}, assignmentsAt); err == nil || !strings.Contains(err.Error(), "wait for it to stop") {
		t.Fatalf("redispatch over delivered dispatch returned %v", err)
	}
}

func TestInboxOmitsOutOfTurnRedispatchUntilItIsCurrent(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	a, _ := stoppedAssignment(t, s)
	queued, err := s.AssignmentRedispatched(a.ID, a.Owner, []string{"try again"}, assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	messages, err := s.Inbox(a.Worker, 0, 0, assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range messages {
		if message.Dispatch == queued.ID {
			t.Fatalf("all-message inbox previewed queued work: %+v", messages)
		}
	}
	if messages, err := s.Inbox(a.Worker, queued.ID, 0, assignmentsAt); err == nil || len(messages) != 0 {
		t.Fatalf("dispatch-specific inbox previewed queued work: %+v (%v)", messages, err)
	}
	if next, err := s.NextDispatch(a.Worker); err != nil || next.ID != queued.ID {
		t.Fatalf("next dispatch is %+v (%v), want redispatch %d", next, err, queued.ID)
	}
	messages, err = s.Inbox(a.Worker, queued.ID, queued.ID, assignmentsAt)
	if err != nil || len(messages) != 1 || !messages[0].Fetched || messages[0].State != DispatchDelivered {
		t.Fatalf("current redispatch was not fetched: %+v (%v)", messages, err)
	}
}

func TestInboxOmitsOutOfTurnCancellation(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	a, first := newAssignment(t, s, jobWithBranch(t, s))
	working(t, s, first)
	_, cancellation, err := s.CancellationRequested(a.ID, a.Owner, []string{"stop"}, assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	messages, err := s.Inbox(a.Worker, cancellation.ID, first.ID, assignmentsAt)
	if err == nil || len(messages) != 0 {
		t.Fatalf("out-of-turn cancellation was previewed: %+v (%v)", messages, err)
	}
	if next, err := s.NextDispatch(a.Worker); err != nil || next.ID != cancellation.ID {
		t.Fatalf("next dispatch is %+v (%v), want cancellation %d", next, err, cancellation.ID)
	}
}

func TestAReportAfterRefusedRedispatchLeavesNoQueuedWork(t *testing.T) {
	for _, kind := range []string{ReportResult, ReportBlocked} {
		t.Run(kind, func(t *testing.T) {
			s := open(t, filepath.Join(t.TempDir(), "store.db"))
			a, workingDispatch := newAssignment(t, s, jobWithBranch(t, s))
			working(t, s, workingDispatch)
			if _, err := s.AssignmentRedispatched(a.ID, a.Owner, []string{"try again"}, assignmentsAt); err == nil || !strings.Contains(err.Error(), "wait for it to stop") {
				t.Fatalf("redispatch during active work returned %v", err)
			}
			if kind == ReportResult {
				if _, err := s.EffectRecorded(a.Worker, 1, workingDispatch.ID, "push", "origin/"+a.Workspace.Branch, assignmentsAt); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := s.ReportSubmitted(submission(workingDispatch, kind), assignmentsAt); err != nil {
				t.Fatal(err)
			}
			want := AssignmentActive
			if kind == ReportResult {
				want = AssignmentSubmitted
			}
			if got, err := s.Assignment(a.ID); err != nil || got.State != want {
				t.Fatalf("assignment after %s is %+v (%v)", kind, got, err)
			}
			if _, err := s.NextDispatch(a.Worker); err != sql.ErrNoRows {
				t.Fatalf("the refused redispatch left queued work after %s (%v)", kind, err)
			}
		})
	}
}

func TestRedispatchPreservesQueuedDecisionAnswer(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	a, _ := stoppedAssignment(t, s)
	request, _, err := s.DecisionRequested(workerRequest(a), assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	_, answer, err := s.DecisionAnswered(DecisionAnswer{Request: request.Request, Version: 1, Answerer: a.Owner, Text: "in-memory"}, assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	redispatch, err := s.AssignmentRedispatched(a.ID, a.Owner, []string{"continue"}, assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := s.Inbox(a.Worker, 0, 0, assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range preview {
		if message.Dispatch == answer.ID || message.Dispatch == redispatch.ID {
			t.Fatalf("inbox previewed queued answer or redispatch: %+v", preview)
		}
	}
	if next, err := s.NextDispatch(a.Worker); err != nil || next.ID != answer.ID {
		t.Fatalf("next dispatch is %+v (%v), want queued decision answer %d", next, err, answer.ID)
	}
	messages, err := s.Inbox(a.Worker, answer.ID, answer.ID, assignmentsAt)
	if err != nil || len(messages) != 1 || messages[0].Kind != MessageDecisionAnswer || !messages[0].Fetched || messages[0].RequestData == nil || messages[0].RequestData.Request != request.Request {
		t.Fatalf("worker's decision answer is %+v (%v)", messages, err)
	}
	if next, err := s.NextDispatch(a.Worker); err != nil || next.ID != redispatch.ID {
		t.Fatalf("next dispatch is %+v (%v), want redispatch %d", next, err, redispatch.ID)
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
	messages, err := s.Inbox("worker1@engineering", again.ID, again.ID, assignmentsAt)
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
	messages, err := s.Inbox("leader@engineering", told.ID, told.ID, assignmentsAt)
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
