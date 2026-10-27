// SPDX-License-Identifier: Apache-2.0

package store

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestALeadersBlockedReportGoesToCoordinationAndItsResolutionBackToTheLeader(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	job := jobWithBranch(t, s)
	a, _ := newAssignment(t, s, job)
	b, d, err := s.BlockedReported(BlockedReportInput{
		Job: job, Leader: "leader@engineering", Recipient: "leader@coordination",
		Reason: " the Go build cache is missing ", Needs: "a writable build cache", Blocks: []int64{a.ID, a.ID},
	}, assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	if b.ID == 0 || b.State != BlockedOpen || b.Reason != "the Go build cache is missing" || len(b.Blocks) != 1 || b.Blocks[0] != a.ID {
		t.Fatalf("the report is %+v", b)
	}
	if d.Agent != "leader@coordination" {
		t.Fatalf("the report went to %s, want Coordination's leader", d.Agent)
	}
	messages, err := s.Inbox("leader@coordination", d.ID, 0, assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].Kind != ReportBlocked || messages[0].Sender != "leader@engineering" || messages[0].BlockedData == nil || messages[0].BlockedData.Needs != "a writable build cache" {
		t.Fatalf("Coordination's inbox is %+v", messages)
	}
	if open, err := s.JobBlockedReports(job); err != nil || len(open) != 1 || open[0].ID != b.ID {
		t.Fatalf("the job's blocked reports are %+v (%v)", open, err)
	}
	// It is not a decision request, and nothing waits on the user for it.
	if pending, err := s.PendingDecisions(); err != nil || len(pending) != 0 {
		t.Fatalf("the pending decisions are %+v (%v)", pending, err)
	}

	resolved, back, err := s.BlockedResolved(b.ID, "leader@coordination", "the cache is writable now", assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.State != BlockedResolved || resolved.Note != "the cache is writable now" || resolved.ResolvedBy != "leader@coordination" || back.Agent != "leader@engineering" {
		t.Fatalf("the resolution is %+v, sent as %+v", resolved, back)
	}
	messages, err = s.Inbox("leader@engineering", back.ID, 0, assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].Kind != MessageBlockedResolved || messages[0].Body != "the cache is writable now" || messages[0].BlockedData == nil || messages[0].BlockedData.ID != b.ID {
		t.Fatalf("the leader's inbox is %+v", messages)
	}
	if _, _, err := s.BlockedResolved(b.ID, "leader@coordination", "again", assignmentsAt); err == nil || !strings.Contains(err.Error(), "already resolved") {
		t.Fatalf("resolving it again returned %v", err)
	}
	// The Log shows each entry with the body of the message it caused.
	for kind, want := range map[string]string{KindBlockedReportFiled: "the Go build cache is missing", KindBlockedReportResolved: "the cache is writable now"} {
		entries, _, _, err := s.JournalPage(JournalQuery{Kinds: []string{kind}, Agent: "leader@engineering", Limit: 10})
		if err != nil || len(entries) != 1 {
			t.Fatalf("%s journaled %+v (%v), want once, found by the leader that filed the report", kind, entries, err)
		}
		if body, err := s.MessageBodyOf(entries[0]); err != nil || body != want {
			t.Errorf("the body of %s is %q (%v), want %q", kind, body, err, want)
		}
	}
}

func TestABlockedReportNamesItsJobAReasonAndItsNeedsAndOnlyTheLeadersOwnLiveAssignments(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	job := jobWithBranch(t, s)
	a, _ := newAssignment(t, s, job)
	base := BlockedReportInput{Job: job, Leader: "leader@engineering", Recipient: "leader@coordination", Reason: "broken tool", Needs: "a working tool"}
	for _, tt := range []struct {
		change func(*BlockedReportInput)
		want   string
	}{
		{func(in *BlockedReportInput) { in.Job = 0 }, "names its job"},
		{func(in *BlockedReportInput) { in.Job = 99 }, "job 99 does not exist"},
		{func(in *BlockedReportInput) { in.Reason = " " }, "needs --reason"},
		{func(in *BlockedReportInput) { in.Needs = "" }, "needs --needs"},
		{func(in *BlockedReportInput) { in.Blocks = []int64{99} }, "assignment 99 does not exist"},
		{func(in *BlockedReportInput) { in.Leader, in.Blocks = "leader@quality", []int64{a.ID} }, "is owned by leader@engineering, not leader@quality"},
	} {
		in := base
		tt.change(&in)
		if _, _, err := s.BlockedReported(in, assignmentsAt); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%+v returned %v, want %q", in, err, tt.want)
		}
	}
	if _, _, err := s.BlockedResolved(99, "leader@coordination", "fixed", assignmentsAt); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("resolving a report that does not exist returned %v", err)
	}
	b, _, err := s.BlockedReported(base, assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.BlockedResolved(b.ID, "leader@coordination", " ", assignmentsAt); err == nil || !strings.Contains(err.Error(), "needs --note") {
		t.Errorf("resolving with no note returned %v", err)
	}
}
