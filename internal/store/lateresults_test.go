// SPDX-License-Identifier: Apache-2.0

package store

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// closedReason returns why dispatch was closed, as it records it.
func closedReason(t *testing.T, s *Store, dispatch int64) string {
	t.Helper()
	var closed string
	if err := s.db.QueryRow(`SELECT closed FROM dispatches WHERE id = ?`, dispatch).Scan(&closed); err != nil {
		t.Fatal(err)
	}
	return closed
}

// journaledClose returns why the dispatch.changed entry that stopped dispatch
// says it was closed.
func journaledClose(t *testing.T, s *Store, dispatch int64) string {
	t.Helper()
	entries, err := s.Journal()
	if err != nil {
		t.Fatal(err)
	}
	closed := ""
	for _, e := range entries {
		var data struct {
			ID     int64  `json:"id"`
			State  string `json:"state"`
			Closed string `json:"closed"`
		}
		if e.Kind == KindDispatchChanged && json.Unmarshal(e.Data, &data) == nil && data.ID == dispatch && data.State == DispatchStopped {
			closed = data.Closed
		}
	}
	return closed
}

func TestAResultOfATurnThatEndedWithNoReportIsTakenOnItsDispatch(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	// The worker's turn ended with no report, as when the sub-agents it
	// started are still running.
	a, d := stoppedAssignment(t, s)
	if got := closedReason(t, s, d.ID); got != ClosedTurnEnded {
		t.Fatalf("the dispatch records it was closed as %q, want %q", got, ClosedTurnEnded)
	}
	if got := journaledClose(t, s, d.ID); got != ClosedTurnEnded {
		t.Errorf("the journal says the dispatch was closed as %q, want %q", got, ClosedTurnEnded)
	}
	if late, err := s.LateDispatch(a.Worker, 1); err != nil || late != d.ID {
		t.Fatalf("the late dispatch is %d (%v), want %d", late, err, d.ID)
	}

	// The finished work is pushed and recorded, and its result submitted,
	// on the dispatch.
	if _, err := s.EffectRecorded(a.Worker, 1, d.ID, "push", "origin/"+a.Workspace.Branch+"@"+otherCommit, assignmentsAt); err != nil {
		t.Fatalf("recording the late push: %v", err)
	}
	r, owners, err := s.ReportSubmitted(submission(d, ReportResult), assignmentsAt)
	if err != nil {
		t.Fatalf("the late result was refused: %v", err)
	}
	if r.Dispatch != d.ID || owners.Agent != a.Owner {
		t.Errorf("the late result is %+v, sent to %+v", r, owners)
	}
	if got, _ := s.Assignment(a.ID); got.State != AssignmentSubmitted {
		t.Errorf("the assignment is %s, want submitted", got.State)
	}
	if _, _, err := s.ReportSubmitted(submission(d, ReportResult), assignmentsAt); err == nil {
		t.Error("a second late result was taken")
	}
}

func TestABlockedReportOfATurnThatEndedWithNoReportIsTakenOnItsDispatch(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	a, d := stoppedAssignment(t, s)
	r, owners, err := s.ReportSubmitted(submission(d, ReportBlocked), assignmentsAt)
	if err != nil {
		t.Fatalf("the late blocked report was refused: %v", err)
	}
	if r.Kind != ReportBlocked || r.Dispatch != d.ID || owners.Agent != a.Owner {
		t.Errorf("the late blocked report is %+v, sent to %+v", r, owners)
	}
	// A blocked report leaves the assignment active, and ends the dispatch:
	// neither a result nor another blocked report goes on it.
	if got, _ := s.Assignment(a.ID); got.State != AssignmentActive {
		t.Errorf("the assignment is %s, want it still active", got.State)
	}
	for _, kind := range []string{ReportResult, ReportBlocked} {
		if _, _, err := s.ReportSubmitted(submission(d, kind), assignmentsAt); err == nil {
			t.Errorf("a %s was taken on a dispatch that already ended in a blocked report", kind)
		}
	}
}

func TestALateResultIsRefusedOnADispatchClosedAnyOtherWay(t *testing.T) {
	for name, tt := range map[string]struct {
		// close leaves the assignment's worker with no current dispatch,
		// and returns the generation the late result comes from.
		close func(t *testing.T, s *Store, a Assignment, d Dispatch) int
		want  string
	}{
		"ended in a report": {
			func(t *testing.T, s *Store, a Assignment, d Dispatch) int {
				if _, _, err := s.ReportSubmitted(submission(d, ReportBlocked), assignmentsAt); err != nil {
					t.Fatal(err)
				}
				if stopped, err := s.StopDispatchIfWorking(d.ID, 1, "", assignmentsAt); err != nil || !stopped {
					t.Fatalf("stopping the dispatch: %v, %v", stopped, err)
				}
				if got := closedReason(t, s, d.ID); got != ClosedReported {
					t.Errorf("the reported dispatch was closed as %q, want %q", got, ClosedReported)
				}
				return 1
			},
			"already ended in a blocked report",
		},
		"cancelling": {
			func(t *testing.T, s *Store, a Assignment, d Dispatch) int {
				stop(t, s, d)
				if _, _, err := s.CancellationRequested(a.ID, a.Owner, []string{"not needed"}, assignmentsAt); err != nil {
					t.Fatal(err)
				}
				return 1
			},
			"is cancelling, so it takes no new report",
		},
		"cancelled and given to another worker": {
			func(t *testing.T, s *Store, a Assignment, d Dispatch) int {
				stop(t, s, d)
				if _, err := s.db.Exec(`UPDATE assignments SET state = ? WHERE id = ?`, AssignmentCancelled, a.ID); err != nil {
					t.Fatal(err)
				}
				return 1
			},
			"carries no assignment that takes a report",
		},
		"from another session": {
			func(t *testing.T, s *Store, a Assignment, d Dispatch) int {
				stop(t, s, d)
				return 2
			},
			"went to an earlier session",
		},
		"with no recorded close": {
			func(t *testing.T, s *Store, a Assignment, d Dispatch) int {
				if err := s.ChangeDispatch(d.ID, 1, DispatchWorking, DispatchStopped, "", assignmentsAt); err != nil {
					t.Fatal(err)
				}
				return 1
			},
			"was closed (unrecorded), so it takes no report",
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := open(t, filepath.Join(t.TempDir(), "store.db"))
			a, d := newAssignment(t, s, jobWithBranch(t, s))
			working(t, s, d)
			if _, err := s.EffectRecorded(a.Worker, 1, d.ID, "push", "origin/"+a.Workspace.Branch+"@"+otherCommit, assignmentsAt); err != nil {
				t.Fatal(err)
			}
			generation := tt.close(t, s, a, d)
			_, err := s.LateDispatch(a.Worker, generation)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("the late dispatch was found with %v, want it refused saying %q", err, tt.want)
			} else if strings.Contains(err.Error(), "run `asmai inbox`") || strings.Contains(err.Error(), "fetch it with `asmai inbox`") {
				// Fetching by hand makes no dispatch current, so it is no way out.
				t.Errorf("the refusal sends the worker to `asmai inbox`: %v", err)
			}
			for _, kind := range []string{ReportResult, ReportBlocked} {
				sub := submission(d, kind)
				sub.Generation = generation
				if _, _, err := s.ReportSubmitted(sub, assignmentsAt); err == nil || !strings.Contains(err.Error(), "is not the dispatch") && !strings.Contains(err.Error(), "already ended") {
					t.Errorf("a late %s was submitted with %v", kind, err)
				}
			}
		})
	}
}

func TestABlockedReportBeforeRedispatchDeliveryUsesTheStoppedDispatch(t *testing.T) {
	for _, state := range []string{DispatchCreated, DispatchNudged, DispatchUnknown, DispatchDelivered} {
		t.Run(state, func(t *testing.T) {
			s := open(t, filepath.Join(t.TempDir(), "store.db"))
			a, d := stoppedAssignment(t, s)
			next, err := s.AssignmentRedispatched(a.ID, a.Owner, []string{"try again"}, assignmentsAt)
			if err != nil {
				t.Fatal(err)
			}
			if state != DispatchCreated {
				if err := s.ChangeDispatch(next.ID, 1, DispatchCreated, state, "", assignmentsAt); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.LateDispatch(a.Worker, 1); err == nil || !strings.Contains(err.Error(), "is "+state+", and is not your session's current dispatch") {
				t.Errorf("a result can use the old dispatch: %v", err)
			}
			if got, err := s.BlockedDispatch(a.Worker, 1); err != nil || got != d.ID {
				t.Fatalf("blocked dispatch is %d (%v), want %d", got, err, d.ID)
			}
			r, owner, err := s.ReportSubmitted(submission(d, ReportBlocked), assignmentsAt)
			if err != nil || r.Dispatch != d.ID || owner.Agent != a.Owner {
				t.Fatalf("blocked report is %+v, owner %+v, error %v", r, owner, err)
			}
			if got, err := s.WorkerDispatch(a.ID); err != nil || got.Dispatch.ID != next.ID || got.Dispatch.State != state {
				t.Errorf("redispatch changed: %+v (%v)", got, err)
			}
			if _, err := s.BlockedDispatch(a.Worker, 1); err == nil || !strings.Contains(err.Error(), "already ended in a blocked report") {
				t.Errorf("second blocked report can use the old dispatch: %v", err)
			}
		})
	}
}

func TestABlockedReportBeforeRedispatchDeliveryStillRequiresAnActiveAssignmentAndTheSameSession(t *testing.T) {
	for name, change := range map[string]func(t *testing.T, s *Store, a Assignment){
		"cancelling": func(t *testing.T, s *Store, a Assignment) {
			if _, _, err := s.CancellationRequested(a.ID, a.Owner, []string{"not needed"}, assignmentsAt); err != nil {
				t.Fatal(err)
			}
		},
		"reassigned": func(t *testing.T, s *Store, a Assignment) {
			if _, err := s.db.Exec(`UPDATE assignments SET worker = ? WHERE id = ?`, "worker2@engineering", a.ID); err != nil {
				t.Fatal(err)
			}
		},
		"earlier session": func(t *testing.T, s *Store, a Assignment) {},
	} {
		t.Run(name, func(t *testing.T) {
			s := open(t, filepath.Join(t.TempDir(), "store.db"))
			a, d := stoppedAssignment(t, s)
			if _, err := s.AssignmentRedispatched(a.ID, a.Owner, []string{"try again"}, assignmentsAt); err != nil {
				t.Fatal(err)
			}
			change(t, s, a)
			generation := 1
			if name == "earlier session" {
				generation = 2
			}
			if _, err := s.BlockedDispatch(a.Worker, generation); err == nil {
				t.Error("blocked dispatch was offered")
			}
			sub := submission(d, ReportBlocked)
			sub.Generation = generation
			if _, _, err := s.ReportSubmitted(sub, assignmentsAt); err == nil {
				t.Error("blocked report was accepted")
			}
		})
	}
}

// stop ends the turn of working dispatch d with no report.
func stop(t *testing.T, s *Store, d Dispatch) {
	t.Helper()
	if stopped, err := s.StopDispatchIfWorking(d.ID, 1, "", assignmentsAt); err != nil || !stopped {
		t.Fatalf("stopping the dispatch: %v, %v", stopped, err)
	}
}

func TestADispatchAnOwnersRecoveryStopsRecordsWhy(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	a, _ := stoppedAssignment(t, s)
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
	if _, err := s.AssignmentRedispatched(a.ID, a.Owner, []string{"cause fixed"}, assignmentsAt); err != nil {
		t.Fatal(err)
	}
	if got := closedReason(t, s, unknown.ID); got != ClosedRedispatched {
		t.Errorf("the dropped dispatch was closed as %q, want %q", got, ClosedRedispatched)
	}
	if got := journaledClose(t, s, unknown.ID); got != ClosedRedispatched {
		t.Errorf("the journal says the dropped dispatch was closed as %q, want %q", got, ClosedRedispatched)
	}
}
