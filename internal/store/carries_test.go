// SPDX-License-Identifier: Apache-2.0

package store

import (
	"path/filepath"
	"strings"
	"testing"
)

var grillingQuestions = []Question{{ID: "q1", Title: "Who are the users?", Body: "Name them.", Options: []string{"admins", "everyone"}, Recommendation: "everyone"}, {ID: "q2", Title: "What is out of scope?"}}

// carried is a worker's grilling request to its owning leader, and the
// leader's request that carries it to the user in Lavish, whose page is open.
type carried struct {
	*lavishRequest
	assignment Assignment
	worker     DecisionRequest
	carrying   DecisionRequest
}

func newCarried(t *testing.T) *carried {
	t.Helper()
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	job := jobWithBranch(t, s)
	a, _ := newAssignment(t, s, job)
	c := &carried{lavishRequest: &lavishRequest{t: t, s: s, job: job, now: assignmentsAt}, assignment: a}
	in := workerRequest(a)
	in.Kind, in.Questions = RequestGrilling, grillingQuestions
	var err error
	if c.worker, _, err = s.DecisionRequested(in, c.tick()); err != nil {
		t.Fatal(err)
	}
	c.carrying = c.carry(c.worker.Request)
	return c
}

// carry has the owning leader carry worker request to the user.
func (c *carried) carry(request int64) DecisionRequest {
	c.t.Helper()
	r, _, err := c.s.DecisionRequested(DecisionRequestInput{
		Job: c.job, Requester: owner, Recipient: ToUser, Notify: CoordinationLeader, Carries: request, Blocks: []int64{c.assignment.ID},
		Kind: RequestGrilling, Questions: grillingQuestions,
	}, c.tick())
	if err != nil {
		c.t.Fatal(err)
	}
	return c.open(r)
}

func TestALeadersRequestRecordsTheWorkersRequestAndVersionItCarriesToTheUser(t *testing.T) {
	c := newCarried(t)
	if c.carrying.Carries != c.worker.Request || c.carrying.CarriesVersion != 1 || c.carrying.Surface != SurfaceLavish || c.carrying.Recipient != ToUser {
		t.Fatalf("the carrying request is %+v, want it to carry request %d version 1 to the user in Lavish", c.carrying, c.worker.Request)
	}
	// A new version of the worker's request is a new thing to carry: the
	// leader's next version carries it, and keeps carrying the same request.
	in := workerRequest(c.assignment)
	in.Request, in.Kind, in.Questions, in.Content = c.worker.Request, RequestGrilling, grillingQuestions, "new evidence"
	if _, _, err := c.s.DecisionRequested(in, c.tick()); err != nil {
		t.Fatal(err)
	}
	again, _, err := c.s.DecisionRequested(DecisionRequestInput{Request: c.carrying.Request, Job: c.job, Requester: owner, Kind: RequestGrilling, Questions: grillingQuestions}, c.tick())
	if err != nil || again.Carries != c.worker.Request || again.CarriesVersion != 2 {
		t.Fatalf("the new version is %+v (%v), want it to carry request %d version 2", again, err, c.worker.Request)
	}
}

func TestACarryingRequestIsRefusedForARequestTheLeaderDoesNotOwn(t *testing.T) {
	c := newCarried(t)
	base := DecisionRequestInput{Job: c.job, Requester: owner, Recipient: ToUser, Notify: CoordinationLeader, Kind: RequestGrilling, Questions: grillingQuestions}
	for _, tt := range []struct {
		name   string
		mutate func(*DecisionRequestInput)
		want   string
	}{
		{"a request that is not there", func(in *DecisionRequestInput) { in.Carries = 999 }, "does not exist"},
		{"another leader's worker's request", func(in *DecisionRequestInput) { in.Carries, in.Requester = c.worker.Request, "leader@quality" }, "goes to leader@engineering, not leader@quality"},
		{"a request of a leader to the user", func(in *DecisionRequestInput) { in.Carries = c.carrying.Request }, "is not a worker's request"},
		{"a request to the conversation", func(in *DecisionRequestInput) { in.Carries, in.Recipient = c.worker.Request, owner }, "only a request to the user carries"},
		{"a request that is not pending any more", func(in *DecisionRequestInput) {
			if _, err := c.s.db.Exec(`UPDATE decision_requests SET state = ? WHERE id = ?`, RequestWithdrawn, c.worker.ID); err != nil {
				t.Fatal(err)
			}
			in.Carries = c.worker.Request
		}, "is withdrawn"},
	} {
		in := base
		tt.mutate(&in)
		if _, _, err := c.s.DecisionRequested(in, c.tick()); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("a request carrying %s returned %v, want it refused: %q", tt.name, err, tt.want)
		}
	}
}

func TestTheUsersOwnAnswerToTheCarryingRequestIsWhatAnswersTheWorkersGrillingRequest(t *testing.T) {
	c := newCarried(t)
	// Nothing answers it while the user has not.
	if _, err := carriedAnswer(c.s.db, c.worker, c.carrying.Request, owner); err == nil || !strings.Contains(err.Error(), "pending") {
		t.Fatalf("the answer before the user's = %v, want it refused as pending", err)
	}
	page := c.carrying
	c.collect(page, answerPrompt(page, "q1", "admins", ""))
	if _, err := carriedAnswer(c.s.db, c.worker, c.carrying.Request, owner); err == nil || !strings.Contains(err.Error(), "pending") {
		t.Fatalf("the answer when the user answered one question of two = %v, want it refused as pending", err)
	}
	c.collect(page, answerPrompt(page, "q2", "", "billing"))
	answered, err := carriedAnswer(c.s.db, c.worker, c.carrying.Request, owner)
	if err != nil {
		t.Fatal(err)
	}
	if answered.AnsweredBy != ToUser || answered.Cites == 0 || !strings.Contains(answered.Answer, "admins") || !strings.Contains(answered.Answer, "billing") {
		t.Fatalf("the carried answer is %+v, want the user's own words and the Lavish answer that completed it", answered)
	}
	// Only the leader that carried it returns it, for the request it carries,
	// at the version it carries.
	for name, tt := range map[string]struct {
		worker  DecisionRequest
		request int64
		leader  string
		want    string
	}{
		"another leader":              {c.worker, c.carrying.Request, "leader@quality", "is leader@engineering's, not leader@quality's"},
		"another request":             {c.worker, c.worker.Request, owner, "is not a request of the user's"},
		"a request that is not there": {c.worker, 999, owner, "does not exist"},
		"another worker's request":    {DecisionRequest{Request: c.worker.Request + 50, Version: 1, Job: c.job}, c.carrying.Request, owner, "carries decision request"},
		"another version":             {DecisionRequest{Request: c.worker.Request, Version: 2, Job: c.job}, c.carrying.Request, owner, "carries version 1"},
	} {
		if _, err := carriedAnswer(c.s.db, tt.worker, tt.request, tt.leader); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("returning %s's answer = %v, want it refused: %q", name, err, tt.want)
		}
	}
}

func TestAGrillingRequestKeepsItsKindThroughItsVersionsAndItsCarryingRequestKeepsItsQuestions(t *testing.T) {
	c := newCarried(t)
	// A new version cannot change a grilling question into another kind of
	// request, which a leader could answer from the records.
	in := workerRequest(c.assignment)
	in.Request, in.Kind, in.Questions = c.worker.Request, RequestQuestions, grillingQuestions
	if _, _, err := c.s.DecisionRequested(in, c.tick()); err == nil || !strings.Contains(err.Error(), "stays a grilling request") {
		t.Errorf("a new version of another kind returned %v, want it refused", err)
	}
	// What the leader carries to the user is what the worker asked.
	for name, questions := range map[string][]Question{
		"a reworded question": {{ID: "q1", Title: "Anything?", Options: []string{"admins", "everyone"}}, grillingQuestions[1]},
		"a dropped question":  grillingQuestions[:1],
	} {
		_, _, err := c.s.DecisionRequested(DecisionRequestInput{Job: c.job, Requester: owner, Recipient: ToUser, Notify: CoordinationLeader, Carries: c.worker.Request, Kind: RequestGrilling, Questions: questions}, c.tick())
		if err == nil || !strings.Contains(err.Error(), "unchanged") {
			t.Errorf("carrying %s returned %v, want it refused: the worker's questions are carried unchanged", name, err)
		}
	}
}
