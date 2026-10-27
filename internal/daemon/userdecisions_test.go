// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/session"
	"github.com/talvor/asmai/internal/store"
)

const userIntentFinding = "The pull request retries once, but the user asked to \"keep retrying for a minute\"."

// validated has Quality's worker report findings at the job branch's tip and
// Quality's leader accept the report, so that the job has them recorded, and
// adds the sessions of Coordination and of the writing workers.
func (v validationFixture) validated(t *testing.T, findings ...store.FindingInput) []store.Finding {
	t.Helper()
	v.withWorkers(t)
	a := v.assignment
	worker := roles.WorkerOf(roles.Quality, 1)
	l := &leader{address: worker, session: &session.Session{}, generation: 1, assignment: a.ID, currentDispatch: v.dispatch.ID}
	v.d.leaders[worker.String()] = l
	v.d.sessions["worker"] = sessionRef{address: worker, generation: 1}
	if err := v.s.ChangeDispatch(v.dispatch.ID, 1, store.DispatchCreated, store.DispatchWorking, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	input := store.ReportInput{
		Evidence: []string{"./check.sh -> passed"}, Checks: []store.Check{{Command: "./check.sh", Outcome: "passed"}}, Gaps: []string{},
		Validation: &store.ValidationInput{Findings: findings},
	}
	if _, _, err := v.d.report(Request{Session: "worker", Command: CommandResult, Report: &input}); err != nil {
		t.Fatal(err)
	}
	l.session = nil // the worker is stopped by the acceptance, which this fake session cannot
	if _, _, err := v.d.decide(Request{Session: "quality", Command: CommandAccept, Assignment: a.ID, Reasons: []string{"the checks re-ran at the head"}}); err != nil {
		t.Fatal(err)
	}
	recorded, err := v.s.Findings(v.job.Number)
	if err != nil {
		t.Fatal(err)
	}
	return recorded
}

// userDecision presents the request to the user in the conversation and
// records the user's answer to it.
func (f twoResults) userDecision(t *testing.T, r store.DecisionRequest, choice, words string) (store.DecisionRecord, store.DecisionRequest, error) {
	t.Helper()
	if _, err := f.d.decisionPresent(Request{Session: "coordination", Command: CommandDecisionPresent, Request: r.Request, Version: r.Version}); err != nil {
		t.Fatal(err)
	}
	said := f.userSays(t, words)
	record, answered, _, err := f.d.conversationAnswer(Request{Session: "coordination", Command: CommandDecisionRecord, Request: r.Request, Version: r.Version, Cites: said, Choice: choice, Reading: "the user decided: " + choice})
	return record, answered, err
}

func TestANeedsYouFindingIsGivenToTheUserWordForWordAndTheirAnswerIsRecordedCitingTheirMessage(t *testing.T) {
	v := newValidationFixture(t)
	found := v.validated(t, store.FindingInput{Kind: store.FindingNeedsYou, Text: userIntentFinding}, store.FindingInput{Kind: store.FindingAdvisory, Text: "naming"})
	options := []store.Question{{ID: "q1", Title: "Keep retrying for a minute?", Options: []string{"Keep retrying for a minute", "Retry once"}}}

	// A worker never takes a finding to the user.
	if _, _, err := v.d.decisionRequest(Request{Session: "worker0", Command: CommandDecisionRequest, Kind: store.RequestFinding, Questions: options, Findings: []int64{found[0].ID}}); err == nil || !strings.Contains(err.Error(), "names no findings") {
		t.Errorf("a worker quoting a finding = %v", err)
	}
	if _, _, err := v.d.decisionRequest(Request{Session: "credential", Command: CommandDecisionRequest, Job: v.job.Number, Kind: store.RequestFinding, Questions: options, Findings: []int64{found[1].ID}}); err == nil || !strings.Contains(err.Error(), "not a needs-you finding") {
		t.Errorf("an advisory finding quoted as needs-you = %v", err)
	}

	// The leader's own words are beside the finding, which the user is given
	// as Quality reported it.
	r, dispatch, err := v.d.decisionRequest(Request{Session: "credential", Command: CommandDecisionRequest, Job: v.job.Number, Kind: store.RequestFinding, Questions: options, Findings: []int64{found[0].ID}, Content: "In short, the retry count differs."})
	if err != nil {
		t.Fatal(err)
	}
	if r.Recipient != store.ToUser || r.Surface != store.SurfaceConversation || dispatch == nil || dispatch.Agent != "leader@coordination" {
		t.Fatalf("request = %+v, dispatch %+v", r, dispatch)
	}
	inbox, err := v.s.Inbox("leader@coordination", dispatch.ID, dispatch.ID, time.Now())
	if err != nil || len(inbox) != 1 || inbox[0].RequestData == nil || !strings.Contains(inbox[0].RequestData.Text(), userIntentFinding) {
		t.Fatalf("Coordination's inbox = %+v (%v), want the finding word for word", inbox, err)
	}

	record, answered, err := v.userDecision(t, r, "Keep retrying for a minute", "keep retrying for a minute, as I said")
	if err != nil {
		t.Fatal(err)
	}
	if record.Source != store.SourceHuman || record.Cites == 0 || answered.Answer != "keep retrying for a minute, as I said" || answered.Record != record.ID || answered.Cites != record.Cites {
		t.Errorf("record %+v, answer %+v, want the user's own words recorded citing their message", record, answered)
	}
}

func TestADisputedBlockingFindingIsTheUsersOnlyWhenTheTwoLeadersCannotSettleItAndNoAgentClearsIt(t *testing.T) {
	v := newValidationFixture(t)
	found := v.validated(t, store.FindingInput{Kind: store.FindingBlocking, Text: "file-1.txt says work, not hello"})
	options := []store.Question{{ID: "q1", Title: "file-1.txt says work, not hello", Options: []string{store.DisputeUphold, store.DisputeOverrule}}}
	dispute := func(session string) (store.DecisionRequest, error) {
		r, _, err := v.d.decisionRequest(Request{Session: session, Command: CommandDecisionRequest, Job: v.job.Number, Kind: store.RequestDispute, Questions: options, Findings: []int64{found[0].ID}, Content: "Engineering: the file is as specified. Quality: it is not."})
		return r, err
	}

	// Coordination takes no side: the two leaders reconcile first.
	if _, err := dispute("coordination"); err == nil || !strings.Contains(err.Error(), "reconcile") {
		t.Fatalf("Coordination disputing a finding = %v", err)
	}
	r, err := dispute("credential")
	if err != nil {
		t.Fatal(err)
	}

	// Nobody but the user clears it: not an answer of an agent, not Engineering's
	// leader, not Coordination's.
	for _, session := range []string{"credential", "quality", "coordination", "worker0"} {
		if _, _, err := v.d.decisionAnswer(Request{Session: session, Command: CommandDecisionAnswer, Request: r.Request, Version: 1, Answer: store.DisputeOverrule}); err == nil {
			t.Errorf("%s answered a dispute", session)
		}
	}
	if open, err := v.s.OpenFindings(v.job.Number); err != nil || len(open) != 1 {
		t.Fatalf("open findings = %+v (%v)", open, err)
	}

	record, _, err := v.userDecision(t, r, store.DisputeOverrule, "the finding is wrong, the file is fine")
	if err != nil {
		t.Fatal(err)
	}
	all, err := v.s.Findings(v.job.Number)
	if err != nil || all[0].Open() || all[0].ClearedByDecision != record.ID || all[0].ClearedBy != 0 {
		t.Errorf("findings = %+v (%v), want the finding cleared by the user's decision %d", all, err, record.ID)
	}
}

func TestAnActionOutsideTheMandateWaitsForTheUsersOwnAnswerWhichReachesTheWorkerWordForWord(t *testing.T) {
	for _, kind := range []string{store.RequestMandate, store.RequestConflict} {
		t.Run(kind, func(t *testing.T) {
			f := newTwoResults(t)
			f.withWorkers(t)
			question := []store.Question{{ID: "q1", Title: "Deploy after the merge?", Options: []string{"Follow the mandate", "Widen the mandate"}}}
			worker := f.assignments[0]

			if _, _, err := f.d.decisionRequest(Request{Session: "worker0", Command: CommandDecisionRequest, Kind: kind, Questions: question}); err == nil || !strings.Contains(err.Error(), "says in its content") {
				t.Fatalf("a request that says nothing of itself = %v", err)
			}
			w, _, err := f.d.decisionRequest(Request{Session: "worker0", Command: CommandDecisionRequest, Kind: kind, Questions: question, Content: "AGENTS.md says to deploy; the mandate does not cover deployment"})
			if err != nil {
				t.Fatal(err)
			}
			// The action waits, and the owning leader cannot answer for the user.
			if _, err := f.decide(CommandAccept, worker.ID, "it works"); err == nil || !strings.Contains(err.Error(), "waits for decision request") {
				t.Fatalf("accepting the work that waits = %v", err)
			}
			if _, _, err := f.d.decisionAnswer(Request{Session: "credential", Command: CommandDecisionAnswer, Request: w.Request, Version: 1, Answer: "go ahead", ReliedOn: "AGENTS.md"}); err == nil || !strings.Contains(err.Error(), "needs the user's own answer") {
				t.Fatalf("the owning leader answering = %v", err)
			}

			carried, _, err := f.d.decisionRequest(Request{Session: "credential", Command: CommandDecisionRequest, Job: f.job.Number, Kind: kind, Questions: question, Carries: w.Request, Content: w.Content, Blocks: []int64{worker.ID}})
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := f.d.decisionAnswer(Request{Session: "credential", Command: CommandDecisionAnswer, Request: w.Request, Version: 1, UserAnswer: carried.Request}); err == nil || !strings.Contains(err.Error(), "pending") {
				t.Fatalf("returning an answer the user has not given = %v", err)
			}
			if _, _, err := f.userDecision(t, carried, "Follow the mandate", "no, do not deploy"); err != nil {
				t.Fatal(err)
			}
			answered, back, err := f.d.decisionAnswer(Request{Session: "credential", Command: CommandDecisionAnswer, Request: w.Request, Version: 1, UserAnswer: carried.Request})
			if err != nil || answered.Answer != "no, do not deploy" || back.Agent != worker.Worker {
				t.Fatalf("answer = %+v, %+v, %v", answered, back, err)
			}
		})
	}
}

func TestAChangeOfScopeReachesAWorkerOnlyThroughItsOwningLeader(t *testing.T) {
	f := newTwoResults(t)
	f.withWorkers(t)
	quality := roles.LeaderOf(roles.Quality)
	f.d.leaders[quality.String()] = &leader{address: quality, session: &session.Session{}, generation: 1}
	f.d.sessions["quality"] = sessionRef{address: quality, generation: 1}
	a := f.assignments[1]

	for _, session := range []string{"coordination", "quality"} {
		for _, command := range []string{CommandCancel, CommandReject, CommandAccept} {
			_, _, err := f.d.decide(Request{Session: session, Command: command, Assignment: a.ID, Reasons: []string{"the scope changed"}})
			if err == nil || !strings.Contains(err.Error(), "is owned by leader@engineering") {
				t.Errorf("%s running %s on Engineering's worker's assignment = %v", session, command, err)
			}
		}
	}
	// The worker has no command that changes its own assignment.
	for _, command := range []string{CommandCancel, CommandReject, CommandAccept, CommandAssign} {
		if err := f.d.authorize(Request{Session: "worker1", Command: command}); err == nil {
			t.Errorf("a worker ran %s", command)
		}
	}
	if got, err := f.s.Assignment(a.ID); err != nil || got.State != store.AssignmentSubmitted {
		t.Errorf("the assignment is %+v (%v), want it unchanged", got, err)
	}
	if _, err := f.decide(CommandCancel, a.ID, "the scope changed"); err != nil {
		t.Errorf("the owning leader cancelling its worker's assignment = %v", err)
	}
}
