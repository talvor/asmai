// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/fakeprovider"
	"github.com/talvor/asmai/internal/lavish"
	"github.com/talvor/asmai/internal/store"
)

// printedBeforeTheConversation returns what asmai printed on the terminal
// before it took the screen for the live conversation, as lines.
func (a *attachedTerminal) printedBeforeTheConversation() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	before, _, _ := bytes.Cut(a.written, []byte("\x1b[?1049h"))
	var lines []string
	for line := range strings.SplitSeq(string(before), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// leaveFrom has the user leave the conversation in a, and waits for asmai to
// exit.
func leaveFrom(t *testing.T, a *attachedTerminal) {
	t.Helper()
	a.ptmx.WriteString("\x1d\r")
	if code := a.exited(t); code != 0 {
		t.Fatalf("leaving the conversation, asmai exited %d, want 0", code)
	}
}

// When nothing changed since the user left, and nothing waits on them, the
// catch-up is one line, printed before the live conversation; the first
// conversation says as much, and so does a return.
func TestTheCatchUpIsOneLineWhenNothingChanged(t *testing.T) {
	readyFactory(t, []string{
		`{"hook":"SessionStart","payload":{"hook_event_name":"SessionStart"}}`,
		inputBox(""),
		`{"expect":"only typed by a test that means to"}`,
	})
	if _, stderr, code := runAsmai(t, "start"); code != 0 {
		t.Fatal(stderr)
	}
	for _, visit := range []string{"first", "return"} {
		a := inTerminal(t, 100, 25)
		waitFor(t, "Coordination's input box", a.shows("❯ Try"))
		if got := a.printedBeforeTheConversation(); len(got) != 1 || got[0] != nothingChanged {
			t.Errorf("the %s visit printed %q before the conversation, want only %q", visit, got, nothingChanged)
		}
		leaveFrom(t, a)
	}
}

// A broken journal read is reported to the returning terminal. It must not
// silently become the one-line empty catch-up or move the last-left marker.
func TestTheCatchUpReadFailureReachesTheReturningTerminal(t *testing.T) {
	stateDir := readyFactory(t, []string{
		`{"hook":"SessionStart","payload":{"hook_event_name":"SessionStart"}}`,
		inputBox(""),
		`{"expect":"only typed by a test that means to"}`,
	})
	if _, stderr, code := runAsmai(t, "start"); code != 0 {
		t.Fatal(stderr)
	}
	a := inTerminal(t, 100, 25)
	waitFor(t, "Coordination's input box", a.shows("❯ Try"))
	leaveFrom(t, a)
	db, err := sql.Open("sqlite3", filepath.Join(stateDir, "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var before int
	if err := db.QueryRow(`SELECT COUNT(*) FROM journal WHERE kind = 'conversation.left'`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO journal(at, kind, data) VALUES (?, 'unsent.saved', '{broken')`, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	b := inTerminal(t, 100, 25)
	if code := b.exited(t); code != 1 {
		t.Errorf("returning attach exited %d, want an error", code)
	}
	printed := strings.Join(b.printedBeforeTheConversation(), "\n")
	if !strings.Contains(printed, "reading the text you left unsent") || strings.Contains(printed, nothingChanged) {
		t.Errorf("returning attach printed %q, want the journal read error", printed)
	}
	t.Logf("failed return before live screen:\n%s", printed)
	var after int
	if err := db.QueryRow(`SELECT COUNT(*) FROM journal WHERE kind = 'conversation.left'`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Errorf("failed return moved the departure count from %d to %d", before, after)
	}
}

func TestFailedStartCheckAppearsAsAHoldWithItsCauseInStatus(t *testing.T) {
	stateDir := factoryHome(t)
	writeConfig(t, staffing)
	trustFactory(t, stateDir)
	startUnready(t)
	stdout, stderr, code := runAsmai(t, "status")
	if code != 0 || !strings.Contains(stdout, "hold") || !strings.Contains(stdout, "leader@coordination is held, not started") || !strings.Contains(stdout, "fix:") {
		t.Errorf("status exited %d printing %s%s, want the failed start check hold and its fix", code, stdout, stderr)
	}
	t.Logf("status with a failed start check:\n%s", stdout)
}

// The catch-up on a return offers back the text the user left unsent, without
// retyping it into Coordination's input box, and a lost sign-in shows
// Claude Code's own sign-in command, in the catch-up as in `asmai status`.
func TestTheCatchUpOffersTheUnsentTextBackAndShowsALostSignIn(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ASMAI_TEST_DIR", dir)
	signedOut := filepath.Join(dir, "signed-out")
	t.Setenv(fakeprovider.SignedOutFileEnv, signedOut)
	readyFactory(t, []string{
		`{"hook":"SessionStart","payload":{"hook_event_name":"SessionStart"}}`,
		inputBox(""),
		`{"expect":"draft"}`,
		inputBox("draft"),
		fakeRun(`touch "$ASMAI_TEST_DIR/drafted"`, 0, ""),
		// The saved text is cleared at the boundary, and never typed again.
		`{"expect":"\u0003"}`,
		inputBox(""),
		`{"expect":"only typed by a test that means to"}`,
	})
	if _, stderr, code := runAsmai(t, "start"); code != 0 {
		t.Fatal(stderr)
	}

	a := inTerminal(t, 100, 25)
	waitFor(t, "Coordination's input box", a.shows("❯ Try"))
	a.ptmx.WriteString("draft")
	waitFor(t, "the user's unsent text", touched(dir, "drafted"))
	leaveFrom(t, a)
	waitFor(t, "the text to be saved", func() bool { return len(journaled(t, "unsent.saved")) == 1 })

	if err := os.WriteFile(signedOut, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := runAsmai(t, "status")
	if code != 0 || !strings.Contains(stdout, "sign-in") || !strings.Contains(stdout, "sign in again with: ") || !strings.Contains(stdout, " auth login") {
		t.Errorf("asmai status printed %s%s (exit %d), want the lost sign-in with its native command", stdout, stderr, code)
	}
	var status struct {
		Events []struct {
			Kind    string `json:"kind"`
			Command string `json:"command"`
		} `json:"events"`
	}
	asmaiJSON(t, &status, "status")
	if len(status.Events) != 1 || status.Events[0].Kind != "sign-in" || !strings.HasSuffix(status.Events[0].Command, " auth login") {
		t.Errorf("asmai status --json lists the events %+v, want only the lost sign-in", status.Events)
	}

	b := inTerminal(t, 100, 25)
	waitFor(t, "Coordination's input box", b.shows("❯ Try"))
	printed := strings.Join(b.printedBeforeTheConversation(), "\n")
	for _, want := range []string{"Since you left:", "sign-in: Claude Code's sign-in is lost", "sign in again with: ", " auth login", "| draft"} {
		if !strings.Contains(printed, want) {
			t.Errorf("the catch-up printed before the conversation is\n%s\nwant it to hold %q", printed, want)
		}
	}
	if b.shows("draft")() {
		t.Errorf("the saved text was typed into the conversation again: %q", b.lines())
	}
	leaveFrom(t, b)

	// What was offered is not offered again.
	if texts, _ := savedUnsent(t); len(texts) != 1 {
		t.Errorf("the journal saved the unsent texts %q, want only the one", texts)
	}
	c := inTerminal(t, 100, 25)
	waitFor(t, "Coordination's input box", c.shows("❯ Try"))
	if printed := strings.Join(c.printedBeforeTheConversation(), "\n"); strings.Contains(printed, "draft") {
		t.Errorf("the next return printed %s, want the unsent text offered once", printed)
	}
}

// What waits on the user while they are away is told on their return, before
// the live conversation, and in `asmai status`: the decision Coordination
// asked in the conversation and Coordination's own native prompt, as the
// daemon recorded each.
func TestTheCatchUpTellsOnReturnOfWhatWaitedWhileTheUserWasAway(t *testing.T) {
	testDir := t.TempDir()
	t.Setenv("ASMAI_TEST_DIR", testDir)
	readyFactory(t, []string{
		`{"screen": "> "}`,
		`{"expect": "Greet the user\r"}`,
		`{"hook": "UserPromptSubmit", "payload": {"hook_event_name": "UserPromptSubmit", "prompt": "Greet the user"}}`,
		fakeRun(`asmai job open --message latest --repository fixture --reading 'Greet the user' --criterion 'Greets'`, 0, "Opened job 1"),
		fakeRun(`asmai decision request --job 1 --title 'Which greeting?' --option hello --option hi`, 0, "decision request 1, version 1, recorded for job 1; it goes to user and is answered in conversation"),
		`{"hook": "PermissionRequest", "payload": {"hook_event_name": "PermissionRequest", "tool_name": "Bash", "tool_input": {"command": "rm -rf build"}}}`,
		`{"expect": "only typed by a test that means to"}`,
	})
	origin := filepath.Join(t.TempDir(), "fixture.git")
	gitIn(t, t.TempDir(), "init", "--bare", "-b", "main", origin)
	work := t.TempDir()
	gitIn(t, work, "clone", "--quiet", origin, "fixture")
	checkout := filepath.Join(work, "fixture")
	commitSetUp(t, checkout, "initial")
	gitIn(t, checkout, "push", "--quiet", "origin", "HEAD:main")
	if _, stderr, code := runAsmai(t, "repo", "add", origin); code != 0 {
		t.Fatalf("repo add: %s", stderr)
	}

	a := inTerminal(t, 300, 31)
	waitFor(t, "Coordination's prompt", a.shows(">"))
	a.ptmx.WriteString("Greet the user\r")
	waitFor(t, "the decision and the native prompt on the status line", func() bool {
		line := a.lines()[30]
		return strings.Contains(line, "decision request 1 waits on you") && strings.Contains(line, "rm -rf build")
	})
	leaveFrom(t, a)

	// M5 adds the pause command. Seed its recorded state while the user is
	// away, then check the running daemon through status and attach.
	db, err := sql.Open("sqlite3", filepath.Join(os.Getenv("HOME"), ".local", "state", "asmai", "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE jobs SET state = ? WHERE number = 1`, store.JobPaused); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(filepath.Join(os.Getenv("HOME"), ".local", "state", "asmai", "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r, _, err := s.DecisionRequested(store.DecisionRequestInput{
		Job: 1, Requester: "leader@coordination", Recipient: store.ToUser,
		Kind: store.RequestQuestions, Questions: []store.Question{{ID: "style", Title: "Which style?"}},
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	pageURL := fmt.Sprintf("http://127.0.0.1:%d/session/disposable", lavish.Port)
	if _, err := s.LavishPageOpened(r.ID, "/disposable.html", pageURL, time.Now()); err != nil {
		t.Fatal(err)
	}
	// A recorded PR and red check observation are enough for the return
	// view; the paused job keeps the CI watcher from contacting a host.
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	const head = "disposable-head"
	message, err := db.Exec(`INSERT INTO messages(job, sender, recipient, kind, body) VALUES (1, 'leader@coordination', 'leader@engineering', 'fixture', 'fixture')`)
	if err != nil {
		t.Fatal(err)
	}
	messageID, err := message.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	dispatch, err := db.Exec(`INSERT INTO dispatches(message, agent, generation, state, updated_at) VALUES (?, 'leader@engineering', 1, 'working', ?)`, messageID, stamp)
	if err != nil {
		t.Fatal(err)
	}
	dispatchID, err := dispatch.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	effect, err := db.Exec(`INSERT INTO effects(job, dispatch, agent, generation, kind, ref, at) VALUES (1, ?, 'leader@engineering', 1, 'pull-request', 'https://example.invalid/pr/7', ?)`, dispatchID, stamp)
	if err != nil {
		t.Fatal(err)
	}
	effectID, err := effect.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	assignment, err := db.Exec(`INSERT INTO assignments(job, owner, worker, outcome, criteria, state, created_at) VALUES (1, 'leader@engineering', 'worker1@engineering', 'Add the greeting', '[]', 'active', ?)`, stamp)
	if err != nil {
		t.Fatal(err)
	}
	assignmentID, err := assignment.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	report, err := db.Exec(`INSERT INTO reports(assignment, dispatch, kind, agent, commit_id, data, created_at) VALUES (?, ?, 'validation', 'worker1@quality', ?, '{}', ?)`, assignmentID, dispatchID, head, stamp)
	if err != nil {
		t.Fatal(err)
	}
	reportID, err := report.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO findings(job, assignment, report, kind, text, commit_id, created_at) VALUES (1, ?, ?, ?, 'Which greeting? The request is ambiguous.', ?, ?)`, assignmentID, reportID, store.FindingNeedsYou, head, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO pushes(job, branch, commit_id, result, leader, dispatch, created_at) VALUES (1, 'fixture-branch', ?, 'pushed', 'leader@engineering', ?, ?)`, head, dispatchID, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO pull_requests(job, number, url, title, draft, not_ready, branch, head, state, leader, dispatch, effect, created_at) VALUES (1, 7, 'https://example.invalid/pr/7', 'Greeting', 0, 0, 'fixture-branch', ?, 'OPEN', 'leader@engineering', ?, ?, ?)`, head, dispatchID, effectID, stamp); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ChecksObserved(store.ChecksObservation{Job: 1, Number: 7, Head: head, Verdict: "red", Checks: []store.CheckState{{Name: "test", State: "failure"}}}, time.Now()); err != nil {
		t.Fatal(err)
	}

	wants := []string{
		"waiting on you: leader@coordination waits at a native prompt the user answers in the conversation: Bash `rm -rf build`",
		"waiting on you: job 1: decision request 1 waits on you in the conversation",
		"waiting on you: job 1: decision request 2 waits on you in Lavish",
		"waiting on you: job 1 finding 1: Which greeting?",
		"job failed: job 1",
		"job paused: job 1",
	}
	stdout, stderr, code := runAsmai(t, "status")
	if code != 0 {
		t.Fatalf("asmai status exited %d: %s", code, stderr)
	}
	t.Logf("status with a paused job and waiting requests:\n%s", stdout)
	for _, want := range []string{"waiting on you", "decision request 1 waits on you", "finding 1: Which greeting?", "rm -rf build", "job failed", "job paused", pageURL} {
		if !strings.Contains(stdout, want) {
			t.Errorf("asmai status printed %s, want it to hold %q", stdout, want)
		}
	}

	b := inTerminal(t, 300, 31)
	waitFor(t, "the conversation", func() bool { return len(b.printedBeforeTheConversation()) > 0 })
	printed := strings.Join(b.printedBeforeTheConversation(), "\n")
	t.Logf("return attach before live screen:\n%s", printed)
	if !strings.Contains(printed, "Since you left:") {
		t.Errorf("the return printed\n%s\nwant a catch-up before the conversation", printed)
	}
	for _, want := range wants {
		if !strings.Contains(printed, want) {
			t.Errorf("the catch-up printed before the conversation is\n%s\nwant it to hold %q", printed, want)
		}
	}
	for _, want := range []string{"answer it in Lavish: " + pageURL, fmt.Sprintf("ssh -N -L %d:127.0.0.1:%d", lavish.Port, lavish.Port)} {
		if !strings.Contains(printed, want) {
			t.Errorf("the catch-up printed before the conversation is\n%s\nwant it to hold %q", printed, want)
		}
	}
	leaveFrom(t, b)
	if _, err := db.Exec(`INSERT INTO pushes(job, branch, commit_id, result, leader, dispatch, created_at) VALUES (1, 'fixture-branch', 'new-head', 'pushed', 'leader@engineering', ?, ?)`, dispatchID, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	status, stderr, code := runAsmai(t, "status")
	if code != 0 || strings.Contains(status, "job failed") {
		t.Errorf("status after a newer push exited %d printing %s%s, want no old-head failure", code, status, stderr)
	}
	c := inTerminal(t, 300, 31)
	waitFor(t, "the return after a newer push", func() bool { return len(c.printedBeforeTheConversation()) > 0 })
	if later := strings.Join(c.printedBeforeTheConversation(), "\n"); strings.Contains(later, "job failed") {
		t.Errorf("the return after a newer push printed %q, want no old-head failure", later)
	}
}

// A job that finished while the user was away is told on their return, before
// the live conversation, and in `asmai status`.
func TestTheCatchUpTellsOnReturnOfAJobThatFinishedWhileTheUserWasAway(t *testing.T) {
	testDir := t.TempDir()
	t.Setenv("ASMAI_TEST_DIR", testDir)
	readyFactory(t, []string{
		`{"screen": "> "}`,
		`{"expect": "Greet the user\r"}`,
		`{"hook": "UserPromptSubmit", "payload": {"hook_event_name": "UserPromptSubmit", "prompt": "Greet the user"}}`,
		fakeRun(`asmai job open --message latest --repository fixture --reading 'Greet the user' --criterion 'Greets'`, 0, "Opened job 1"),
		// The agent carries on after the user has left, and cancels the job.
		fakeRun(`while [ ! -e "$ASMAI_TEST_DIR/go" ]; do sleep 0.1; done; asmai job cancel 1 --message latest`, 0, "Cancelled job 1"),
		`{"expect": "only typed by a test that means to"}`,
	})
	origin := filepath.Join(t.TempDir(), "fixture.git")
	gitIn(t, t.TempDir(), "init", "--bare", "-b", "main", origin)
	work := t.TempDir()
	gitIn(t, work, "clone", "--quiet", origin, "fixture")
	checkout := filepath.Join(work, "fixture")
	commitSetUp(t, checkout, "initial")
	gitIn(t, checkout, "push", "--quiet", "origin", "HEAD:main")
	if _, stderr, code := runAsmai(t, "repo", "add", origin); code != 0 {
		t.Fatalf("repo add: %s", stderr)
	}

	a := inTerminal(t, 200, 25)
	waitFor(t, "Coordination's prompt", a.shows(">"))
	a.ptmx.WriteString("Greet the user\r")
	waitFor(t, "the job to be opened", func() bool {
		out, _, _ := runAsmai(t, "jobs")
		return strings.Contains(out, "1")
	})
	leaveFrom(t, a)

	if err := os.WriteFile(filepath.Join(testDir, "go"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the agent to cancel the job", func() bool {
		out, _, _ := runAsmai(t, "status")
		return strings.Contains(out, "was cancelled")
	})
	stdout, _, _ := runAsmai(t, "status")
	if !strings.Contains(stdout, "was cancelled") {
		t.Errorf("asmai status printed %s, want the finished job", stdout)
	}

	b := inTerminal(t, 200, 25)
	waitFor(t, "the conversation to take the screen", func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return bytes.Contains(b.written, []byte("\x1b[?1049h"))
	})
	printed := strings.Join(b.printedBeforeTheConversation(), "\n")
	for _, want := range []string{"Since you left:", "job 1", "was cancelled"} {
		if !strings.Contains(printed, want) {
			t.Errorf("the catch-up printed before the conversation is\n%s\nwant it to hold %q", printed, want)
		}
	}
}
