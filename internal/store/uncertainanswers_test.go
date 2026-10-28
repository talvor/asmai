// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// heldAnswer gives the stopped assignment's worker a decision answer
// whose nudge was typed in generation 1 and never acknowledged: the window a
// worker that exits before running its inbox leaves.
func heldAnswer(t *testing.T, s *Store) (Assignment, Dispatch) {
	t.Helper()
	a, _ := stoppedAssignment(t, s)
	request, _, err := s.DecisionRequested(workerRequest(a), assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	_, answer, err := s.DecisionAnswered(DecisionAnswer{Request: request.Request, Version: 1, Answerer: a.Owner, Text: "continue"}, assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ChangeDispatch(answer.ID, 1, DispatchCreated, DispatchUnknown, "", assignmentsAt); err != nil {
		t.Fatal(err)
	}
	return a, answer
}

func TestAnUnacknowledgedDecisionAnswerIsTheUncertainAnswerOfItsGeneration(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	a, answer := heldAnswer(t, s)
	got, err := s.UncertainAnswer(a.ID, 1)
	if err != nil || got.ID != answer.ID || got.State != DispatchUnknown || got.Agent != a.Worker {
		t.Fatalf("uncertain answer of generation 1 is %+v (%v), want dispatch %d", got, err, answer.ID)
	}
	if got, err := s.UncertainAnswer(a.ID, 2); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("a later generation's uncertain answer is %+v (%v), want none", got, err)
	}
}

func TestAFetchedDecisionAnswerIsNotUncertain(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	a, answer := heldAnswer(t, s)
	if err := s.ObservedAutomated(Dispatch{ID: answer.ID, Agent: a.Worker, Generation: 1}, "engineering", []byte(`{}`), transcript, assignmentsAt); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Inbox(a.Worker, answer.ID, answer.ID, assignmentsAt); err != nil {
		t.Fatal(err)
	}
	if got, err := s.UncertainAnswer(a.ID, 1); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("a fetched answer is uncertain: %+v (%v)", got, err)
	}
}

func TestAWorkersExitHoldsItsAssignmentAgainstTheUncertainAnswer(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	a, answer := heldAnswer(t, s)
	told, err := s.AssignmentNeedsReconciliation(a.ID, answer.ID, "worker exited", "Reconcile it.", assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	if told.Agent != a.Owner {
		t.Fatalf("the reconciliation went to %s, want the owner %s", told.Agent, a.Owner)
	}
	if got, err := s.Assignment(a.ID); err != nil || got.State != AssignmentNeedsReconciliation {
		t.Fatalf("assignment is %+v (%v)", got, err)
	}
	var state string
	if err := s.db.QueryRow(`SELECT state FROM dispatches WHERE id = ?`, answer.ID).Scan(&state); err != nil || state != DispatchUnknown {
		t.Fatalf("the uncertain answer became %s (%v), want it kept unknown", state, err)
	}
	if got, err := s.UncertainAnswer(a.ID, 1); err != nil || got.ID != answer.ID {
		t.Fatalf("held assignment's uncertain answer is %+v (%v)", got, err)
	}
}

// heldAnswerForReconciliation is heldAnswer with the worker's exit recorded:
// the assignment held for its owner against the uncertain answer.
func heldAnswerForReconciliation(t *testing.T, s *Store) (Assignment, Dispatch) {
	t.Helper()
	a, answer := heldAnswer(t, s)
	if _, err := s.AssignmentNeedsReconciliation(a.ID, answer.ID, "worker exited", "Reconcile it.", assignmentsAt); err != nil {
		t.Fatal(err)
	}
	return a, answer
}

func dispatchState(t *testing.T, s *Store, id int64) string {
	t.Helper()
	var state string
	if err := s.db.QueryRow(`SELECT state FROM dispatches WHERE id = ?`, id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestTheOwnerDeliversAnUncertainAnswerAgainInANewDispatch(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	a, answer := heldAnswerForReconciliation(t, s)
	got, again, err := s.AnswerRedelivered(answer.ID, a.Owner, []string{"the worker's session exited"}, assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	if again.ID == answer.ID || again.Agent != a.Worker || again.State != DispatchCreated {
		t.Fatalf("the answer was delivered again in %+v, want a new dispatch to %s", again, a.Worker)
	}
	if got.State != AssignmentActive {
		t.Fatalf("the assignment is %s after the answer was delivered again, want active", got.State)
	}
	if state := dispatchState(t, s, answer.ID); state != DispatchStopped {
		t.Fatalf("the uncertain dispatch is %s, want stopped", state)
	}
	if next, err := s.NextDispatch(a.Worker); err != nil || next.ID != again.ID {
		t.Fatalf("next dispatch is %+v (%v), want the answer delivered again, %d", next, err, again.ID)
	}
	messages, err := s.Inbox(a.Worker, again.ID, again.ID, assignmentsAt)
	if err != nil || len(messages) != 1 {
		t.Fatalf("inbox is %+v (%v)", messages, err)
	}
	m := messages[0]
	if m.Kind != MessageDecisionAnswer || m.RequestData == nil || !strings.Contains(m.Body, "continue") || !strings.Contains(m.Body, "asmai brief") {
		t.Fatalf("the answer delivered again is %+v, want the same answer to the same request, with how to pick the assignment up", m)
	}
	entries := entriesOf(t, s, KindDecisionAnswerRedelivered)
	if len(entries) != 1 || entries[0]["previous"] != float64(answer.ID) || entries[0]["dispatch"] != float64(again.ID) || entries[0]["owner"] != a.Owner {
		t.Fatalf("journaled redeliveries: %+v", entries)
	}
}

func TestTheOwnerRetiresAnUncertainAnswerAndThenRedispatches(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	a, answer := heldAnswerForReconciliation(t, s)
	got, err := s.AnswerRetired(answer.ID, a.Owner, []string{"the question no longer applies"}, assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != AssignmentNeedsReconciliation {
		t.Fatalf("the assignment is %s after the answer was retired, want it still held", got.State)
	}
	if state := dispatchState(t, s, answer.ID); state != DispatchStopped {
		t.Fatalf("the retired dispatch is %s, want stopped", state)
	}
	entries := entriesOf(t, s, KindDecisionAnswerRetired)
	if len(entries) != 1 || entries[0]["dispatch"] != float64(answer.ID) || entries[0]["owner"] != a.Owner || !strings.Contains(entries[0]["body"].(string), "continue") {
		t.Fatalf("journaled retirements: %+v, want the answer's dispatch, owner and the answer itself", entries)
	}
	redispatch, err := s.AssignmentRedispatched(a.ID, a.Owner, []string{"carry on without it"}, assignmentsAt)
	if err != nil {
		t.Fatalf("redispatch after retiring the answer: %v", err)
	}
	if next, err := s.NextDispatch(a.Worker); err != nil || next.ID != redispatch.ID {
		t.Fatalf("next dispatch is %+v (%v), want the redispatch %d", next, err, redispatch.ID)
	}
}

func TestRedispatchOverAHeldUncertainAnswerNamesTheWaysToRecoverIt(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	a, answer := heldAnswerForReconciliation(t, s)
	_, err := s.AssignmentRedispatched(a.ID, a.Owner, []string{"continue"}, assignmentsAt)
	if err == nil {
		t.Fatal("redispatch over a held uncertain answer was accepted, discarding the answer")
	}
	for _, want := range []string{fmt.Sprintf("decision answer dispatch %d", answer.ID), fmt.Sprintf("asmai answer redeliver --dispatch %d", answer.ID), fmt.Sprintf("asmai answer retire --dispatch %d", answer.ID)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not say %q", err, want)
		}
	}
	if state := dispatchState(t, s, answer.ID); state != DispatchUnknown {
		t.Fatalf("the answer became %s, want it kept", state)
	}
}

func TestOnlyTheOwnerRecoversAHeldUncertainAnswer(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	a, answer := heldAnswerForReconciliation(t, s)
	if _, _, err := s.AnswerRedelivered(answer.ID, "leader@quality", []string{"mine"}, assignmentsAt); err == nil || !strings.Contains(err.Error(), "owned by "+a.Owner) {
		t.Errorf("another leader's redelivery returned %v", err)
	}
	if _, err := s.AnswerRetired(answer.ID, "leader@quality", []string{"mine"}, assignmentsAt); err == nil || !strings.Contains(err.Error(), "owned by "+a.Owner) {
		t.Errorf("another leader's retirement returned %v", err)
	}
	if _, err := s.AnswerRetired(answer.ID, a.Owner, nil, assignmentsAt); err == nil {
		t.Error("a retirement without a reason was accepted")
	}
	if state := dispatchState(t, s, answer.ID); state != DispatchUnknown {
		t.Fatalf("the answer became %s, want it kept", state)
	}
}

func TestOnlyAHeldUncertainAnswerIsRecovered(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	// Not yet held: the worker may still acknowledge the nudge.
	a, answer := heldAnswer(t, s)
	if _, _, err := s.AnswerRedelivered(answer.ID, a.Owner, []string{"early"}, assignmentsAt); err == nil || !strings.Contains(err.Error(), "not an uncertain decision answer of an assignment that needs reconciliation") {
		t.Errorf("redelivery before the assignment was held returned %v", err)
	}
	if _, err := s.AnswerRetired(answer.ID, a.Owner, []string{"early"}, assignmentsAt); err == nil || !strings.Contains(err.Error(), "not an uncertain decision answer of an assignment that needs reconciliation") {
		t.Errorf("retirement before the assignment was held returned %v", err)
	}
	// The work dispatch is no decision answer.
	work, err := s.WorkerDispatch(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AssignmentNeedsReconciliation(a.ID, answer.ID, "worker exited", "Reconcile it.", assignmentsAt); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AnswerRetired(work.Dispatch.ID, a.Owner, []string{"wrong one"}, assignmentsAt); err == nil {
		t.Error("retiring the work dispatch as an answer was accepted")
	}
	// Retired once, it is not recovered again.
	if _, err := s.AnswerRetired(answer.ID, a.Owner, []string{"retire"}, assignmentsAt); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AnswerRedelivered(answer.ID, a.Owner, []string{"again"}, assignmentsAt); err == nil {
		t.Error("a retired answer was delivered again")
	}
}

// entriesOf returns the data of each journal entry of kind, oldest first.
func entriesOf(t *testing.T, s *Store, kind string) []map[string]any {
	t.Helper()
	var data []map[string]any
	for _, e := range journaledKinds(t, s, kind) {
		var d map[string]any
		if err := json.Unmarshal(e.Data, &d); err != nil {
			t.Fatal(err)
		}
		data = append(data, d)
	}
	return data
}
