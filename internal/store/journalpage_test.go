// SPDX-License-Identifier: Apache-2.0

package store

import (
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// sendHandoffs sends n handoffs on each of two jobs, alternating, and returns
// the IDs of the handoff.sent entries in order.
func sendHandoffs(t *testing.T, s *Store, n int) []int64 {
	t.Helper()
	at := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	for job := 1; job <= 2; job++ {
		if _, err := s.db.Exec(`INSERT INTO jobs(repository,state) VALUES('fixture',?)`, JobOpen); err != nil {
			t.Fatal(err)
		}
	}
	var sent []int64
	for i := 0; i < 2*n; i++ {
		job := int64(i%2 + 1)
		receiver := "leader@engineering"
		if job == 2 {
			receiver = "leader@quality"
		}
		h := Handoff{Job: job, Sender: "leader@coordination", Receiver: receiver, Outcome: fmt.Sprintf("outcome %d\nsecond line", i), Decisions: "none", Evidence: "none", Constraints: "none", Permissions: "none", Criteria: []string{"complete"}}
		if _, _, err := s.SendHandoff(h, at.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	entries, _, _, err := s.JournalPage(JournalQuery{Kinds: []string{KindHandoffSent}, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		sent = append(sent, e.ID)
	}
	return sent
}

func ids(entries []Entry) []int64 {
	out := []int64{}
	for _, e := range entries {
		out = append(out, e.ID)
	}
	return out
}

func TestJournalPageReadsTheLatestEntriesAndPagesBothWays(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	sent := sendHandoffs(t, s, 5)
	q := JournalQuery{Kinds: []string{KindHandoffSent}, Limit: 4}

	latest, older, newer, err := s.JournalPage(q)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids(latest), sent[6:]) || !older || newer {
		t.Errorf("latest page is %v older %v newer %v, want %v older true newer false", ids(latest), older, newer, sent[6:])
	}

	q.Before = latest[0].ID
	back, older, newer, err := s.JournalPage(q)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids(back), sent[2:6]) || !older || !newer {
		t.Errorf("page before is %v older %v newer %v, want %v older true newer true", ids(back), older, newer, sent[2:6])
	}

	q.Before, q.After = 0, back[len(back)-1].ID
	forward, older, newer, err := s.JournalPage(q)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids(forward), sent[6:]) || !older || newer {
		t.Errorf("page after is %v older %v newer %v, want %v older true newer false", ids(forward), older, newer, sent[6:])
	}

	q.After = sent[len(sent)-1]
	none, older, newer, err := s.JournalPage(q)
	if err != nil || len(none) != 0 || older || newer {
		t.Errorf("page after the head is %v older %v newer %v (%v), want it empty", ids(none), older, newer, err)
	}
}

func TestJournalPageFiltersByJobAgentAndKind(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	sent := sendHandoffs(t, s, 3)
	for _, tt := range []struct {
		name string
		q    JournalQuery
		want []int64
	}{
		{"a job", JournalQuery{Kinds: []string{KindHandoffSent}, Job: 2, Limit: 100}, []int64{sent[1], sent[3], sent[5]}},
		{"an agent as receiver", JournalQuery{Kinds: []string{KindHandoffSent}, Agent: "leader@engineering", Limit: 100}, []int64{sent[0], sent[2], sent[4]}},
		{"an agent as sender", JournalQuery{Kinds: []string{KindHandoffSent}, Agent: "leader@coordination", Limit: 100}, sent},
		{"a job and an agent", JournalQuery{Kinds: []string{KindHandoffSent}, Job: 1, Agent: "leader@quality", Limit: 100}, []int64{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, _, _, err := s.JournalPage(tt.q)
			if err != nil || !slices.Equal(ids(got), tt.want) {
				t.Errorf("page is %v (%v), want %v", ids(got), err, tt.want)
			}
		})
	}
	all, _, _, err := s.JournalPage(JournalQuery{Limit: 1000})
	if err != nil || len(all) <= len(sent) {
		t.Errorf("the whole journal has %d entries (%v), want more than the %d handoffs", len(all), err, len(sent))
	}
}

func TestMessageBodyOfReadsTheMessageAnEntryCaused(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	sent := sendHandoffs(t, s, 1)
	entries, _, _, err := s.JournalPage(JournalQuery{Before: sent[0] + 2, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if entries[0].Kind != KindHandoffSent || entries[1].Kind != KindMessageCreated {
		t.Fatalf("entries are %s and %s, want a handoff and its message", entries[0].Kind, entries[1].Kind)
	}
	for _, e := range entries {
		body, err := s.MessageBodyOf(e)
		if err != nil || body != "outcome 0\nsecond line" {
			t.Errorf("%s has body %q (%v), want the handoff's outcome", e.Kind, body, err)
		}
	}
	other, _, _, err := s.JournalPage(JournalQuery{Kinds: []string{KindDispatchChanged}, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if body, err := s.MessageBodyOf(other[0]); err != nil || body != "" {
		t.Errorf("a dispatch change has body %q (%v), want none", body, err)
	}
}
