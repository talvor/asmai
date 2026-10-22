// SPDX-License-Identifier: Apache-2.0

package store

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAFollowUpIsANewJobLinkedToTheOneBeforeIt(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	at := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	if err := s.ConversationEntered("leader@coordination", "coordination", 1, "/dev/pts/3", at); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ObservedWitnessed(Witnessed{Agent: "leader@coordination", Role: "coordination", Generation: 1, Terminal: "/dev/pts/3", Text: "open jobs"}, "UserPromptSubmit", json.RawMessage(`{"prompt":"open jobs"}`), at); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"fixture", "other"} {
		if err := s.RepositoryAdded(Repository{Name: name, Origin: "https://example.test/" + name, DefaultBranch: "main", Clone: "/" + name, AddedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	var jobs []Job
	for _, repository := range []string{"fixture", "fixture", "fixture", "other", ""} {
		mandate := MandateTestedPR
		if repository == "" {
			mandate = MandateResearchOnly
		}
		j, err := s.JobOpened(Job{Repository: repository, Reading: "test", Mandate: mandate, Criteria: []string{"complete"}}, true, at)
		if err != nil {
			t.Fatal(err)
		}
		jobs = append(jobs, j)
	}
	first, second, third, otherRepository, none := jobs[0].Number, jobs[1].Number, jobs[2].Number, jobs[3].Number, jobs[4].Number

	for name, link := range map[string][2]int64{
		"to itself":                      {second, second},
		"to a job opened after it":       {first, second},
		"to a job that does not exist":   {second, 99},
		"from a job that does not exist": {99, first},
		"to a job of another repository": {otherRepository, first},
		"to a job with no repository":    {second, none},
	} {
		if _, err := s.JobLinked(link[0], link[1], at); err == nil {
			t.Errorf("job %d was linked as a follow-up to job %d, %s", link[0], link[1], name)
		}
	}

	linked, err := s.JobLinked(second, first, at)
	if err != nil {
		t.Fatal(err)
	}
	if linked.Follows != first {
		t.Errorf("the linked job follows job %d, want %d", linked.Follows, first)
	}
	if got, err := s.Job(second); err != nil || got.Follows != first {
		t.Errorf("the stored job follows job %d (%v), want %d", got.Follows, err, first)
	}
	if got, err := s.Job(first); err != nil || got.Follows != 0 {
		t.Errorf("the earlier job follows job %d (%v), want none", got.Follows, err)
	}

	// Linking again to the same job changes nothing, and to another is refused.
	if _, err := s.JobLinked(second, first, at); err != nil {
		t.Errorf("linking again to the same job returned %v", err)
	}
	if _, err := s.JobLinked(third, first, at); err != nil {
		t.Fatal(err)
	}
	if _, err := s.JobLinked(third, second, at); err == nil || !strings.Contains(err.Error(), "already follows job 1") {
		t.Errorf("linking a follow-up to a second job returned %v", err)
	}

	journal, err := s.Journal()
	if err != nil {
		t.Fatal(err)
	}
	linkedEntries := 0
	for _, e := range journal {
		if e.Kind == KindJobLinked {
			linkedEntries++
		}
	}
	if linkedEntries != 2 {
		t.Errorf("the journal holds %d job.linked entries, want 2", linkedEntries)
	}
}
