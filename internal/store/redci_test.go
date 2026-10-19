// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

var (
	redTest  = []CheckState{{Name: "CI / test", Status: "COMPLETED", Conclusion: "FAILURE", State: "failed", URL: "https://github.com/talvor/asmai-fixture/actions/runs/41/job/1"}}
	redBoth  = append(slices.Clone(redTest), CheckState{Name: "CI / lint", Status: "COMPLETED", Conclusion: "FAILURE", State: "failed"})
	pendingT = []CheckState{{Name: "CI / test", Status: "IN_PROGRESS", State: "pending"}}
)

// observe records the checks on head of job's pull request #7, with verdict.
func observe(t *testing.T, s *Store, job int64, head, verdict string, checks []CheckState) ChecksObservation {
	t.Helper()
	o, _, err := s.ChecksObserved(ChecksObservation{Job: job, Number: 7, Head: head, Verdict: verdict, Checks: checks}, assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestTheDeliveryOwnerIsToldOnceEachTimeAHeadsChecksTurnRed(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	job, _ := pushedAndRecorded(t, s)
	tell := func(o ChecksObservation) bool {
		t.Helper()
		d, told, err := s.ChecksRed(o, owner, "red", assignmentsAt)
		if err != nil {
			t.Fatal(err)
		}
		if told && (d.ID == 0 || d.Agent != owner) {
			t.Errorf("the red checks went as %+v", d)
		}
		return told
	}
	red := observe(t, s, job, tipCommit, "red", redTest)
	if !tell(red) {
		t.Error("red checks were not told")
	}
	if tell(red) {
		t.Error("the same red checks were told twice")
	}
	// Another check failing while the head stays red is not told again.
	if tell(observe(t, s, job, tipCommit, "red", redBoth)) {
		t.Error("red checks were told again while they stayed red")
	}
	// Once re-run, the checks run again, and turning red again is told.
	observe(t, s, job, tipCommit, "waiting", pendingT)
	if !tell(observe(t, s, job, tipCommit, "red", redTest)) {
		t.Error("red checks after a re-run were not told")
	}
	// A newer head's red checks are told.
	if !tell(observe(t, s, job, otherCommit, "red", redTest)) {
		t.Error("a newer head's red checks were not told")
	}
	// Only the latest observation, red, is told.
	green := observe(t, s, job, otherCommit, "green", nil)
	if _, _, err := s.ChecksRed(green, owner, "red", assignmentsAt); err == nil {
		t.Error("green checks were told as red")
	}
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM messages WHERE kind = ? AND recipient = ? AND sender = ?`, MessageChecksRed, owner, Daemon).Scan(&n); err != nil || n != 3 {
		t.Errorf("%d red-checks messages (%v), want 3", n, err)
	}
}

func TestAHeadsFailedJobsAreReRunOnceOnADelegatedDecisionAndASecondReRunIsRefused(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	job, d := pushedAndRecorded(t, s)
	if _, err := s.RerunOf(job, tipCommit); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("a head never re-run has %v", err)
	}
	decision := DecisionRecord{Job: job, Leader: owner, Choice: ChoiceRerunFailed, Rationale: "the runner lost its network", Evidence: "the log shows a DNS failure fetching a dependency", Dispatch: d.ID}
	rerun := Rerun{Job: job, Number: 7, Head: tipCommit, Runs: []int64{41}, Checks: redTest}

	for _, bad := range []DecisionRecord{
		{Job: job, Leader: owner, Choice: "fix", Rationale: "x", Evidence: "y", Dispatch: d.ID},
		{Job: job, Leader: owner, Choice: ChoiceRerunFailed, Rationale: " ", Evidence: "y", Dispatch: d.ID},
		{Job: job, Leader: owner, Choice: ChoiceRerunFailed, Rationale: "x", Evidence: "", Dispatch: d.ID},
	} {
		if _, _, err := s.RerunDecided(bad, rerun, 1, assignmentsAt); err == nil {
			t.Errorf("the decision %+v was recorded", bad)
		}
	}
	if _, _, err := s.RerunDecided(decision, rerun, 2, assignmentsAt); err == nil {
		t.Error("a superseded session's decision was recorded")
	}
	if _, _, err := s.RerunDecided(decision, Rerun{Job: job, Number: 7, Head: tipCommit}, 1, assignmentsAt); err == nil {
		t.Error("a re-run of no workflow run was recorded")
	}

	got, r, err := s.RerunDecided(decision, rerun, 1, assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID == 0 || got.Source != SourceDelegated || got.Choice != ChoiceRerunFailed || got.Rationale != decision.Rationale || got.Evidence != decision.Evidence || got.Dispatch != d.ID {
		t.Errorf("the decision is %+v", got)
	}
	if r.ID == 0 || r.Decision != got.ID || r.Head != tipCommit || !slices.Equal(r.Runs, []int64{41}) || len(r.Checks) != 1 {
		t.Errorf("the re-run is %+v", r)
	}
	if earlier, err := s.RerunOf(job, tipCommit); err != nil || earlier.ID != r.ID {
		t.Errorf("the head's re-run is %+v (%v)", earlier, err)
	}

	// A second re-run of the same head is refused, and nothing is recorded.
	if _, _, err := s.RerunDecided(decision, rerun, 1, assignmentsAt); err == nil || !strings.Contains(err.Error(), "a second re-run of the same head is refused") {
		t.Errorf("a second re-run of the head: %v", err)
	}
	if records, err := s.DecisionRecords(job); err != nil || len(records) != 1 || !reflect.DeepEqual(records[0], got) {
		t.Errorf("the decisions are %+v (%v)", records, err)
	}
	// A newer head may have its own.
	newer := rerun
	newer.Head = otherCommit
	if _, _, err := s.RerunDecided(decision, newer, 1, assignmentsAt); err != nil {
		t.Errorf("a newer head's re-run: %v", err)
	}
	if reruns, err := s.Reruns(job); err != nil || len(reruns) != 2 || reruns[0].Head != tipCommit || reruns[1].Head != otherCommit {
		t.Errorf("the re-runs are %+v (%v)", reruns, err)
	}
	if n := len(journaledKinds(t, s, KindDecisionRecorded)); n != 2 {
		t.Errorf("%d decisions journaled, want 2", n)
	}
	if n := len(journaledKinds(t, s, KindChecksRerun)); n != 2 {
		t.Errorf("%d re-runs journaled, want 2", n)
	}
}

func TestANewerPushReplacesTheCIWaitOnTheOlderHeadOnce(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	job, _ := pushedAndRecorded(t, s)
	for i, want := range []bool{true, false} {
		replaced, err := s.CIWaitReplaced(job, tipCommit, otherCommit, assignmentsAt)
		if err != nil || replaced != want {
			t.Errorf("replacement %d: %v (%v), want %v", i, replaced, err, want)
		}
	}
	entries := journaledKinds(t, s, KindCIWaitReplaced)
	if len(entries) != 1 || !strings.Contains(string(entries[0].Data), `"from":"`+tipCommit+`"`) || !strings.Contains(string(entries[0].Data), `"to":"`+otherCommit+`"`) {
		t.Errorf("the replacements journaled are %+v", entries)
	}
}

func TestTheDeliveryOwnerIsToldAgainOfTheFirstRedChecksOfAHeadAfterItsReRun(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	job, d := pushedAndRecorded(t, s)
	tell := func(o ChecksObservation) bool {
		t.Helper()
		_, told, err := s.ChecksRed(o, owner, "red", assignmentsAt)
		if err != nil {
			t.Fatal(err)
		}
		return told
	}
	if !tell(observe(t, s, job, tipCommit, "red", redTest)) {
		t.Fatal("red checks were not told")
	}
	decision := DecisionRecord{Job: job, Leader: owner, Choice: ChoiceRerunFailed, Rationale: "the runner lost its network", Evidence: "the log shows a DNS failure", Dispatch: d.ID}
	if _, _, err := s.RerunDecided(decision, Rerun{Job: job, Number: 7, Head: tipCommit, Runs: []int64{41}, Checks: redTest}, 1, assignmentsAt); err != nil {
		t.Fatal(err)
	}
	// The observation from before the re-run is not told again.
	before, err := s.LatestChecks(job)
	if err != nil {
		t.Fatal(err)
	}
	if tell(before) {
		t.Error("the red checks from before the re-run were told again")
	}
	// The re-run fails fast, with no pending observation between: red again.
	again := []CheckState{redTest[0]}
	again[0].URL = "https://github.com/talvor/asmai-fixture/actions/runs/41/job/2"
	red := observe(t, s, job, tipCommit, "red", again)
	if !tell(red) {
		t.Error("the re-run's red checks were not told")
	}
	if tell(red) {
		t.Error("the re-run's red checks were told twice")
	}
}
