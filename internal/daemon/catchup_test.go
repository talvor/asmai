// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/providers"
	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/session"
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
	f.pushedPullRequest(t)
	if _, _, err := f.s.ChecksObserved(store.ChecksObservation{Job: f.job.Number, Number: 7, Head: f.started, Verdict: "red", Checks: []store.CheckState{{Name: "test", State: "failure"}}}, time.Now()); err != nil {
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
	if e := eventOf(t, c.Events, EventFailed); e.Job != f.job.Number || !strings.Contains(e.Text, "checks on its pull request are red") {
		t.Errorf("the failed job is %+v, want job %d", e, f.job.Number)
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
	resubmitted, err := f.decide(CommandReject, f.assignments[0].ID, "the test needs another pass")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.AssignmentNeedsReconciliation(f.assignments[0].ID, resubmitted.Dispatch.ID, "its session cannot be resumed", "Reconcile it.", time.Now()); err != nil {
		t.Fatal(err)
	}
	if current, err := f.s.WorkerDispatch(f.assignments[0].ID); err != nil || current.Dispatch.ID != resubmitted.Dispatch.ID || current.Dispatch.State != store.DispatchUnknown {
		t.Fatalf("the interrupted dispatch is %+v (%v), want an unknown outcome", current, err)
	}
	c, err = f.d.catchUp()
	if err != nil {
		t.Fatal(err)
	}
	status, err := f.d.events(0)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(c.Events, status) {
		t.Fatalf("catch-up events %+v differ from status events %+v", c.Events, status)
	}
	if e := eventOf(t, c.Events, EventHold); e.Job != f.job.Number || e.Agent != f.assignments[0].Worker || !strings.Contains(e.Text, "outcome nobody knows") {
		t.Errorf("the unknown dispatch outcome is %+v", e)
	}
}

// A lost sign-in is told of with the provider's own sign-in command (04 rule
// 18); a sign-in that holds is not.
func TestTheCatchUpTellsOfALostSignInWithItsNativeCommand(t *testing.T) {
	f := newTwoResults(t)
	f.withWorkers(t)
	signedIn := func(loggedIn bool) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "provider with spaces", "claude")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		script := fmt.Sprintf("#!/bin/sh\necho '{\"loggedIn\":%t,\"authMethod\":\"claude.ai\"}'\n", loggedIn)
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		f.d.cfg.Pins = testPins("2.1.292")
		if _, err := f.s.ProviderInstalled(store.ProviderInstall{Name: providers.ClaudeCode, Version: "2.1.292", Path: path, SHA256: "x", InstalledAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		return path
	}
	signedIn(true)
	if c, err := f.d.catchUp(); err != nil || len(c.Events) != 0 {
		t.Fatalf("with the sign-in held, the catch-up is %+v (%v), want nothing", c, err)
	}
	path := signedIn(false)
	c, err := f.d.catchUp()
	if err != nil {
		t.Fatal(err)
	}
	e := eventOf(t, c.Events, EventSignIn)
	if e.Command != providers.ShellQuote(path)+" auth login" || !strings.Contains(e.Text, "signed out") {
		t.Errorf("the lost sign-in is %+v, want Claude Code's own `auth login` command", e)
	}
}

func (f twoResults) pushedPullRequest(t *testing.T) {
	t.Helper()
	branch := jobBranchFor(f.job)
	at := time.Now()
	if err := f.s.ChangeDispatch(f.reported[0].ID, 1, store.DispatchCreated, store.DispatchWorking, "", at); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.JobBranchPushed(store.Push{Job: f.job.Number, Branch: branch, Commit: f.started, Leader: f.assignments[0].Owner, Dispatch: f.reported[0].ID}, 1, nil, at); err != nil {
		t.Fatal(err)
	}
	pr := store.PullRequest{Job: f.job.Number, Number: 7, URL: "https://github.com/talvor/asmai-fixture/pull/7", Title: "Add a greeting", Branch: branch, Head: f.started, State: "OPEN", Leader: f.assignments[0].Owner, Dispatch: f.reported[0].ID}
	if _, _, err := f.s.PullRequestRecorded(pr, 1, pr.URL, at); err != nil {
		t.Fatal(err)
	}
}

func TestTheCatchUpAndStatusTellOfAcceptedNeedsYouFindingsAndFailedStartChecks(t *testing.T) {
	v := newValidationFixture(t)
	found := v.validated(t, store.FindingInput{Kind: store.FindingNeedsYou, Text: "Which greeting?\nThe request is ambiguous."})
	v.d.mu.Lock()
	v.d.held("worker1@quality", []Check{{Name: "provider", Problem: "changed since qualification", Fix: "restore the pinned provider"}})
	v.d.held("leader@research", []Check{{Name: "trust", Problem: "folder is untrusted", Fix: "trust the folder"}})
	v.d.mu.Unlock()
	catchUp, err := v.d.catchUp()
	if err != nil {
		t.Fatal(err)
	}
	status, err := v.d.events(0)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(catchUp.Events, status) {
		t.Fatalf("catch-up events %+v differ from status events %+v", catchUp.Events, status)
	}
	if e := eventOf(t, status, EventWaiting); e.Job != v.job.Number || !strings.Contains(e.Text, fmt.Sprintf("finding %d: Which greeting?", found[0].ID)) {
		t.Errorf("the accepted needs-you finding is %+v", e)
	}
	holds := map[string]string{}
	for _, e := range status {
		if e.Kind == EventHold {
			holds[e.Agent] = e.Text
		}
	}
	if len(holds) != 2 || !strings.Contains(holds["worker1@quality"], "provider: changed since qualification (fix: restore the pinned provider)") || !strings.Contains(holds["leader@research"], "trust: folder is untrusted (fix: trust the folder)") {
		t.Errorf("the failed start-check holds are %+v", holds)
	}
	v.d.mu.Lock()
	address := roles.LeaderOf(roles.Research)
	v.d.leaders[address.String()] = &leader{address: address, session: &session.Session{}}
	if err := v.d.ensureRoleLeader(roles.Research); err != nil {
		v.d.mu.Unlock()
		t.Fatal(err)
	}
	v.d.mu.Unlock()
	status, err = v.d.events(0)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range status {
		if e.Kind == EventHold && e.Agent == address.String() {
			t.Errorf("started agent still has a hold: %+v", e)
		}
	}
	if e := eventOf(t, status, EventHold); e.Agent != "worker1@quality" {
		t.Errorf("starting another agent removed the worker hold: %+v", status)
	}
}

func TestFailedCatchUpReadPreservesLastDepartureAndUnsentText(t *testing.T) {
	f := newTwoResults(t)
	f.withWorkers(t)
	coordination := roles.LeaderOf(roles.Coordination).String()
	if err := f.s.ConversationLeft(coordination, roles.Coordination, 1, "/dev/pts/test", LeftByDetaching, InputAutomation, "draft to recover", time.Now()); err != nil {
		t.Fatal(err)
	}
	last, _, err := f.s.LastLeft()
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", f.d.cfg.Paths.Store)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO journal (at, kind, data) VALUES (?, ?, ?)`, time.Now().UTC().Format(time.RFC3339Nano), store.KindUnsentTextSaved, "{broken"); err != nil {
		t.Fatal(err)
	}
	s, err := session.Start(session.Spec{Path: "/bin/sleep", Args: []string{"30"}, Columns: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Stop(time.Second) })
	f.d.mu.Lock()
	f.d.leaders[coordination].session = s
	f.d.mu.Unlock()
	server, client := connectedPair(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.d.converse(server, bufio.NewReader(server), Request{Command: CommandConversation, Terminal: "/dev/pts/new"})
	}()
	var response Response
	if err := json.NewDecoder(client).Decode(&response); err != nil {
		t.Fatal(err)
	}
	<-done
	if !strings.Contains(response.Error, "reading the text you left unsent") {
		t.Errorf("failed attach answered %+v", response)
	}
	if after, _, err := f.s.LastLeft(); err != nil || after != last {
		t.Errorf("failed attach moved last departure from %d to %d (%v)", last, after, err)
	}
	f.d.mu.Lock()
	active, owned := f.d.conversation, f.d.leaders[coordination].userInput
	f.d.mu.Unlock()
	if active != nil || owned {
		t.Errorf("failed attach left a conversation active or its input owned: %v, %v", active, owned)
	}
	var data string
	if err := db.QueryRow(`SELECT data FROM journal WHERE kind = ? AND id > ? ORDER BY id LIMIT 1`, store.KindUnsentTextSaved, last).Scan(&data); err != nil {
		t.Fatal(err)
	}
	var unsent store.UnsentText
	if err := json.Unmarshal([]byte(data), &unsent); err != nil || unsent.Left != last || unsent.Text != "draft to recover" {
		t.Errorf("the departure's saved text is %+v (%v)", unsent, err)
	}
}

func TestFailedTakeoverCatchUpStillClearsSavedInputBeforeNudging(t *testing.T) {
	f := newTwoResults(t)
	f.withWorkers(t)
	coordination := roles.LeaderOf(roles.Coordination).String()
	capture := filepath.Join(t.TempDir(), "cleared")
	rule := strings.Repeat("─", 60)
	screen := fmt.Sprintf("\x1b[2J\x1b[H%s\r\n❯ draft to recover\r\n%s\x1b[2;19H", rule, rule)
	script := `printf '%s' "$ASMAI_SCREEN"; stty raw -echo; dd bs=1 count=1 of="$ASMAI_CAPTURE" 2>/dev/null; sleep 60`
	s, err := session.Start(session.Spec{Path: "/bin/sh", Args: []string{"-c", script}, Env: append(os.Environ(), "ASMAI_SCREEN="+screen, "ASMAI_CAPTURE="+capture), Columns: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Stop(time.Second) })
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if text, readable := providers.ClaudeCodeUnsentText(s.Screen()); readable && text == "draft to recover" {
			break
		}
	}
	if text, readable := providers.ClaudeCodeUnsentText(s.Screen()); !readable || text != "draft to recover" {
		t.Fatalf("the terminal shows %q (readable %t), want the unsent draft", text, readable)
	}
	f.d.mu.Lock()
	l := f.d.leaders[coordination]
	l.session, l.ready, l.userInput = s, false, true
	f.d.conversation = &conversation{leader: l, session: s, generation: 1, terminal: "/dev/pts/old", takenOver: make(chan string, 1)}
	f.d.mu.Unlock()
	db, err := sql.Open("sqlite3", f.d.cfg.Paths.Store)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER corrupt_takeover_catch_up AFTER INSERT ON journal WHEN NEW.kind = 'conversation.left' BEGIN INSERT INTO journal(at, kind, data) VALUES (NEW.at, 'unsent.saved', '{broken'); END`); err != nil {
		t.Fatal(err)
	}
	server, client := connectedPair(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.d.converse(server, bufio.NewReader(server), Request{Command: CommandConversation, Terminal: "/dev/pts/new"})
	}()
	var response Response
	if err := json.NewDecoder(client).Decode(&response); err != nil {
		t.Fatal(err)
	}
	<-done
	if !strings.Contains(response.Error, "reading the text you left unsent") {
		t.Fatalf("failed takeover answered %+v", response)
	}
	f.d.mu.Lock()
	active, clear, owned := f.d.conversation, l.clearUnsent, l.userInput
	l.ready = true
	f.d.mu.Unlock()
	if active != nil || !clear || owned {
		t.Fatalf("failed takeover left conversation %v, clear obligation %t, user input %t", active, clear, owned)
	}
	var savedData string
	if err := db.QueryRow(`SELECT data FROM journal WHERE kind = ? AND data != '{broken' ORDER BY id DESC LIMIT 1`, store.KindUnsentTextSaved).Scan(&savedData); err != nil {
		t.Fatal(err)
	}
	var saved store.UnsentText
	if err := json.Unmarshal([]byte(savedData), &saved); err != nil || saved.Text != "draft to recover" {
		t.Fatalf("saved unsent text = %+v (%v)", saved, err)
	}
	f.d.nudge(coordination)
	if key, ok := typed(t, capture); !ok || key != providers.ClaudeCodeClearInput {
		t.Errorf("the resumed terminal received %q (%t), want the clear-input key", key, ok)
	}
}

func TestAgentStatusKeepsDecisionLinksWithTheUser(t *testing.T) {
	f := newLavishFactory(t)
	page := f.request(t, 0, "Several questions")
	s, err := session.Start(session.Spec{Path: "/bin/sleep", Args: []string{"30"}, Columns: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Stop(time.Second) })
	f.d.mu.Lock()
	for _, leader := range f.d.leaders {
		leader.session = nil
	}
	f.d.leaders[roles.LeaderOf(roles.Engineering).String()].session = s
	f.d.mu.Unlock()
	dir, err := os.MkdirTemp(".", "s")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for range 2 {
			conn, err := l.AcceptUnix()
			if err != nil {
				return
			}
			f.d.handle(conn)
			conn.Close()
		}
	}()
	t.Setenv(SessionCredential, "")
	user, err := Call(socket, Request{Command: CommandStatus, Events: true})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := Call(socket, Request{Command: CommandStatus, Session: "credential", Events: true})
	if err != nil {
		t.Fatal(err)
	}
	if e := eventOf(t, user.Events, EventWaiting); e.Link != page.PageURL {
		t.Errorf("the user received %+v, want the pending page", e)
	}
	for _, e := range agent.Events {
		if e.Link != "" {
			t.Errorf("agent status disclosed a decision page: %+v", e)
		}
	}
}

func TestTheCatchUpDoesNotCallOldHeadsFailed(t *testing.T) {
	f := newTwoResults(t)
	f.pushedPullRequest(t)
	if err := f.s.ConversationLeft("leader@coordination", roles.Coordination, 1, "/dev/pts/test", LeftByDetaching, InputAutomation, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	red := store.ChecksObservation{Job: f.job.Number, Number: 7, Head: f.started, Verdict: "red", Checks: []store.CheckState{{Name: "test", State: "failure"}}}
	if _, _, err := f.s.ChecksObserved(red, time.Now()); err != nil {
		t.Fatal(err)
	}
	if c, err := f.d.catchUp(); err != nil || !slices.Contains(kindsOf(c.Events), EventFailed) {
		t.Fatalf("red checks on the pushed head: %+v (%v)", c, err)
	}
	pr := store.PullRequest{Job: f.job.Number, Number: 8, URL: "https://github.com/talvor/asmai-fixture/pull/8", Title: "Add a greeting", Branch: jobBranchFor(f.job), Head: f.started, State: "OPEN", Leader: f.assignments[0].Owner, Dispatch: f.reported[0].ID}
	if _, _, err := f.s.PullRequestRecorded(pr, 1, pr.URL, time.Now()); err != nil {
		t.Fatal(err)
	}
	last, _, err := f.s.LastLeft()
	if err != nil {
		t.Fatal(err)
	}
	if c, err := f.d.catchUp(); err != nil || slices.Contains(kindsOf(c.Events), EventFailed) {
		t.Fatalf("old PR's red checks after a new PR on the same head: %+v (%v)", c, err)
	}
	if events, err := f.d.events(last); err != nil || slices.Contains(kindsOf(events), EventFailed) {
		t.Fatalf("status reported the old PR's red checks: %+v (%v)", events, err)
	}
	red.Number = 8
	if _, _, err := f.s.ChecksObserved(red, time.Now()); err != nil {
		t.Fatal(err)
	}
	if c, err := f.d.catchUp(); err != nil || !slices.Contains(kindsOf(c.Events), EventFailed) {
		t.Fatalf("red checks on the current PR: %+v (%v)", c, err)
	}
	db, err := sql.Open("sqlite3", f.d.cfg.Paths.Store)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO pushes (job, branch, commit_id, result, outside, found, leader, dispatch, effect, merged_in, created_at)
		SELECT job, branch, 'new-head', result, outside, found, leader, dispatch, effect, merged_in, created_at
		FROM pushes WHERE job = ? ORDER BY id DESC LIMIT 1`, f.job.Number); err != nil {
		t.Fatal(err)
	}
	if c, err := f.d.catchUp(); err != nil || slices.Contains(kindsOf(c.Events), EventFailed) {
		t.Fatalf("old red checks after a newer push: %+v (%v)", c, err)
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
