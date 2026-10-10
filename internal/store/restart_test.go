// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	nativeID   = "6b8b4567-327b-4c23-9e0d-2ed1e8c0a8f3"
	transcript = "/home/u/.claude/projects/ws/" + nativeID + ".jsonl"
)

// startWorker records the session of generation of worker1@engineering for
// the assignment, as the daemon does when it starts it, and the provider's
// identification of it.
func startWorker(t *testing.T, s *Store, assignment int64, resumes string) int {
	t.Helper()
	generation, err := s.SessionStarted(SessionStart{
		Agent: "worker1@engineering", Role: "engineering", Provider: "claude-code", Version: "2.1.292", Model: "opus",
		Executable: "/p", Args: []string{}, Dir: "/ws/repo", PID: 41, At: assignmentsAt, Assignment: assignment, Resumes: resumes,
	})
	if err != nil {
		t.Fatal(err)
	}
	return generation
}

// stoppedAssignment makes an assignment whose worker fetched it and stopped
// its first turn without a report, in the generation of its session, which
// is identified by the provider as nativeID.
func stoppedAssignment(t *testing.T, s *Store) (Assignment, Dispatch) {
	t.Helper()
	job := jobWithBranch(t, s)
	a, d := newAssignment(t, s, job)
	if g := startWorker(t, s, a.ID, ""); g != 1 {
		t.Fatalf("the worker's first generation is %d", g)
	}
	if err := s.SessionIdentified("worker1@engineering", 1, nativeID, transcript, assignmentsAt); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Inbox("worker1@engineering", d.ID, assignmentsAt); err != nil {
		t.Fatal(err)
	}
	if err := s.WorkFetchedDispatch(d.ID, "worker1@engineering", 1, assignmentsAt); err != nil {
		t.Fatal(err)
	}
	if stopped, err := s.StopDispatchIfWorking(d.ID, 1, transcript, assignmentsAt); err != nil || !stopped {
		t.Fatalf("stopping the dispatch: %v, %v", stopped, err)
	}
	return a, d
}

func TestTheProvidersOwnIdentifierOfASessionIsRecordedOnceAndJournaled(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	job := jobWithBranch(t, s)
	a, _ := newAssignment(t, s, job)
	startWorker(t, s, a.ID, "")

	if _, err := s.ResumableSession(a.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("a session the provider has not identified is resumable: %v", err)
	}
	// A hook of a session this store never recorded is no session of its own.
	if err := s.SessionIdentified("worker9@engineering", 1, nativeID, transcript, assignmentsAt); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := s.SessionIdentified("worker1@engineering", 1, nativeID, transcript, assignmentsAt); err != nil {
			t.Fatal(err)
		}
	}
	// Later hooks may leave the transcript out, which is not a change.
	if err := s.SessionIdentified("worker1@engineering", 1, nativeID, "", assignmentsAt); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, k := range kinds(t, s) {
		if k == KindSessionIdentified {
			n++
		}
	}
	if n != 1 {
		t.Errorf("the journal holds %d identifications of the session, want 1", n)
	}
	got, err := s.ResumableSession(a.ID)
	if err != nil || got.ID != nativeID || got.Transcript != transcript || got.Agent != "worker1@engineering" || got.Generation != 1 || got.Provider != "claude-code" || got.Assignment != a.ID {
		t.Errorf("the resumable session is %+v (%v)", got, err)
	}

	// A later session of the same assignment is the one to resume.
	if g := startWorker(t, s, a.ID, nativeID); g != 2 {
		t.Fatalf("the worker's second generation is %d", g)
	}
	if err := s.SessionIdentified("worker1@engineering", 2, "second-native", "/t2", assignmentsAt); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ResumableSession(a.ID); err != nil || got.ID != "second-native" || got.Generation != 2 {
		t.Errorf("the resumable session is %+v (%v), want the second", got, err)
	}
}

func TestAStoppedWorkersAssignmentIsResumedInANewDispatchThatIsRecordedAsOne(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	a, d := stoppedAssignment(t, s)

	w, err := s.WorkerDispatch(a.ID)
	if err != nil || w.Dispatch.ID != d.ID || w.Dispatch.State != DispatchStopped || !w.Fetched || w.Kind != MessageAssignment || w.Report != "" {
		t.Fatalf("the worker's latest dispatch is %+v (%v)", w, err)
	}
	inState, err := s.WorkersInState(AssignmentActive)
	if err != nil || len(inState) != 1 || inState[0].Assignment.ID != a.ID {
		t.Fatalf("the active assignments' workers are %+v (%v)", inState, err)
	}

	if _, err := s.AssignmentResumed(a.ID, d.ID, "", "body", assignmentsAt); err == nil {
		t.Error("a resumption that names no native session was recorded")
	}
	if _, err := s.AssignmentResumed(a.ID, d.ID+1, nativeID, "body", assignmentsAt); err == nil {
		t.Error("a resumption of a dispatch that is not the worker's latest was recorded")
	}
	resumed, err := s.AssignmentResumed(a.ID, d.ID, nativeID, "Continue.", assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Agent != "worker1@engineering" || resumed.State != DispatchCreated || resumed.Resumes != d.ID || resumed.NativeSession != nativeID || resumed.ID == d.ID {
		t.Errorf("the resumption's dispatch is %+v, want a new, created one that resumes %d in %s", resumed, d.ID, nativeID)
	}
	// The dispatch that stopped is as it was, and the resumption is the
	// worker's latest, which is waiting for it.
	if got, _ := s.WorkerDispatch(a.ID); got.Dispatch.ID != resumed.ID || got.Kind != MessageResumption || got.Fetched || got.Dispatch.NativeSession != nativeID {
		t.Errorf("the worker's latest dispatch is %+v, want the unfetched resumption", got)
	}
	var state string
	if err := s.db.QueryRow(`SELECT state FROM dispatches WHERE id = ?`, d.ID).Scan(&state); err != nil || state != DispatchStopped {
		t.Errorf("the dispatch that stopped is %s (%v), want it still stopped", state, err)
	}
	if _, err := s.AssignmentResumed(a.ID, d.ID, nativeID, "again", assignmentsAt); err == nil {
		t.Error("the same dispatch was resumed twice")
	}
	if got, err := s.Assignment(a.ID); err != nil || got.State != AssignmentActive {
		t.Errorf("the assignment is %+v (%v), want it still active", got, err)
	}

	// The worker fetches it as it does any message, and sees what it resumes.
	messages, err := s.Inbox("worker1@engineering", resumed.ID, assignmentsAt)
	if err != nil || len(messages) != 1 || messages[0].Kind != MessageResumption || messages[0].Resumes != d.ID || messages[0].Body != "Continue." || messages[0].AssignmentData == nil || messages[0].Sender != Daemon {
		t.Fatalf("the worker's inbox holds %+v (%v)", messages, err)
	}
	entries, err := s.Journal()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if e.Kind == KindAssignmentResumed {
			found = strings.Contains(string(e.Data), nativeID) && strings.Contains(string(e.Data), `"resumes":`)
		}
	}
	if !found {
		t.Errorf("the journal holds %v, want the resumption with its native session", kinds(t, s))
	}
}

// Evidence is correlated to the dispatch and the generation it came from: what
// the session before a restart made is journaled and never completes, or is
// taken for, the dispatch that resumes it.
func TestEvidenceFromBeforeARestartNeverChangesTheResumedDispatchAndEveryResultStaysTiedToItsDispatch(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	a, stopped := stoppedAssignment(t, s)
	if _, err := s.EffectRecorded("worker1@engineering", 1, stopped.ID, "commit", otherCommit, assignmentsAt); err == nil {
		t.Fatal("an effect was recorded in a dispatch that had stopped")
	}
	resumed, err := s.AssignmentResumed(a.ID, stopped.ID, nativeID, "Continue.", assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	// The restart: a new session, which the resumption's dispatch is bound to.
	if g := startWorker(t, s, a.ID, nativeID); g != 2 {
		t.Fatalf("the second generation is %d", g)
	}
	var generation int
	if err := s.db.QueryRow(`SELECT generation FROM dispatches WHERE id = ?`, resumed.ID).Scan(&generation); err != nil || generation != 2 {
		t.Fatalf("the resumption is bound to generation %d (%v), want 2", generation, err)
	}

	// Late evidence of the old session: refused, whatever it is.
	if err := s.ChangeDispatch(resumed.ID, 1, DispatchCreated, DispatchNudged, "", assignmentsAt); err == nil {
		t.Error("the old session's acknowledgment moved the resumption")
	}
	if err := s.WorkFetchedDispatch(resumed.ID, "worker1@engineering", 1, assignmentsAt); err == nil {
		t.Error("the old session's fetch moved the resumption")
	}
	if stoppedNow, err := s.StopDispatchIfWorking(resumed.ID, 1, "", assignmentsAt); err != nil || stoppedNow {
		t.Errorf("the old session's stop ended the resumption: %v, %v", stoppedNow, err)
	}
	if err := s.ChangeDispatch(stopped.ID, 2, DispatchStopped, DispatchWorking, "", assignmentsAt); err == nil {
		t.Error("the new session moved the dispatch that stopped before the restart")
	}
	if _, err := s.EffectRecorded("worker1@engineering", 1, resumed.ID, "commit", otherCommit, assignmentsAt); err == nil {
		t.Error("the old session recorded an effect in the resumption")
	}
	if _, _, err := s.ReportSubmitted(submission(resumed, ReportResult), assignmentsAt); err == nil {
		t.Error("the old session's result was recorded for the resumption")
	}
	var state string
	if err := s.db.QueryRow(`SELECT state FROM dispatches WHERE id = ?`, resumed.ID).Scan(&state); err != nil || state != DispatchCreated {
		t.Fatalf("the resumption is %s (%v), want it untouched by evidence from before the restart", state, err)
	}

	// The new session's own evidence goes through the dispatch's states, and
	// its result is tied to the resumption.
	for _, step := range [][2]string{{DispatchCreated, DispatchNudged}} {
		if err := s.ChangeDispatch(resumed.ID, 2, step[0], step[1], "", assignmentsAt); err != nil {
			t.Fatal(err)
		}
	}
	messages, err := s.Inbox("worker1@engineering", resumed.ID, assignmentsAt)
	if err != nil || len(messages) != 1 {
		t.Fatalf("the inbox holds %+v (%v)", messages, err)
	}
	if err := s.WorkFetchedDispatch(resumed.ID, "worker1@engineering", 2, assignmentsAt); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EffectRecorded("worker1@engineering", 2, resumed.ID, "push", "origin/asmai/job-1/1", assignmentsAt); err != nil {
		t.Fatal(err)
	}
	sub := submission(resumed, ReportResult)
	sub.Generation = 2
	r, told, err := s.ReportSubmitted(sub, assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	if r.Dispatch != resumed.ID || len(r.Effects) != 1 || r.Effects[0].Dispatch != resumed.ID || told.Agent != "leader@engineering" {
		t.Errorf("the result is %+v, told as %+v, want it tied to the resumption %d", r, told, resumed.ID)
	}
	if got, err := s.WorkerDispatch(a.ID); err == nil && got.Dispatch.ID == resumed.ID && got.Report != ReportResult {
		t.Errorf("the resumption ended in %q, want a result", got.Report)
	}
	// The dispatch that stopped has no report, and nothing was taken for it.
	var reports int
	if err := s.db.QueryRow(`SELECT count(*) FROM reports WHERE dispatch = ?`, stopped.ID).Scan(&reports); err != nil || reports != 0 {
		t.Errorf("%d reports are tied to the dispatch that stopped before the restart (%v)", reports, err)
	}
}

func TestAnAssignmentThatNeedsReconciliationIsHeldWithItsWorkerAndItsOwnerIsTold(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	job := jobWithBranch(t, s)
	a, d := newAssignment(t, s, job)
	startWorker(t, s, a.ID, "")
	working(t, s, d)

	if _, err := s.AssignmentNeedsReconciliation(a.ID, d.ID, "", "body", assignmentsAt); err == nil {
		t.Error("an assignment was held with no reason")
	}
	told, err := s.AssignmentNeedsReconciliation(a.ID, d.ID, "its turn was cut off", "Reconcile it.", assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	if told.Agent != "leader@engineering" || told.State != DispatchCreated {
		t.Errorf("the owner's dispatch is %+v, want a created one for the owning leader", told)
	}
	if got, _ := s.Assignment(a.ID); got.State != AssignmentNeedsReconciliation {
		t.Errorf("the assignment is %s", got.State)
	}
	var state string
	if err := s.db.QueryRow(`SELECT state FROM dispatches WHERE id = ?`, d.ID).Scan(&state); err != nil || state != DispatchUnknown {
		t.Errorf("the cut off dispatch is %s (%v), want unknown", state, err)
	}
	pending, err := s.PendingLeaders()
	if err != nil || !slices.Equal(pending, []string{"leader@engineering"}) {
		t.Errorf("the agents with a message waiting are %v (%v), want only the owner", pending, err)
	}
	messages, err := s.Inbox("leader@engineering", told.ID, assignmentsAt)
	if err != nil || len(messages) != 1 || messages[0].Kind != MessageReconciliation || messages[0].Body != "Reconcile it." || messages[0].AssignmentData == nil || messages[0].AssignmentData.State != AssignmentNeedsReconciliation {
		t.Fatalf("the owner's inbox holds %+v (%v)", messages, err)
	}

	// It is held: its worker's number and slot stay, nothing else can be
	// done with it, and the worker is not started for it again.
	if worker, slot, err := s.Allocate("engineering"); err != nil || worker != 2 || slot != 2 {
		t.Errorf("the next worker is %d in slot %d (%v), want it to keep worker 1 and its slot", worker, slot, err)
	}
	if live, err := s.LiveAssignmentOf("worker1@engineering"); err != nil || live.ID != a.ID {
		t.Errorf("the worker's live assignment is %+v (%v)", live, err)
	}
	if _, _, err := s.CancellationRequested(a.ID, "leader@engineering", []string{"x"}, assignmentsAt); err == nil {
		t.Error("an assignment that needs reconciliation was cancelled")
	}
	if _, err := s.AssignmentNeedsReconciliation(a.ID, d.ID, "again", "body", assignmentsAt); err == nil {
		t.Error("an assignment was held twice")
	}
	if _, err := s.AssignmentResumed(a.ID, d.ID, nativeID, "body", assignmentsAt); err == nil {
		t.Error("an assignment that needs reconciliation was resumed")
	}
}

// A resumption the worker never fetched is not waiting for a worker whose
// assignment then needs reconciliation.
func TestAResumptionThatCouldNotStartLeavesNoMessageWaitingForItsWorker(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	a, d := stoppedAssignment(t, s)
	resumed, err := s.AssignmentResumed(a.ID, d.ID, nativeID, "Continue.", assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingLeaders()
	if err != nil || !slices.Equal(pending, []string{"worker1@engineering"}) {
		t.Fatalf("the agents with a message waiting are %v (%v), want the worker for its resumption", pending, err)
	}
	if _, err := s.AssignmentNeedsReconciliation(a.ID, resumed.ID, "it could not be resumed", "body", assignmentsAt); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := s.db.QueryRow(`SELECT state FROM dispatches WHERE id = ?`, resumed.ID).Scan(&state); err != nil || state != DispatchUnknown {
		t.Errorf("the resumption that could not start is %s (%v), want unknown", state, err)
	}
	pending, err = s.PendingLeaders()
	if err != nil || !slices.Equal(pending, []string{"leader@engineering"}) {
		t.Errorf("the agents with a message waiting are %v (%v), want only the owner told", pending, err)
	}
}

func TestEachLeaderWithOpenWorkInAJobIsToldOnceToLoadItsBrief(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	job := jobWithBranch(t, s)
	// An open job is open work for Coordination alone: no handoff, no
	// assignment, nothing in an inbox.
	work, err := s.OpenLeaderWork()
	if err != nil || len(work) != 1 || work[0] != (LeaderWork{Leader: "leader@coordination", Job: job}) {
		t.Fatalf("the open work is %+v (%v), want Coordination's in job %d", work, err, job)
	}
	h := Handoff{Job: job, Sender: "leader@coordination", Receiver: "leader@engineering", Outcome: "o", Decisions: "none", Evidence: "none", Constraints: "none", Permissions: "none", Criteria: []string{"c"}}
	if _, _, err := s.SendHandoff(h, assignmentsAt); err != nil {
		t.Fatal(err)
	}
	newAssignment(t, s, job)
	work, err = s.OpenLeaderWork()
	want := []LeaderWork{{"leader@coordination", job}, {"leader@engineering", job}}
	if err != nil || !slices.Equal(work, want) {
		t.Fatalf("the open work is %+v (%v), want %+v", work, err, want)
	}

	d, made, err := s.LeaderRestored("leader@engineering", job, "Load the brief.", assignmentsAt)
	if err != nil || !made || d.Agent != "leader@engineering" || d.State != DispatchCreated {
		t.Fatalf("telling the leader: %+v, %v, %v", d, made, err)
	}
	// Told again while it has not yet been nudged, nothing more is made.
	if again, made, err := s.LeaderRestored("leader@engineering", job, "Load the brief.", assignmentsAt); err != nil || made || again.ID != 0 {
		t.Errorf("telling the leader again made %+v, %v (%v)", again, made, err)
	}
	messages, err := s.Inbox("leader@engineering", d.ID, assignmentsAt)
	if err != nil || len(messages) != 1 || messages[0].Kind != MessageRestoration || messages[0].Job != job || messages[0].Sender != Daemon || messages[0].Handoff != 0 || messages[0].Assignment != 0 {
		t.Fatalf("the restored leader's inbox holds %+v (%v)", messages, err)
	}
	// Once fetched, the next restart tells it again.
	if _, made, err := s.LeaderRestored("leader@engineering", job, "Load the brief.", assignmentsAt); err != nil || !made {
		t.Errorf("telling the leader after it fetched: %v, %v", made, err)
	}
}

func TestPendingRestorationTracksNewOpenJobs(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	job1 := jobWithBranch(t, s)
	d, made, err := s.LeaderRestored("leader@engineering", job1, "Load job 1.", assignmentsAt)
	if err != nil || !made {
		t.Fatalf("telling the leader: %+v, %v, %v", d, made, err)
	}
	if err := s.ChangeDispatch(d.ID, 0, DispatchCreated, DispatchUnknown, "", assignmentsAt); err != nil {
		t.Fatal(err)
	}
	result, err := s.db.Exec(`INSERT INTO jobs(repository, state, reading) VALUES('fixture', ?, 'Add another greeting')`, JobOpen)
	if err != nil {
		t.Fatal(err)
	}
	job2, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	body := "Load the briefs of jobs 1 and 2."
	if _, made, err := s.LeaderRestored("leader@engineering", job2, body, assignmentsAt); err != nil || made {
		t.Fatalf("refreshing the pending restoration: made %v (%v)", made, err)
	}
	messages, err := s.Inbox("leader@engineering", d.ID, assignmentsAt)
	if err != nil || len(messages) != 1 || messages[0].Job != job2 || messages[0].Body != body {
		t.Fatalf("the pending restoration is %+v (%v), want the current jobs and body", messages, err)
	}
}

func TestAnEndedJobIsNoOpenWork(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	job := jobWithBranch(t, s)
	newAssignment(t, s, job)
	if _, err := s.db.Exec(`UPDATE jobs SET state = 'delivered' WHERE number = ?`, job); err != nil {
		t.Fatal(err)
	}
	work, err := s.OpenLeaderWork()
	if err != nil || len(work) != 0 {
		t.Errorf("an ended job has open work %+v (%v)", work, err)
	}
}

// A store at schema 8, before a dispatch could resume another and the
// provider's sessions were recorded, is migrated with its dispatches.
func TestOpenMigratesAStoreThatRecordsNoNativeSessions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	db, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	statements := append([]string{}, migrations[:8]...)
	statements = append(statements,
		`INSERT INTO jobs(repository, state) VALUES('fixture', 'open')`,
		`INSERT INTO handoffs(job,sender,receiver,outcome,decisions,evidence,constraints,permissions,criteria,state,created_at) VALUES(1,'leader@coordination','leader@engineering','outcome','d','e','c','p','[]','pending','2026-10-08T09:00:00Z')`,
		`INSERT INTO messages(handoff,job,sender,recipient,kind,body) VALUES(1,1,'leader@coordination','leader@engineering','handoff','outcome')`,
		`INSERT INTO dispatches(message,agent,generation,state,updated_at) VALUES(1,'leader@engineering',0,'created','2026-10-08T09:01:00Z')`,
		`PRAGMA user_version = 8`)
	for _, stmt := range statements {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	db.Close()

	s := open(t, path)
	messages, err := s.Inbox("leader@engineering", 0, assignmentsAt)
	if err != nil || len(messages) != 1 || messages[0].Resumes != 0 || messages[0].Dispatch != 1 {
		t.Fatalf("the migrated inbox holds %+v (%v)", messages, err)
	}
	if _, err := s.SessionStarted(SessionStart{Agent: "leader@engineering", Role: "engineering", Provider: "claude-code", Version: "2.1.292", Model: "opus", Executable: "/p", Args: []string{}, Dir: "/a", PID: 4, At: time.Now()}); err != nil {
		t.Fatalf("the migrated store cannot record a session: %v", err)
	}
	if err := s.SessionIdentified("leader@engineering", 1, nativeID, transcript, time.Now()); err != nil {
		t.Errorf("the migrated store cannot identify a session: %v", err)
	}
}
