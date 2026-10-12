// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestChecksAreRecordedEachTimeTheyChange(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	job := jobWithBranch(t, s)
	if _, err := s.LatestChecks(job); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("a job with no checks read has %v", err)
	}
	none := ChecksObservation{Job: job, Number: 7, Head: tipCommit, Verdict: "waiting"}
	pending := ChecksObservation{Job: job, Number: 7, Head: tipCommit, Verdict: "waiting", Checks: []CheckState{{Name: "CI / test", Status: "IN_PROGRESS", State: "pending"}}}
	green := ChecksObservation{Job: job, Number: 7, Head: tipCommit, Verdict: "green", Checks: []CheckState{{Name: "CI / test", Status: "COMPLETED", Conclusion: "SUCCESS", State: "passed"}}}
	for i, tt := range []struct {
		o    ChecksObservation
		want bool
	}{{none, true}, {none, false}, {pending, true}, {pending, false}, {green, true}, {green, false}} {
		got, recorded, err := s.ChecksObserved(tt.o, assignmentsAt)
		if err != nil {
			t.Fatal(err)
		}
		if recorded != tt.want || got.Verdict != tt.o.Verdict || got.ID == 0 {
			t.Errorf("observation %d recorded %v as %+v, want recorded %v", i, recorded, got, tt.want)
		}
	}
	latest, err := s.LatestChecks(job)
	if err != nil || latest.Verdict != "green" || len(latest.Checks) != 1 || latest.Checks[0] != green.Checks[0] {
		t.Errorf("the latest checks are %+v (%v)", latest, err)
	}
	if n := len(journaledKinds(t, s, KindChecksObserved)); n != 3 {
		t.Errorf("checks were journaled %d times, want 3", n)
	}
}

// pushedAndRecorded gives s a job whose job branch is at tipCommit, pushed by
// the daemon, with its pull request #7 recorded, and returns it with the
// dispatch Engineering's leader is working on.
func pushedAndRecorded(t *testing.T, s *Store) (int64, Dispatch) {
	t.Helper()
	job, d := leaderWorking(t, s)
	if _, err := s.db.Exec(`UPDATE job_branches SET tip = ? WHERE job = ?`, tipCommit, job); err != nil {
		t.Fatal(err)
	}
	if _, err := s.JobBranchPushed(Push{Job: job, Branch: "asmai/job-1-add-a-greeting", Commit: tipCommit, Leader: owner, Dispatch: d.ID}, 1, nil, assignmentsAt); err != nil {
		t.Fatal(err)
	}
	p := PullRequest{Job: job, Number: 7, URL: "https://github.com/talvor/asmai-fixture/pull/7", Title: "Add a greeting", Draft: true, Branch: "asmai/job-1-add-a-greeting", Head: tipCommit, State: "OPEN", Leader: owner, Dispatch: d.ID}
	if _, _, err := s.PullRequestRecorded(p, 1, p.URL, assignmentsAt); err != nil {
		t.Fatal(err)
	}
	return job, d
}

func TestAGreenHeadIsDeliveredOnlyOnceQualityValidatedItAndTheDaemonPushedIt(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	job, _ := pushedAndRecorded(t, s)
	green := []CheckState{{Name: "CI / test", Status: "COMPLETED", Conclusion: "SUCCESS", State: "passed"}}
	delivery := Delivery{Job: job, Number: 7, URL: "https://github.com/talvor/asmai-fixture/pull/7", Head: tipCommit, Checks: green}

	// Not validated yet.
	if err := s.TestedPR(job, tipCommit); err == nil || !strings.Contains(err.Error(), "Quality has not validated") {
		t.Errorf("an unvalidated head is a tested pull request: %v", err)
	}
	if _, _, err := s.JobDelivered(delivery, "delivered", assignmentsAt); err == nil {
		t.Error("an unvalidated head was delivered")
	}
	a, d := newValidation(t, s, job)
	if err := s.TestedPR(job, tipCommit); err == nil || !strings.Contains(err.Error(), "has not ended") {
		t.Errorf("a head with a live validation is a tested pull request: %v", err)
	}
	if _, _, err := s.ReportSubmitted(validationOf(a, d, tipCommit, ValidationInput{}), assignmentsAt); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ValidationAccepted(Acceptance{Assignment: a.ID, Leader: qualityOwner, Reasons: []string{"the checks re-ran at the head"}}, owner, assignmentsAt); err != nil {
		t.Fatal(err)
	}
	if err := s.TestedPR(job, tipCommit); err != nil {
		t.Errorf("the validated, pushed head is not a tested pull request: %v", err)
	}
	// Another head is not the one pushed.
	if err := s.TestedPR(job, otherCommit); err == nil || !strings.Contains(err.Error(), "not pushed at "+otherCommit) {
		t.Errorf("a head the daemon did not push is a tested pull request: %v", err)
	}

	for name, d := range map[string]Delivery{
		"with no checks":            {Job: job, Number: 7, URL: delivery.URL, Head: tipCommit, Checks: []CheckState{}},
		"with a check pending":      {Job: job, Number: 7, URL: delivery.URL, Head: tipCommit, Checks: append([]CheckState{{Name: "lint", State: "pending"}}, green...)},
		"with a check failed":       {Job: job, Number: 7, URL: delivery.URL, Head: tipCommit, Checks: []CheckState{{Name: "CI / test", State: "failed"}}},
		"of another pull request":   {Job: job, Number: 8, URL: delivery.URL, Head: tipCommit, Checks: green},
		"of a head it did not push": {Job: job, Number: 7, URL: delivery.URL, Head: otherCommit, Checks: green},
	} {
		if _, _, err := s.JobDelivered(d, "delivered", assignmentsAt); err == nil {
			t.Errorf("a pull request %s was delivered", name)
		}
	}

	got, dispatch, err := s.JobDelivered(delivery, "Job 1 is delivered", assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	if got.Number != 7 || got.Head != tipCommit || got.At.IsZero() || dispatch.Agent != "leader@coordination" {
		t.Errorf("the delivery is %+v, told in %+v", got, dispatch)
	}
	j, err := s.Job(job)
	if err != nil || j.State != JobEnded {
		t.Errorf("the delivered job is %+v (%v)", j, err)
	}
	if read, err := s.JobDelivery(job); err != nil || read.URL != delivery.URL || read.Head != tipCommit || len(read.Checks) != 1 {
		t.Errorf("the delivery reads back as %+v (%v)", read, err)
	}
	messages, err := s.Inbox("leader@coordination", dispatch.ID, assignmentsAt)
	if err != nil || len(messages) != 1 || messages[0].Kind != MessageDelivery || messages[0].Sender != Daemon || messages[0].Job != job || messages[0].Body != "Job 1 is delivered" {
		t.Errorf("Coordination's leader is told %+v (%v)", messages, err)
	}
	// The job ended: it is delivered once, and nothing about it is a tested
	// pull request any more.
	if _, _, err := s.JobDelivered(delivery, "again", assignmentsAt); err == nil {
		t.Error("an ended job was delivered again")
	}
	if err := s.TestedPR(job, tipCommit); err == nil || !strings.Contains(err.Error(), "is ended") {
		t.Errorf("an ended job's head is a tested pull request: %v", err)
	}
	for _, kind := range []string{KindJobDelivered, KindJobEnded} {
		if n := len(journaledKinds(t, s, kind)); n != 1 {
			t.Errorf("%s is journaled %d times", kind, n)
		}
	}

	if done, err := s.LocalRemoved(job); err != nil || done {
		t.Errorf("the job's local removal is %v (%v) before it is journaled", done, err)
	}
	if err := s.JobLocalRemoved(job, []string{"/views/job-1"}, assignmentsAt); err != nil {
		t.Fatal(err)
	}
	if done, err := s.LocalRemoved(job); err != nil || !done {
		t.Errorf("the job's local removal is %v (%v) once journaled", done, err)
	}
}

func journaledKinds(t *testing.T, s *Store, kind string) []Entry {
	t.Helper()
	journal, err := s.Journal()
	if err != nil {
		t.Fatal(err)
	}
	var entries []Entry
	for _, e := range journal {
		if e.Kind == kind {
			entries = append(entries, e)
		}
	}
	return entries
}
