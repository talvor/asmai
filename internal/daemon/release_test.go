// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bufio"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/store"
)

// released runs a release of the worker's input and returns the daemon's
// answer.
func (w takenWorker) released(t *testing.T, note string) Response {
	t.Helper()
	server, client := connectedPair(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.f.d.release(server, Request{Command: CommandRelease, Agent: w.name, Terminal: "/dev/pts/9", Note: note})
	}()
	resp := answered(t, bufio.NewReader(client))
	<-done
	return resp
}

// hold takes the worker's input at a boundary.
func (w takenWorker) hold(t *testing.T) (client interface{ Close() error }, done chan struct{}) {
	t.Helper()
	w.boundary()
	c, r, d := w.taking(t, "/dev/pts/7", false)
	if resp := answered(t, r); resp.Error != "" {
		t.Fatalf("taking was answered %+v", resp)
	}
	return c, d
}

func (w takenWorker) typedMessage(t *testing.T, text string) {
	t.Helper()
	_, _, err := w.f.s.ObservedWitnessed(store.Witnessed{Agent: w.name, Role: roles.Engineering, Generation: w.l.generation, Terminal: "/dev/pts/7", Text: text}, "UserPromptSubmit", json.RawMessage(`{"prompt":"x"}`), time.Now())
	if err != nil {
		t.Fatal(err)
	}
}

// reopened has the owner reject the worker's result, which makes the
// assignment active again with a dispatch of the worker's to work on.
func (w takenWorker) reopened(t *testing.T) {
	t.Helper()
	a := w.f.assignments[0]
	if _, _, err := w.f.s.ResultRejected(a.ID, a.Owner, []string{"not yet"}, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func (w takenWorker) dispatchAndAssignment(t *testing.T) (store.WorkerDispatch, store.Assignment) {
	t.Helper()
	d, err := w.f.s.WorkerDispatch(w.f.assignments[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	return d, d.Assignment
}

// If the user only answered native prompts, the same dispatch continues:
// nothing is superseded, nobody is sent a record, and the input is back with
// automation.
func TestReleaseAfterOnlyPromptAnswersLetsTheSameDispatchContinue(t *testing.T) {
	w := newTakenWorker(t, "")
	w.reopened(t)
	client, done := w.hold(t)
	before, _ := w.dispatchAndAssignment(t)
	if err := w.f.s.PromptAnsweredByUser(store.PromptAnswer{Agent: w.name, Generation: w.l.generation, Terminal: "/dev/pts/7", Prompt: "run it?", Dispatch: before.Dispatch.ID, Keys: "y\r"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	resp := w.released(t, "")
	if resp.Error != "" || resp.Released == nil || resp.Released.Intervention.Typed() || len(resp.Released.Intervention.Answers) != 1 || resp.Released.Superseded != 0 {
		t.Fatalf("the release was answered %+v", resp)
	}
	after, a := w.dispatchAndAssignment(t)
	// The nudges the hold kept back go on, in the same dispatch.
	if after.Dispatch.ID != before.Dispatch.ID || after.Dispatch.State == store.DispatchStopped || a.State != store.AssignmentActive {
		t.Errorf("the dispatch went from %+v to %+v with the assignment %s, want the same dispatch, not stopped, and the assignment active", before.Dispatch, after.Dispatch, a.State)
	}
	if got := takeJournal(t, w.f.s, store.KindAssignmentNeedsReconciliation); len(got) != 0 {
		t.Errorf("the journal holds %v, want no reconciliation", got)
	}
	w.f.d.mu.Lock()
	taken, userInput := w.l.taken, w.l.userInput
	w.f.d.mu.Unlock()
	if taken != nil || userInput {
		t.Errorf("the agent is still held after the release: taken %v, user input %v", taken, userInput)
	}
	if status := w.f.d.inputOwnerOf(t, w.name); status == InputUser {
		t.Errorf("the input is still with the %s", status)
	}
	client.Close()
	<-done
}

func (d *daemon) inputOwnerOf(t *testing.T, agent string) string {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.leaders[agent].inputOwner()
}

// If the user typed any message, the dispatch is superseded, the assignment
// needs reconciliation and the owning leader receives the full record; the
// leader's redispatch is a new dispatch.
func TestReleaseAfterATypedMessageSupersedesTheDispatchAndSendsTheRecordToTheOwner(t *testing.T) {
	w := newTakenWorker(t, "automated draft")
	w.reopened(t)
	w.working(1)
	client, r, done := w.taking(t, "/dev/pts/7", true)
	if resp := answered(t, r); resp.Error != "" {
		t.Fatalf("taking was answered %+v", resp)
	}
	before, a := w.dispatchAndAssignment(t)
	w.typedMessage(t, "use the other approach,\nplease")
	if err := w.f.s.PromptAnsweredByUser(store.PromptAnswer{Agent: w.name, Generation: w.l.generation, Terminal: "/dev/pts/7", Prompt: "run the formatter?", Dispatch: before.Dispatch.ID, Keys: "y\r"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := w.f.s.ChangeDispatch(before.Dispatch.ID, 1, store.DispatchCreated, store.DispatchWorking, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := w.f.s.EffectRecorded(w.name, w.l.generation, before.Dispatch.ID, "commit", "abc123", time.Now()); err != nil {
		t.Fatal(err)
	}
	resp := w.released(t, "keep it small")
	if resp.Error != "" || resp.Released == nil {
		t.Fatalf("the release was answered %+v", resp)
	}
	got := resp.Released
	if got.Superseded != before.Dispatch.ID || got.Assignment != a.ID || got.Owner != a.Owner || got.Told.Agent != a.Owner {
		t.Fatalf("the release was %+v, want dispatch %d superseded, assignment %d reconciled and %s told", got, before.Dispatch.ID, a.ID, a.Owner)
	}
	after, now := w.dispatchAndAssignment(t)
	if after.Dispatch.State != store.DispatchStopped || now.State != store.AssignmentNeedsReconciliation {
		t.Errorf("the dispatch is %s and the assignment %s, want it stopped and needing reconciliation", after.Dispatch.State, now.State)
	}
	superseded := false
	for _, change := range takeJournal(t, w.f.s, store.KindDispatchChanged) {
		superseded = superseded || change["closed"] == store.ClosedSuperseded && change["id"] == float64(before.Dispatch.ID)
	}
	if !superseded {
		t.Errorf("no dispatch change closes dispatch %d as superseded", before.Dispatch.ID)
	}
	inbox, err := w.f.s.Inbox(a.Owner, 0, got.Told.ID, time.Now())
	if err != nil || len(inbox) == 0 {
		t.Fatalf("the owner's inbox: %v, %v", inbox, err)
	}
	var m *store.Message
	for i := range inbox {
		if inbox[i].Kind == store.MessageIntervention {
			m = &inbox[i]
		}
	}
	if m == nil {
		t.Fatalf("the owner was sent %+v, want an intervention message", inbox)
	}
	for _, want := range []string{"use the other approach,\n    please", "run the formatter?", `"y\r"`, "automated draft", "commit abc123", "keep it small", "asmai redispatch --assignment"} {
		if !strings.Contains(m.Body, want) {
			t.Errorf("the intervention record lacks %q:\n%s", want, m.Body)
		}
	}
	redispatched, err := w.f.s.AssignmentRedispatched(a.ID, a.Owner, []string{"the user corrected the approach"}, time.Now())
	if err != nil {
		t.Fatalf("redispatching after the release: %v", err)
	}
	if redispatched.ID <= before.Dispatch.ID || redispatched.Agent != w.name {
		t.Errorf("the redispatch is %+v, want a new dispatch of %s", redispatched, w.name)
	}
	released := takeJournal(t, w.f.s, store.KindInputReleased)
	if len(released) != 1 || released[0]["note"] != "keep it small" || released[0]["superseded"] != float64(before.Dispatch.ID) {
		t.Errorf("the journal records the release as %v", released)
	}
	client.Close()
	<-done
}

// The record is of one hold: a second hold's record starts afresh.
func TestAnInterventionRecordCoversOnlyTheHoldItEnds(t *testing.T) {
	w := newTakenWorker(t, "")
	client, done := w.hold(t)
	w.typedMessage(t, "first hold")
	if resp := w.released(t, ""); resp.Error != "" {
		t.Fatal(resp.Error)
	}
	client.Close()
	<-done
	client, done = w.hold(t)
	resp := w.released(t, "")
	if resp.Error != "" || resp.Released.Intervention.Typed() {
		t.Fatalf("the second release was %+v, want no messages from the first hold", resp)
	}
	client.Close()
	<-done
}

func TestReleaseRefusesAnAgentTheUserDoesNotHold(t *testing.T) {
	w := newTakenWorker(t, "")
	resp := w.released(t, "")
	if !strings.Contains(resp.Error, "you do not hold") {
		t.Errorf("releasing an agent not held was answered %+v", resp)
	}
}

// Only the user releases: an agent's credential is refused.
func TestAnAgentsCredentialCannotRelease(t *testing.T) {
	f := newTwoResults(t)
	f.withWorkers(t)
	for _, credential := range []string{"worker0", "coordination"} {
		err := f.d.authorize(Request{Command: CommandRelease, Agent: f.assignments[1].Worker, Session: credential})
		if err == nil || !strings.Contains(err.Error(), "an agent cannot run this command; ask the user to run `asmai release`") {
			t.Errorf("%s's credential releasing gave %v, want it refused", credential, err)
		}
	}
	if err := f.d.authorize(Request{Command: CommandRelease, Agent: f.assignments[1].Worker}); err != nil {
		t.Errorf("the user releasing gave %v, want it allowed", err)
	}
}
