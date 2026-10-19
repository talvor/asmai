// SPDX-License-Identifier: Apache-2.0

package store

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var choiceQuestion = []Question{{ID: "q1", Title: "Which cache?", Options: []string{"redis", "in-memory"}, Recommendation: "in-memory"}}

// workerRequest is a worker's choice request about its assignment, as the
// daemon makes it: to the owning leader, holding the assignment's work.
func workerRequest(a Assignment) DecisionRequestInput {
	return DecisionRequestInput{
		Job: a.Job, Requester: a.Worker, Recipient: a.Owner, Notify: a.Owner, Assignment: a.ID,
		Kind: RequestChoice, Questions: choiceQuestion, Blocks: []int64{a.ID},
	}
}

func TestAWorkersRequestGoesToItsOwningLeaderAndNeverToTheUser(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	job := jobWithBranch(t, s)
	a, _ := newAssignment(t, s, job)

	r, dispatch, err := s.DecisionRequested(workerRequest(a), assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	if r.Request != r.ID || r.Version != 1 || r.State != RequestPending || r.Recipient != a.Owner || r.Surface != SurfaceConversation {
		t.Fatalf("request = %+v", r)
	}
	if dispatch == nil || dispatch.Agent != "leader@engineering" {
		t.Fatalf("dispatch = %+v, want one to the owning leader", dispatch)
	}
	inbox, err := s.Inbox("leader@engineering", dispatch.ID, assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	if len(inbox) != 1 || inbox[0].Kind != MessageDecisionRequest || inbox[0].RequestData == nil || inbox[0].RequestData.Request != r.Request || inbox[0].Sender != a.Worker {
		t.Fatalf("owning leader's inbox = %+v", inbox)
	}
	// The user is not shown what a worker asks its leader.
	pending, err := s.PendingDecisions()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("the user is listed %+v", pending)
	}
	var toCoordination int
	if err := s.db.QueryRow(`SELECT count(*) FROM messages WHERE recipient = 'leader@coordination'`).Scan(&toCoordination); err != nil || toCoordination != 0 {
		t.Fatalf("messages to Coordination = %d, %v", toCoordination, err)
	}
}

func TestANewVersionReplacesTheOldOneWhichIsShownAsReplaced(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	job := jobWithBranch(t, s)
	a, _ := newAssignment(t, s, job)
	first, _, err := s.DecisionRequested(workerRequest(a), assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}

	again := workerRequest(a)
	again.Request, again.Content = first.Request, "new evidence: redis is already deployed"
	second, dispatch, err := s.DecisionRequested(again, assignmentsAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if second.Request != first.Request || second.Version != 2 || second.State != RequestPending || dispatch == nil {
		t.Fatalf("second = %+v, dispatch %+v", second, dispatch)
	}
	versions, err := s.JobDecisionRequests(job)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 2 || versions[0].Version != 1 || versions[0].State != RequestReplaced || versions[0].SettledAt == nil || versions[1].State != RequestPending {
		t.Fatalf("versions = %+v", versions)
	}
	latest, err := s.DecisionRequest(first.Request)
	if err != nil || latest.Version != 2 {
		t.Fatalf("latest = %+v, %v", latest, err)
	}

	// Only the requester issues a new version, and only of a pending one.
	other := workerRequest(a)
	other.Request, other.Requester = first.Request, "leader@engineering"
	if _, _, err := s.DecisionRequested(other, assignmentsAt); err == nil || !strings.Contains(err.Error(), "only the requester") {
		t.Fatalf("a new version by another = %v", err)
	}
	entries, err := s.Journal()
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, e := range entries {
		kinds[e.Kind]++
	}
	if kinds[KindDecisionRequested] != 2 || kinds[KindDecisionReplaced] != 1 {
		t.Fatalf("journal kinds = %v", kinds)
	}
}

func TestAnAnswerNamingAnOlderVersionIsRefusedAndAppliesToNothing(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	job := jobWithBranch(t, s)
	a, _ := newAssignment(t, s, job)
	first, _, err := s.DecisionRequested(workerRequest(a), assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	again := workerRequest(a)
	again.Request = first.Request
	if _, _, err := s.DecisionRequested(again, assignmentsAt); err != nil {
		t.Fatal(err)
	}

	_, _, err = s.DecisionAnswered(DecisionAnswer{Request: first.Request, Version: 1, Answerer: a.Owner, Text: "redis"}, assignmentsAt)
	if err == nil || !strings.Contains(err.Error(), "replaced by version 2") || !strings.Contains(err.Error(), "only to the version it answers") {
		t.Fatalf("an answer to version 1 = %v", err)
	}
	latest, err := s.DecisionRequest(first.Request)
	if err != nil || latest.State != RequestPending || latest.Answer != "" {
		t.Fatalf("after the refusal the request is %+v, %v", latest, err)
	}
	if _, _, err := s.DecisionAnswered(DecisionAnswer{Request: first.Request, Version: 3, Answerer: a.Owner, Text: "redis"}, assignmentsAt); err == nil || !strings.Contains(err.Error(), "no version 3") {
		t.Fatalf("an answer to a version that does not exist = %v", err)
	}
	if _, _, err := s.DecisionAnswered(DecisionAnswer{Request: first.Request, Version: 2, Answerer: a.Owner, Text: " "}, assignmentsAt); err == nil {
		t.Fatal("an answer without text was recorded")
	}

	answered, dispatch, err := s.DecisionAnswered(DecisionAnswer{Request: first.Request, Version: 2, Answerer: a.Owner, Text: "in-memory", ReliedOn: "the job's decisions"}, assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	if answered.State != RequestAnswered || answered.Answer != "in-memory" || answered.AnsweredBy != a.Owner || dispatch.Agent != a.Worker {
		t.Fatalf("answered = %+v, dispatch %+v", answered, dispatch)
	}
	// An answer settles the version once.
	if _, _, err := s.DecisionAnswered(DecisionAnswer{Request: first.Request, Version: 2, Answerer: a.Owner, Text: "redis"}, assignmentsAt); err == nil || !strings.Contains(err.Error(), "answered, not pending") {
		t.Fatalf("a second answer = %v", err)
	}
}

func TestWorkThatDependsOnAPendingRequestWaitsAndIndependentWorkContinues(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	job := jobWithBranch(t, s)
	dependent, dependentDispatch := newAssignment(t, s, job)
	independent, independentDispatch := newAssignment(t, s, job)
	working(t, s, dependentDispatch)
	working(t, s, independentDispatch)
	r, _, err := s.DecisionRequested(workerRequest(dependent), assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}

	submit := func(a Assignment, d Dispatch, kind string) error {
		if _, err := s.EffectRecorded(a.Worker, 1, d.ID, "push", "origin/"+a.Workspace.Branch, assignmentsAt); err != nil {
			t.Fatal(err)
		}
		sub := submission(d, kind)
		sub.Agent = a.Worker
		_, _, err := s.ReportSubmitted(sub, assignmentsAt)
		return err
	}
	if err := submit(dependent, dependentDispatch, ReportResult); err == nil || !strings.Contains(err.Error(), "waits for decision request") {
		t.Fatalf("the dependent result = %v, want it to wait", err)
	}
	if err := submit(independent, independentDispatch, ReportResult); err != nil {
		t.Fatalf("the independent result = %v, want it to go on", err)
	}
	// Reporting that it is blocked is not finishing the work.
	if err := submit(dependent, dependentDispatch, ReportBlocked); err != nil {
		t.Fatalf("the dependent worker's blocked report = %v", err)
	}
	if a, err := s.Assignment(dependent.ID); err != nil || a.State != AssignmentActive {
		t.Fatalf("the dependent assignment = %+v, %v", a, err)
	}

	// Once the request is answered the work goes on, in the dispatch that
	// carried the answer.
	_, answer, err := s.DecisionAnswered(DecisionAnswer{Request: r.Request, Version: 1, Answerer: dependent.Owner, Text: "in-memory"}, assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ChangeDispatch(answer.ID, 1, DispatchCreated, DispatchWorking, "", assignmentsAt); err != nil {
		t.Fatal(err)
	}
	if err := submit(dependent, answer, ReportResult); err != nil {
		t.Fatalf("the dependent result after the answer = %v", err)
	}
}

func TestPausingAJobKeepsItsPendingRequestsAnswerableAndCancellingWithdrawsThem(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	job := jobWithBranch(t, s)
	a, _ := newAssignment(t, s, job)
	paused, _, err := s.DecisionRequested(workerRequest(a), assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	cancelled, _, err := s.DecisionRequested(workerRequest(a), assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE jobs SET state = ? WHERE number = ?`, JobPaused, job); err != nil {
		t.Fatal(err)
	}

	// A paused job's pending decisions stay answerable.
	answered, _, err := s.DecisionAnswered(DecisionAnswer{Request: paused.Request, Version: 1, Answerer: a.Owner, Text: "redis"}, assignmentsAt)
	if err != nil || answered.State != RequestAnswered {
		t.Fatalf("answering in a paused job = %+v, %v", answered, err)
	}
	// Nothing was withdrawn by the pause.
	if still, err := s.DecisionRequest(cancelled.Request); err != nil || still.State != RequestPending {
		t.Fatalf("after the pause the other request is %+v, %v", still, err)
	}

	// Cancelling the job withdraws what is still pending, and only that.
	withdrawn, err := s.JobDecisionsWithdrawn(job, assignmentsAt.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(withdrawn) != 1 || withdrawn[0].Request != cancelled.Request || withdrawn[0].State != RequestWithdrawn {
		t.Fatalf("withdrawn = %+v", withdrawn)
	}
	if got, _ := s.DecisionRequest(paused.Request); got.State != RequestAnswered {
		t.Fatalf("the answered request is now %s", got.State)
	}
	if got, _ := s.DecisionRequest(cancelled.Request); got.State != RequestWithdrawn || got.SettledAt == nil {
		t.Fatalf("the withdrawn request is %+v", got)
	}
	if _, err := s.db.Exec(`UPDATE jobs SET state = 'cancelled' WHERE number = ?`, job); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.DecisionAnswered(DecisionAnswer{Request: cancelled.Request, Version: 1, Answerer: a.Owner, Text: "redis"}, assignmentsAt); err == nil {
		t.Fatal("a withdrawn request was answered")
	}
	if _, _, err := s.DecisionRequested(workerRequest(a), assignmentsAt); err == nil || !strings.Contains(err.Error(), "takes no decision request") {
		t.Fatalf("a request in a cancelled job = %v", err)
	}
	if work, err := s.OpenLeaderWork(); err != nil || len(work) != 0 {
		t.Fatalf("open work of a cancelled job = %+v, %v", work, err)
	}
}

func TestALeadersRequestGoesToTheUserWithItsAnswerSurface(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	job := jobWithBranch(t, s)
	a, _ := newAssignment(t, s, job)

	single, dispatch, err := s.DecisionRequested(DecisionRequestInput{
		Job: job, Requester: "leader@engineering", Recipient: ToUser, Notify: "leader@coordination",
		Kind: RequestChoice, Questions: choiceQuestion, Blocks: []int64{a.ID},
	}, assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	several, _, err := s.DecisionRequested(DecisionRequestInput{
		Job: job, Requester: "leader@engineering", Recipient: ToUser,
		Kind: RequestQuestions, Content: "a prototype", Questions: []Question{{ID: "a", Title: "A?"}, {ID: "b", Title: "B?"}},
	}, assignmentsAt.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if dispatch == nil || dispatch.Agent != "leader@coordination" || single.Surface != SurfaceConversation || several.Surface != SurfaceLavish {
		t.Fatalf("single %+v, several %+v, dispatch %+v", single, several, dispatch)
	}
	pending, err := s.PendingDecisions()
	if err != nil || len(pending) != 2 || pending[0].Request != single.Request || pending[1].Request != several.Request {
		t.Fatalf("pending = %+v, %v", pending, err)
	}
	work, err := s.OpenLeaderWork()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range work {
		found = found || (w.Leader == "leader@engineering" && w.Job == job)
	}
	if !found {
		t.Fatalf("a leader with a decision pending has open work: %+v", work)
	}
	// Listing changes nothing.
	if again, _ := s.PendingDecisions(); len(again) != 2 || again[0].State != RequestPending {
		t.Fatalf("listing answered or changed something: %+v", again)
	}
}

func TestAnAnswerSurfaceIsAChoiceInTheConversationAndEverythingElseInLavish(t *testing.T) {
	two := []Question{{ID: "a", Title: "A?"}, {ID: "b", Title: "B?"}}
	for _, c := range []struct {
		kind      string
		questions []Question
		want      string
	}{
		{RequestChoice, choiceQuestion, SurfaceConversation},
		{RequestProvider, choiceQuestion, SurfaceConversation},
		{RequestReview, choiceQuestion, SurfaceLavish},
		{RequestGrilling, choiceQuestion, SurfaceLavish},
		{RequestWayfinder, choiceQuestion, SurfaceLavish},
		{RequestQuestions, two, SurfaceLavish},
	} {
		if got := AnswerSurface(c.kind, c.questions); got != c.want {
			t.Errorf("surface of %s with %d questions = %s, want %s", c.kind, len(c.questions), got, c.want)
		}
	}

	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	job := jobWithBranch(t, s)
	base := DecisionRequestInput{Job: job, Requester: "leader@engineering", Recipient: ToUser}
	for name, mutate := range map[string]func(*DecisionRequestInput){
		"a kind":                    func(in *DecisionRequestInput) { in.Kind = "other"; in.Questions = choiceQuestion },
		"a question":                func(in *DecisionRequestInput) { in.Kind = RequestQuestions },
		"one question for a choice": func(in *DecisionRequestInput) { in.Kind = RequestChoice; in.Questions = two },
		"options for a choice": func(in *DecisionRequestInput) {
			in.Kind = RequestChoice
			in.Questions = []Question{{ID: "a", Title: "A?"}}
		},
		"a title": func(in *DecisionRequestInput) { in.Kind = RequestQuestions; in.Questions = []Question{{ID: "a"}} },
		"distinct ids": func(in *DecisionRequestInput) {
			in.Kind = RequestQuestions
			in.Questions = []Question{{ID: "a", Title: "A"}, {ID: "a", Title: "B"}}
		},
		"a recommendation among the options": func(in *DecisionRequestInput) {
			in.Kind = RequestChoice
			in.Questions = []Question{{ID: "a", Title: "A", Options: []string{"x", "y"}, Recommendation: "z"}}
		},
		"a job": func(in *DecisionRequestInput) { in.Kind = RequestChoice; in.Questions = choiceQuestion; in.Job = 99 },
		"an assignment of its job": func(in *DecisionRequestInput) {
			in.Kind = RequestChoice
			in.Questions = choiceQuestion
			in.Blocks = []int64{99}
		},
	} {
		in := base
		mutate(&in)
		if _, _, err := s.DecisionRequested(in, assignmentsAt); err == nil {
			t.Errorf("a request without %s was recorded", name)
		}
	}
}

func TestDecisionRequestsSurviveReopeningTheStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	s := open(t, path)
	job := jobWithBranch(t, s)
	a, _ := newAssignment(t, s, job)
	r, _, err := s.DecisionRequested(workerRequest(a), assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	s = open(t, path)
	got, err := s.DecisionRequest(r.Request)
	if err != nil || got.State != RequestPending || got.Questions[0].Title != "Which cache?" || len(got.Blocks) != 1 {
		t.Fatalf("after reopening = %+v, %v", got, err)
	}
}
