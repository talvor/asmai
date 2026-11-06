// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"fmt"
	"io"
	"log/slog"
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

// stateWorker is an agent whose terminal shows a dialog, not its input box,
// until the test creates ready, and then shows the input box. Every key typed
// into it is recorded in capture. It has one message waiting.
type stateWorker struct {
	s        *store.Store
	d        *daemon
	name     string
	l        *leader
	capture  string
	ready    string
	dispatch int64
}

func newStateWorker(t *testing.T, dialog string) stateWorker {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	const name = "leader@engineering"
	if _, err := s.SessionStarted(store.SessionStart{Agent: name, Role: roles.Engineering, Provider: "claude-code", Version: "v", Model: "m", Executable: "/bin/sh", Dir: t.TempDir(), PID: 1, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	dispatch := addPendingDispatch(t, s, name, store.DispatchCreated, 1)
	dir := t.TempDir()
	capture, ready := filepath.Join(dir, "typed"), filepath.Join(dir, "ready")
	rule := strings.Repeat("─", 60)
	box := fmt.Sprintf("\x1b[2J\x1b[H%s\r\n❯ \r\n%s\x1b[2;3H", rule, rule)
	script := `stty raw -echo; printf '%s' "$ASMAI_DIALOG"; while [ ! -e "$ASMAI_READY" ]; do sleep 0.05; done; printf '%s' "$ASMAI_BOX"; cat >> "$ASMAI_CAPTURE"`
	sess, err := session.Start(session.Spec{Path: "/bin/sh", Args: []string{"-c", script}, Env: append(os.Environ(), "ASMAI_DIALOG=\x1b[2J\x1b[H"+dialog, "ASMAI_BOX="+box, "ASMAI_READY="+ready, "ASMAI_CAPTURE="+capture), Dir: t.TempDir(), Columns: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Stop(time.Second) })
	eventually(t, "the worker's dialog", func() bool { return strings.Contains(providers.ScreenText(sess.Screen()), dialog) })
	l := &leader{address: roles.LeaderOf(roles.Engineering), session: sess, generation: 1, ready: true}
	d := &daemon{leaders: map[string]*leader{name: l}, store: s, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	return stateWorker{s: s, d: d, name: name, l: l, capture: capture, ready: ready, dispatch: dispatch}
}

func (w stateWorker) typed() string {
	got, _ := os.ReadFile(w.capture)
	return string(got)
}

func (w stateWorker) held() bool {
	w.d.mu.Lock()
	defer w.d.mu.Unlock()
	return w.l.hold != nil
}

// An input state that is not on the allow-list, which starts empty, holds the
// agent: nothing is typed over it, and its message stays a dispatch that
// waits, not cancelled and not retried.
func TestAnInputStateNotOnTheAllowListHoldsTheAgentAndNothingIsTypedOverIt(t *testing.T) {
	w := newStateWorker(t, "Claude Code has a new version. Update now?")
	if got := w.d.modals(); len(got) != 0 {
		t.Fatalf("the allow-list = %v, want empty", got)
	}
	before, err := w.s.NextDispatch(w.name)
	if err != nil {
		t.Fatal(err)
	}
	w.d.nudge(w.name)
	if !w.held() {
		t.Fatal("the agent is not held at the dialog")
	}
	time.Sleep(400 * time.Millisecond)
	if got := w.typed(); got != "" {
		t.Fatalf("the dialog was typed over: %q", got)
	}
	after, err := w.s.NextDispatch(w.name)
	if err != nil || after.ID != before.ID || after.State != before.State {
		t.Fatalf("the held agent's dispatch is %+v (%v), want %+v waiting, as it was", after, err, before)
	}
	if held := takeJournal(t, w.s, store.KindInputHeld); len(held) != 1 || held[0]["agent"] != w.name {
		t.Fatalf("the journal holds %v, want one hold of %s", held, w.name)
	}
	if lines := w.d.inputHolds(); len(lines) != 1 || !strings.Contains(lines[0], w.name) {
		t.Fatalf("the status line shows %v, want the hold", lines)
	}
	// A second nudge does not hold it again.
	w.d.nudge(w.name)
	if held := takeJournal(t, w.s, store.KindInputHeld); len(held) != 1 {
		t.Fatalf("the journal holds %d holds, want the one", len(held))
	}
}

// Coordination brings the hold to the user: its leader is sent a message
// about it, while the agent's own message waits.
func TestCoordinationIsToldOfTheHoldSoItCanBringItToTheUser(t *testing.T) {
	w := newStateWorker(t, "Choose a theme")
	w.d.nudge(w.name)
	coordination := roles.LeaderOf(roles.Coordination).String()
	next, err := w.s.NextDispatch(coordination)
	if err != nil {
		t.Fatalf("Coordination has no message about the hold: %v", err)
	}
	if next.Agent != coordination {
		t.Fatalf("dispatch %+v is not Coordination's", next)
	}
	var found bool
	for _, m := range takeJournal(t, w.s, store.KindMessageCreated) {
		if m["kind"] == store.MessageInputHeld && m["recipient"] == coordination {
			found = true
		}
	}
	if !found {
		t.Fatal("no message of kind input.held went to Coordination")
	}
}

// The hold ends when the agent is at its input box, and the message that
// waited is then typed, once.
func TestTheHoldEndsAtTheInputBoxAndTheWaitingMessageIsTyped(t *testing.T) {
	w := newStateWorker(t, "Choose a theme")
	w.d.nudge(w.name)
	if !w.held() {
		t.Fatal("the agent is not held")
	}
	if err := os.WriteFile(w.ready, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the hold to end", func() bool { return !w.held() })
	eventually(t, "the waiting message to be typed", func() bool { return strings.Contains(w.typed(), "asmai inbox --dispatch") })
	if lifted := takeJournal(t, w.s, store.KindInputHoldLifted); len(lifted) != 1 {
		t.Fatalf("the journal holds %v, want one end of a hold", lifted)
	}
}

// A modal on the allow-list is answered with its safe default; the nudge is
// not typed until the agent is at its input box.
func TestAQualifiedModalIsAnsweredWithItsSafeDefaultAndNothingElse(t *testing.T) {
	w := newStateWorker(t, "Claude Code has a new version. Update now?")
	w.d.modalList = []providers.Modal{{Name: "update", Shows: "new version", Answer: providers.AnswerEscape}}
	w.d.nudge(w.name)
	if err := os.WriteFile(w.ready, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("\x1basmai inbox --dispatch %d\r", w.dispatch)
	eventually(t, "the modal's answer and then the nudge", func() bool { return w.typed() == want })
	if w.held() {
		t.Fatal("a qualified modal holds the agent")
	}
	if answered := takeJournal(t, w.s, store.KindModalAnswered); len(answered) != 1 || answered[0]["modal"] != "update" {
		t.Fatalf("the journal holds %v, want the modal's answer", answered)
	}
}
