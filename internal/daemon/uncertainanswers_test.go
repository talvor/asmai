// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/store"
)

// typedAnswer has the owner of assignment id answer a question its worker
// asked, and the answer's nudge typed to the worker in generation of its
// session and never acknowledged: whether the worker saw it is unknown.
func typedAnswer(t *testing.T, s *store.Store, id int64, generation int) (store.Assignment, store.Dispatch) {
	t.Helper()
	a, err := s.Assignment(id)
	if err != nil {
		t.Fatal(err)
	}
	request, _, err := s.DecisionRequested(store.DecisionRequestInput{
		Job: a.Job, Requester: a.Worker, Recipient: a.Owner, Notify: a.Owner, Assignment: a.ID,
		Kind: store.RequestChoice, Questions: []store.Question{{ID: "q1", Title: "Which greeting?", Options: []string{"hello", "hi"}}}, Blocks: []int64{a.ID},
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_, answer, err := s.DecisionAnswered(store.DecisionAnswer{Request: request.Request, Version: 1, Answerer: a.Owner, Text: "hello"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ChangeDispatch(answer.ID, generation, store.DispatchCreated, store.DispatchUnknown, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	return a, answer
}

// heldAgainst returns the dispatch of each assignment.needs_reconciliation
// entry of assignment id, in order.
func heldAgainst(t *testing.T, s *store.Store, id int64) []int64 {
	t.Helper()
	entries, err := s.Journal()
	if err != nil {
		t.Fatal(err)
	}
	var held []int64
	for _, e := range entries {
		if e.Kind != store.KindAssignmentNeedsReconciliation {
			continue
		}
		var h struct {
			Assignment int64 `json:"assignment"`
			Dispatch   int64 `json:"dispatch"`
		}
		if err := json.Unmarshal(e.Data, &h); err != nil {
			t.Fatal(err)
		}
		if h.Assignment == id {
			held = append(held, h.Dispatch)
		}
	}
	return held
}

// ownerTold returns the bodies of the reconciliation messages of assignment
// id waiting for its owner.
func ownerTold(t *testing.T, s *store.Store, a store.Assignment) []string {
	t.Helper()
	messages, err := s.Inbox(a.Owner, 0, 0, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var told []string
	for _, m := range messages {
		if m.Kind == store.MessageReconciliation && m.Assignment == a.ID {
			told = append(told, m.Body)
		}
	}
	return told
}

// A clean stop and start inside the window, after the answer's nudge was
// typed and before the worker acknowledged it, holds the assignment against
// the answer and tells the owner how to recover it; the worker is not
// resumed past it.
func TestAStartAfterACleanStopHoldsAnAssignmentAgainstAnAnswerItsWorkerMayNotHaveSeen(t *testing.T) {
	s, ids := restartStore(t, []restartCase{{name: "stopped", native: true, transcript: true, leave: store.DispatchStopped}})
	a, answer := typedAnswer(t, s, ids["stopped"], 1)

	recoveringDaemon(s).recoverWork(true)

	if got := assignmentState(t, s, a.ID); got != store.AssignmentNeedsReconciliation {
		t.Fatalf("the assignment is %s after the restart, want it held for reconciliation", got)
	}
	if got := heldAgainst(t, s, a.ID); !slices.Equal(got, []int64{answer.ID}) {
		t.Errorf("the assignment was held against dispatches %v, want the answer's, %d", got, answer.ID)
	}
	if w, err := s.WorkerDispatch(a.ID); err != nil || w.Kind == store.MessageResumption {
		t.Errorf("the worker's latest dispatch is %+v (%v), want it not resumed past the answer", w, err)
	}
	if got, err := s.UncertainAnswer(a.ID, 1); err != nil || got.ID != answer.ID {
		t.Errorf("the uncertain answer is %+v (%v), want dispatch %d kept unknown", got, err, answer.ID)
	}
	told := ownerTold(t, s, a)
	if len(told) != 1 || !strings.Contains(told[0], "stopped and started again") ||
		!strings.Contains(told[0], fmt.Sprintf("asmai answer redeliver --dispatch %d", answer.ID)) ||
		!strings.Contains(told[0], fmt.Sprintf("asmai answer retire --dispatch %d", answer.ID)) {
		t.Fatalf("the owner was told %q, want how to deliver the answer again or retire it", told)
	}
	pending, err := s.PendingLeaders()
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(pending, a.Worker) {
		t.Errorf("%s, whose assignment is held, has a message waiting among %v", a.Worker, pending)
	}

	_, again, err := s.AnswerRedelivered(answer.ID, a.Owner, []string{"the factory stopped before the worker read it"}, time.Now())
	if err != nil {
		t.Fatalf("delivering the answer again after the restart: %v", err)
	}
	if next, err := s.NextDispatch(a.Worker); err != nil || next.ID != again.ID {
		t.Fatalf("the worker's next dispatch is %+v (%v), want the answer delivered again, %d", next, err, again.ID)
	}
}

// The owner may instead retire the answer held across the restart, and then
// redispatch is not refused.
func TestAnAnswerHeldAcrossARestartCanBeRetiredAndTheAssignmentRedispatched(t *testing.T) {
	s, ids := restartStore(t, []restartCase{{name: "stopped", native: true, transcript: true, leave: store.DispatchStopped}})
	a, answer := typedAnswer(t, s, ids["stopped"], 1)
	recoveringDaemon(s).recoverWork(true)

	if _, err := s.AssignmentRedispatched(a.ID, a.Owner, []string{"too early"}, time.Now()); err == nil || !strings.Contains(err.Error(), "asmai answer redeliver") {
		t.Fatalf("redispatch over the held answer returned %v, want it to name the ways to recover the answer", err)
	}
	if _, err := s.AnswerRetired(answer.ID, a.Owner, []string{"carried in the redispatch"}, time.Now()); err != nil {
		t.Fatalf("retiring the answer after the restart: %v", err)
	}
	if _, err := s.AssignmentRedispatched(a.ID, a.Owner, []string{"use hello"}, time.Now()); err != nil {
		t.Fatalf("redispatch after the answer was retired: %v", err)
	}
	if got := assignmentState(t, s, a.ID); got != store.AssignmentActive {
		t.Errorf("the assignment is %s after the redispatch, want active", got)
	}
}

// A worker whose next session also exits before acknowledging the answer
// delivered again is held against the new dispatch, and its owner is told
// again: the answer is never stranded.
func TestASecondExitAfterTheAnswerIsDeliveredAgainHoldsTheAssignmentAgainstTheNewDispatch(t *testing.T) {
	s, ids := restartStore(t, []restartCase{{name: "stopped", native: true, transcript: true, leave: store.DispatchStopped}})
	a, answer := typedAnswer(t, s, ids["stopped"], 1)
	d := recoveringDaemon(s)
	w, err := s.WorkerDispatch(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !d.reconcileExit(w, 1, "exit status 1") {
		t.Fatal("the first exit did not hold the assignment")
	}
	_, again, err := s.AnswerRedelivered(answer.ID, a.Owner, []string{"the worker exited before reading it"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	generation, err := s.SessionStarted(store.SessionStart{Agent: a.Worker, Role: roles.Engineering, Provider: "claude-code", Version: "2.1.292", Model: "opus", Executable: "/p", Args: []string{}, Dir: a.Workspace.Path, PID: 9, At: time.Now(), Assignment: a.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ChangeDispatch(again.ID, generation, store.DispatchCreated, store.DispatchUnknown, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if w, err = s.WorkerDispatch(a.ID); err != nil {
		t.Fatal(err)
	}
	if !d.reconcileExit(w, generation, "exit status 1") {
		t.Fatal("the second exit did not hold the assignment")
	}
	if got := heldAgainst(t, s, a.ID); !slices.Equal(got, []int64{answer.ID, again.ID}) {
		t.Errorf("the assignment was held against dispatches %v, want %d then %d", got, answer.ID, again.ID)
	}
	if told := ownerTold(t, s, a); len(told) != 2 || !strings.Contains(told[1], fmt.Sprintf("asmai answer redeliver --dispatch %d", again.ID)) {
		t.Fatalf("the owner was told %q, want a second reconciliation naming dispatch %d", told, again.ID)
	}
	if _, err := s.AnswerRetired(again.ID, a.Owner, []string{"redispatch instead"}, time.Now()); err != nil {
		t.Fatalf("retiring the answer delivered again: %v", err)
	}
}

// A later session's exit still finds an answer typed in an earlier session
// and never acknowledged.
func TestALaterExitHoldsTheAssignmentAgainstAnEarlierSessionsUncertainAnswer(t *testing.T) {
	s, ids := restartStore(t, []restartCase{{name: "stopped", native: true, transcript: true, leave: store.DispatchStopped}})
	a, answer := typedAnswer(t, s, ids["stopped"], 1)
	generation, err := s.SessionStarted(store.SessionStart{Agent: a.Worker, Role: roles.Engineering, Provider: "claude-code", Version: "2.1.292", Model: "opus", Executable: "/p", Args: []string{}, Dir: a.Workspace.Path, PID: 9, At: time.Now(), Assignment: a.ID})
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.WorkerDispatch(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !recoveringDaemon(s).reconcileExit(w, generation, "exit status 1") {
		t.Fatal("the later exit did not hold the assignment")
	}
	if got := heldAgainst(t, s, a.ID); !slices.Equal(got, []int64{answer.ID}) {
		t.Errorf("the assignment was held against dispatches %v, want the earlier session's answer, %d", got, answer.ID)
	}
	if _, err := s.UncertainAnswer(a.ID, generation); errors.Is(err, sql.ErrNoRows) {
		t.Error("the held answer is no longer uncertain")
	}
}
