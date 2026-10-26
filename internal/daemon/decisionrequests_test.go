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

// withWorkers adds the session of each assignment's worker to the daemon of
// f, working on the dispatch that gave it the assignment, and one of
// Coordination's leader.
func (f twoResults) withWorkers(t *testing.T) {
	t.Helper()
	for i, a := range f.assignments {
		w, err := f.s.WorkerDispatch(a.ID)
		if err != nil {
			t.Fatal(err)
		}
		inbox, err := f.s.Inbox(a.Worker, 0, w.Dispatch.ID, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		worker, err := roles.ParseAddress(a.Worker)
		if err != nil {
			t.Fatal(err)
		}
		credential := "worker" + string(rune('0'+i))
		f.d.leaders[a.Worker] = &leader{address: worker, session: &session.Session{}, generation: 1, currentDispatch: inbox[0].Dispatch}
		f.d.sessions[credential] = sessionRef{address: worker, generation: 1}
	}
	coordination := roles.LeaderOf(roles.Coordination)
	f.d.leaders[coordination.String()] = &leader{address: coordination, session: &session.Session{}, generation: 1}
	f.d.sessions["coordination"] = sessionRef{address: coordination, generation: 1}
}

var cacheQuestion = []store.Question{{ID: "q1", Title: "Which cache?", Options: []string{"redis", "in-memory"}}}

func TestAWorkersDecisionRequestGoesToItsOwningLeaderAndHoldsOnlyItsOwnWork(t *testing.T) {
	f := newTwoResults(t)
	f.withWorkers(t)

	r, dispatch, err := f.d.decisionRequest(Request{Session: "worker0", Command: CommandDecisionRequest, Kind: store.RequestChoice, Questions: cacheQuestion})
	if err != nil {
		t.Fatal(err)
	}
	owner := roles.LeaderOf(roles.Engineering).String()
	if r.Requester != f.assignments[0].Worker || r.Recipient != owner || r.Job != f.job.Number || r.Assignment != f.assignments[0].ID || dispatch == nil || dispatch.Agent != owner {
		t.Fatalf("request = %+v, dispatch %+v", r, dispatch)
	}
	// It is not the user's to see or answer.
	if pending, err := f.s.PendingDecisions(); err != nil || len(pending) != 0 {
		t.Fatalf("pending for the user = %+v, %v", pending, err)
	}
	// A worker takes no job of its own choosing and holds no one else's work.
	for _, in := range []Request{{Job: f.job.Number}, {Blocks: []int64{f.assignments[1].ID}}} {
		in.Session, in.Command, in.Kind, in.Questions = "worker0", CommandDecisionRequest, store.RequestChoice, cacheQuestion
		if _, _, err := f.d.decisionRequest(in); err == nil || !strings.Contains(err.Error(), "about its own assignment") {
			t.Fatalf("a worker's request with %+v = %v", in, err)
		}
	}

	// The owning leader may not accept the dependent result yet, and accepts
	// the independent one.
	if _, err := f.decide(CommandAccept, f.assignments[0].ID, "it works"); err == nil || !strings.Contains(err.Error(), "waits for decision request") {
		t.Fatalf("accepting the dependent result = %v", err)
	}
	if a, err := f.s.Assignment(f.assignments[0].ID); err != nil || a.State != store.AssignmentSubmitted {
		t.Fatalf("the dependent assignment = %+v, %v", a, err)
	}
	if _, err := f.decide(CommandAccept, f.assignments[1].ID, "it works"); err != nil {
		t.Fatalf("accepting the independent result = %v", err)
	}

	// Only the owning leader answers, and only the version it names.
	other := Request{Session: "coordination", Command: CommandDecisionAnswer, Request: r.Request, Version: 1, Answer: "redis"}
	if _, _, err := f.d.decisionAnswer(other); err == nil || !strings.Contains(err.Error(), "goes to "+owner) {
		t.Fatalf("an answer by another leader = %v", err)
	}
	next, _, err := f.d.decisionRequest(Request{Session: "worker0", Command: CommandDecisionRequest, Request: r.Request, Kind: store.RequestChoice, Questions: cacheQuestion, Content: "redis is deployed already"})
	if err != nil || next.Version != 2 {
		t.Fatalf("a new version = %+v, %v", next, err)
	}
	answer := Request{Session: "credential", Command: CommandDecisionAnswer, Request: r.Request, Version: 1, Answer: "redis", ReliedOn: "the job's decisions"}
	if _, _, err := f.d.decisionAnswer(answer); err == nil || !strings.Contains(err.Error(), "an answer applies only to the version it answers") {
		t.Fatalf("an answer to the replaced version = %v", err)
	}
	answer.Version = 2
	answered, back, err := f.d.decisionAnswer(answer)
	if err != nil || answered.State != store.RequestAnswered || back.Agent != f.assignments[0].Worker {
		t.Fatalf("answer = %+v, %+v, %v", answered, back, err)
	}
	// The dependent work no longer waits: what stops its acceptance now is
	// the independent result accepted before it, which moved the job branch,
	// so the work returns to its worker.
	resp, err := f.decide(CommandAccept, f.assignments[0].ID, "it works")
	if err != nil || !resp.Returned {
		t.Fatalf("accepting the dependent result after the answer = %+v, %v, want it returned to its worker", resp, err)
	}
}

func TestALeadersDecisionRequestGoesToTheUserThroughCoordination(t *testing.T) {
	f := newTwoResults(t)
	f.withWorkers(t)

	r, dispatch, err := f.d.decisionRequest(Request{Session: "credential", Command: CommandDecisionRequest, Job: f.job.Number, Kind: store.RequestChoice, Questions: cacheQuestion, Blocks: []int64{f.assignments[0].ID}})
	if err != nil {
		t.Fatal(err)
	}
	if r.Recipient != store.ToUser || r.Surface != store.SurfaceConversation || dispatch == nil || dispatch.Agent != "leader@coordination" {
		t.Fatalf("request = %+v, dispatch %+v", r, dispatch)
	}
	pending, err := f.s.PendingDecisions()
	if err != nil || len(pending) != 1 || pending[0].Request != r.Request {
		t.Fatalf("pending = %+v, %v", pending, err)
	}
	// No agent answers for the user.
	if _, _, err := f.d.decisionAnswer(Request{Session: "credential", Command: CommandDecisionAnswer, Request: r.Request, Version: 1, Answer: "redis"}); err == nil || !strings.Contains(err.Error(), "goes to the user") {
		t.Fatalf("an agent's answer to the user's decision = %v", err)
	}
	if _, _, err := f.d.decisionAnswer(Request{Session: "coordination", Command: CommandDecisionAnswer, Request: r.Request, Version: 1, Answer: "redis"}); err == nil || !strings.Contains(err.Error(), "goes to the user") {
		t.Fatalf("Coordination's answer without the user's = %v", err)
	}
	// A leader holds only assignments it owns, in a job it names.
	for name, in := range map[string]Request{
		"no job":           {Kind: store.RequestChoice, Questions: cacheQuestion},
		"another's work":   {Job: f.job.Number, Kind: store.RequestChoice, Questions: cacheQuestion, Blocks: []int64{999}},
		"an unknown kind":  {Job: f.job.Number, Kind: "other", Questions: cacheQuestion},
		"another's leader": {Job: f.job.Number, Request: r.Request + 100, Kind: store.RequestChoice, Questions: cacheQuestion},
	} {
		in.Session, in.Command = "credential", CommandDecisionRequest
		if _, _, err := f.d.decisionRequest(in); err == nil {
			t.Errorf("a request with %s was recorded", name)
		}
	}
	// Coordination presents its own request itself: nothing is sent to it.
	own, dispatch, err := f.d.decisionRequest(Request{Session: "coordination", Command: CommandDecisionRequest, Job: f.job.Number, Kind: store.RequestReview, Questions: cacheQuestion})
	if err != nil || dispatch != nil || own.Surface != store.SurfaceLavish {
		t.Fatalf("Coordination's own request = %+v, %+v, %v", own, dispatch, err)
	}
}

func TestListingDecisionsAnswersNothing(t *testing.T) {
	f := newTwoResults(t)
	f.withWorkers(t)
	r, _, err := f.d.decisionRequest(Request{Session: "credential", Command: CommandDecisionRequest, Job: f.job.Number, Kind: store.RequestChoice, Questions: cacheQuestion})
	if err != nil {
		t.Fatal(err)
	}
	before, err := f.s.Journal()
	if err != nil {
		t.Fatal(err)
	}
	pending, err := f.s.PendingDecisions()
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending = %+v, %v", pending, err)
	}
	after, err := f.s.Journal()
	if err != nil || len(after) != len(before) {
		t.Fatalf("listing wrote %d journal entries, %v", len(after)-len(before), err)
	}
	if again, err := f.s.DecisionRequest(r.Request); err != nil || again.State != store.RequestPending {
		t.Fatalf("after listing the request is %+v, %v", again, err)
	}
}
