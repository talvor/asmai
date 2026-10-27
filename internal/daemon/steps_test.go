// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/store"
)

var seamsQuestion = []store.Question{{ID: "q1", Title: "Which seams do the tests go through?"}}

// userDecided records the user's own decision of the factory's job, citing the
// message that opened it, as Coordination does for their words.
func (f lavishFactory) userDecided(t *testing.T, choice string) store.DecisionRecord {
	t.Helper()
	r, err := f.s.DecisionRecorded(store.DecisionRecord{
		Job: f.job.Number, Leader: "leader@coordination", Source: store.SourceHuman, Cites: f.job.Witness,
		Choice: choice, ReliedOn: "the user's message", Rationale: "the user chose it", Evidence: "their message",
	}, 1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// step has the first worker ask its owning leader for the human step of skill.
func (f lavishFactory) step(t *testing.T, skill, step string) store.DecisionRequest {
	t.Helper()
	asked, dispatch, err := f.d.decisionRequest(Request{Session: "worker0", Command: CommandDecisionRequest, Kind: store.RequestStep, Skill: skill, Step: step, Questions: seamsQuestion})
	if err != nil {
		t.Fatal(err)
	}
	if asked.Recipient != "leader@engineering" || asked.Skill != skill || asked.Step != step || dispatch == nil || dispatch.Agent != "leader@engineering" {
		t.Fatalf("the worker's step request is %+v, dispatch %+v, want a request of %s's %s to its owning leader", asked, dispatch, skill, step)
	}
	return asked
}

func TestAStepTheUsersRecordedDecisionCoversIsAnsweredFromItWithoutAskingTheUser(t *testing.T) {
	f := newLavishFactory(t)
	seams := f.userDecided(t, "tdd: the test seams are the handler and the parser")
	asked := f.step(t, "tdd", "the test seams")
	if pending, err := f.s.PendingDecisions(); err != nil || len(pending) != 0 {
		t.Fatalf("the user is asked what a worker asks its leader: %+v (%v)", pending, err)
	}

	// Words about what the answer rests on are not a record.
	if _, _, err := f.d.decisionAnswer(Request{Session: "credential", Command: CommandDecisionAnswer, Request: asked.Request, Version: 1, Answer: "the handler", ReliedOn: "the job's decisions"}); err == nil || !strings.Contains(err.Error(), "relies on a record") {
		t.Fatalf("answering a step from words returned %v, want it refused", err)
	}
	// Another leader cannot answer it.
	if _, _, err := f.d.decisionAnswer(Request{Session: "coordination", Command: CommandDecisionAnswer, Request: asked.Request, Version: 1, Answer: "the handler", Records: []int64{seams.ID}}); err == nil || !strings.Contains(err.Error(), "goes to leader@engineering") {
		t.Fatalf("another leader answering a step returned %v, want it refused", err)
	}

	answered, back, err := f.d.decisionAnswer(Request{Session: "credential", Command: CommandDecisionAnswer, Request: asked.Request, Version: 1, Answer: "Test through the handler and the parser.", Records: []int64{seams.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if answered.State != store.RequestAnswered || len(answered.ReliedRecords) != 1 || answered.ReliedRecords[0] != seams.ID || back.Agent != f.assignments[0].Worker {
		t.Fatalf("the answer is %+v, dispatch %+v, want it to rely on decision record %d and go to the worker", answered, back, seams.ID)
	}
	var told bool
	messages, err := f.s.Inbox(f.assignments[0].Worker, back.ID, back.ID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range messages {
		told = told || m.Kind == store.MessageDecisionAnswer && strings.Contains(m.Body, "Test through the handler") && strings.Contains(m.Body, fmt.Sprintf("--record %d", seams.ID))
	}
	if !told {
		t.Error("the worker was not told the record its result is to name")
	}
	if pending, err := f.s.PendingDecisions(); err != nil || len(pending) != 0 {
		t.Errorf("answering from the records reached the user: %+v (%v)", pending, err)
	}
}

func TestOnlyAWorkerAsksAStepAndItNamesItsSkillAndStep(t *testing.T) {
	f := newLavishFactory(t)
	if _, _, err := f.d.decisionRequest(Request{Session: "credential", Command: CommandDecisionRequest, Job: f.job.Number, Kind: store.RequestStep, Skill: "tdd", Step: "the test seams", Questions: seamsQuestion}); err == nil || !strings.Contains(err.Error(), "a worker's") {
		t.Errorf("a leader's step request returned %v, want it refused: a leader carries an uncovered step with a request of its own", err)
	}
	if _, _, err := f.d.decisionRequest(Request{Session: "worker0", Command: CommandDecisionRequest, Kind: store.RequestStep, Questions: seamsQuestion}); err == nil || !strings.Contains(err.Error(), "names the skill and the step") {
		t.Errorf("a step request without its skill returned %v, want it refused", err)
	}
	if pending, err := f.s.PendingDecisions(); err != nil || len(pending) != 0 {
		t.Errorf("a refused step request reached the user: %+v (%v)", pending, err)
	}
}

func TestAStepNoRecordCoversReachesTheUserInLavishAndTheirAnswerCoversItForTheRestOfTheJob(t *testing.T) {
	f := newLavishFactory(t)
	asked := f.step(t, "tdd", "the test seams")

	// Nothing the leader holds covers it, so it goes to the user as a request
	// of the leader's own that carries the worker's.
	if _, _, err := f.d.decisionAnswer(Request{Session: "credential", Command: CommandDecisionAnswer, Request: asked.Request, Version: 1, Answer: "the handler"}); err == nil || !strings.Contains(err.Error(), "relies on a record") {
		t.Fatalf("answering a step no record covers returned %v, want it refused", err)
	}
	carried, dispatch, err := f.d.decisionRequest(Request{Session: "credential", Command: CommandDecisionRequest, Job: f.job.Number, Kind: store.RequestQuestions, Carries: asked.Request, Questions: seamsQuestion, Content: "tdd asks which seams the tests go through.", Blocks: []int64{f.assignments[0].ID}})
	if err != nil {
		t.Fatal(err)
	}
	if carried.Recipient != store.ToUser || carried.Surface != store.SurfaceLavish || dispatch == nil || dispatch.Agent != "leader@coordination" {
		t.Fatalf("the leader's request is %+v, dispatch %+v, want a Lavish request to the user through Coordination", carried, dispatch)
	}
	page := f.waitFor(t, carried, "the page to be opened", func(r store.DecisionRequest) bool { return r.PageURL != "" })
	f.userSends(t, page, true, answer(page, "q1", "", "the handler and the parser"))
	done := f.waitFor(t, page, "the user's answer", func(r store.DecisionRequest) bool { return r.State == store.RequestAnswered })

	returned, _, err := f.d.decisionAnswer(Request{Session: "credential", Command: CommandDecisionAnswer, Request: asked.Request, Version: 1, UserAnswer: carried.Request})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(returned.Answer, "the handler and the parser") || len(returned.ReliedRecords) != 1 {
		t.Fatalf("the worker's answer is %+v, want the user's words and the record that holds them", returned)
	}
	record, err := f.s.DecisionRecord(returned.ReliedRecords[0])
	if err != nil {
		t.Fatal(err)
	}
	if record.Job != f.job.Number || record.Source != store.SourceHuman || record.Cites != done.Cites || !strings.Contains(record.Choice, "tdd") {
		t.Fatalf("the user's answer is recorded as %+v, want their own decision of this job, citing Lavish answer %d", record, done.Cites)
	}

	// The same step, asked again in the job, is covered by it.
	again := f.step(t, "tdd", "the test seams")
	if _, _, err := f.d.decisionAnswer(Request{Session: "credential", Command: CommandDecisionAnswer, Request: again.Request, Version: 1, Answer: "As the user said: the handler and the parser.", Records: []int64{record.ID}}); err != nil {
		t.Errorf("answering the step again from the user's recorded answer returned %v", err)
	}
	if pending, err := f.s.PendingDecisions(); err != nil || len(pending) != 0 {
		t.Errorf("the user was asked again for a step their answer covers: %+v (%v)", pending, err)
	}
}
