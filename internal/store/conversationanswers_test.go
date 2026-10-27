// SPDX-License-Identifier: Apache-2.0

package store

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// conversation is a job whose conversation with Coordination the user has
// begun, with its leader's request for a choice between two approaches.
type conversation struct {
	t   *testing.T
	s   *Store
	job int64
	// request is the leader's request, version 1.
	request DecisionRequest
}

func newConversation(t *testing.T) *conversation {
	t.Helper()
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	c := &conversation{t: t, s: s, job: jobWithBranch(t, s)}
	opening := c.say(CoordinationLeader, "add a cache")
	if _, err := s.db.Exec(`UPDATE jobs SET witness = ? WHERE number = ?`, opening, c.job); err != nil {
		t.Fatal(err)
	}
	r, dispatch, err := s.DecisionRequested(DecisionRequestInput{
		Job: c.job, Requester: owner, Recipient: ToUser, Notify: CoordinationLeader,
		Kind: RequestChoice, Questions: choiceQuestion,
	}, assignmentsAt)
	if err != nil || dispatch == nil {
		t.Fatalf("request = %+v, dispatch %+v, %v", r, dispatch, err)
	}
	c.request = r
	return c
}

// say journals a message the user submitted to agent, and returns its ID.
func (c *conversation) say(agent, text string) int64 {
	c.t.Helper()
	w := Witnessed{Agent: agent, Role: strings.TrimPrefix(agent, "leader@"), Generation: 1, Terminal: "/dev/pts/3", Text: text}
	_, message, err := c.s.ObservedWitnessed(w, "UserPromptSubmit", json.RawMessage(`{}`), assignmentsAt)
	if err != nil {
		c.t.Fatal(err)
	}
	return message
}

// nudge journals an observation of an automated prompt submission, as the
// daemon's nudge is, and returns its entry's ID.
func (c *conversation) nudge() int64 {
	c.t.Helper()
	if err := c.s.Observed(CoordinationLeader, "coordination", 1, "UserPromptSubmit", json.RawMessage(`{}`), assignmentsAt); err != nil {
		c.t.Fatal(err)
	}
	entries := journaledKinds(c.t, c.s, KindObservation)
	return entries[len(entries)-1].ID
}

func (c *conversation) present(r DecisionRequest) DecisionRequest {
	c.t.Helper()
	shown, err := c.s.DecisionPresented(r.Request, r.Version, CoordinationLeader, assignmentsAt)
	if err != nil {
		c.t.Fatal(err)
	}
	return shown
}

func (c *conversation) answer(r DecisionRequest, cites int64) (DecisionRecord, DecisionRequest, *Dispatch, error) {
	c.t.Helper()
	return c.s.ConversationAnswerRecorded(DecisionRecord{
		Job: c.job, Leader: CoordinationLeader, Choice: "in-memory", Request: r.Request, Version: r.Version,
		Cites: cites, Reading: "the user picked the in-memory cache",
	}, 1, assignmentsAt.Add(time.Minute))
}

func TestAChoiceIsPresentedOnceAndAnsweredByTheUsersOwnMessageWithCoordinationsReading(t *testing.T) {
	c := newConversation(t)

	// The answer surface is fixed when the version is made, and an answer
	// waits for the version to be shown.
	if c.request.Surface != SurfaceConversation || c.request.Shown != 0 {
		t.Fatalf("request = %+v", c.request)
	}
	early := c.say(CoordinationLeader, "in-memory please")
	if _, _, _, err := c.answer(c.request, early); err == nil || !strings.Contains(err.Error(), "has not been presented") {
		t.Fatalf("an answer before the version was shown = %v", err)
	}
	if _, err := c.s.DecisionPresented(c.request.Request, 1, owner, assignmentsAt); err == nil || !strings.Contains(err.Error(), "only "+CoordinationLeader) {
		t.Fatalf("another agent presenting = %v", err)
	}

	shown := c.present(c.request)
	if shown.Shown == 0 {
		t.Fatalf("presented = %+v", shown)
	}
	// Presented once.
	if _, err := c.s.DecisionPresented(c.request.Request, 1, CoordinationLeader, assignmentsAt); err == nil || !strings.Contains(err.Error(), "presented already") {
		t.Fatalf("presenting again = %v", err)
	}
	if n := len(journaledKinds(t, c.s, KindDecisionPresented)); n != 1 {
		t.Fatalf("%d presentations journaled, want 1", n)
	}

	// The message the user sent before the version was shown is not its answer.
	if _, _, _, err := c.answer(shown, early); err == nil {
		t.Fatal("a message from before the version was shown answered it")
	}
	c.present(c.request)
	answered := c.say(CoordinationLeader, "go with in-memory, redis is overkill")
	record, r, dispatch, err := c.answer(shown, answered)
	if err != nil {
		t.Fatal(err)
	}

	if record.Source != SourceHuman || record.Cites != answered || record.Request != r.Request || record.Version != 1 || record.Reading != "the user picked the in-memory cache" || record.Choice != "in-memory" {
		t.Errorf("record = %+v", record)
	}
	if !strings.Contains(record.Text("", ""), "the user picked the in-memory cache") || !strings.Contains(record.Text("", ""), "version 1 of decision request") {
		t.Errorf("the record reads %q", record.Text("", ""))
	}
	if r.State != RequestAnswered || r.Answer != "go with in-memory, redis is overkill" || r.AnsweredBy != ToUser || r.Cites != answered || r.Reading != record.Reading || r.Record != record.ID {
		t.Errorf("answered request = %+v", r)
	}
	if read, err := c.s.DecisionRecord(record.ID); err != nil || read.Reading != record.Reading || read.Request != r.Request {
		t.Errorf("the record read back is %+v (%v)", read, err)
	}
	if pending, err := c.s.PendingDecisions(); err != nil || len(pending) != 0 {
		t.Errorf("pending = %+v (%v)", pending, err)
	}

	// The owning leader receives the user's words and the reading together.
	if dispatch == nil || dispatch.Agent != owner {
		t.Fatalf("dispatch = %+v", dispatch)
	}
	inbox, err := c.s.Inbox(owner, dispatch.ID, dispatch.ID, assignmentsAt)
	if err != nil || len(inbox) != 1 {
		t.Fatalf("owning leader's inbox = %+v (%v)", inbox, err)
	}
	m := inbox[0]
	for _, want := range []string{"go with in-memory, redis is overkill", "the user picked the in-memory cache"} {
		if m.Kind != MessageDecisionAnswer || m.Sender != CoordinationLeader || !strings.Contains(m.Body, want) {
			t.Errorf("message %+v lacks %q", m, want)
		}
	}
	if m.RequestData == nil || m.RequestData.Cites != answered || !strings.Contains(m.RequestData.Text(), "Coordination's reading") {
		t.Errorf("the message's request = %+v", m.RequestData)
	}

	// The journal holds the decision and the answer, citing the same message.
	var journaled DecisionRecord
	if entries := journaledKinds(t, c.s, KindDecisionRecorded); len(entries) != 1 || json.Unmarshal(entries[0].Data, &journaled) != nil || journaled.Cites != answered || journaled.Reading != record.Reading {
		t.Errorf("decisions journaled = %+v", entries)
	}
	if n := len(journaledKinds(t, c.s, KindDecisionAnswered)); n != 1 {
		t.Errorf("%d answers journaled, want 1", n)
	}

	// An answered version takes no second answer.
	if _, _, _, err := c.answer(shown, c.say(CoordinationLeader, "actually redis")); err == nil || !strings.Contains(err.Error(), "answered, not pending") {
		t.Errorf("a second answer = %v", err)
	}
}

// requireRefusedAndPresentedAgain checks that err refused the record and
// cleared the presentation of the current version, naming it.
func (c *conversation) requireRefusedAndPresentedAgain(err error, current DecisionRequest, reason string) {
	c.t.Helper()
	var refused *AnswerRefused
	if !errors.As(err, &refused) {
		c.t.Fatalf("error = %v, want a refusal", err)
	}
	if !strings.Contains(refused.Reason, reason) {
		c.t.Errorf("refused for %q, want %q", refused.Reason, reason)
	}
	if refused.Current.Request != current.Request || refused.Current.Version != current.Version || refused.Current.Shown != 0 {
		c.t.Errorf("current version = %+v, want version %d to present again", refused.Current, current.Version)
	}
	for _, want := range []string{"present the current version to the user again", "asmai decision present", current.Questions[0].Title} {
		if !strings.Contains(err.Error(), want) {
			c.t.Errorf("the refusal %q lacks %q", err.Error(), want)
		}
	}
	read, readErr := c.s.DecisionRequest(current.Request)
	if readErr != nil || read.State != RequestPending || read.Shown != 0 || read.Version != current.Version {
		c.t.Errorf("after the refusal the request is %+v (%v)", read, readErr)
	}
	if records, recordsErr := c.s.DecisionRecords(c.job); recordsErr != nil || len(records) != 0 {
		c.t.Errorf("the refusal left records %+v (%v)", records, recordsErr)
	}
}

func TestACitationOfANudgeIsRefusedAndTheCurrentVersionIsPresentedAgain(t *testing.T) {
	c := newConversation(t)
	c.present(c.request)
	nudge := c.nudge()

	_, _, dispatch, err := c.answer(c.request, nudge)
	c.requireRefusedAndPresentedAgain(err, c.request, "not a witnessed message")
	if dispatch != nil {
		t.Errorf("the refused record was sent to %+v", dispatch)
	}
	if entries := journaledKinds(t, c.s, KindDecisionRefused); len(entries) != 1 {
		t.Errorf("%d refusals journaled, want 1", len(entries))
	}
	if n := len(journaledKinds(t, c.s, KindDecisionAnswered)); n != 0 {
		t.Errorf("%d answers journaled", n)
	}

	// Nothing more is taken until the version is shown again, once.
	reply := c.say(CoordinationLeader, "in-memory")
	if _, _, _, err := c.answer(c.request, reply); err == nil || !strings.Contains(err.Error(), "has not been presented") {
		t.Fatalf("an answer before the version was shown again = %v", err)
	}
	shown := c.present(c.request)
	if shown.Shown <= reply {
		t.Fatalf("presented again at entry %d, not after the message %d", shown.Shown, reply)
	}
	if _, _, _, err := c.answer(shown, reply); err == nil {
		t.Fatal("a message from before the version was shown again answered it")
	}
	c.present(c.request)
	if _, r, _, err := c.answer(c.request, c.say(CoordinationLeader, "in-memory, please")); err != nil || r.State != RequestAnswered {
		t.Fatalf("the user's message after the second presentation = %+v, %v", r, err)
	}

	// Neither a missing entry nor any other entry stands for the user.
	d := newConversation(t)
	d.present(d.request)
	_, _, _, err = d.answer(d.request, 9999)
	d.requireRefusedAndPresentedAgain(err, d.request, "does not exist")
}

func TestAMessageTypedAgainstAnOlderVersionIsRefusedAndTheCurrentVersionIsPresentedAgain(t *testing.T) {
	c := newConversation(t)
	first := c.present(c.request)
	typed := c.say(CoordinationLeader, "in-memory")

	// Material new evidence arrives: the requester issues a new version.
	second, _, err := c.s.DecisionRequested(DecisionRequestInput{
		Request: first.Request, Job: c.job, Requester: owner, Notify: CoordinationLeader,
		Kind: RequestChoice, Content: "redis is already deployed", Questions: choiceQuestion,
	}, assignmentsAt)
	if err != nil || second.Version != 2 || second.Shown != 0 {
		t.Fatalf("second = %+v, %v", second, err)
	}
	if read, err := c.s.DecisionRequest(first.Request); err != nil || read.Version != 2 {
		t.Fatalf("latest = %+v, %v", read, err)
	}

	// The reply to version 1 answers neither version.
	c.present(second)
	_, _, _, err = c.answer(second, typed)
	c.requireRefusedAndPresentedAgain(err, second, "typed against an older version")
	c.present(second)
	_, _, _, err = c.answer(first, typed)
	c.requireRefusedAndPresentedAgain(err, second, "was replaced")

	// The user's message after version 2 was shown does.
	c.present(second)
	record, r, _, err := c.answer(second, c.say(CoordinationLeader, "redis, then"))
	if err != nil || r.Version != 2 || record.Version != 2 || r.Answer != "redis, then" {
		t.Fatalf("record = %+v, request = %+v, %v", record, r, err)
	}
	if n := len(journaledKinds(t, c.s, KindDecisionRefused)); n != 2 {
		t.Errorf("%d refusals journaled, want 2", n)
	}
}

func TestOnlyAMessageToCoordinationInAPresentedChoiceIsTheUsersAnswer(t *testing.T) {
	c := newConversation(t)
	shown := c.present(c.request)
	toCoordination := c.say(CoordinationLeader, "in-memory")
	toEngineering := c.say(owner, "in-memory")
	answer := func(mutate func(*DecisionRecord)) error {
		d := DecisionRecord{Job: c.job, Leader: CoordinationLeader, Choice: "in-memory", Request: shown.Request, Version: 1, Cites: toCoordination, Reading: "in-memory"}
		mutate(&d)
		_, _, _, err := c.s.ConversationAnswerRecorded(d, 1, assignmentsAt)
		return err
	}
	for _, tt := range []struct {
		name   string
		mutate func(*DecisionRecord)
		want   string
	}{
		{"a message to another agent", func(d *DecisionRecord) { d.Cites = toEngineering }, "not in the conversation"},
		{"no reading", func(d *DecisionRecord) { d.Reading = " " }, "reading"},
		{"no citation", func(d *DecisionRecord) { d.Cites = 0 }, "cites the witnessed message"},
		{"no choice", func(d *DecisionRecord) { d.Choice = "" }, "its choice"},
		{"another agent as the leader", func(d *DecisionRecord) { d.Leader = owner }, "only " + CoordinationLeader},
		{"a request that does not exist", func(d *DecisionRecord) { d.Request = 99 }, "does not exist"},
		{"a version that does not exist", func(d *DecisionRecord) { d.Version = 5 }, "no version 5"},
		{"another job", func(d *DecisionRecord) { d.Job = c.job + 1 }, "about job"},
	} {
		if err := answer(tt.mutate); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v, want a refusal naming %q", tt.name, err, tt.want)
		} else if errors.As(err, new(*AnswerRefused)) {
			t.Errorf("%s: a plain mistake cleared the presentation", tt.name)
		}
	}
	// None of those changed the version: it is still shown and pending.
	if read, err := c.s.DecisionRequest(shown.Request); err != nil || read.Shown != shown.Shown || read.State != RequestPending {
		t.Fatalf("request = %+v, %v", read, err)
	}
	if err := answer(func(*DecisionRecord) {}); err != nil {
		t.Fatalf("the user's message to Coordination = %v", err)
	}
}

func TestADecisionRequestAnsweredInLavishOrToAnAgentIsNotAnsweredInTheConversation(t *testing.T) {
	c := newConversation(t)
	lavish, _, err := c.s.DecisionRequested(DecisionRequestInput{
		Job: c.job, Requester: owner, Recipient: ToUser, Notify: CoordinationLeader,
		Kind: RequestReview, Questions: choiceQuestion,
	}, assignmentsAt)
	if err != nil || lavish.Surface != SurfaceLavish {
		t.Fatalf("lavish = %+v, %v", lavish, err)
	}
	if _, err := c.s.DecisionPresented(lavish.Request, 1, CoordinationLeader, assignmentsAt); err == nil || !strings.Contains(err.Error(), "Lavish") {
		t.Errorf("presenting a Lavish request in the conversation = %v", err)
	}
	said := c.say(CoordinationLeader, "in-memory")
	if _, _, _, err := c.answer(lavish, said); err == nil || !strings.Contains(err.Error(), "Lavish") {
		t.Errorf("answering a Lavish request in the conversation = %v", err)
	}

	a, _ := newAssignment(t, c.s, c.job)
	worker, _, err := c.s.DecisionRequested(workerRequest(a), assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.s.DecisionPresented(worker.Request, 1, CoordinationLeader, assignmentsAt); err == nil || !strings.Contains(err.Error(), "not to the user") {
		t.Errorf("presenting a worker's request = %v", err)
	}
	if _, _, _, err := c.answer(worker, said); err == nil || !strings.Contains(err.Error(), "not to the user") {
		t.Errorf("recording the user's answer to a worker's request = %v", err)
	}
}

func TestAReadingAndAVersionBelongOnlyToAHumanAnswerOfARequest(t *testing.T) {
	c := newConversation(t)
	base := DecisionRecord{Job: c.job, Leader: owner, Source: SourceDelegated, ReliedOn: "the mandate", Choice: "x", Rationale: "y", Evidence: "z"}
	for _, tt := range []struct {
		name   string
		mutate func(*DecisionRecord)
		want   string
	}{
		{"a reading with no request", func(d *DecisionRecord) { d.Reading = "r" }, "belong to an answer"},
		{"a version with no request", func(d *DecisionRecord) { d.Version = 1 }, "belong to an answer"},
		{"a delegated answer", func(d *DecisionRecord) { d.Request, d.Version, d.Reading = 1, 1, "r" }, "only a human decision answers"},
		{"a human answer with no reading", func(d *DecisionRecord) { d.Source, d.Cites, d.Request, d.Version = SourceHuman, 1, 1, 1 }, "reading"},
	} {
		d := base
		tt.mutate(&d)
		if err := d.Check(); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v, want a refusal naming %q", tt.name, err, tt.want)
		}
	}
}
