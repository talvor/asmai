// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/providers"
	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/session"
	"github.com/talvor/asmai/internal/store"
)

// takenWorker is a worker whose session is a terminal that shows its screen,
// the input box with draft in it when draft is not empty, and records every
// key typed into it in capture.
type takenWorker struct {
	f       twoResults
	name    string
	l       *leader
	capture string
}

func newTakenWorker(t *testing.T, draft string) takenWorker {
	t.Helper()
	f := newTwoResults(t)
	f.withWorkers(t)
	name := f.assignments[0].Worker
	capture := filepath.Join(t.TempDir(), "typed")
	rule := strings.Repeat("─", 60)
	screen := fmt.Sprintf("\x1b[2J\x1b[H%s\r\n❯ %s\r\n%s\x1b[2;%dH", rule, draft, rule, 4+len([]rune(draft)))
	script := `stty raw -echo; printf '%s' "$ASMAI_SCREEN"; cat >> "$ASMAI_CAPTURE"`
	s, err := session.Start(session.Spec{Path: "/bin/sh", Args: []string{"-c", script}, Env: append(os.Environ(), "ASMAI_SCREEN="+screen, "ASMAI_CAPTURE="+capture), Columns: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Stop(time.Second) })
	eventually(t, "the worker's screen", func() bool {
		text, readable := providers.ClaudeCodeUnsentText(s.Screen())
		return readable && text == draft
	})
	f.d.mu.Lock()
	l := f.d.leaders[name]
	l.session = s
	f.d.mu.Unlock()
	return takenWorker{f: f, name: name, l: l, capture: capture}
}

// working puts the worker in the middle of an automated turn, and boundary
// ends it.
func (w takenWorker) working(dispatch int64) {
	w.f.d.mu.Lock()
	defer w.f.d.mu.Unlock()
	w.l.ready, w.l.currentDispatch, w.l.currentPromptID = false, dispatch, "turn"
}

func (w takenWorker) boundary() {
	w.f.d.mu.Lock()
	defer w.f.d.mu.Unlock()
	w.l.ready, w.l.currentDispatch, w.l.currentPromptID = true, 0, ""
}

// typedSoFar is everything typed into the worker's terminal so far.
func (w takenWorker) typedSoFar() string {
	got, _ := os.ReadFile(w.capture)
	return string(got)
}

// taking runs a take of the worker from terminal, and returns the user's end
// of the connection and a channel that closes when the take ends.
func (w takenWorker) taking(t *testing.T, terminal string, interrupt bool) (*net.UnixConn, *bufio.Reader, chan struct{}) {
	t.Helper()
	server, client := connectedPair(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.f.d.take(server, bufio.NewReader(server), Request{Command: CommandTake, Agent: w.name, Terminal: terminal, Interrupt: interrupt})
	}()
	return client, bufio.NewReader(client), done
}

// answered reads the daemon's answer to a take.
func answered(t *testing.T, r *bufio.Reader) Response {
	t.Helper()
	var resp Response
	line, err := r.ReadBytes('\n')
	if err != nil {
		t.Fatalf("reading the daemon's answer to the take: %v", err)
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("reading the daemon's answer to the take: %v", err)
	}
	return resp
}

// takeJournal returns the data of the journal entries of kind.
func takeJournal(t *testing.T, s *store.Store, kind string) []map[string]any {
	t.Helper()
	entries, err := s.Journal()
	if err != nil {
		t.Fatal(err)
	}
	var found []map[string]any
	for _, e := range entries {
		if e.Kind != kind {
			continue
		}
		var data map[string]any
		if err := json.Unmarshal(e.Data, &data); err != nil {
			t.Fatal(err)
		}
		found = append(found, data)
	}
	return found
}

// With a turn running, a take waits for its boundary: nothing is answered
// and nothing is nudged until then, and then the user holds the input.
func TestATakeWaitsForTheRunningTurnsBoundaryAndNoNudgeSlipsInBetween(t *testing.T) {
	w := newTakenWorker(t, "")
	w.working(41)
	client, r, done := w.taking(t, "/dev/pts/7", false)

	client.SetReadDeadline(time.Now().Add(400 * time.Millisecond))
	if _, err := r.Peek(1); err == nil {
		t.Fatal("the take was answered in the middle of the turn")
	}
	client.SetReadDeadline(time.Time{})
	w.f.d.mu.Lock()
	waiting := w.l.taking
	w.l.ready, w.l.currentDispatch = true, 0
	nudge := w.f.d.mayNudge(w.l)
	w.f.d.mu.Unlock()
	if waiting != 1 || nudge {
		t.Fatalf("while the take waits, %d takes are counted and a nudge may be typed: %t, want 1 and false", waiting, nudge)
	}

	resp := answered(t, r)
	if resp.Error != "" || resp.Attached == nil || resp.Attached.Input != InputUser || resp.Attached.Taken != "/dev/pts/7" {
		t.Fatalf("the take was answered %+v, want the agent attached with the user's input", resp)
	}
	w.f.d.mu.Lock()
	held, counted, nudge := w.l.taken != nil, w.l.taking, w.f.d.mayNudge(w.l)
	w.f.d.mu.Unlock()
	if !held || counted != 0 || nudge {
		t.Errorf("after the take, held %t, takes counted %d, nudge allowed %t, want true, 0 and false: automated messages wait in the inbox", held, counted, nudge)
	}
	if taken := takeJournal(t, w.f.s, store.KindInputTaken); len(taken) != 1 || taken[0]["interrupted"] != nil || taken[0]["agent"] != w.name {
		t.Errorf("the journal holds the takes %v, want one that waited", taken)
	}
	if got := w.typedSoFar(); got != "" {
		t.Errorf("the worker's terminal received %q, want nothing", got)
	}
	client.Close()
	<-done
	// Detaching leaves the input with the user.
	w.f.d.mu.Lock()
	held = w.l.taken != nil
	w.f.d.mu.Unlock()
	if !held {
		t.Errorf("the terminal's end released the input, want the user to keep holding it")
	}
}

func TestInterruptBeforeTheFirstBoundaryWaitsForReadiness(t *testing.T) {
	w := newTakenWorker(t, "")
	w.f.d.mu.Lock()
	w.l.ready, w.l.currentDispatch, w.l.pendingDispatch = false, 0, 0
	w.f.d.mu.Unlock()
	client, r, done := w.taking(t, "/dev/pts/7", true)
	client.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, err := r.Peek(1); err == nil {
		t.Fatal("the interrupting take was answered before the first boundary")
	}
	client.SetReadDeadline(time.Time{})
	if got := w.typedSoFar(); got != "" {
		t.Errorf("startup received %q, want no interrupt key", got)
	}
	w.boundary()
	client.SetReadDeadline(time.Now().Add(time.Second))
	if resp := answered(t, r); resp.Error != "" || resp.Attached == nil || resp.Attached.Input != InputUser {
		t.Fatalf("the take after readiness was answered %+v", resp)
	}
	if got := w.typedSoFar(); got != "" {
		t.Errorf("the ready session received %q, want no interrupt key", got)
	}
	if taken := takeJournal(t, w.f.s, store.KindInputTaken); len(taken) != 1 || taken[0]["interrupted"] != nil {
		t.Errorf("the take at the first boundary was journaled as %v", taken)
	}
	client.Close()
	<-done
}

func TestANativePromptIsABoundaryDuringAnAutomatedTurn(t *testing.T) {
	w := newTakenWorker(t, "")
	w.working(41)
	w.f.d.mu.Lock()
	w.l.prompt = &WaitingPrompt{Agent: w.name, About: "permission", Since: time.Now()}
	w.f.d.mu.Unlock()
	client, r, done := w.taking(t, "/dev/pts/7", false)
	client.SetReadDeadline(time.Now().Add(time.Second))
	resp := answered(t, r)
	if resp.Error != "" || resp.Attached == nil || resp.Attached.Input != InputUser {
		t.Fatalf("taking at the native prompt was answered %+v", resp)
	}
	if got := w.typedSoFar(); got != "" {
		t.Errorf("taking at the prompt typed %q, want no interrupt", got)
	}
	if taken := takeJournal(t, w.f.s, store.KindInputTaken); len(taken) != 1 || taken[0]["interrupted"] != nil {
		t.Errorf("the prompt boundary was journaled as %v, want a take without an interrupt", taken)
	}
	client.Close()
	<-done
}

// A take that waits stops waiting when its client goes, and the nudges it
// held back go on.
func TestATakeWhoseClientGoesBeforeTheBoundaryHoldsNothing(t *testing.T) {
	w := newTakenWorker(t, "")
	w.working(41)
	client, _, done := w.taking(t, "/dev/pts/7", false)
	time.Sleep(150 * time.Millisecond)
	client.Close()
	<-done
	w.f.d.mu.Lock()
	defer w.f.d.mu.Unlock()
	if w.l.taken != nil || w.l.taking != 0 {
		t.Errorf("the abandoned take left the hold %v and %d takes counted, want none", w.l.taken, w.l.taking)
	}
}

func TestAWaitingTakeCannotDisplaceANewerInterruptingTake(t *testing.T) {
	w := newTakenWorker(t, "")
	w.working(41)
	older, olderReader, olderDone := w.taking(t, "/dev/pts/1", false)
	eventually(t, "the older take waiting", func() bool {
		w.f.d.mu.Lock()
		defer w.f.d.mu.Unlock()
		return w.l.taking == 1
	})
	newer, newerReader, newerDone := w.taking(t, "/dev/pts/2", true)
	if resp := answered(t, newerReader); resp.Error != "" || resp.Attached == nil || resp.Attached.Taken != "/dev/pts/2" {
		t.Fatalf("the newer take was answered %+v", resp)
	}
	older.SetReadDeadline(time.Now().Add(time.Second))
	if resp := answered(t, olderReader); !strings.Contains(resp.Error, "superseded") {
		t.Errorf("the older take was answered %+v, want superseded", resp)
	}
	w.f.d.mu.Lock()
	holder := w.l.taken.terminal
	w.f.d.mu.Unlock()
	if holder != "/dev/pts/2" {
		t.Errorf("the hold moved to %s, want the newer terminal", holder)
	}
	if taken := takeJournal(t, w.f.s, store.KindInputTaken); len(taken) != 1 || taken[0]["terminal"] != "/dev/pts/2" {
		t.Errorf("the takes journaled are %v, want only the newer take", taken)
	}
	older.Close()
	newer.Close()
	<-olderDone
	<-newerDone
}

func TestAWaitingTakeDoesNotCountAgainstAReplacementSession(t *testing.T) {
	w := newTakenWorker(t, "")
	w.working(41)
	w.f.d.mu.Lock()
	w.l.generation = 1
	w.f.d.mu.Unlock()
	client, r, done := w.taking(t, "/dev/pts/1", false)
	eventually(t, "the old session's take waiting", func() bool {
		w.f.d.mu.Lock()
		defer w.f.d.mu.Unlock()
		return w.l.taking == 1
	})
	w.f.d.mu.Lock()
	w.l.session, w.l.generation, w.l.taking = &session.Session{}, 2, 0
	w.l.ready, w.l.currentDispatch = false, 0
	w.f.d.mu.Unlock()
	client.SetReadDeadline(time.Now().Add(time.Second))
	if resp := answered(t, r); !strings.Contains(resp.Error, "session ended") {
		t.Errorf("the old take was answered %+v, want session ended", resp)
	}
	<-done
	w.f.d.mu.Lock()
	w.l.ready = true
	counted, mayNudge := w.l.taking, w.f.d.mayNudge(w.l)
	w.f.d.mu.Unlock()
	if counted != 0 || !mayNudge {
		t.Errorf("the replacement has %d takes counted and may nudge %t, want zero and true", counted, mayNudge)
	}
	client.Close()
}

// --interrupt does not wait for the boundary: Esc reaches the terminal, the
// turn's state is unknown until the session reports a finished turn, and the
// interrupt is journaled with the dispatch it interrupted.
func TestATakeWithInterruptInterruptsTheRunningTurnInstead(t *testing.T) {
	w := newTakenWorker(t, "")
	w.working(41)
	client, r, done := w.taking(t, "/dev/pts/7", true)
	if resp := answered(t, r); resp.Error != "" || resp.Attached == nil || resp.Attached.Input != InputUser {
		t.Fatalf("the interrupting take was answered %+v, want the agent attached", resp)
	}
	if key, ok := typed(t, w.capture); !ok || key != "\x1b" {
		t.Errorf("the worker's terminal received %q (%t), want Esc", key, ok)
	}
	w.f.d.mu.Lock()
	interrupted := w.l.interrupted
	w.f.d.mu.Unlock()
	if interrupted != 41 {
		t.Errorf("the interrupted dispatch is %d, want 41 recognised at the next Stop", interrupted)
	}
	taken := takeJournal(t, w.f.s, store.KindInputTaken)
	if len(taken) != 1 || taken[0]["interrupted"] != true || taken[0]["dispatch"] != float64(41) {
		t.Errorf("the journal holds the takes %v, want one that interrupted dispatch 41", taken)
	}
	client.Close()
	<-done
}

// An unsent automated draft in the input box is recorded and cleared before
// the input is handed over: the answer comes after the clear-input key was
// typed, and the journal holds the draft word for word.
func TestAnUnsentAutomatedDraftIsRecordedAndClearedBeforeTheHandover(t *testing.T) {
	w := newTakenWorker(t, "asmai inbox --dispatch 9")
	w.boundary()
	client, r, done := w.taking(t, "/dev/pts/7", false)
	if resp := answered(t, r); resp.Error != "" || resp.Attached == nil {
		t.Fatalf("the take was answered %+v", resp)
	}
	if got := w.typedSoFar(); got != providers.ClaudeCodeClearInput {
		t.Errorf("by the time the take was answered the terminal had received %q, want only the clear-input key", got)
	}
	cleared := takeJournal(t, w.f.s, store.KindDraftCleared)
	if len(cleared) != 1 || cleared[0]["text"] != "asmai inbox --dispatch 9" || cleared[0]["terminal"] != "/dev/pts/7" || cleared[0]["agent"] != w.name {
		t.Errorf("the journal holds the cleared drafts %v, want the automated draft", cleared)
	}
	client.Close()
	<-done
}

func TestAFailedDraftClearAbortsTheHandover(t *testing.T) {
	w := newTakenWorker(t, "asmai inbox --dispatch 9")
	w.boundary()
	w.l.session.Stop(time.Second)
	client, r, done := w.taking(t, "/dev/pts/7", false)
	client.SetReadDeadline(time.Now().Add(time.Second))
	if resp := answered(t, r); !strings.Contains(resp.Error, "clearing the automated draft") {
		t.Errorf("the failed clear was answered %+v, want an error", resp)
	}
	<-done
	w.f.d.mu.Lock()
	held := w.l.taken != nil
	w.f.d.mu.Unlock()
	if held || len(takeJournal(t, w.f.s, store.KindInputTaken)) != 0 || len(takeJournal(t, w.f.s, store.KindDraftCleared)) != 0 {
		t.Errorf("a failed clear left a hold or journaled a successful take")
	}
	client.Close()
}

func TestAFailedInterruptWriteLeavesTheTurnAndInputUntouched(t *testing.T) {
	w := newTakenWorker(t, "")
	w.working(41)
	w.l.session.Stop(time.Second)
	client, r, done := w.taking(t, "/dev/pts/7", true)
	client.SetReadDeadline(time.Now().Add(time.Second))
	if resp := answered(t, r); !strings.Contains(resp.Error, "interrupting") {
		t.Errorf("the failed interrupt was answered %+v, want an error", resp)
	}
	<-done
	w.f.d.mu.Lock()
	held, interrupted, taking := w.l.taken != nil, w.l.interrupted, w.l.taking
	w.f.d.mu.Unlock()
	if held || interrupted != 0 || taking != 0 || len(takeJournal(t, w.f.s, store.KindInputTaken)) != 0 {
		t.Errorf("the failed interrupt left hold %t, interrupted %d, takes %d, or a take journal entry", held, interrupted, taking)
	}
	client.Close()
}

func TestInterruptingAPendingSubmissionSettlesItBeforeTakingInput(t *testing.T) {
	w := newTakenWorker(t, "")
	w.boundary()
	work, err := w.f.s.WorkerDispatch(w.f.assignments[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	id := work.Dispatch.ID
	if err := w.f.s.ChangeDispatch(id, 1, store.DispatchDelivered, store.DispatchUnknown, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	prompt := fmt.Sprintf("asmai inbox --dispatch %d", id)
	w.f.d.mu.Lock()
	w.l.ready, w.l.inputUnknown, w.l.pendingDispatch, w.l.pendingPrompt = false, true, id, prompt
	w.f.d.mu.Unlock()

	client, r, done := w.taking(t, "/dev/pts/7", true)
	client.SetReadDeadline(time.Now().Add(2 * time.Second))
	if resp := answered(t, r); resp.Error != "" || resp.Attached == nil || resp.Attached.Input != InputUser {
		t.Fatalf("interrupting the pending submission answered %+v", resp)
	}
	if got := w.typedSoFar(); got != interruptKey {
		t.Errorf("the terminal received %q, want one interrupt key", got)
	}
	w.f.d.mu.Lock()
	pending, unknown, retry, nudge := w.l.pendingDispatch, w.l.inputUnknown, w.l.renudge, w.f.d.mayNudge(w.l)
	w.f.d.mu.Unlock()
	if pending != 0 || unknown || retry != id || nudge {
		t.Errorf("the pending dispatch left pending %d, unknown %t, retry %d, nudge allowed %t", pending, unknown, retry, nudge)
	}
	ref := sessionRef{address: w.l.address, generation: 1}
	if matched, err := w.f.d.automatedSubmitted(ref, prompt, "late", "", json.RawMessage(`{"prompt":"`+prompt+`"}`)); err != nil || matched {
		t.Errorf("the late submission hook matched %t with error %v", matched, err)
	}
	if changed := takeJournal(t, w.f.s, store.KindDispatchChanged); len(changed) == 0 || changed[len(changed)-1]["id"] != float64(id) || changed[len(changed)-1]["state"] != store.DispatchNudged || changed[len(changed)-1]["interrupted"] != true {
		t.Errorf("the pending dispatch did not return to the inbox as interrupted: %v", changed)
	}
	client.Close()
	<-done
}

func TestAJournalFailureRestoresTheAutomatedDraft(t *testing.T) {
	w := newTakenWorker(t, "asmai inbox --dispatch 9")
	w.boundary()
	client, r, done := w.taking(t, "/dev/pts/7", false)
	eventually(t, "the draft clear key", func() bool { return w.typedSoFar() == providers.ClaudeCodeClearInput })
	if err := w.f.s.Close(); err != nil {
		t.Fatal(err)
	}
	client.SetReadDeadline(time.Now().Add(time.Second))
	if resp := answered(t, r); !strings.Contains(resp.Error, "journaling the take") {
		t.Errorf("the failed journal was answered %+v, want an error", resp)
	}
	<-done
	w.f.d.mu.Lock()
	held, taking := w.l.taken != nil, w.l.taking
	w.f.d.mu.Unlock()
	if held || taking != 0 {
		t.Errorf("the failed journal left hold %t and takes %d, want neither", held, taking)
	}
	wantInput := providers.ClaudeCodeClearInput + pasteStart + "asmai inbox --dispatch 9" + pasteEnd
	eventually(t, "the automated draft restored in the worker's input", func() bool { return w.typedSoFar() == wantInput })
	if got := w.typedSoFar(); got != wantInput {
		t.Errorf("the worker's input received %q, want the draft restored with %q", got, wantInput)
	}
	reopened, err := store.Open(w.f.d.cfg.Paths.Store)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if taken := takeJournal(t, reopened, store.KindInputTaken); len(taken) != 0 {
		t.Errorf("the failed take was journaled as %v", taken)
	}
	if cleared := takeJournal(t, reopened, store.KindDraftCleared); len(cleared) != 0 {
		t.Errorf("the failed take journaled the draft as cleared: %v", cleared)
	}
	client.Close()
}

func TestAFailedMovedTakeKeepsThePreviousTerminalTyping(t *testing.T) {
	w := newTakenWorker(t, "")
	w.boundary()
	older, olderReader, olderDone := w.taking(t, "/dev/pts/1", false)
	answered(t, olderReader)
	w.f.d.mu.Lock()
	previous := w.l.taken
	w.f.d.mu.Unlock()
	if err := w.f.s.Close(); err != nil {
		t.Fatal(err)
	}
	newer, newerReader, newerDone := w.taking(t, "/dev/pts/2", false)
	newer.SetReadDeadline(time.Now().Add(time.Second))
	if resp := answered(t, newerReader); !strings.Contains(resp.Error, "journaling the take") {
		t.Errorf("the failed move was answered %+v, want an error", resp)
	}
	<-newerDone
	w.f.d.mu.Lock()
	holder, displaced := w.l.taken, previous.displaced
	w.f.d.mu.Unlock()
	if holder != previous || displaced {
		t.Errorf("the failed move left holder %p and displaced %t, want the old holder %p undisplaced", holder, displaced, previous)
	}
	line, _ := json.Marshal(ClientMessage{Keys: []byte("still here")})
	older.Write(append(line, '\n'))
	eventually(t, "the original terminal's keys", func() bool { return w.typedSoFar() == "still here" })
	older.Close()
	newer.Close()
	<-olderDone
}

func TestANewerTakeWaitsUntilTheOlderDraftIsCleared(t *testing.T) {
	w := newTakenWorker(t, "asmai inbox --dispatch 9")
	w.working(41)
	older, olderReader, olderDone := w.taking(t, "/dev/pts/1", true)
	eventually(t, "the older take's interrupt", func() bool { return w.typedSoFar() == interruptKey })
	newer, newerReader, newerDone := w.taking(t, "/dev/pts/2", false)
	newer.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if _, err := newerReader.Peek(1); err == nil {
		t.Fatal("the newer take answered before draft clearing finished")
	}
	newer.SetReadDeadline(time.Time{})
	if resp := answered(t, olderReader); resp.Error != "" || resp.Attached == nil {
		t.Fatalf("the older take was answered %+v", resp)
	}
	if resp := answered(t, newerReader); resp.Error != "" || resp.Attached == nil || resp.Attached.Taken != "/dev/pts/2" {
		t.Fatalf("the newer take was answered %+v", resp)
	}
	line, _ := json.Marshal(ClientMessage{Keys: []byte("new")})
	newer.Write(append(line, '\n'))
	eventually(t, "the newer terminal's keys", func() bool { return strings.HasSuffix(w.typedSoFar(), "new") })
	if got := w.typedSoFar(); got != interruptKey+providers.ClaudeCodeClearInput+"new" {
		t.Errorf("the terminal received %q, want interrupt, clear, then newer keys", got)
	}
	older.Close()
	newer.Close()
	<-olderDone
	<-newerDone
}

// A newer take of an agent the user already holds moves the hold: the older
// terminal is told and only observes, the newer one types, nothing is cleared
// again, and the journal says the hold moved.
func TestANewerTakeMovesTheHoldToTheNewTerminal(t *testing.T) {
	w := newTakenWorker(t, "")
	w.boundary()
	older, olderReader, olderDone := w.taking(t, "/dev/pts/1", false)
	answered(t, olderReader)
	w.f.d.mu.Lock()
	previous := w.l.taken
	w.f.d.mu.Unlock()
	newer, newerReader, newerDone := w.taking(t, "/dev/pts/2", false)
	// The newer take is not made to wait for a boundary, as the user holds
	// the input already.
	w.working(5)
	if resp := answered(t, newerReader); resp.Error != "" || resp.Attached.Taken != "/dev/pts/2" {
		t.Fatalf("the newer take was answered %+v", resp)
	}
	var told Frame
	for told.TakenOver == "" {
		told = readFrame(t, olderReader)
	}
	if told.TakenOver != "/dev/pts/2" {
		t.Fatalf("the older terminal was told %+v, want the hold moved to /dev/pts/2", told)
	}
	w.f.d.mu.Lock()
	holder := w.l.taken.terminal
	w.f.d.mu.Unlock()
	if holder != "/dev/pts/2" {
		t.Errorf("the hold is in %s, want /dev/pts/2", holder)
	}
	initialServer, initialClient := connectedPair(t)
	initialDone := make(chan struct{})
	go func() {
		defer close(initialDone)
		w.f.d.stream(initialServer, bufio.NewReader(initialServer), Request{Columns: 30, Rows: 12}, w.l.session, &Leader{Agent: w.name, Generation: w.l.generation}, nil, nil, nil, previous)
	}()
	answered(t, bufio.NewReader(initialClient))
	initialClient.Close()
	<-initialDone
	if screen := w.l.session.Screen(); screen.Columns != 80 || screen.Rows != 24 {
		t.Errorf("the displaced terminal's initial size changed the screen to %d by %d", screen.Columns, screen.Rows)
	}

	send := func(c *net.UnixConn, keys string) {
		line, _ := json.Marshal(ClientMessage{Keys: []byte(keys)})
		c.Write(append(line, '\n'))
	}
	send(older, "old")
	send(newer, "new")
	eventually(t, "the newer terminal's keys", func() bool { return strings.Contains(w.typedSoFar(), "new") })
	time.Sleep(100 * time.Millisecond)
	if got := w.typedSoFar(); got != "new" {
		t.Errorf("the worker's terminal received %q, want only the newer terminal's \"new\"", got)
	}
	taken := takeJournal(t, w.f.s, store.KindInputTaken)
	if len(taken) != 2 || taken[1]["moved"] != true || taken[1]["terminal"] != "/dev/pts/2" {
		t.Errorf("the journal holds the takes %v, want the second to have moved the hold", taken)
	}
	resize := func(c *net.UnixConn, columns, rows int, leave bool) {
		line, _ := json.Marshal(ClientMessage{Columns: columns, Rows: rows, Leave: leave})
		c.Write(append(line, '\n'))
	}
	resize(newer, 100, 28, false)
	eventually(t, "the new holder's size", func() bool {
		screen := w.l.session.Screen()
		return screen.Columns == 100 && screen.Rows == 28
	})
	resize(older, 40, 15, true)
	<-olderDone
	if screen := w.l.session.Screen(); screen.Columns != 100 || screen.Rows != 28 {
		t.Errorf("the displaced terminal's later size changed the screen to %d by %d", screen.Columns, screen.Rows)
	}
	older.Close()
	newer.Close()
	<-newerDone
}

// Only the user takes: an agent's credential is refused, whichever agent it
// is.
func TestAnAgentsCredentialCannotTake(t *testing.T) {
	f := newTwoResults(t)
	f.withWorkers(t)
	for _, credential := range []string{"worker0", "coordination"} {
		err := f.d.authorize(Request{Command: CommandTake, Agent: f.assignments[1].Worker, Session: credential})
		if err == nil || !strings.Contains(err.Error(), "an agent cannot run this command; ask the user to run `asmai take`") {
			t.Errorf("%s's credential taking gave %v, want it refused", credential, err)
		}
	}
	if err := f.d.authorize(Request{Command: CommandTake, Agent: f.assignments[1].Worker}); err != nil {
		t.Errorf("the user taking gave %v, want it allowed", err)
	}
}

func TestCoordinationsLeaderIsNotTaken(t *testing.T) {
	w := newTakenWorker(t, "the user's unsent chat")
	coordination := roles.LeaderOf(roles.Coordination)
	w.f.d.mu.Lock()
	delete(w.f.d.leaders, w.name)
	w.name, w.l.address, w.l.userInput = coordination.String(), coordination, true
	w.f.d.leaders[w.name] = w.l
	w.f.d.mu.Unlock()
	server, client := connectedPair(t)
	w.f.d.take(server, bufio.NewReader(server), Request{Command: CommandTake, Agent: roles.Coordination, Interrupt: true})
	if resp := answered(t, bufio.NewReader(client)); !strings.Contains(resp.Error, "asmai chat") {
		t.Errorf("taking Coordination was answered %+v, want the conversation command", resp)
	}
	if got := w.typedSoFar(); got != "" {
		t.Errorf("taking Coordination typed %q into the user's chat", got)
	}
	if draft, _ := providers.ClaudeCodeUnsentText(w.l.session.Screen()); draft != "the user's unsent chat" {
		t.Errorf("the user's unsent chat became %q", draft)
	}
	if cleared := takeJournal(t, w.f.s, store.KindDraftCleared); len(cleared) != 0 {
		t.Errorf("the user's chat was cleared as an automated draft: %v", cleared)
	}
	client.Close()
}

// The status line shows the input the user holds, in every attached
// terminal, and a frame carries it when it changes; the status shows the
// owner of the input too.
func TestTheStatusLineShowsTheInputTheUserHolds(t *testing.T) {
	w := newTakenWorker(t, "")
	w.boundary()
	empty := statusLine{prompts: []string{}, decisions: []string{}, jobs: []int64{}}
	if now := w.f.d.statusLine(empty); len(now.held) != 0 {
		t.Fatalf("before any take the status line shows %v held", now.held)
	}
	client, r, done := w.taking(t, "/dev/pts/7", false)
	answered(t, r)
	now := w.f.d.statusLine(empty)
	want := fmt.Sprintf("you hold %s's input", w.name)
	if len(now.held) != 1 || now.held[0] != want {
		t.Fatalf("the status line shows %v held, want %q", now.held, want)
	}
	if f, changed := now.frame(empty, false); !changed || f.Held == nil || len(*f.Held) != 1 {
		t.Errorf("the frame of the change is %+v, want it to carry the held input", f)
	}
	if _, changed := now.frame(now, false); changed {
		t.Errorf("a status line that did not change makes a frame")
	}
	if got := w.f.d.liveAgents()[w.name].input; got != InputUser {
		t.Errorf("the input owner is %q, want %q", got, InputUser)
	}
	client.Close()
	<-done
	// The hold outlives the terminal, and the status line goes on showing it.
	if now := w.f.d.statusLine(empty); len(now.held) != 1 {
		t.Errorf("after detaching the status line shows %v held, want the hold to remain", now.held)
	}
}

// The user answers a native prompt in the terminal of the agent they took, and
// the answer is journaled with the prompt and the dispatch once the agent is
// past it (03 rule 34).
func TestAnswerTypedAtANativePromptWhileTakingIsRecordedWithThePromptAndDispatch(t *testing.T) {
	w := newTakenWorker(t, "")
	w.working(41)
	ref := sessionRef{address: w.l.address, generation: w.l.generation}
	request := json.RawMessage(`{"hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"cd /workspace\nrm -rf build"}}`)
	w.f.d.promptObserved(ref, permissionRequested, request)
	client, r, done := w.taking(t, "/dev/pts/7", false)
	client.SetReadDeadline(time.Now().Add(time.Second))
	if resp := answered(t, r); resp.Error != "" {
		t.Fatalf("taking at the native prompt was answered %+v", resp)
	}
	answer := strings.Repeat("y", 300) + "\r"
	line, _ := json.Marshal(ClientMessage{Keys: []byte(answer)})
	client.Write(append(line, '\n'))
	eventually(t, "the answer in the agent's terminal", func() bool { return w.typedSoFar() == answer })
	if got := takeJournal(t, w.f.s, store.KindPromptAnswered); len(got) != 0 {
		t.Fatalf("the answer was journaled before the agent was past the prompt: %v", got)
	}
	line, _ = json.Marshal(ClientMessage{Keys: []byte("follow-up\r")})
	client.Write(append(line, '\n'))
	eventually(t, "the follow-up in the agent's terminal", func() bool { return w.typedSoFar() == answer+"follow-up\r" })
	w.f.d.promptObserved(ref, turnStopped, json.RawMessage(`{"hook_event_name":"Stop"}`))
	got := takeJournal(t, w.f.s, store.KindPromptAnswered)
	if len(got) != 1 || got[0]["agent"] != w.name || got[0]["terminal"] != "/dev/pts/7" || got[0]["keys"] != answer || got[0]["dispatch"] != float64(41) || got[0]["prompt"] != string(request) {
		t.Fatalf("the user's answer was journaled as %v", got)
	}
	client.Close()
	<-done
}

// Keys typed with no prompt waiting are not an answer to one.
func TestKeysTypedWithoutANativePromptAreNotJournaledAsAnswers(t *testing.T) {
	w := newTakenWorker(t, "")
	w.boundary()
	ref := sessionRef{address: w.l.address, generation: w.l.generation}
	client, r, done := w.taking(t, "/dev/pts/7", false)
	answered(t, r)
	line, _ := json.Marshal(ClientMessage{Keys: []byte("x")})
	client.Write(append(line, '\n'))
	eventually(t, "the keys", func() bool { return w.typedSoFar() == "x" })
	w.f.d.promptObserved(ref, permissionRequested, json.RawMessage(permissionRequest))
	w.f.d.promptObserved(ref, turnStopped, json.RawMessage(`{"hook_event_name":"Stop"}`))
	if got := takeJournal(t, w.f.s, store.KindPromptAnswered); len(got) != 0 {
		t.Errorf("journaled %v, want no answer", got)
	}
	client.Close()
	<-done
}

func TestMovedTakeRecordsOnlyTheAnswerFromTheNewTerminal(t *testing.T) {
	w := newTakenWorker(t, "")
	w.working(41)
	ref := sessionRef{address: w.l.address, generation: w.l.generation}
	w.f.d.promptObserved(ref, permissionRequested, json.RawMessage(permissionRequest))
	older, olderReader, olderDone := w.taking(t, "/dev/pts/1", false)
	answered(t, olderReader)
	line, _ := json.Marshal(ClientMessage{Keys: []byte("n")})
	older.Write(append(line, '\n'))
	eventually(t, "the first terminal's key", func() bool { return w.typedSoFar() == "n" })
	newer, newerReader, newerDone := w.taking(t, "/dev/pts/2", false)
	answered(t, newerReader)
	line, _ = json.Marshal(ClientMessage{Keys: []byte("y\r")})
	newer.Write(append(line, '\n'))
	eventually(t, "the second terminal's answer", func() bool { return w.typedSoFar() == "ny\r" })
	w.f.d.promptObserved(ref, turnStopped, json.RawMessage(`{"hook_event_name":"Stop"}`))
	got := takeJournal(t, w.f.s, store.KindPromptAnswered)
	if len(got) != 1 || got[0]["terminal"] != "/dev/pts/2" || got[0]["keys"] != "y\r" {
		t.Errorf("the moved take's answer was journaled as %v", got)
	}
	older.Close()
	newer.Close()
	<-olderDone
	<-newerDone
}

func TestPromptExitAndFailedInputDoNotRecordAnAnswer(t *testing.T) {
	for _, failedInput := range []bool{false, true} {
		name := "session exit"
		if failedInput {
			name = "failed input"
		}
		t.Run(name, func(t *testing.T) {
			w := newTakenWorker(t, "")
			w.working(41)
			ref := sessionRef{address: w.l.address, generation: w.l.generation}
			w.f.d.promptObserved(ref, permissionRequested, json.RawMessage(permissionRequest))
			client, r, done := w.taking(t, "/dev/pts/7", false)
			answered(t, r)
			w.f.d.mu.Lock()
			c := w.l.taken
			w.f.d.mu.Unlock()
			if failedInput {
				w.l.session.Stop(time.Second)
				w.f.d.typed(c, []byte("\r"))
				w.f.d.promptObserved(ref, turnStopped, json.RawMessage(`{"hook_event_name":"Stop"}`))
			} else {
				line, _ := json.Marshal(ClientMessage{Keys: []byte("\r")})
				client.Write(append(line, '\n'))
				eventually(t, "the key before exit", func() bool { return w.typedSoFar() == "\r" })
				w.f.d.mu.Lock()
				w.f.d.promptEnded(w.l, false)
				w.f.d.mu.Unlock()
			}
			if got := takeJournal(t, w.f.s, store.KindPromptAnswered); len(got) != 0 {
				t.Errorf("journaled an unconfirmed answer: %v", got)
			}
			client.Close()
			<-done
		})
	}
}

func TestConversationAnswerAtANativePromptIsRecorded(t *testing.T) {
	w := newTakenWorker(t, "")
	w.working(41)
	ref := sessionRef{address: w.l.address, generation: w.l.generation}
	w.f.d.promptObserved(ref, permissionRequested, json.RawMessage(permissionRequest))
	c := &conversation{leader: w.l, session: w.l.session, generation: w.l.generation, terminal: "/dev/pts/chat"}
	w.f.d.mu.Lock()
	w.f.d.conversation = c
	w.f.d.mu.Unlock()
	w.f.d.typed(c, []byte("\r"))
	w.f.d.promptObserved(ref, turnStopped, json.RawMessage(`{"hook_event_name":"Stop"}`))
	got := takeJournal(t, w.f.s, store.KindPromptAnswered)
	if len(got) != 1 || got[0]["terminal"] != "/dev/pts/chat" || got[0]["keys"] != "\r" {
		t.Errorf("the conversation's answer was journaled as %v", got)
	}
}
