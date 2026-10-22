// SPDX-License-Identifier: Apache-2.0

package store

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

const needsYouText = "The change drops the retry the user asked for in \"keep retrying for a minute\"; the pull request retries once."

// withFindings has Quality's leader accept a validation of the conversation's
// job that reports findings, and returns the recorded findings.
func (c *conversation) withFindings(findings ...FindingInput) []Finding {
	c.t.Helper()
	a, d := newValidation(c.t, c.s, c.job)
	if _, _, err := c.s.ReportSubmitted(validationOf(a, d, tipCommit, ValidationInput{Findings: findings}), assignmentsAt); err != nil {
		c.t.Fatal(err)
	}
	if _, _, err := c.s.ValidationAccepted(Acceptance{Assignment: a.ID, Leader: qualityOwner, Reasons: []string{"the checks re-ran at the head"}}, owner, assignmentsAt); err != nil {
		c.t.Fatal(err)
	}
	recorded, err := c.s.Findings(c.job)
	if err != nil {
		c.t.Fatal(err)
	}
	return recorded
}

// ask makes a leader's decision request to the user, as the daemon does.
func (c *conversation) ask(requester string, kind string, findings []int64, options ...string) (DecisionRequest, error) {
	c.t.Helper()
	r, _, err := c.s.DecisionRequested(DecisionRequestInput{
		Job: c.job, Requester: requester, Recipient: ToUser, Notify: CoordinationLeader, Kind: kind, Findings: findings,
		Content:   "Engineering's and Quality's positions: " + kind,
		Questions: []Question{{ID: "q1", Title: "What do you decide?", Options: options}},
	}, assignmentsAt)
	return r, err
}

func (c *conversation) answerWith(r DecisionRequest, choice, words string) (DecisionRecord, DecisionRequest, *Dispatch, error) {
	c.t.Helper()
	shown := c.present(r)
	cites := c.say(CoordinationLeader, words)
	return c.s.ConversationAnswerRecorded(DecisionRecord{
		Job: c.job, Leader: CoordinationLeader, Choice: choice, Request: shown.Request, Version: shown.Version,
		Cites: cites, Reading: "the user decided: " + choice,
	}, 1, assignmentsAt.Add(time.Minute))
}

func TestANeedsYouFindingReachesTheUserWordForWordAndTheAnswerCitesTheirMessage(t *testing.T) {
	c := newConversation(t)
	found := c.withFindings(
		FindingInput{Kind: FindingNeedsYou, Text: needsYouText},
		FindingInput{Kind: FindingBlocking, Text: "greeting.txt does not say hello"},
		FindingInput{Kind: FindingAdvisory, Text: "check.sh has no comment"},
	)
	needs, blocking, advisory := found[0], found[1], found[2]

	// Only a needs-you finding goes to the user as one, and a worker never asks.
	for _, other := range []Finding{blocking, advisory} {
		if _, err := c.ask(owner, RequestFinding, []int64{other.ID}, "Keep my words", "Change my words"); err == nil || !strings.Contains(err.Error(), "not a needs-you finding") {
			t.Errorf("a %s finding went to the user as a needs-you finding: %v", other.Kind, err)
		}
	}
	if _, err := c.ask(owner, RequestFinding, nil, "Keep my words", "Change my words"); err == nil || !strings.Contains(err.Error(), "names the findings") {
		t.Errorf("a finding request that names no finding = %v", err)
	}
	if _, _, err := c.s.DecisionRequested(DecisionRequestInput{
		Job: c.job, Requester: "worker1@engineering", Recipient: owner, Kind: RequestFinding, Findings: []int64{needs.ID},
		Questions: []Question{{ID: "q1", Title: "x", Options: []string{"a", "b"}}},
	}, assignmentsAt); err == nil {
		t.Error("a worker quoted a finding to the user")
	}

	r, err := c.ask(owner, RequestFinding, []int64{needs.ID}, "Keep my words", "Change my words")
	if err != nil {
		t.Fatal(err)
	}
	// The user is given the finding as it was reported, whatever the leader's content says.
	if len(r.Quoted) != 1 || r.Quoted[0].Text != needsYouText || r.Quoted[0].ID != needs.ID || r.Surface != SurfaceConversation {
		t.Fatalf("request = %+v", r)
	}
	if want := fmt.Sprintf("Finding %d (needs-you):\n%s\n", needs.ID, needsYouText); !strings.Contains(r.Text(), want) {
		t.Errorf("the user is shown %q", r.Text())
	}

	record, answered, dispatch, err := c.answerWith(r, "Keep my words", "keep my words, retry for a minute")
	if err != nil {
		t.Fatal(err)
	}
	if record.Source != SourceHuman || record.Cites == 0 || answered.Answer != "keep my words, retry for a minute" || answered.Record != record.ID || dispatch == nil || dispatch.Agent != owner {
		t.Errorf("record %+v, answer %+v, dispatch %+v", record, answered, dispatch)
	}
	// Answering a needs-you finding clears nothing: it is not a blocking one.
	if open, _ := c.s.OpenFindings(c.job); len(open) != 1 || open[0].ID != blocking.ID {
		t.Errorf("open findings = %+v", open)
	}
}

func TestADisputedBlockingFindingGoesToTheUserOnlyFromTheTwoLeadersAndTheirDecisionClearsIt(t *testing.T) {
	c := newConversation(t)
	found := c.withFindings(
		FindingInput{Kind: FindingBlocking, Text: "greeting.txt does not say hello"},
		FindingInput{Kind: FindingBlocking, Text: "check.sh has no comment"},
	)
	disputed, other := found[0], found[1]
	options := []string{DisputeUphold, DisputeOverrule}

	for _, requester := range []string{CoordinationLeader, "leader@planning", "leader@research"} {
		if _, err := c.ask(requester, RequestDispute, []int64{disputed.ID}, options...); err == nil || !strings.Contains(err.Error(), "reconcile") {
			t.Errorf("%s disputed a finding: %v", requester, err)
		}
	}
	if _, err := c.ask(owner, RequestDispute, []int64{disputed.ID}, "Uphold", "Overrule"); err == nil || !strings.Contains(err.Error(), "two options") {
		t.Errorf("a dispute with its own options = %v", err)
	}
	if _, err := c.ask(owner, RequestDispute, []int64{disputed.ID + 100}, options...); err == nil || !strings.Contains(err.Error(), "not a finding of job") {
		t.Errorf("a dispute of no finding of the job = %v", err)
	}

	r, err := c.ask(owner, RequestDispute, []int64{disputed.ID}, options...)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Quoted) != 1 || r.Quoted[0].Text != disputed.Text {
		t.Fatalf("quoted = %+v", r.Quoted)
	}

	// The user's choice is one of the two options, whatever Coordination reads.
	shown := c.present(r)
	if _, _, _, err := c.s.ConversationAnswerRecorded(DecisionRecord{
		Job: c.job, Leader: CoordinationLeader, Choice: "ship it", Request: shown.Request, Version: 1,
		Cites: c.say(CoordinationLeader, "ship it"), Reading: "ship it",
	}, 1, assignmentsAt); err == nil || !strings.Contains(err.Error(), "one of its options") {
		t.Errorf("a choice that is neither option = %v", err)
	}
	if open, _ := c.s.OpenFindings(c.job); len(open) != 2 {
		t.Fatalf("open findings = %+v", open)
	}

	record, _, _, err := c.s.ConversationAnswerRecorded(DecisionRecord{
		Job: c.job, Leader: CoordinationLeader, Choice: DisputeOverrule, Request: shown.Request, Version: 1,
		Cites: c.say(CoordinationLeader, "the greeting is fine as it is"), Reading: "the user overruled the finding",
	}, 1, assignmentsAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	all, err := c.s.Findings(c.job)
	if err != nil {
		t.Fatal(err)
	}
	if all[0].Open() || all[0].ClearedByDecision != record.ID || all[0].ClearedBy != 0 || !all[1].Open() {
		t.Errorf("findings = %+v, record %d", all, record.ID)
	}
	if open, _ := c.s.OpenFindings(c.job); len(open) != 1 || open[0].ID != other.ID {
		t.Errorf("open findings = %+v", open)
	}
	if n := len(journaledKinds(t, c.s, KindFindingsOverruled)); n != 1 {
		t.Errorf("%d overrulings journaled, want 1", n)
	}
	// A finding the user's decision cleared is no longer open to dispute.
	if _, err := c.ask(owner, RequestDispute, []int64{disputed.ID}, options...); err == nil || !strings.Contains(err.Error(), "not an open blocking finding") {
		t.Errorf("a dispute of a cleared finding = %v", err)
	}
}

func TestUpholdingADisputedFindingLeavesItOpenForQualitysReValidation(t *testing.T) {
	c := newConversation(t)
	found := c.withFindings(FindingInput{Kind: FindingBlocking, Text: "greeting.txt does not say hello"})
	r, err := c.ask(qualityOwner, RequestDispute, []int64{found[0].ID}, DisputeUphold, DisputeOverrule)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := c.answerWith(r, DisputeUphold, "the finding stands"); err != nil {
		t.Fatal(err)
	}
	if open, _ := c.s.OpenFindings(c.job); len(open) != 1 || !open[0].Open() {
		t.Errorf("open findings = %+v", open)
	}
	if n := len(journaledKinds(t, c.s, KindFindingsOverruled)); n != 0 {
		t.Errorf("%d overrulings journaled, want none", n)
	}
}

func TestARequestOutsideTheMandateOrAConflictingInstructionNeedsTheUsersOwnAnswer(t *testing.T) {
	for _, kind := range []string{RequestMandate, RequestConflict} {
		t.Run(kind, func(t *testing.T) {
			s := open(t, t.TempDir()+"/store.db")
			job := jobWithBranch(t, s)
			a, _ := newAssignment(t, s, job)

			in := workerRequest(a)
			in.Kind, in.Questions = kind, []Question{{ID: "q1", Title: "May I?", Options: []string{"Follow the mandate", "Widen the mandate"}, Recommendation: "Follow the mandate"}}
			if _, _, err := s.DecisionRequested(in, assignmentsAt); err == nil || !strings.Contains(err.Error(), "says in its content") {
				t.Fatalf("a %s request that says nothing of itself = %v", kind, err)
			}
			in.Content = "AGENTS.md says to deploy after every merge; the mandate does not cover deployment"
			worker, _, err := s.DecisionRequested(in, assignmentsAt)
			if err != nil {
				t.Fatal(err)
			}
			if worker.Recipient != owner || len(worker.Blocks) != 1 {
				t.Fatalf("worker request = %+v", worker)
			}

			// No record, recommendation or leader's word stands in for the user.
			if _, _, err := s.DecisionAnswered(DecisionAnswer{Request: worker.Request, Version: 1, Answerer: owner, Text: "go ahead", ReliedOn: "AGENTS.md"}, assignmentsAt); err == nil || !strings.Contains(err.Error(), "needs the user's own answer") {
				t.Fatalf("the owning leader answered it itself: %v", err)
			}

			// The leader carries it to the user unchanged, and the user's answer in the
			// conversation is what answers the worker.
			carry := DecisionRequestInput{
				Job: job, Requester: owner, Recipient: ToUser, Notify: CoordinationLeader, Kind: kind, Carries: worker.Request,
				Content: in.Content, Questions: in.Questions, Blocks: []int64{a.ID},
			}
			reworded := carry
			reworded.Questions = []Question{{ID: "q1", Title: "May I?", Options: []string{"Yes"}}}
			if _, _, err := s.DecisionRequested(reworded, assignmentsAt); err == nil {
				t.Fatal("the leader reworded the worker's question")
			}
			user, _, err := s.DecisionRequested(carry, assignmentsAt)
			if err != nil {
				t.Fatal(err)
			}
			if user.Surface != SurfaceConversation || user.Carries != worker.Request {
				t.Fatalf("carried request = %+v", user)
			}
			if _, _, err := s.DecisionAnswered(DecisionAnswer{Request: worker.Request, Version: 1, Answerer: owner, Carried: user.Request}, assignmentsAt); err == nil || !strings.Contains(err.Error(), "pending") {
				t.Fatalf("returning an answer the user has not given = %v", err)
			}

			c := &conversation{t: t, s: s, job: job}
			if _, err := s.db.Exec(`UPDATE jobs SET witness = ? WHERE number = ?`, c.say(CoordinationLeader, "do the setup"), job); err != nil {
				t.Fatal(err)
			}
			record, _, _, err := c.answerWith(user, "Follow the mandate", "no, do not deploy")
			if err != nil {
				t.Fatal(err)
			}
			if record.Source != SourceHuman || record.Cites == 0 {
				t.Errorf("record = %+v", record)
			}
			returned, _, err := s.DecisionAnswered(DecisionAnswer{Request: worker.Request, Version: 1, Answerer: owner, Carried: user.Request}, assignmentsAt)
			if err != nil {
				t.Fatal(err)
			}
			if returned.Answer != "no, do not deploy" || !strings.Contains(returned.ReliedOn, "witnessed message") {
				t.Errorf("returned = %+v", returned)
			}
		})
	}
}
