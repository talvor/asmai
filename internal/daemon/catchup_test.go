// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/providers"
	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/store"
)

// kindsOf returns the kinds of events, in order.
func kindsOf(events []Event) []string {
	var kinds []string
	for _, e := range events {
		kinds = append(kinds, e.Kind)
	}
	return kinds
}

// eventOf returns the first event of kind.
func eventOf(t *testing.T, events []Event, kind string) Event {
	t.Helper()
	for _, e := range events {
		if e.Kind == kind {
			return e
		}
	}
	t.Fatalf("the events %+v hold none of kind %s", events, kind)
	return Event{}
}

// With nothing to tell, and for a user who never left, the catch-up holds no
// event and offers no text.
func TestTheCatchUpHoldsNothingWhenNothingChangedAndNothingWaits(t *testing.T) {
	f := newTwoResults(t)
	f.withWorkers(t)
	c, err := f.d.catchUp()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Events) != 0 || len(c.Unsent) != 0 || c.LavishPort != 0 {
		t.Errorf("the catch-up is %+v, want nothing in it", c)
	}
}

// The catch-up tells of what waits on the user, with the Lavish link of a
// request answered there, of the jobs that ended or failed since the user
// last left and the job paused, of the holds with their causes, of the
// agents at a native prompt in their own terminals, of the input the user
// still holds, and of the text they left unsent, in the order the spec lists
// them (04 rule 17).
func TestTheCatchUpTellsOfEverythingThatChangedAndWaits(t *testing.T) {
	f := newLavishFactory(t)
	coordination := roles.LeaderOf(roles.Coordination).String()
	left := time.Now().Add(-time.Hour)
	if err := f.s.ConversationLeft(coordination, roles.Coordination, 1, "/dev/pts/test", LeftByDetaching, InputAutomation, "half a thought", left); err != nil {
		t.Fatal(err)
	}

	// Waiting on the user: a request answered in Lavish, a choice in the
	// conversation, and Coordination's own native prompt.
	page := f.request(t, 0, "Several questions")
	asked, _, err := f.d.decisionRequest(Request{Session: "credential", Command: CommandDecisionRequest, Job: f.job.Number, Kind: store.RequestChoice, Questions: cacheQuestion})
	if err != nil {
		t.Fatal(err)
	}
	f.d.leaders[coordination].prompt = &WaitingPrompt{Agent: coordination, Generation: 1, About: "Bash `rm -rf build`"}

	// Jobs: one that failed, one the user paused, and one the user
	// cancelled, so it finished.
	other := f.openJob(t, "add a farewell")
	if _, _, err := f.s.ChecksObserved(store.ChecksObservation{Job: other.Number, Number: 7, Head: "abc", Verdict: "red", Checks: []store.CheckState{{Name: "test", State: "failure"}}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	paused := f.openJob(t, "add a salutation")
	f.setJobState(t, paused.Number, store.JobPaused)
	cancelled := f.openJob(t, "add a goodbye")
	if _, err := f.s.JobCancelled(store.JobCancel{Job: cancelled.Number, Witness: cancelled.Witness}, time.Now()); err != nil {
		t.Fatal(err)
	}

	// Holds, native prompts in their own terminals, held input.
	workers := []string{f.assignments[0].Worker, f.assignments[1].Worker}
	f.d.leaders[workers[0]].prompt = &WaitingPrompt{Agent: workers[0], Generation: 1, About: "Bash `cat > x`"}
	f.d.leaders[workers[1]].inputUnknown = true
	f.d.leaders[coordination].inputUnknown = false

	c, err := f.d.catchUp()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{EventWaiting, EventWaiting, EventWaiting, EventFinished, EventFailed, EventPaused, EventHold, EventTerminal}
	if got := kindsOf(c.Events); !slices.Equal(got, want) {
		t.Fatalf("the catch-up tells of %v, want %v:\n%+v", got, want, c.Events)
	}
	waiting := []Event{c.Events[0], c.Events[1], c.Events[2]}
	if !strings.Contains(waiting[0].Text, "native prompt the user answers in the conversation") {
		t.Errorf("the first event is %+v, want Coordination's native prompt", waiting[0])
	}
	if got := fmt.Sprintf("job %d: decision request %d waits on you in the conversation", f.job.Number, asked.Request); waiting[1].Text != got && waiting[2].Text != got {
		t.Errorf("the waiting events are %+v, want one saying %q", waiting, got)
	}
	var link Event
	for _, e := range waiting {
		if e.Link != "" {
			link = e
		}
	}
	if link.Link != page.PageURL || !strings.Contains(link.Text, fmt.Sprintf("decision request %d waits on you in Lavish", page.Request)) {
		t.Errorf("the request answered in Lavish is %+v, want its page %s", link, page.PageURL)
	}
	if c.LavishPort != f.port {
		t.Errorf("the catch-up names Lavish port %d, want %d for the forwarding command", c.LavishPort, f.port)
	}
	if e := eventOf(t, c.Events, EventFinished); e.Job != cancelled.Number || !strings.Contains(e.Text, "was cancelled") {
		t.Errorf("the finished job is %+v, want job %d", e, cancelled.Number)
	}
	if e := eventOf(t, c.Events, EventFailed); e.Job != other.Number || !strings.Contains(e.Text, "checks on its pull request are red") {
		t.Errorf("the failed job is %+v, want job %d", e, other.Number)
	}
	if e := eventOf(t, c.Events, EventPaused); e.Job != paused.Number {
		t.Errorf("the paused job is %+v, want job %d", e, paused.Number)
	}
	if e := eventOf(t, c.Events, EventHold); e.Agent != workers[1] || !strings.Contains(e.Text, "does not know") {
		t.Errorf("the hold is %+v, want %s held with its cause", e, workers[1])
	}
	if e := eventOf(t, c.Events, EventTerminal); e.Agent != workers[0] || !strings.Contains(e.Text, "waits at a native prompt nobody can answer") {
		t.Errorf("the agent in its own terminal is %+v, want %s", e, workers[0])
	}
	if len(c.Unsent) != 1 || c.Unsent[0].Text != "half a thought" {
		t.Errorf("the catch-up offers %+v, want the text the user left unsent", c.Unsent)
	}

	// Once the user has left again, what happened before is no news, and
	// nothing was left unsent this time.
	if err := f.s.ConversationLeft(coordination, roles.Coordination, 1, "/dev/pts/test", LeftByDetaching, InputAutomation, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	c, err = f.d.catchUp()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range c.Events {
		if e.Kind == EventFinished || e.Kind == EventFailed {
			t.Errorf("after the user left again, the catch-up still tells of %+v", e)
		}
	}
	if len(c.Unsent) != 0 {
		t.Errorf("after leaving with nothing unsent, the catch-up offers %+v", c.Unsent)
	}
}

// An agent whose input the user still holds, and an assignment whose dispatch
// has an unknown outcome, are told of.
func TestTheCatchUpTellsOfInputTheUserStillHoldsAndAnUnknownOutcome(t *testing.T) {
	f := newTwoResults(t)
	f.withWorkers(t)
	worker := f.assignments[0].Worker
	f.d.leaders[worker].userInput = true
	c, err := f.d.catchUp()
	if err != nil {
		t.Fatal(err)
	}
	if e := eventOf(t, c.Events, EventInput); e.Agent != worker || e.Text != "you still hold "+worker+"'s input" {
		t.Errorf("the input held is %+v, want %s's", e, worker)
	}

	// The conversation's own input is the user's by being in it.
	coordination := roles.LeaderOf(roles.Coordination).String()
	f.d.leaders[worker].userInput = false
	f.d.leaders[coordination].userInput = true
	f.d.conversation = &conversation{leader: f.d.leaders[coordination], session: f.d.leaders[coordination].session, generation: 1, terminal: "/dev/pts/test"}
	if c, err = f.d.catchUp(); err != nil || slices.Contains(kindsOf(c.Events), EventInput) {
		t.Errorf("with the user in the conversation, the catch-up is %+v (%v), want no input held", c, err)
	}
}

// A lost sign-in is told of with the provider's own sign-in command (04 rule
// 18); a sign-in that holds is not.
func TestTheCatchUpTellsOfALostSignInWithItsNativeCommand(t *testing.T) {
	f := newTwoResults(t)
	f.withWorkers(t)
	signedIn := func(loggedIn bool) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "claude")
		script := fmt.Sprintf("#!/bin/sh\necho '{\"loggedIn\":%t,\"authMethod\":\"claude.ai\"}'\n", loggedIn)
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		f.d.cfg.Pins = testPins("2.1.292")
		if _, err := f.s.ProviderInstalled(store.ProviderInstall{Name: providers.ClaudeCode, Version: "2.1.292", Path: path, SHA256: "x", InstalledAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	signedIn(true)
	if c, err := f.d.catchUp(); err != nil || len(c.Events) != 0 {
		t.Fatalf("with the sign-in held, the catch-up is %+v (%v), want nothing", c, err)
	}
	signedIn(false)
	c, err := f.d.catchUp()
	if err != nil {
		t.Fatal(err)
	}
	e := eventOf(t, c.Events, EventSignIn)
	if !strings.HasSuffix(e.Command, "/claude auth login") || !strings.Contains(e.Text, "signed out") {
		t.Errorf("the lost sign-in is %+v, want Claude Code's own `auth login` command", e)
	}
}

// openJob opens a job in the fixture's repository from the user's words.
func (f twoResults) openJob(t *testing.T, words string) store.Job {
	t.Helper()
	witness := f.userSays(t, words)
	job, err := f.s.JobOpened(store.Job{Repository: "fixture", Witness: witness, Reading: words, Mandate: store.MandateTestedPR, Criteria: []string{"done"}}, false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return job
}

// setJobState sets a job's state, as the pause that M5 adds will.
func (f twoResults) setJobState(t *testing.T, job int64, state string) {
	t.Helper()
	db, err := sql.Open("sqlite3", f.d.cfg.Paths.Store)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE jobs SET state = ? WHERE number = ?`, state, job); err != nil {
		t.Fatal(err)
	}
}
