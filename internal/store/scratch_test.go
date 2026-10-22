// SPDX-License-Identifier: Apache-2.0

package store

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const researcher = "leader@research"

// researchJob gives s a job with the research-only mandate and no repository,
// opened from the user's witnessed message, and returns it.
func researchJob(t *testing.T, s *Store) Job {
	t.Helper()
	if err := s.ConversationEntered("leader@coordination", "coordination", 1, "/dev/pts/3", assignmentsAt); err != nil {
		t.Fatal(err)
	}
	_, witness, err := s.ObservedWitnessed(Witnessed{Agent: "leader@coordination", Role: "coordination", Generation: 1, Terminal: "/dev/pts/3", Text: "find out how the greeting is spelled"}, "UserPromptSubmit", json.RawMessage(`{"prompt":"find out how the greeting is spelled"}`), assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	j, err := s.JobOpened(Job{Witness: witness, Reading: "Find out how the greeting is spelled", Mandate: MandateResearchOnly, Criteria: []string{"a cited answer"}}, false, assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

// scratchAssignment records an assignment of job, which has no repository,
// to Research's next free worker, in an empty scratch workspace.
func scratchAssignment(t *testing.T, s *Store, job int64) (Assignment, Dispatch) {
	t.Helper()
	id, err := s.NextAssignmentID()
	if err != nil {
		t.Fatal(err)
	}
	number, slot, err := s.Allocate("research")
	if err != nil {
		t.Fatal(err)
	}
	a, d, err := s.AssignmentCreated(NewAssignment{
		ID: id, Job: job, Owner: researcher, Worker: "worker" + string(rune('0'+number)) + "@research",
		Outcome: "Find the spelling", Criteria: []string{"it names its source"},
		Workspace: Workspace{Slot: slot, Path: "/ws/job-1/assignment-1/scratch", Tmp: "/ws/job-1/assignment-1/tmp"},
	}, assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	return a, d
}

// scratchResult submits a result of the assignment a, whose dispatch is d.
func scratchResult(t *testing.T, s *Store, a Assignment, d Dispatch) {
	t.Helper()
	working(t, s, d)
	sub := Submission{Agent: a.Worker, Generation: 1, Dispatch: d.ID, Kind: ReportResult, Input: ReportInput{
		Evidence: []string{"https://example.test/spec#greeting"}, Artifacts: []string{"findings.md"}, Gaps: []string{},
	}}
	if _, _, err := s.ReportSubmitted(sub, assignmentsAt); err != nil {
		t.Fatal(err)
	}
}

func TestAResearchOnlyJobMayTargetNoRepositoryAndAJobOfAnotherMandateMayNot(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	if err := s.ConversationEntered("leader@coordination", "coordination", 1, "/dev/pts/3", assignmentsAt); err != nil {
		t.Fatal(err)
	}
	_, witness, err := s.ObservedWitnessed(Witnessed{Agent: "leader@coordination", Role: "coordination", Generation: 1, Terminal: "/dev/pts/3", Text: "research it"}, "UserPromptSubmit", json.RawMessage(`{"prompt":"research it"}`), assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	j, err := s.JobOpened(Job{Witness: witness, Reading: "research", Mandate: MandateResearchOnly, Criteria: []string{"an answer"}}, false, assignmentsAt)
	if err != nil || j.Repository != "" || j.Mandate != MandateResearchOnly {
		t.Errorf("a research-only job with no repository = %+v, %v", j, err)
	}
	if again, err := s.Job(j.Number); err != nil || again.Repository != "" || again.State != JobOpen {
		t.Errorf("job %d is read back as %+v (%v), want an open job with no repository", j.Number, again, err)
	}
	for _, mandate := range []string{MandateTestedPR, MandatePlanningOnly} {
		if _, err := s.JobOpened(Job{Witness: witness, Reading: "build it", Mandate: mandate, Criteria: []string{"an outcome"}}, false, assignmentsAt); err == nil || !strings.Contains(err.Error(), "needs a registered repository") {
			t.Errorf("a %s job with no repository = %v, want it refused", mandate, err)
		}
	}
}

func TestAJobWithoutARepositoryGivesEachAssignmentAnEmptyScratchWorkspaceWithNoBranch(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	j := researchJob(t, s)

	for name, w := range map[string]Workspace{
		"a branch":         {Slot: 1, Path: "/ws/scratch", Tmp: "/ws/tmp", Branch: "asmai/job-1/1"},
		"a commit":         {Slot: 1, Path: "/ws/scratch", Tmp: "/ws/tmp", Base: tipCommit},
		"a path":           {Slot: 1, Tmp: "/ws/tmp"},
		"a temporary path": {Slot: 1, Path: "/ws/scratch"},
	} {
		_, _, err := s.AssignmentCreated(NewAssignment{ID: 1, Job: j.Number, Owner: researcher, Worker: "worker1@research", Outcome: "x", Criteria: []string{"y"}, Workspace: w}, assignmentsAt)
		if err == nil {
			t.Errorf("a scratch workspace with %s was recorded", name)
		}
	}
	if _, _, err := s.AssignmentCreated(NewAssignment{ID: 1, Job: j.Number, Owner: researcher, Worker: "worker1@research", Outcome: "x", Criteria: []string{"y"}, ReadOnly: true, Workspace: Workspace{Slot: 1, Path: "/ws/scratch", Tmp: "/ws/tmp"}}, assignmentsAt); err == nil {
		t.Error("a read-only assignment of a job with no repository was recorded: there is no commit to examine")
	}

	a, d := scratchAssignment(t, s, j.Number)
	if !a.Scratch || a.JobBranch != "" || a.Workspace.Branch != "" || a.Workspace.Base != "" || a.ReadOnly || a.Commit != "" || d.Agent != a.Worker {
		t.Errorf("the assignment is %+v, dispatch %+v, want a scratch one with no job branch, no assignment branch and no commit", a, d)
	}
	got, err := s.Assignment(a.ID)
	if err != nil || !got.Scratch || got.Workspace.Path != "/ws/job-1/assignment-1/scratch" || got.JobBranch != "" {
		t.Errorf("the assignment is read back as %+v (%v)", got, err)
	}
	if live, err := s.LiveAssignmentOf(a.Worker); err != nil || live.ID != a.ID || !live.Scratch {
		t.Errorf("the worker's live assignment is %+v (%v)", live, err)
	}
	if all, err := s.AssignmentsForJob(j.Number); err != nil || len(all) != 1 {
		t.Errorf("the job's assignments are %+v (%v)", all, err)
	}
	if _, err := s.JobBranch(j.Number); err == nil {
		t.Error("a job with no repository has a job branch")
	}
}

func TestAScratchResultLinksItsEvidenceAndNamesNoCommitNoPushAndNoPRSection(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	j := researchJob(t, s)
	a, d := scratchAssignment(t, s, j.Number)
	working(t, s, d)
	result := func(mutate func(*Submission)) error {
		sub := Submission{Agent: a.Worker, Generation: 1, Dispatch: d.ID, Kind: ReportResult, Input: ReportInput{Evidence: []string{"https://example.test/spec#greeting"}, Artifacts: []string{"findings.md"}, Gaps: []string{}}}
		mutate(&sub)
		_, _, err := s.ReportSubmitted(sub, assignmentsAt)
		return err
	}
	for name, mutate := range map[string]func(*Submission){
		"evidence":          func(s *Submission) { s.Input.Evidence = nil },
		"nonblank evidence": func(s *Submission) { s.Input.Evidence = []string{" "} },
		"findings":          func(s *Submission) { s.Input.Validation = &ValidationInput{} },
	} {
		if err := result(mutate); err == nil {
			t.Errorf("a scratch result without %s was recorded", name)
		}
	}
	if got, _ := s.Assignment(a.ID); got.State != AssignmentActive {
		t.Fatalf("a refused result moved the assignment to %s", got.State)
	}
	if err := result(func(*Submission) {}); err != nil {
		t.Fatalf("a scratch result with evidence and artifacts, and no commit, push or PR section, returned %v", err)
	}
	if got, _ := s.Assignment(a.ID); got.State != AssignmentSubmitted {
		t.Errorf("the assignment is %s, want submitted", got.State)
	}
	if latest, err := s.LatestResult(a.ID); err != nil || latest.Commit != "" || len(latest.Artifacts) != 1 {
		t.Errorf("the latest result is %+v (%v)", latest, err)
	}
	messages, err := s.Inbox(researcher, 0, assignmentsAt)
	if err != nil || len(messages) != 1 || messages[0].Kind != ReportResult || messages[0].AssignmentData == nil || !messages[0].AssignmentData.Scratch {
		t.Fatalf("the leader's inbox holds %+v (%v)", messages, err)
	}
}

func TestAcceptingAScratchResultEndsTheAssignmentAndMovesNothing(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	j := researchJob(t, s)
	a, d := scratchAssignment(t, s, j.Number)
	scratchResult(t, s, a, d)

	if _, _, err := s.ScratchResultAccepted(Acceptance{Assignment: a.ID, Leader: "leader@planning", Reasons: []string{"x"}}, "", assignmentsAt); err == nil {
		t.Error("an acceptance by a leader that does not own the assignment was recorded")
	}
	if _, _, err := s.ScratchResultAccepted(Acceptance{Assignment: a.ID, Leader: researcher}, "", assignmentsAt); err == nil {
		t.Error("an acceptance without reasons was recorded")
	}
	decision, dispatch, err := s.ScratchResultAccepted(Acceptance{Assignment: a.ID, Leader: researcher, Reasons: []string{"the spelling is cited"}}, "", assignmentsAt)
	if err != nil || dispatch != nil || decision.Kind != DecisionAccepted || decision.Commit != "" || decision.Report == 0 {
		t.Fatalf("accepting a result returned %+v, %v, %v", decision, dispatch, err)
	}
	if got, _ := s.Assignment(a.ID); got.State != AssignmentAccepted {
		t.Errorf("the assignment is %s, want accepted", got.State)
	}
	if got, _ := s.Job(j.Number); got.State != JobOpen {
		t.Errorf("accepting one assignment's result ended job %d, which is %s: only the leader's acceptance of the outcome ends a job", j.Number, got.State)
	}
	if worker, slot, err := s.Allocate("research"); err != nil || worker != 1 || slot != 1 {
		t.Errorf("after the acceptance the next worker and slot are %d and %d (%v), want them free again", worker, slot, err)
	}
	if strings.Contains(strings.Join(kinds(t, s), ","), KindJobBranchMoved) {
		t.Error("accepting a scratch result moved a job branch")
	}
}

func TestTheOwningLeadersAcceptanceOfTheOutcomeEndsAJobWithoutARepositoryAndTellsCoordinationWhereTheResultsAre(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	j := researchJob(t, s)
	a, d := scratchAssignment(t, s, j.Number)
	scratchResult(t, s, a, d)
	other, otherDispatch := scratchAssignment(t, s, j.Number)
	working(t, s, otherDispatch)

	// Another assignment is still live, so the outcome is not yet in.
	if _, _, err := s.ScratchResultAccepted(Acceptance{Assignment: a.ID, Leader: researcher, Reasons: []string{"x"}, EndsJob: true}, "Job 1 ended", assignmentsAt); err == nil || !strings.Contains(err.Error(), "assignment") {
		t.Fatalf("accepting the outcome while assignment %d is live returned %v, want it refused", other.ID, err)
	}
	if got, _ := s.Assignment(a.ID); got.State != AssignmentSubmitted {
		t.Fatalf("a refused acceptance moved the assignment to %s", got.State)
	}
	if _, _, err := s.CancellationRequested(other.ID, researcher, []string{"not needed"}, assignmentsAt); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AssignmentCancelled(other.ID, Cancellation{}, assignmentsAt); err != nil {
		t.Fatal(err)
	}

	decision, dispatch, err := s.ScratchResultAccepted(Acceptance{Assignment: a.ID, Leader: researcher, Reasons: []string{"the spelling is cited"}, EndsJob: true}, "Job 1 ended: the results are in /ws/job-1", assignmentsAt)
	if err != nil || decision.Kind != DecisionAccepted || dispatch == nil || dispatch.Agent != "leader@coordination" {
		t.Fatalf("accepting the outcome returned %+v, %+v, %v, want a dispatch to Coordination's leader", decision, dispatch, err)
	}
	if got, _ := s.Job(j.Number); got.State != JobEnded {
		t.Errorf("the job is %s, want ended", got.State)
	}
	messages, err := s.Inbox("leader@coordination", 0, assignmentsAt)
	if err != nil || len(messages) != 1 || messages[0].Kind != MessageDelivery || messages[0].Sender != Daemon || messages[0].Job != j.Number || messages[0].Body != "Job 1 ended: the results are in /ws/job-1" {
		t.Errorf("Coordination's inbox holds %+v (%v), want the message that says where the results are", messages, err)
	}
	if !strings.Contains(strings.Join(kinds(t, s), ","), KindJobEnded) {
		t.Errorf("the journal holds %v, want the job's end", kinds(t, s))
	}
	if _, _, err := s.AssignmentCreated(NewAssignment{ID: 9, Job: j.Number, Owner: researcher, Worker: "worker1@research", Outcome: "x", Criteria: []string{"y"}, Workspace: Workspace{Slot: 1, Path: "/p", Tmp: "/t"}}, assignmentsAt); err == nil {
		t.Error("an ended job took a new assignment")
	}
}

func TestOnlyAJobWithoutARepositoryEndsOnItsLeadersAcceptance(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	job := jobWithBranch(t, s)
	a, d := newAssignment(t, s, job)
	working(t, s, d)
	if _, err := s.EffectRecorded(a.Worker, 1, d.ID, "push", "origin/asmai/job-1/1@"+otherCommit, assignmentsAt); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ReportSubmitted(submission(d, ReportResult), assignmentsAt); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ScratchResultAccepted(Acceptance{Assignment: a.ID, Leader: "leader@engineering", Reasons: []string{"x"}, EndsJob: true}, "ended", assignmentsAt); err == nil {
		t.Error("a repository job's result was accepted as a scratch one: a repository job ends in its delivery")
	}
	if _, err := s.ResultAccepted(Acceptance{Assignment: a.ID, Leader: "leader@engineering", Reasons: []string{"x"}, FromTip: tipCommit, EndsJob: true}, assignmentsAt); err == nil {
		t.Error("a repository job's acceptance ended it")
	}
}

func TestAScratchAssignmentIsCancelledWithNothingToPush(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	j := researchJob(t, s)
	a, d := scratchAssignment(t, s, j.Number)
	working(t, s, d)
	if _, _, err := s.CancellationRequested(a.ID, researcher, []string{"no longer wanted"}, assignmentsAt); err != nil {
		t.Fatal(err)
	}
	got, err := s.AssignmentCancelled(a.ID, Cancellation{}, assignmentsAt)
	if err != nil || got.State != AssignmentCancelled {
		t.Fatalf("cancelling a scratch assignment with nothing pushed returned %+v, %v", got, err)
	}
}

func TestAJobWithoutARepositoryHasItsScratchWorkspacesCleanedOnlyOnceItHasEnded(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	j := researchJob(t, s)
	a, d := scratchAssignment(t, s, j.Number)
	scratchResult(t, s, a, d)

	if err := s.ScratchCleaned(j.Number, []string{"/ws/job-1"}, assignmentsAt); err == nil || !strings.Contains(err.Error(), "open") {
		t.Errorf("cleaning the scratch workspaces of an open job returned %v, want it refused", err)
	}
	if cleaned, err := s.CleanedScratch(j.Number); err != nil || cleaned {
		t.Errorf("an open job's scratch workspaces are cleaned = %v (%v)", cleaned, err)
	}
	if _, _, err := s.ScratchResultAccepted(Acceptance{Assignment: a.ID, Leader: researcher, Reasons: []string{"x"}, EndsJob: true}, "ended", assignmentsAt); err != nil {
		t.Fatal(err)
	}
	if err := s.ScratchCleaned(j.Number, []string{"/ws/job-1"}, assignmentsAt); err != nil {
		t.Fatalf("cleaning the scratch workspaces of an ended job returned %v", err)
	}
	if cleaned, err := s.CleanedScratch(j.Number); err != nil || !cleaned {
		t.Errorf("an ended job's scratch workspaces are cleaned = %v (%v)", cleaned, err)
	}
	if !strings.Contains(strings.Join(kinds(t, s), ","), KindScratchCleaned) {
		t.Errorf("the journal holds %v, want the cleaning", kinds(t, s))
	}

	repoJob := jobWithBranch(t, s)
	if err := s.ScratchCleaned(repoJob, nil, assignmentsAt); err == nil || !strings.Contains(err.Error(), "repository") {
		t.Errorf("cleaning a repository job's scratch workspaces returned %v, want it refused: it has none", err)
	}
	if err := s.ScratchCleaned(99, nil, time.Now()); err == nil {
		t.Error("cleaning a job that does not exist succeeded")
	}
}
