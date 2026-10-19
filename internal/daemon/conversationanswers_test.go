// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/store"
)

// userSays journals a message the user submitted to Coordination's leader
// from their own keyboard, and returns its ID.
func (f twoResults) userSays(t *testing.T, text string) int64 {
	t.Helper()
	w := store.Witnessed{Agent: "leader@coordination", Role: "coordination", Generation: 1, Terminal: "/dev/pts/test", Text: text}
	_, message, err := f.s.ObservedWitnessed(w, "UserPromptSubmit", json.RawMessage(`{}`), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func TestAChoiceBetweenTwoApproachesIsPresentedOnceAndTheUsersOwnMessageAnswersIt(t *testing.T) {
	f := newTwoResults(t)
	f.withWorkers(t)
	r, _, err := f.d.decisionRequest(Request{Session: "credential", Command: CommandDecisionRequest, Job: f.job.Number, Kind: store.RequestChoice, Questions: cacheQuestion, Blocks: []int64{f.assignments[0].ID}})
	if err != nil {
		t.Fatal(err)
	}
	present := func(session string) error {
		_, err := f.d.decisionPresent(Request{Session: session, Command: CommandDecisionPresent, Request: r.Request, Version: 1})
		return err
	}
	answer := func(session string, cites int64) (store.DecisionRecord, store.DecisionRequest, *store.Dispatch, error) {
		return f.d.conversationAnswer(Request{Session: session, Command: CommandDecisionRecord, Request: r.Request, Version: 1, Cites: cites, Choice: "in-memory", Reading: "the user chose the in-memory cache"})
	}

	// The work that depends on the choice waits for it.
	if _, err := f.decide(CommandAccept, f.assignments[0].ID, "file-1.txt is there"); err == nil || !strings.Contains(err.Error(), "waits for decision request") {
		t.Fatalf("accepting the held work = %v", err)
	}

	// Only Coordination presents a choice to the user, once.
	if err := present("credential"); err == nil || !strings.Contains(err.Error(), "only leader@coordination") {
		t.Fatalf("Engineering presenting = %v", err)
	}
	if err := present("worker0"); err == nil {
		t.Fatal("a worker presented a decision to the user")
	}
	if err := present("coordination"); err != nil {
		t.Fatal(err)
	}
	if err := present("coordination"); err == nil || !strings.Contains(err.Error(), "presented already") {
		t.Fatalf("presenting twice = %v", err)
	}

	// Nobody but Coordination supplies the user's answer, and a nudge is not
	// the user's message: it is refused and the current version is shown again.
	said := f.userSays(t, "go with in-memory")
	for _, session := range []string{"credential", "worker0"} {
		if _, _, _, err := answer(session, said); err == nil || !strings.Contains(err.Error(), "only leader@coordination") {
			t.Fatalf("%s recording the user's answer = %v", session, err)
		}
	}
	if err := f.s.Observed("leader@coordination", "coordination", 1, "UserPromptSubmit", json.RawMessage(`{}`), time.Now()); err != nil {
		t.Fatal(err)
	}
	observed, err := f.s.Journal()
	if err != nil {
		t.Fatal(err)
	}
	nudge := observed[len(observed)-1].ID
	_, _, _, err = answer("coordination", nudge)
	var refusal *userError
	if !errors.As(err, &refusal) || !strings.Contains(err.Error(), "not a witnessed message") || !strings.Contains(err.Error(), "present the current version to the user again") || !strings.Contains(err.Error(), "Which cache?") {
		t.Fatalf("a nudge cited as the user's answer = %v", err)
	}
	if again, err := f.s.DecisionRequest(r.Request); err != nil || again.State != store.RequestPending || again.Shown != 0 {
		t.Fatalf("after the refusal the request is %+v, %v", again, err)
	}

	// Presented again, the user's message after it answers, with the reading.
	if err := present("coordination"); err != nil {
		t.Fatal(err)
	}
	said = f.userSays(t, "go with in-memory")
	// "latest" is the newest message the user sent Coordination.
	record, answered, dispatch, err := f.d.conversationAnswer(Request{Session: "coordination", Command: CommandDecisionRecord, Request: r.Request, Version: 1, LatestWitness: true, Choice: "in-memory", Reading: "the user chose the in-memory cache"})
	if err != nil {
		t.Fatal(err)
	}
	if answered.State != store.RequestAnswered || answered.Answer != "go with in-memory" || answered.Cites != said || answered.Reading != "the user chose the in-memory cache" || answered.Job != f.job.Number {
		t.Errorf("answered = %+v", answered)
	}
	if record.Source != store.SourceHuman || record.Cites != said || record.Reading != answered.Reading || record.Leader != "leader@coordination" {
		t.Errorf("record = %+v", record)
	}
	if dispatch == nil || dispatch.Agent != "leader@engineering" {
		t.Fatalf("dispatch = %+v, want one to the owning leader", dispatch)
	}
	inbox, err := f.s.Inbox("leader@engineering", dispatch.ID, time.Now())
	if err != nil || len(inbox) != 1 || !strings.Contains(inbox[0].Body, "go with in-memory") || !strings.Contains(inbox[0].Body, "the user chose the in-memory cache") {
		t.Fatalf("owning leader's inbox = %+v (%v)", inbox, err)
	}

	// The work that waited goes on, and the user has nothing pending.
	if _, err := f.decide(CommandAccept, f.assignments[0].ID, "file-1.txt is there"); err != nil {
		t.Fatalf("accepting after the answer = %v", err)
	}
	if pending, err := f.s.PendingDecisions(); err != nil || len(pending) != 0 {
		t.Fatalf("pending = %+v, %v", pending, err)
	}
}

func TestOrdinaryWorkAlongASettledRouteRaisesNoDecisionRequest(t *testing.T) {
	f := newTwoResults(t)
	f.withWorkers(t)

	// Accepting a result against its criteria is ordinary authorized work:
	// nothing is asked, nothing is pending, and nobody is nudged.
	before, err := f.s.Journal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.decide(CommandAccept, f.assignments[0].ID, "file-1.txt is there"); err != nil {
		t.Fatal(err)
	}
	after, err := f.s.Journal()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range after[len(before):] {
		if strings.HasPrefix(e.Kind, "decision.") && e.Kind != store.KindDecisionRecorded {
			t.Errorf("settled work journaled %s", e.Kind)
		}
	}
	if pending, err := f.s.PendingDecisions(); err != nil || len(pending) != 0 {
		t.Errorf("pending = %+v, %v", pending, err)
	}
	if requests, err := f.s.JobDecisionRequests(f.job.Number); err != nil || len(requests) != 0 {
		t.Errorf("requests = %+v, %v", requests, err)
	}
}
