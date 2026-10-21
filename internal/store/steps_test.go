// SPDX-License-Identifier: Apache-2.0

package store

import (
	"strings"
	"testing"
	"time"
)

// steps is a job in which Engineering's worker asks its owning leader for
// the human step of a skill, with the user's conversation begun so that
// decisions and grants of the user's can be recorded in it.
type steps struct {
	*conversation
	assignment Assignment
	dispatch   Dispatch
	at         time.Time
}

func newSteps(t *testing.T) *steps {
	t.Helper()
	c := newConversation(t)
	// The conversation's own choice stays pending: it is the leader's, not
	// the worker's.
	a, d := newAssignment(t, c.s, c.job)
	working(t, c.s, d)
	return &steps{conversation: c, assignment: a, dispatch: d, at: grantsAt}
}

func (st *steps) tick() time.Time {
	st.at = st.at.Add(time.Second)
	return st.at
}

// ask is the worker's request for the step of skill, about its assignment.
func (st *steps) ask(skill, step string) (DecisionRequest, error) {
	st.t.Helper()
	in := workerRequest(st.assignment)
	in.Kind, in.Skill, in.Step = RequestStep, skill, step
	r, _, err := st.s.DecisionRequested(in, st.tick())
	return r, err
}

func (st *steps) mustAsk(skill, step string) DecisionRequest {
	st.t.Helper()
	r, err := st.ask(skill, step)
	if err != nil {
		st.t.Fatal(err)
	}
	return r
}

// answer is the owning leader's answer to r from the records and grants.
func (st *steps) answer(r DecisionRequest, records, grants []int64) (DecisionRequest, Dispatch, error) {
	st.t.Helper()
	return st.s.DecisionAnswered(DecisionAnswer{Request: r.Request, Version: r.Version, Answerer: owner, Text: "Seams: the handler and the parser.", Records: records, Grants: grants}, st.tick())
}

// userDecided records a decision of the user's in the job, which Coordination
// does for their words in the conversation.
func (st *steps) userDecided(choice string) DecisionRecord {
	st.t.Helper()
	said := st.say(CoordinationLeader, choice)
	r, err := st.s.DecisionRecorded(DecisionRecord{
		Job: st.job, Leader: CoordinationLeader, Source: SourceHuman, Cites: said, Choice: choice,
		ReliedOn: "the user's message", Rationale: "the user chose it", Evidence: "their message",
	}, 1, st.tick())
	if err != nil {
		st.t.Fatal(err)
	}
	return r
}

// delegated records a decision of a leader's own in the job.
func (st *steps) delegated(choice string) DecisionRecord {
	st.t.Helper()
	r, err := st.s.DecisionRecorded(DecisionRecord{
		Job: st.job, Leader: owner, Source: SourceDelegated, Choice: choice,
		ReliedOn: "the job's mandate", Rationale: "routine", Evidence: "the repository",
	}, 1, st.tick())
	if err != nil {
		st.t.Fatal(err)
	}
	return r
}

// userDecidedElsewhere is a decision of the user's in another job.
func (st *steps) userDecidedElsewhere(choice string) DecisionRecord {
	st.t.Helper()
	other := otherJob(st.t, st.s, "fixture")
	result, err := st.s.db.Exec(`INSERT INTO decision_records(job, leader, source, cites, relied_on, choice, rationale, evidence, created_at) VALUES(?, ?, ?, 1, 'the user''s message', ?, 'they chose it', 'their message', ?)`,
		other, CoordinationLeader, SourceHuman, choice, timestamp(st.tick()))
	if err != nil {
		st.t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		st.t.Fatal(err)
	}
	r, err := st.s.DecisionRecord(id)
	if err != nil {
		st.t.Fatal(err)
	}
	return r
}

// granted records a grant of action the user gave in the conversation.
func (st *steps) granted(scope, repository, action, approved string) Grant {
	st.t.Helper()
	said := st.say(CoordinationLeader, "you may "+approved)
	in := GrantInput{Scope: scope, Repository: repository, Action: action, Approved: approved, Cites: said}
	if scope == ScopeJob {
		in.Job = st.job
	}
	if action == ActionSpend {
		in.Amount, in.Currency = 1000, "USD"
	}
	g, err := st.s.GrantRecorded(in, CoordinationLeader, st.tick())
	if err != nil {
		st.t.Fatal(err)
	}
	return g
}

func TestAStepRequestNamesTheSkillAndTheStepAndOnlyAWorkerAsksIt(t *testing.T) {
	st := newSteps(t)
	r := st.mustAsk("tdd", "the test seams")
	if r.Kind != RequestStep || r.Skill != "tdd" || r.Step != "the test seams" || r.Recipient != owner || len(r.Blocks) != 1 {
		t.Fatalf("the step request is %+v, want a request of tdd's test seams to %s that holds its assignment", r, owner)
	}
	if pending, err := st.s.PendingDecisions(); err != nil || len(pending) != 1 || pending[0].Requester != owner {
		t.Errorf("the user is asked %+v (%v), want only the leader's own request: a worker's step goes to its leader", pending, err)
	}
	text := r.Text()
	if !strings.Contains(text, "step of tdd: the test seams") {
		t.Errorf("the request reads %q, want it to say whose step it is", text)
	}

	for name, mutate := range map[string]func(*DecisionRequestInput){
		"a skill":            func(in *DecisionRequestInput) { in.Skill = " " },
		"a step":             func(in *DecisionRequestInput) { in.Step = "" },
		"a worker to ask it": func(in *DecisionRequestInput) { in.Requester, in.Recipient = owner, ToUser },
	} {
		in := workerRequest(st.assignment)
		in.Kind, in.Skill, in.Step = RequestStep, "tdd", "the test seams"
		mutate(&in)
		if _, _, err := st.s.DecisionRequested(in, st.tick()); err == nil {
			t.Errorf("a step request without %s was recorded", name)
		}
	}
	in := workerRequest(st.assignment)
	in.Skill, in.Step = "tdd", "the test seams"
	if _, _, err := st.s.DecisionRequested(in, st.tick()); err == nil || !strings.Contains(err.Error(), "only a step request names") {
		t.Errorf("a choice request that names a skill returned %v, want it refused", err)
	}

	// A new version keeps the step, and cannot turn into another kind.
	in = workerRequest(st.assignment)
	in.Request, in.Kind, in.Content = r.Request, RequestStep, "the repository has no tests yet"
	again, _, err := st.s.DecisionRequested(in, st.tick())
	if err != nil || again.Version != 2 || again.Skill != "tdd" || again.Step != "the test seams" {
		t.Fatalf("the new version is %+v (%v), want it to keep its skill and step", again, err)
	}
	in.Kind = RequestChoice
	if _, _, err := st.s.DecisionRequested(in, st.tick()); err == nil {
		t.Error("a step request took a version of another kind")
	}
}

func TestAStepTheUsersDecisionOfTheJobCoversIsSatisfiedWithoutAskingTheUser(t *testing.T) {
	st := newSteps(t)
	seams := st.userDecided("tdd test seams: the handler and the parser")
	r := st.mustAsk("tdd", "the test seams")

	answered, dispatch, err := st.answer(r, []int64{seams.ID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if answered.State != RequestAnswered || answered.AnsweredBy != owner || len(answered.ReliedRecords) != 1 || answered.ReliedRecords[0] != seams.ID || len(answered.ReliedGrants) != 0 {
		t.Fatalf("the answer is %+v, want it to rely on decision record %d", answered, seams.ID)
	}
	if !strings.Contains(answered.ReliedOn, "decision record 1") && !strings.Contains(answered.ReliedOn, "decision record") {
		t.Errorf("the answer relied on %q, want it to name the record", answered.ReliedOn)
	}
	// The worker is told the record to name in its result.
	inbox, err := st.s.Inbox(st.assignment.Worker, 0, st.tick())
	if err != nil {
		t.Fatal(err)
	}
	var told bool
	for _, m := range inbox {
		told = told || m.Kind == MessageDecisionAnswer && strings.Contains(m.Body, "Seams: the handler") && strings.Contains(m.Body, "--record 1")
	}
	if !told || dispatch.Agent != st.assignment.Worker {
		t.Errorf("the worker's inbox holds %+v, want the answer and the record it names, in dispatch %+v", inbox, dispatch)
	}
	// Nothing reached the user for it, and the assignment no longer waits.
	for _, p := range mustPending(t, st.s) {
		if p.Requester != owner || p.Carries != 0 {
			t.Errorf("the user was asked %+v for a step a record covers", p)
		}
	}
	if blocking, err := st.s.BlockingDecisions(st.assignment.ID); err != nil || len(blocking) != 0 {
		t.Errorf("the assignment still waits for %+v (%v)", blocking, err)
	}
}

func mustPending(t *testing.T, s *Store) []DecisionRequest {
	t.Helper()
	pending, err := s.PendingDecisions()
	if err != nil {
		t.Fatal(err)
	}
	return pending
}

func TestAStepIsSatisfiedOnlyByTheUsersOwnRecordOfItsJobThatNamesItsSkill(t *testing.T) {
	st := newSteps(t)
	delegated := st.delegated("tdd test seams: the leader chose the handler")
	elsewhere := st.userDecidedElsewhere("tdd test seams: somewhere else")
	other := st.userDecided("the cache is in-memory")
	r := st.mustAsk("tdd", "the test seams")

	for name, tt := range map[string]struct {
		record int64
		want   string
	}{
		"a decision of another job":           {elsewhere.ID, "is of job"},
		"a delegated decision":                {delegated.ID, "delegated decision"},
		"the user's decision about something": {other.ID, "does not name tdd"},
		"a record that is not there":          {999, "does not exist"},
	} {
		_, _, err := st.answer(r, []int64{tt.record}, nil)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("answering from %s returned %v, want it refused: %q", name, err, tt.want)
		}
	}
	if _, _, err := st.answer(r, nil, nil); err == nil || !strings.Contains(err.Error(), "relies on a record") {
		t.Errorf("answering a step from no record returned %v, want it refused: the answer names the record it relies on", err)
	}
	if _, _, err := st.s.DecisionAnswered(DecisionAnswer{Request: r.Request, Version: r.Version, Answerer: owner, Text: "Seams: any.", ReliedOn: "the job's decisions"}, st.tick()); err == nil || !strings.Contains(err.Error(), "relies on a record") {
		t.Errorf("answering a step with only words about what it relies on returned %v, want it refused", err)
	}
	if got, _ := st.s.DecisionRequest(r.Request); got.State != RequestPending {
		t.Errorf("a refused answer left the step %s, want it pending", got.State)
	}
}

func TestAStepIsSatisfiedByAGrantOfADecisionWhoseScopeCoversItsJob(t *testing.T) {
	st := newSteps(t)
	if err := st.s.RepositoryAdded(Repository{Name: "other", Origin: "/other", DefaultBranch: "main", Clone: "/other-clone", AddedAt: assignmentsAt}); err != nil {
		t.Fatal(err)
	}
	r := st.mustAsk("to-tickets", "the approval of the breakdown")

	factory := st.granted(ScopeFactory, "", ActionDecision, "to-tickets breakdowns are approved")
	repo := st.granted(ScopeRepository, "fixture", ActionDecision, "to-tickets breakdowns are approved for fixture")
	elsewhere := st.granted(ScopeRepository, "other", ActionDecision, "to-tickets breakdowns are approved for other")
	spend := st.granted(ScopeJob, "", ActionSpend, "to-tickets spend")
	unnamed := st.granted(ScopeFactory, "", ActionDecision, "the cache is in-memory")
	revoked := st.granted(ScopeFactory, "", ActionDecision, "to-tickets breakdowns are approved until revoked")
	if _, err := st.s.GrantEnded(revoked.ID, GrantRevoked, "user", "", st.tick()); err != nil {
		t.Fatal(err)
	}

	for name, tt := range map[string]struct {
		grant int64
		want  string
	}{
		"a grant of another repository":   {elsewhere.ID, "does not cover job"},
		"a grant of spending":             {spend.ID, "not a decision"},
		"a grant that names another step": {unnamed.ID, "does not name to-tickets"},
		"a revoked grant":                 {revoked.ID, "revoked"},
		"a grant that is not there":       {999, "does not exist"},
	} {
		if _, _, err := st.answer(r, nil, []int64{tt.grant}); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("answering from %s returned %v, want it refused: %q", name, err, tt.want)
		}
	}
	answered, _, err := st.answer(r, nil, []int64{repo.ID})
	if err != nil || len(answered.ReliedGrants) != 1 || answered.ReliedGrants[0] != repo.ID {
		t.Fatalf("answering from a grant of the job's repository = %+v (%v)", answered, err)
	}
	later := st.mustAsk("to-tickets", "the approval of the breakdown")
	if answered, _, err = st.answer(later, nil, []int64{factory.ID}); err != nil || answered.ReliedGrants[0] != factory.ID {
		t.Fatalf("answering from a factory-wide grant = %+v (%v)", answered, err)
	}
}

func TestAnUncoveredStepGoesToTheUserAndTheirAnswerIsRecordedForTheRestOfTheJob(t *testing.T) {
	st := newSteps(t)
	worker := st.mustAsk("tdd", "the test seams")
	if _, _, err := st.answer(worker, nil, nil); err == nil {
		t.Fatal("a step no record covers was answered")
	}

	// The leader carries it to the user in Lavish.
	l := &lavishRequest{t: t, s: st.s, job: st.job, now: st.at}
	carrying, _, err := st.s.DecisionRequested(DecisionRequestInput{
		Job: st.job, Requester: owner, Recipient: ToUser, Notify: CoordinationLeader, Carries: worker.Request, Blocks: []int64{st.assignment.ID},
		Kind: RequestQuestions, Questions: []Question{{ID: "q1", Title: "Which seams does tdd test through?"}},
	}, l.tick())
	if err != nil {
		t.Fatal(err)
	}
	page := l.open(carrying)
	l.collect(page, answerPrompt(page, "q1", "", "the handler and the parser"))
	st.at = l.now

	// Their answer answers the worker, and the daemon records it as the
	// user's decision of the job.
	answered, _, err := st.s.DecisionAnswered(DecisionAnswer{Request: worker.Request, Version: 1, Answerer: owner, Carried: carrying.Request}, st.tick())
	if err != nil {
		t.Fatal(err)
	}
	if answered.AnsweredBy != owner || !strings.Contains(answered.Answer, "the handler and the parser") || len(answered.ReliedRecords) != 1 {
		t.Fatalf("the answer is %+v, want the user's words and the record that holds them", answered)
	}
	record, err := st.s.DecisionRecord(answered.ReliedRecords[0])
	if err != nil {
		t.Fatal(err)
	}
	if record.Job != st.job || record.Source != SourceHuman || !record.Lavish || record.Cites == 0 || !strings.Contains(record.Choice, "tdd") || !strings.Contains(record.Choice, "the handler and the parser") {
		t.Errorf("the recorded decision is %+v, want the user's own answer, in this job, naming tdd", record)
	}

	// For the rest of the job, the step is covered by it.
	again := st.mustAsk("tdd", "the test seams")
	if _, _, err := st.answer(again, []int64{record.ID}, nil); err != nil {
		t.Errorf("answering the same step again from the user's recorded answer returned %v", err)
	}
	// And in no other job.
	asked, _, err := st.s.DecisionRequested(DecisionRequestInput{
		Job: otherJob(t, st.s, "fixture"), Requester: "worker9@engineering", Recipient: owner, Notify: owner,
		Kind: RequestStep, Skill: "tdd", Step: "the test seams", Questions: choiceQuestion,
	}, st.tick())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.s.DecisionAnswered(DecisionAnswer{Request: asked.Request, Version: 1, Answerer: owner, Text: "Seams.", Records: []int64{record.ID}}, st.tick()); err == nil || !strings.Contains(err.Error(), "is of job") {
		t.Errorf("answering another job's step from this job's record returned %v, want it refused", err)
	}
}

func TestAWorkersResultNamesTheRecordsItsStepsRelied(t *testing.T) {
	st := newSteps(t)
	seams := st.userDecided("tdd test seams: the handler and the parser")
	grant := st.granted(ScopeFactory, "", ActionDecision, "code-review compares against the job branch")
	elsewhere := st.userDecidedElsewhere("tdd test seams: elsewhere")
	delegated := st.delegated("tdd test seams: the leader's own choice")
	spend := st.granted(ScopeJob, "", ActionSpend, "spend on tdd")
	if _, err := st.s.EffectRecorded(st.assignment.Worker, 1, st.dispatch.ID, "push", "origin/asmai/job-1/1", assignmentsAt); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		skill, step string
		records     []int64
		grants      []int64
	}{{"tdd", "the test seams", []int64{seams.ID}, nil}, {"code-review", "the comparison point", nil, []int64{grant.ID}}} {
		r := st.mustAsk(step.skill, step.step)
		if _, _, err := st.answer(r, step.records, step.grants); err != nil {
			t.Fatal(err)
		}
	}
	sub := func(mutate func(*ReportInput)) Submission {
		s := submission(st.dispatch, ReportResult)
		mutate(&s.Input)
		return s
	}
	for name, tt := range map[string]struct {
		input func(*ReportInput)
		want  string
	}{
		"no record named":         {func(in *ReportInput) {}, "names no record for the tdd step"},
		"only the decision named": {func(in *ReportInput) { in.RecordsRelied = []int64{seams.ID} }, "names no grant for the code-review step"},
		"only the grant named":    {func(in *ReportInput) { in.GrantsRelied = []int64{grant.ID} }, "names no record for the tdd step"},
		"a record of another job": {func(in *ReportInput) {
			in.RecordsRelied, in.GrantsRelied = []int64{seams.ID, elsewhere.ID}, []int64{grant.ID}
		}, "is of job"},
		"a grant that is not there":  {func(in *ReportInput) { in.RecordsRelied, in.GrantsRelied = []int64{seams.ID}, []int64{grant.ID, 999} }, "does not exist"},
		"a record that is not there": {func(in *ReportInput) { in.RecordsRelied, in.GrantsRelied = []int64{seams.ID, 999}, []int64{grant.ID} }, "does not exist"},
		"a delegated decision": {func(in *ReportInput) {
			in.RecordsRelied, in.GrantsRelied = []int64{seams.ID, delegated.ID}, []int64{grant.ID}
		}, "delegated decision"},
		"a grant of spending": {func(in *ReportInput) {
			in.RecordsRelied, in.GrantsRelied = []int64{seams.ID}, []int64{grant.ID, spend.ID}
		}, "not a decision"},
		"a record named twice or more": {func(in *ReportInput) {
			in.RecordsRelied, in.GrantsRelied = []int64{seams.ID, seams.ID}, []int64{grant.ID}
		}, "twice"},
	} {
		if _, _, err := st.s.ReportSubmitted(sub(tt.input), assignmentsAt); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("a result with %s returned %v, want it refused: %q", name, err, tt.want)
		}
	}
	r, _, err := st.s.ReportSubmitted(sub(func(in *ReportInput) { in.RecordsRelied, in.GrantsRelied = []int64{seams.ID}, []int64{grant.ID} }), assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.RecordsRelied) != 1 || r.RecordsRelied[0] != seams.ID || len(r.GrantsRelied) != 1 || r.GrantsRelied[0] != grant.ID {
		t.Errorf("the result is %+v, want it to name the record and the grant it relied on", r)
	}
	messages, err := st.s.Inbox(owner, 0, assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	var reached bool
	for _, m := range messages {
		reached = reached || m.Kind == ReportResult && m.ReportData != nil && len(m.ReportData.RecordsRelied) == 1
	}
	if !reached {
		t.Errorf("the owning leader's inbox lacks the records the result relied on: %+v", messages)
	}
}

func TestAResultOfAWorkerWhoAskedNoStepNamesNoRecord(t *testing.T) {
	st := newSteps(t)
	seams := st.userDecided("tdd test seams: the handler and the parser")
	if _, err := st.s.EffectRecorded(st.assignment.Worker, 1, st.dispatch.ID, "push", "origin/asmai/job-1/1", assignmentsAt); err != nil {
		t.Fatal(err)
	}
	s := submission(st.dispatch, ReportResult)
	if _, _, err := st.s.ReportSubmitted(s, assignmentsAt); err != nil {
		t.Fatalf("a result that relied on no step returned %v", err)
	}
	_ = seams
}

func TestAnUncoveredStepAnsweredInTheConversationIsSatisfiedByTheRecordOfThatAnswer(t *testing.T) {
	st := newSteps(t)
	worker := st.mustAsk("tdd", "the test seams")

	// The leader carries a choice between two ways to the user, which they
	// answer in the conversation: Coordination records their words.
	carrying, _, err := st.s.DecisionRequested(DecisionRequestInput{
		Job: st.job, Requester: owner, Recipient: ToUser, Notify: CoordinationLeader, Carries: worker.Request, Blocks: []int64{st.assignment.ID},
		Kind: RequestChoice, Questions: []Question{{ID: "q1", Title: "Test through which seam?", Options: []string{"the handler", "the parser"}}},
	}, st.tick())
	if err != nil {
		t.Fatal(err)
	}
	st.present(carrying)
	said := st.say(CoordinationLeader, "through the handler")
	record, _, _, err := st.s.ConversationAnswerRecorded(DecisionRecord{
		Job: st.job, Leader: CoordinationLeader, Choice: "the handler", Request: carrying.Request, Version: carrying.Version,
		Cites: said, Reading: "the user picked the handler seam",
	}, 1, st.tick())
	if err != nil {
		t.Fatal(err)
	}
	// The user's answer in the conversation is not a Lavish answer the
	// daemon relays: the leader answers from the record that holds it.
	if _, _, err := st.s.DecisionAnswered(DecisionAnswer{Request: worker.Request, Version: 1, Answerer: owner, Carried: carrying.Request}, st.tick()); err == nil {
		t.Fatal("a conversation answer was returned as a Lavish one")
	}
	answered, _, err := st.answer(worker, []int64{record.ID}, nil)
	if err != nil || len(answered.ReliedRecords) != 1 || answered.ReliedRecords[0] != record.ID {
		t.Fatalf("answering from the record of the user's answer = %+v (%v)", answered, err)
	}
	// It answers the step it carried and no other skill's.
	other := st.mustAsk("to-spec", "the test seams")
	if _, _, err := st.answer(other, []int64{record.ID}, nil); err == nil || !strings.Contains(err.Error(), "does not name to-spec") {
		t.Errorf("answering another skill's step from it returned %v, want it refused", err)
	}
}

func TestTheUsersAnswerToAnUncoveredStepIsReturnedAndRecordedWhileTheJobIsPaused(t *testing.T) {
	st := newSteps(t)
	worker := st.mustAsk("tdd", "the test seams")
	l := &lavishRequest{t: t, s: st.s, job: st.job, now: st.at}
	carrying, _, err := st.s.DecisionRequested(DecisionRequestInput{
		Job: st.job, Requester: owner, Recipient: ToUser, Notify: CoordinationLeader, Carries: worker.Request, Blocks: []int64{st.assignment.ID},
		Kind: RequestQuestions, Questions: []Question{{ID: "q1", Title: "Which seams does tdd test through?"}},
	}, l.tick())
	if err != nil {
		t.Fatal(err)
	}
	page := l.open(carrying)
	l.collect(page, answerPrompt(page, "q1", "", "the handler"))
	if _, err := st.s.db.Exec(`UPDATE jobs SET state = ? WHERE number = ?`, JobPaused, st.job); err != nil {
		t.Fatal(err)
	}
	answered, _, err := st.s.DecisionAnswered(DecisionAnswer{Request: worker.Request, Version: 1, Answerer: owner, Carried: carrying.Request}, l.tick())
	if err != nil || len(answered.ReliedRecords) != 1 {
		t.Fatalf("returning the user's answer while the job is paused = %+v (%v), want it answered and recorded: a paused job's pending decisions stay answerable", answered, err)
	}
	record, err := st.s.DecisionRecord(answered.ReliedRecords[0])
	if err != nil || record.Request != carrying.Request || !strings.Contains(record.Text("", "  "), "in Lavish") {
		t.Errorf("the record is %+v (%v), want it to say it answers the request that carried the step, in Lavish", record, err)
	}
}
