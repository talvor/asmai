// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var assignmentsAt = time.Date(2026, 10, 9, 9, 0, 0, 0, time.UTC)

const (
	tipCommit   = "1111111111111111111111111111111111111111"
	otherCommit = "2222222222222222222222222222222222222222"
)

// jobWithBranch gives s a repository job with a job branch.
func jobWithBranch(t *testing.T, s *Store) int64 {
	t.Helper()
	if err := s.RepositoryAdded(Repository{Name: "fixture", Origin: "/origin", DefaultBranch: "main", Clone: "/clone", AddedAt: assignmentsAt}); err != nil {
		t.Fatal(err)
	}
	result, err := s.db.Exec(`INSERT INTO jobs(repository, state, reading) VALUES('fixture', ?, 'Add a greeting')`, JobOpen)
	if err != nil {
		t.Fatal(err)
	}
	job, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.JobBranchMade(JobBranch{Job: job, Repository: "fixture", Name: "asmai/job-1-add-a-greeting", StartedFrom: tipCommit, View: "/views/job-1"}, assignmentsAt); err != nil {
		t.Fatal(err)
	}
	return job
}

// newAssignment records an assignment of job to the next free worker of
// Engineering, and returns it with its dispatch.
func newAssignment(t *testing.T, s *Store, job int64) (Assignment, Dispatch) {
	t.Helper()
	id, err := s.NextAssignmentID()
	if err != nil {
		t.Fatal(err)
	}
	number, slot, err := s.Allocate("engineering")
	if err != nil {
		t.Fatal(err)
	}
	a, d, err := s.AssignmentCreated(NewAssignment{
		ID: id, Job: job, Owner: "leader@engineering", Worker: "worker" + string(rune('0'+number)) + "@engineering",
		Outcome: "Add greeting.txt", Criteria: []string{"it says hello"},
		Workspace: Workspace{Slot: slot, Path: "/ws/repo", Tmp: "/ws/tmp", Branch: "asmai/job-1/" + string(rune('0'+id)), Base: tipCommit},
	}, assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	return a, d
}

// working puts d, a dispatch of agent, in generation 1 and working, as its
// fetch does.
func working(t *testing.T, s *Store, d Dispatch) {
	t.Helper()
	if err := s.ChangeDispatch(d.ID, 1, DispatchCreated, DispatchWorking, "", assignmentsAt); err != nil {
		t.Fatal(err)
	}
}

func TestAJobBranchIsRecordedOnceWithTheCommitItStartedFrom(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	job := jobWithBranch(t, s)
	b, err := s.JobBranch(job)
	if err != nil {
		t.Fatal(err)
	}
	if b.Name != "asmai/job-1-add-a-greeting" || b.StartedFrom != tipCommit || b.Tip != tipCommit || b.View != "/views/job-1" || b.Repository != "fixture" {
		t.Errorf("the job branch is %+v", b)
	}
	// Making it again changes nothing, and answers with what is there.
	again, err := s.JobBranchMade(JobBranch{Job: job, Repository: "fixture", Name: "asmai/other", StartedFrom: otherCommit, View: "/elsewhere"}, assignmentsAt.Add(time.Hour))
	if err != nil || again != b {
		t.Errorf("recording the job branch again returned %+v (%v), want %+v", again, err, b)
	}
	if got := strings.Count(strings.Join(kinds(t, s), ","), KindJobBranchMade); got != 1 {
		t.Errorf("the journal holds %d job branches, want 1", got)
	}
	if _, err := s.JobBranch(job + 1); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("a job with no branch returned %v, want no rows", err)
	}
	if _, err := s.JobBranchMade(JobBranch{Job: job + 1, Repository: "fixture", Name: "n", StartedFrom: tipCommit, View: "v"}, assignmentsAt); err == nil {
		t.Error("a branch was recorded for a job that does not exist")
	}
}

func TestAnAssignmentIsRecordedWithItsWorkspaceAndTheDispatchThatDeliversIt(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	job := jobWithBranch(t, s)
	a, d := newAssignment(t, s, job)
	if a.ID != 1 || a.State != AssignmentActive || a.Owner != "leader@engineering" || a.Worker != "worker1@engineering" || a.JobBranch != "asmai/job-1-add-a-greeting" {
		t.Errorf("the assignment is %+v", a)
	}
	if d.Agent != "worker1@engineering" || d.State != DispatchCreated {
		t.Errorf("the dispatch is %+v, want a created one for the worker", d)
	}
	got, err := s.Assignment(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.JobBranch != "asmai/job-1-add-a-greeting" || got.Workspace.Slot != 1 || got.Workspace.Base != tipCommit || got.Workspace.Branch != "asmai/job-1/1" || got.Criteria[0] != "it says hello" {
		t.Errorf("the assignment reads back as %+v", got)
	}
	if want := "assignment.created,workspace.created,message.created,dispatch.changed"; !strings.HasSuffix(strings.Join(kinds(t, s), ","), want) {
		t.Errorf("the journal holds %v, want it to end with %s", kinds(t, s), want)
	}

	// The worker's inbox gives the assignment itself.
	messages, err := s.Inbox("worker1@engineering", 0, assignmentsAt)
	if err != nil || len(messages) != 1 || messages[0].Kind != MessageAssignment || messages[0].AssignmentData == nil || messages[0].AssignmentData.ID != a.ID || messages[0].Handoff != 0 {
		t.Fatalf("the worker's inbox holds %+v (%v)", messages, err)
	}
	if mine, err := s.DispatchAssignment(d.ID); err != nil || mine.ID != a.ID {
		t.Errorf("the dispatch is about %+v (%v), want assignment %d", mine, err, a.ID)
	}
	if live, err := s.LiveAssignmentOf("worker1@engineering"); err != nil || live.ID != a.ID {
		t.Errorf("the worker carries %+v (%v), want assignment %d", live, err, a.ID)
	}
}

func TestAnAssignmentNeedsWhatItsWorkerIsToBeGiven(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	job := jobWithBranch(t, s)
	base := NewAssignment{ID: 1, Job: job, Owner: "leader@engineering", Worker: "worker1@engineering", Outcome: "outcome", Criteria: []string{"criterion"},
		Workspace: Workspace{Slot: 1, Path: "/p", Tmp: "/t", Branch: "b", Base: tipCommit}}
	for name, mutate := range map[string]func(*NewAssignment){
		"an outcome":                  func(n *NewAssignment) { n.Outcome = " " },
		"acceptance criteria":         func(n *NewAssignment) { n.Criteria = nil },
		"text in each criterion":      func(n *NewAssignment) { n.Criteria = []string{"ok", " "} },
		"an owning leader":            func(n *NewAssignment) { n.Owner = "" },
		"a workspace made at the tip": func(n *NewAssignment) { n.Workspace.Base = otherCommit },
		"a job that exists":           func(n *NewAssignment) { n.Job = job + 1 },
	} {
		n := base
		mutate(&n)
		if _, _, err := s.AssignmentCreated(n, assignmentsAt); err == nil {
			t.Errorf("an assignment without %s was recorded", name)
		}
	}
	if _, err := s.db.Exec(`UPDATE jobs SET state = ?`, JobEnded); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AssignmentCreated(base, assignmentsAt); err == nil || !strings.Contains(err.Error(), "ended") {
		t.Errorf("an assignment of an ended job returned %v", err)
	}
}

func TestWorkerNumbersAndSlotsStartAtTheLowestFreeAndAreReused(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	job := jobWithBranch(t, s)
	if worker, slot, err := s.Allocate("engineering"); err != nil || worker != 1 || slot != 1 {
		t.Fatalf("the first worker and slot are %d and %d (%v), want 1 and 1", worker, slot, err)
	}
	var made []Assignment
	for range 3 {
		a, _ := newAssignment(t, s, job)
		made = append(made, a)
	}
	for i, a := range made {
		if want := "worker" + string(rune('1'+i)) + "@engineering"; a.Worker != want || a.Workspace.Slot != i+1 {
			t.Errorf("assignment %d has %s in slot %d, want %s in slot %d", a.ID, a.Worker, a.Workspace.Slot, want, i+1)
		}
	}
	// The second assignment ends: its worker number and slot are the lowest
	// free ones, and the next worker takes them.
	if _, err := s.db.Exec(`UPDATE assignments SET state = 'accepted' WHERE id = ?`, made[1].ID); err != nil {
		t.Fatal(err)
	}
	if worker, slot, err := s.Allocate("engineering"); err != nil || worker != 2 || slot != 2 {
		t.Errorf("after the second ended, the next worker and slot are %d and %d (%v), want 2 and 2", worker, slot, err)
	}
	if worker, _, err := s.Allocate("quality"); err != nil || worker != 1 {
		t.Errorf("Quality's first worker is %d (%v), want 1: numbers are counted for each role", worker, err)
	}
	if _, err := s.db.Exec(`UPDATE assignments SET state = 'cancelled' WHERE id = ?`, made[0].ID); err != nil {
		t.Fatal(err)
	}
	if worker, slot, err := s.Allocate("engineering"); err != nil || worker != 1 || slot != 1 {
		t.Errorf("after the first ended too, the next worker and slot are %d and %d (%v), want 1 and 1", worker, slot, err)
	}
	if live, err := s.LiveAssignmentOf("worker2@engineering"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("an ended assignment is still carried: %+v (%v)", live, err)
	}
}

func TestAnEffectIsTaggedWithTheDispatchItWasMadeIn(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	job := jobWithBranch(t, s)
	a, d := newAssignment(t, s, job)
	const worker = "worker1@engineering"
	if _, err := s.EffectRecorded(worker, 1, d.ID, "commit", tipCommit, assignmentsAt); err == nil {
		t.Error("an effect was recorded in a dispatch the worker has not fetched")
	}
	working(t, s, d)
	e, err := s.EffectRecorded(worker, 1, d.ID, "push", "origin/asmai/job-1/1@"+otherCommit, assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	if e.ID != 1 || e.Job != job || e.Assignment != a.ID || e.Dispatch != d.ID || e.Agent != worker || e.Generation != 1 || e.Kind != "push" {
		t.Errorf("the effect is %+v", e)
	}
	for name, call := range map[string]func() error{
		"another agent's": func() error {
			_, err := s.EffectRecorded("worker2@engineering", 1, d.ID, "commit", "x", assignmentsAt)
			return err
		},
		"a superseded session": func() error { _, err := s.EffectRecorded(worker, 2, d.ID, "commit", "x", assignmentsAt); return err },
		"a dispatch that does not exist": func() error {
			_, err := s.EffectRecorded(worker, 1, d.ID+9, "commit", "x", assignmentsAt)
			return err
		},
		"no kind":         func() error { _, err := s.EffectRecorded(worker, 1, d.ID, " ", "x", assignmentsAt); return err },
		"a kind of words": func() error { _, err := s.EffectRecorded(worker, 1, d.ID, "a push", "x", assignmentsAt); return err },
		"no ref":          func() error { _, err := s.EffectRecorded(worker, 1, d.ID, "push", "", assignmentsAt); return err },
	} {
		if call() == nil {
			t.Errorf("an effect with %s was recorded", name)
		}
	}
	if got := strings.Count(strings.Join(kinds(t, s), ","), KindEffectRecorded); got != 1 {
		t.Errorf("the journal holds %d effects, want 1", got)
	}
}

func submission(d Dispatch, kind string) Submission {
	in := ReportInput{Evidence: []string{"check.sh passes"}, Tests: []string{"check.sh"}, Checks: []Check{{Command: "./check.sh", Outcome: "passed"}}, Gaps: []string{}, PRSection: "Adds greeting.txt."}
	if kind == ReportBlocked {
		in = ReportInput{Reason: "which format?", Needs: "the user"}
	}
	return Submission{Agent: "worker1@engineering", Generation: 1, Dispatch: d.ID, Kind: kind, Commit: otherCommit, JobTip: tipCommit, TookInTip: true, Input: in}
}

func TestAResultSubmitsTheAssignmentAndGoesToItsOwningLeader(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	job := jobWithBranch(t, s)
	a, d := newAssignment(t, s, job)
	working(t, s, d)
	if _, err := s.EffectRecorded("worker1@engineering", 1, d.ID, "commit", otherCommit, assignmentsAt); err != nil {
		t.Fatal(err)
	}

	for name, mutate := range map[string]func(*Submission){
		"a PR section":       func(s *Submission) { s.Input.PRSection = " " },
		"evidence":           func(s *Submission) { s.Input.Evidence = nil },
		"a commit":           func(s *Submission) { s.Commit = "" },
		"its own worker":     func(s *Submission) { s.Agent = "worker2@engineering" },
		"a live session":     func(s *Submission) { s.Generation = 2 },
		"a known kind":       func(s *Submission) { s.Kind = "done" },
		"the job branch tip": func(s *Submission) { s.TookInTip = false },
	} {
		sub := submission(d, ReportResult)
		mutate(&sub)
		if _, _, err := s.ReportSubmitted(sub, assignmentsAt); err == nil {
			t.Errorf("a result without %s was recorded", name)
		}
	}
	if _, _, err := s.ReportSubmitted(submission(d, ReportResult), assignmentsAt); err == nil || !strings.Contains(err.Error(), "push") {
		t.Errorf("a result without a recorded push returned %v", err)
	}
	if _, err := s.EffectRecorded("worker1@engineering", 1, d.ID, "push", "origin/asmai/job-1/1", assignmentsAt); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Assignment(a.ID); got.State != AssignmentActive {
		t.Fatalf("a refused result moved the assignment to %s", got.State)
	}

	r, leaderDispatch, err := s.ReportSubmitted(submission(d, ReportResult), assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	if r.ID != 1 || r.Kind != ReportResult || r.Assignment != a.ID || r.Commit != otherCommit || len(r.Effects) != 2 || r.Effects[0].Kind != "commit" || r.Effects[1].Kind != "push" || r.Gaps == nil || r.Artifacts == nil {
		t.Errorf("the result is %+v", r)
	}
	if got, _ := s.Assignment(a.ID); got.State != AssignmentSubmitted {
		t.Errorf("the assignment is %s, want submitted", got.State)
	}
	if leaderDispatch.Agent != "leader@engineering" || leaderDispatch.State != DispatchCreated {
		t.Errorf("the result's dispatch is %+v, want a created one for the owning leader", leaderDispatch)
	}
	messages, err := s.Inbox("leader@engineering", 0, assignmentsAt)
	if err != nil || len(messages) != 1 || messages[0].Kind != ReportResult || messages[0].ReportData == nil || messages[0].ReportData.PRSection != "Adds greeting.txt." || messages[0].AssignmentData.State != AssignmentSubmitted {
		t.Fatalf("the leader's inbox holds %+v (%v)", messages, err)
	}
	if !strings.Contains(strings.Join(kinds(t, s), ","), KindResultSubmitted) {
		t.Errorf("the journal holds %v, want the result", kinds(t, s))
	}

	// A dispatch ends in one report, and a submitted assignment takes none.
	if _, _, err := s.ReportSubmitted(submission(d, ReportBlocked), assignmentsAt); err == nil || !strings.Contains(err.Error(), "submitted") {
		t.Errorf("a report of a submitted assignment returned %v", err)
	}
	if reports, err := s.Reports(a.ID); err != nil || len(reports) != 1 {
		t.Errorf("the assignment has reports %+v (%v), want 1", reports, err)
	}
}

func TestABlockedReportLeavesTheAssignmentActiveAndEndsItsDispatchOnce(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	job := jobWithBranch(t, s)
	a, d := newAssignment(t, s, job)
	working(t, s, d)
	if _, _, err := s.ReportSubmitted(Submission{Agent: "worker1@engineering", Generation: 1, Dispatch: d.ID, Kind: ReportBlocked, Commit: otherCommit}, assignmentsAt); err == nil {
		t.Error("a blocked report that says nothing was recorded")
	}
	if _, _, err := s.ReportSubmitted(submission(d, ReportBlocked), assignmentsAt); err == nil || !strings.Contains(err.Error(), "push") {
		t.Errorf("a blocked report without a recorded push returned %v", err)
	}
	withoutTip := submission(d, ReportBlocked)
	withoutTip.TookInTip = false
	if _, _, err := s.ReportSubmitted(withoutTip, assignmentsAt); err == nil || !strings.Contains(err.Error(), "tip") {
		t.Errorf("a blocked report that has not taken in the job tip returned %v", err)
	}
	if _, err := s.EffectRecorded("worker1@engineering", 1, d.ID, "push", "origin/asmai/job-1/1", assignmentsAt); err != nil {
		t.Fatal(err)
	}
	r, leaderDispatch, err := s.ReportSubmitted(submission(d, ReportBlocked), assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	if r.Kind != ReportBlocked || r.Reason != "which format?" || r.Needs != "the user" || leaderDispatch.Agent != "leader@engineering" {
		t.Errorf("the blocked report is %+v for %+v", r, leaderDispatch)
	}
	if got, _ := s.Assignment(a.ID); got.State != AssignmentActive {
		t.Errorf("the assignment is %s, want it still active", got.State)
	}
	if _, _, err := s.ReportSubmitted(submission(d, ReportResult), assignmentsAt); err == nil || !strings.Contains(err.Error(), "already ended in a blocked report") {
		t.Errorf("a second report in the dispatch returned %v", err)
	}
	if !strings.Contains(strings.Join(kinds(t, s), ","), KindBlockedReported) {
		t.Errorf("the journal holds %v, want the blocked report", kinds(t, s))
	}
}

// A store at schema 6, which keeps its messages in a table that needs a
// handoff, is migrated with every message and dispatch it holds.
func TestOpenMigratesAStoreWhoseMessagesAllHaveAHandoff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	db, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	statements := append([]string{}, migrations[:6]...)
	statements = append(statements,
		`INSERT INTO jobs(repository, state) VALUES('fixture', 'open')`,
		`INSERT INTO handoffs(job,sender,receiver,outcome,decisions,evidence,constraints,permissions,criteria,state,created_at) VALUES(1,'leader@coordination','leader@engineering','outcome','d','e','c','p','[]','pending','2026-10-08T09:00:00Z')`,
		`INSERT INTO messages(handoff,job,sender,recipient,kind,body,fetched_at) VALUES(1,1,'leader@coordination','leader@engineering','handoff','outcome','2026-10-08T09:01:00Z')`,
		`INSERT INTO dispatches(message,agent,generation,state,updated_at) VALUES(1,'leader@engineering',1,'delivered','2026-10-08T09:01:00Z')`,
		`PRAGMA user_version = 6`)
	for _, stmt := range statements {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	db.Close()

	s := open(t, path)
	messages, err := s.Inbox("leader@engineering", 0, assignmentsAt)
	if err != nil || len(messages) != 1 || messages[0].Handoff != 1 || messages[0].Body != "outcome" || !messages[0].Fetched || messages[0].HandoffData == nil {
		t.Fatalf("the migrated inbox holds %+v (%v)", messages, err)
	}
	var dispatches int
	if err := s.db.QueryRow(`SELECT count(*) FROM dispatches JOIN messages ON messages.id = dispatches.message`).Scan(&dispatches); err != nil || dispatches != 1 {
		t.Errorf("%d dispatches still reach their messages (%v), want 1", dispatches, err)
	}
	// The next message continues from the last, and can be about an
	// assignment instead of a handoff.
	if _, err := s.JobBranchMade(JobBranch{Job: 1, Repository: "fixture", Name: "asmai/job-1-x", StartedFrom: tipCommit, View: "/v"}, assignmentsAt); err != nil {
		t.Fatal(err)
	}
	newAssignment(t, s, 1)
	next, err := s.Inbox("worker1@engineering", 0, assignmentsAt)
	if err != nil || len(next) != 1 || next[0].ID != 2 || next[0].Handoff != 0 || next[0].Assignment != 1 {
		t.Errorf("the first message after the migration is %+v (%v), want message 2 about assignment 1", next, err)
	}
}
