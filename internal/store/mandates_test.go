// SPDX-License-Identifier: Apache-2.0

package store

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAJobOpensWithTheTestedPRMandateOrThePlanningOnlyOneAndNoOther(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	at := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	if err := s.ConversationEntered("leader@coordination", "coordination", 1, "/dev/pts/3", at); err != nil {
		t.Fatal(err)
	}
	_, witness, err := s.ObservedWitnessed(Witnessed{Agent: "leader@coordination", Role: "coordination", Generation: 1, Terminal: "/dev/pts/3", Text: "plan the greeting"}, "UserPromptSubmit", json.RawMessage(`{"prompt":"plan the greeting"}`), at)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RepositoryAdded(Repository{Name: "fixture", Origin: "https://example.test/fixture", DefaultBranch: "main", Clone: "/fixture", AddedAt: at}); err != nil {
		t.Fatal(err)
	}
	for _, mandate := range []string{MandateTestedPR, MandatePlanningOnly} {
		j, err := s.JobOpened(Job{Repository: "fixture", Witness: witness, Reading: "plan", Mandate: mandate, Criteria: []string{"a plan"}}, false, at)
		if err != nil || j.Mandate != mandate {
			t.Errorf("a job with the %s mandate = %+v, %v", mandate, j, err)
		}
		if again, err := s.Job(j.Number); err != nil || again.Mandate != mandate {
			t.Errorf("job %d is read back with mandate %q (%v), want %q", j.Number, again.Mandate, err, mandate)
		}
	}
	for _, mandate := range []string{"", "research-only", "everything"} {
		_, err := s.JobOpened(Job{Repository: "fixture", Witness: witness, Reading: "plan", Mandate: mandate, Criteria: []string{"a plan"}}, false, at)
		if err == nil || !strings.Contains(err.Error(), MandateTestedPR) || !strings.Contains(err.Error(), MandatePlanningOnly) {
			t.Errorf("a job with the %q mandate = %v, want it refused, naming the mandates a job may have", mandate, err)
		}
	}
}
