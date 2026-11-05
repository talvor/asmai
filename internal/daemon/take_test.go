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
	script := `printf '%s' "$ASMAI_SCREEN"; stty raw -echo; cat >> "$ASMAI_CAPTURE"`
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
	if err := json.NewDecoder(r).Decode(&resp); err != nil {
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
	var early Response
	if err := json.NewDecoder(r).Decode(&early); err == nil {
		t.Fatalf("the take was answered in the middle of the turn: %+v", early)
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

// A newer take of an agent the user already holds moves the hold: the older
// terminal is told and only observes, the newer one types, nothing is cleared
// again, and the journal says the hold moved.
func TestANewerTakeMovesTheHoldToTheNewTerminal(t *testing.T) {
	w := newTakenWorker(t, "")
	w.boundary()
	older, olderReader, olderDone := w.taking(t, "/dev/pts/1", false)
	answered(t, olderReader)
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
	older.Close()
	newer.Close()
	<-olderDone
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

// The conversation is Coordination's input already, so it is not taken.
func TestCoordinationsLeaderIsNotTaken(t *testing.T) {
	f := newTwoResults(t)
	f.withWorkers(t)
	server, client := connectedPair(t)
	f.d.take(server, bufio.NewReader(server), Request{Command: CommandTake, Agent: roles.Coordination})
	resp := answered(t, bufio.NewReader(client))
	if !strings.Contains(resp.Error, "asmai chat") {
		t.Errorf("taking Coordination was answered %+v, want it sent to the conversation", resp)
	}
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
