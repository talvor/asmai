// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/providers"
	"github.com/talvor/asmai/internal/session"
	"github.com/talvor/asmai/internal/store"
	"github.com/talvor/asmai/internal/vt"
)

// M4's cases qualify the conversation and the user's intervention against the
// pinned Claude Code: what the daemon reads from its screen and the keys it
// types, which no scripted provider can vouch for.

const (
	// setupRepository is a fixture repository without the skills' setup, so
	// that registering it makes Coordination's leader a setup job to do: an
	// automated dispatch to the leader, which the user did not type.
	setupRepository = "qualification-setup"
)

// State returns the screen of agent as the daemon's emulation of its terminal
// has it.
func (f *Factory) State(agent string) (vt.State, error) {
	resp, err := daemon.Call(f.paths.Socket, daemon.Request{Command: daemon.CommandAttach, Agent: agent, Snapshot: true})
	if err != nil {
		return vt.State{}, err
	}
	if resp.Screen == nil {
		return vt.State{}, errors.New("the daemon did not answer with the agent's screen")
	}
	return *resp.Screen, nil
}

// Conversation is a terminal in the conversation with Coordination's
// leader, as `asmai chat` holds it, driven over the daemon's socket.
type Conversation struct {
	conn interface{ Close() error }
	enc  *json.Encoder
	// Frames are what the daemon sends this terminal after its answer.
	Frames chan daemon.Frame
	done   chan struct{}
	left   bool
}

// Converse enters the conversation as the terminal named terminal.
func (f *Factory) Converse(terminal string) (*Conversation, error) {
	conn, r, _, err := daemon.Open(f.paths.Socket, daemon.Request{Command: daemon.CommandConversation, Terminal: terminal})
	if err != nil {
		if conn != nil {
			conn.Close()
		}
		return nil, err
	}
	c := &Conversation{conn: conn, enc: json.NewEncoder(conn), Frames: make(chan daemon.Frame, 4096), done: make(chan struct{})}
	go func() {
		defer close(c.done)
		dec := json.NewDecoder(r)
		for {
			var fr daemon.Frame
			if err := dec.Decode(&fr); err != nil {
				return
			}
			select {
			case c.Frames <- fr:
			default:
			}
		}
	}()
	return c, nil
}

// Type types keys as the user does, each as an input event of its own, a
// moment apart.
func (c *Conversation) Type(keys ...string) error {
	for _, key := range keys {
		if err := c.enc.Encode(daemon.ClientMessage{Keys: []byte(key)}); err != nil {
			return err
		}
		time.Sleep(300 * time.Millisecond)
	}
	return nil
}

// Leave leaves the conversation, as Ctrl-] does, and waits for the daemon to
// have it.
func (c *Conversation) Leave() error {
	if c.left {
		return nil
	}
	c.left = true
	defer c.conn.Close()
	if err := c.enc.Encode(daemon.ClientMessage{Leave: true}); err != nil {
		return err
	}
	select {
	case <-c.done:
	case <-time.After(5 * time.Second):
	}
	return nil
}

// TakenOver returns the terminal that took the conversation over, once it
// has, and whether it has.
func (c *Conversation) TakenOver(wait time.Duration) (string, bool) {
	deadline := time.After(wait)
	for {
		select {
		case fr := <-c.Frames:
			if fr.TakenOver != "" {
				return fr.TakenOver, true
			}
		case <-deadline:
			return "", false
		}
	}
}

// entriesAfter returns the journal entries of kind after baseline.
func entriesAfter(entries []store.Entry, baseline int64, kind string) []store.Entry {
	var found []store.Entry
	for _, e := range entries {
		if e.ID > baseline && e.Kind == kind {
			found = append(found, e)
		}
	}
	return found
}

// coordinationReady starts the factory with Coordination's leader at its
// input box and settled, as a user's conversation finds it.
func (h *Harness) coordinationReady(ctx context.Context, f *Factory) (store.Agent, error) {
	if _, err := h.ready(ctx, f); err != nil {
		return store.Agent{}, err
	}
	leader, ok, err := f.Leader()
	if err != nil {
		return store.Agent{}, err
	}
	if !ok || leader.State != store.AgentRunning {
		return store.Agent{}, errors.New("Coordination's leader is not running")
	}
	if err := h.awaitSessionStart(ctx, f, leader); err != nil {
		return store.Agent{}, err
	}
	if err := h.awaitPromptSurface(ctx, f); err != nil {
		return store.Agent{}, err
	}
	return leader, h.awaitSettled(ctx, f)
}

// awaitScreen waits until the agent's screen satisfies ok.
func (h *Harness) awaitScreen(ctx context.Context, f *Factory, agent, what string, ok func(vt.State) bool) (vt.State, error) {
	deadline := time.Now().Add(h.Wait)
	for {
		if err := ctx.Err(); err != nil {
			return vt.State{}, err
		}
		state, err := f.State(agent)
		if err != nil {
			return vt.State{}, err
		}
		if ok(state) {
			return state, nil
		}
		if time.Now().After(deadline) {
			lines, _ := f.Screen()
			return state, fmt.Errorf("waiting for %s on %s's screen: it did not come within %s%s", what, agent, h.Wait, h.screenNote(lines))
		}
		time.Sleep(h.Poll)
	}
}

// unsentIs reports whether the agent's input box holds exactly text.
func unsentIs(text string) func(vt.State) bool {
	return func(s vt.State) bool {
		got, readable := providers.ClaudeCodeUnsentText(s)
		return readable && got == text
	}
}

// registerSetupRepository registers a repository that has not been set up for
// the skills, which makes Coordination's leader a setup job: an automated
// dispatch of the daemon's, not a message of the user's.
func (h *Harness) registerSetupRepository(ctx context.Context, f *Factory) error {
	origin := filepath.Join(f.paths.Dir, "qualification-fixtures", setupRepository+".git")
	if err := os.MkdirAll(filepath.Dir(origin), 0o700); err != nil {
		return err
	}
	if _, err := git(ctx, "/", "init", "--bare", "-b", "main", origin); err != nil {
		return err
	}
	work, err := os.MkdirTemp("", "asmai-qualification-fixture-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	if _, err = git(ctx, work, "clone", origin, "fixture"); err != nil {
		return err
	}
	checkout := filepath.Join(work, "fixture")
	if err := os.WriteFile(filepath.Join(checkout, "README.md"), []byte("# Fixture\n"), 0o644); err != nil {
		return err
	}
	if _, err = git(ctx, checkout, "add", "README.md"); err != nil {
		return err
	}
	if _, err = gitAs(ctx, checkout, harnessAuthor, "commit", "-m", "Initial fixture"); err != nil {
		return err
	}
	if _, err = git(ctx, checkout, "push", "origin", "HEAD:main"); err != nil {
		return err
	}
	clone := filepath.Join(f.paths.Repositories, setupRepository)
	return h.answerTrust(ctx, f, "asmai repo add", []string{"repo", "add", origin, "--name", setupRepository}, []string{clone})
}

// dispatchTo returns the first dispatch created for agent after baseline.
func dispatchTo(entries []store.Entry, baseline int64, agent string) (int64, bool) {
	for _, e := range entriesAfter(entries, baseline, store.KindDispatchChanged) {
		var d struct {
			ID    int64  `json:"id"`
			Agent string `json:"agent"`
			State string `json:"state"`
		}
		if json.Unmarshal(e.Data, &d) == nil && d.State == store.DispatchCreated && d.Agent == agent {
			return d.ID, true
		}
	}
	return 0, false
}

// c6 qualifies that a native input state which is not on the allow-list holds
// the agent instead of being typed over (03 rules 24 and 25): the allow-list
// of the pin starts empty, so Claude Code's model picker, opened with /model
// by the user, is such a state. With it on the leader's screen, a setup job
// for the leader is dispatched; the daemon must hold the agent, journal the
// hold with the screen, type nothing at the picker, and, once the user leaves
// the picker, lift the hold and deliver the job.
func (h *Harness) c6(ctx context.Context, f *Factory, r *Result) error {
	leader, err := h.coordinationReady(ctx, f)
	if err != nil {
		return err
	}
	pin := h.Subject.Pins.ClaudeCode.Version
	if modals := providers.QualifiedModals(providers.ClaudeCode, pin); len(modals) > 0 {
		r.observe("the allow-list of Claude Code %s names %d modal(s); the picker this case opens must not be one of them", pin, len(modals))
		for _, m := range modals {
			if strings.Contains(m.Shows, "model") {
				return fmt.Errorf("the allow-list names %q, so /model's picker is answered, not held: this case needs a state off the list", m.Name)
			}
		}
	}
	chat, err := f.Converse("the qualification harness")
	if err != nil {
		return err
	}
	if err := chat.Type("/model"); err != nil {
		chat.Leave()
		return err
	}
	if err := chat.Type("\r"); err != nil {
		chat.Leave()
		return err
	}
	picker, err := h.awaitScreen(ctx, f, leader.Agent, "a native picker over the input box", func(s vt.State) bool { return !providers.ClaudeCodeAtInput(s) })
	chat.Leave()
	if err != nil {
		return err
	}
	pickerText := providers.ScreenText(picker)
	r.observe("the user's /model opened a native picker on %s, which the allow-list does not name", leader.Agent)

	before, err := factoryJournal(f)
	if err != nil {
		return err
	}
	baseline := lastEntry(before)
	if err := h.registerSetupRepository(ctx, f); err != nil {
		return err
	}
	var held store.InputHold
	if err := h.awaitJournal(ctx, f, "the daemon to hold "+leader.Agent+" at the picker", func(entries []store.Entry) (bool, error) {
		for _, e := range entriesAfter(entries, baseline, store.KindInputHeld) {
			var hold store.InputHold
			if json.Unmarshal(e.Data, &hold) == nil && hold.Agent == leader.Agent {
				held = hold
				return true, nil
			}
		}
		return false, nil
	}); err != nil {
		return err
	}
	if held.Screen == "" {
		return errors.New("the hold's journal entry records no screen, so the user cannot be shown what the agent waits at")
	}
	r.observe("%s was held at the picker: the hold is journaled with its screen, and the dispatch for the setup job waits", leader.Agent)

	// Nothing may be typed over the picker while it is held.
	time.Sleep(3 * time.Second)
	after, err := f.State(leader.Agent)
	if err != nil {
		return err
	}
	if providers.ScreenText(after) != pickerText {
		return fmt.Errorf("while the agent was held its screen changed, so something was typed over the picker:\n%s", providers.ScreenText(after))
	}
	entries, err := factoryJournal(f)
	if err != nil {
		return err
	}
	if answered := entriesAfter(entries, baseline, store.KindModalAnswered); len(answered) > 0 {
		return errors.New("the daemon answered a modal although the allow-list is empty")
	}
	r.observe("while it was held the screen did not change: nothing was typed over the picker")

	// The user leaves the picker; the hold ends and the job is delivered.
	if err := f.Keys("\x1b"); err != nil {
		return err
	}
	return h.awaitJournal(ctx, f, "the hold to lift once the agent is at its input box", func(entries []store.Entry) (bool, error) {
		return len(entriesAfter(entries, baseline, store.KindInputHoldLifted)) > 0, nil
	})
}

// c10 qualifies the detection of the user's unsent text and the clearing of
// the input box (04 rules 15 and 16): text typed in the conversation and not
// sent is read from the box exactly; leaving the conversation saves it and
// clears the box, so that the daemon's nudge is never typed after it.
func (h *Harness) c10(ctx context.Context, f *Factory, r *Result) error {
	leader, err := h.coordinationReady(ctx, f)
	if err != nil {
		return err
	}
	const draft = "a half written thought"
	before, err := factoryJournal(f)
	if err != nil {
		return err
	}
	baseline := lastEntry(before)
	chat, err := f.Converse("the qualification harness")
	if err != nil {
		return err
	}
	if err := chat.Type(draft); err != nil {
		chat.Leave()
		return err
	}
	if _, err := h.awaitScreen(ctx, f, leader.Agent, fmt.Sprintf("the unsent text %q in the input box", draft), unsentIs(draft)); err != nil {
		chat.Leave()
		return err
	}
	r.observe("the unsent text %q was read exactly from %s's input box, past its prompt mark and placeholder", draft, leader.Agent)
	if err := chat.Leave(); err != nil {
		return err
	}
	if err := h.awaitJournal(ctx, f, "the unsent text to be saved", func(entries []store.Entry) (bool, error) {
		for _, e := range entriesAfter(entries, baseline, store.KindUnsentTextSaved) {
			var saved store.UnsentText
			if json.Unmarshal(e.Data, &saved) == nil && saved.Text == draft {
				return true, nil
			}
		}
		return false, nil
	}); err != nil {
		return err
	}
	r.observe("leaving the conversation saved the unsent text in the journal, word for word")

	// The text is the user's, and stays in the box until automation has
	// something to type: at that boundary the box is cleared, and the nudge
	// is typed alone, never after the user's text.
	mark, err := factoryJournal(f)
	if err != nil {
		return err
	}
	baseline = lastEntry(mark)
	if err := h.registerSetupRepository(ctx, f); err != nil {
		return err
	}
	dispatch, err := h.awaitNudge(ctx, f, leader.Agent, baseline)
	if err != nil {
		return fmt.Errorf("the nudge for the automated dispatch was not the only text typed, or never was: %w", err)
	}
	if _, err := h.awaitScreen(ctx, f, leader.Agent, "the saved text gone from the box", func(s vt.State) bool {
		text, readable := providers.ClaudeCodeUnsentText(s)
		return readable && !strings.Contains(text, draft)
	}); err != nil {
		return err
	}
	r.observe("when dispatch %d came for the leader the box was cleared first (Ctrl-C), and its nudge was submitted exactly, not after the user's text", dispatch)
	return nil
}

// awaitNudge waits for the automated dispatch created for agent after
// baseline, and for agent's session to submit exactly its nudge, `asmai inbox
// --dispatch N`. It returns the dispatch.
func (h *Harness) awaitNudge(ctx context.Context, f *Factory, agent string, baseline int64) (int64, error) {
	var dispatch int64
	if err := h.awaitJournal(ctx, f, "the automated dispatch to "+agent, func(entries []store.Entry) (bool, error) {
		id, ok := dispatchTo(entries, baseline, agent)
		dispatch = id
		return ok, nil
	}); err != nil {
		return 0, err
	}
	prompt := fmt.Sprintf("asmai inbox --dispatch %d", dispatch)
	err := h.awaitJournal(ctx, f, fmt.Sprintf("%s to submit %q", agent, prompt), func(entries []store.Entry) (bool, error) {
		for _, e := range entriesAfter(entries, baseline, store.KindObservation) {
			var d struct {
				Agent   string `json:"agent"`
				Event   string `json:"event"`
				Payload struct {
					Prompt string `json:"prompt"`
				} `json:"payload"`
			}
			if json.Unmarshal(e.Data, &d) == nil && d.Agent == agent && d.Event == "UserPromptSubmit" && d.Payload.Prompt == prompt {
				return true, nil
			}
		}
		return false, nil
	})
	return dispatch, err
}

// c13 qualifies that a second attach takes the conversation over (04 rule
// 19): the older terminal is told, only observes from then on — keys it types
// do not reach the agent — and its unsent text is saved.
func (h *Harness) c13(ctx context.Context, f *Factory, r *Result) error {
	leader, err := h.coordinationReady(ctx, f)
	if err != nil {
		return err
	}
	const draft = "typed in the older terminal"
	before, err := factoryJournal(f)
	if err != nil {
		return err
	}
	baseline := lastEntry(before)
	older, err := f.Converse("the older terminal")
	if err != nil {
		return err
	}
	defer older.Leave()
	if err := older.Type(draft); err != nil {
		return err
	}
	if _, err := h.awaitScreen(ctx, f, leader.Agent, "the older terminal's text in the box", unsentIs(draft)); err != nil {
		return err
	}
	newer, err := f.Converse("the newer terminal")
	if err != nil {
		return err
	}
	defer newer.Leave()
	by, ok := older.TakenOver(10 * time.Second)
	if !ok {
		return errors.New("the older terminal was not told that another terminal took the conversation over")
	}
	if by != "the newer terminal" {
		return fmt.Errorf("the older terminal was told %q took over, want the newer terminal", by)
	}
	r.observe("the older terminal was told that %q took the conversation over", by)

	if err := h.awaitJournal(ctx, f, "the older terminal's unsent text to be saved", func(entries []store.Entry) (bool, error) {
		for _, e := range entriesAfter(entries, baseline, store.KindUnsentTextSaved) {
			var saved store.UnsentText
			if json.Unmarshal(e.Data, &saved) == nil && saved.Text == draft && saved.Terminal == "the older terminal" {
				return true, nil
			}
		}
		return false, nil
	}); err != nil {
		return err
	}
	r.observe("its unsent text %q was saved in the journal against that terminal", draft)

	// The older terminal only observes: what it types does not reach the box.
	if err := older.Type("must not arrive"); err != nil {
		return err
	}
	time.Sleep(time.Second)
	state, err := f.State(leader.Agent)
	if err != nil {
		return err
	}
	if text, _ := providers.ClaudeCodeUnsentText(state); strings.Contains(text, "must not arrive") {
		return fmt.Errorf("a key typed in the older terminal reached the agent's box (%q)", text)
	}
	r.observe("a key the older terminal typed afterwards did not reach the agent")

	if err := newer.Type("x"); err != nil {
		return err
	}
	if _, err := h.awaitScreen(ctx, f, leader.Agent, "the newer terminal's key in the box", func(s vt.State) bool {
		text, readable := providers.ClaudeCodeUnsentText(s)
		return readable && strings.HasSuffix(text, "x")
	}); err != nil {
		return fmt.Errorf("the newer terminal's keys did not reach the agent: %w", err)
	}
	r.observe("the newer terminal's keys reach the agent")
	return newer.Type("\x7f")
}

// c9 qualifies the attach client's rendering of the pinned provider's screen
// under its status line at reduced height (03 rule 17): at each height the
// agent's terminal is drawn above the status line, which has the last row to
// itself, and the box the user types in stays on screen.
func (h *Harness) c9(ctx context.Context, f *Factory, r *Result) error {
	leader, err := h.coordinationReady(ctx, f)
	if err != nil {
		return err
	}
	const columns = 100
	env := envValue(f.env, "TERM", "xterm-256color")
	s, err := session.Start(session.Spec{Path: f.asmai, Args: []string{"attach", leader.Agent}, Env: env, Dir: "/", Columns: columns, Rows: 24})
	if err != nil {
		return fmt.Errorf("running asmai attach: %w", err)
	}
	defer s.Stop(5 * time.Second)
	for _, rows := range []int{24, 14, 10, 8} {
		if err := s.Resize(columns, rows); err != nil {
			return err
		}
		var lines []string
		deadline := time.Now().Add(h.Wait)
		for {
			if lines, err = stateLines(s.Screen()); err != nil {
				return err
			}
			if len(lines) == rows && strings.Contains(lines[rows-1], leader.Agent+" · observing") && promptSurface(lines[:rows-1]) {
				break
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			select {
			case <-s.Done():
				return fmt.Errorf("asmai attach ended at %d rows", rows)
			default:
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("at %d rows the attach client did not draw the status line on the last row with the agent's input prompt above it%s", rows, h.screenNote(lines))
			}
			time.Sleep(h.Poll)
		}
		if state := s.Screen(); !statusLineInverse(state, rows-1) {
			return fmt.Errorf("at %d rows the status line on the last row is not drawn inverse", rows)
		}
		r.observe("at %d rows the agent's screen is drawn in the %d rows above the status line, which keeps the last row, and %s's input prompt is on screen", rows, rows-1, leader.Agent)
	}
	return nil
}

// statusLineInverse reports whether row y of state is drawn in reverse video.
func statusLineInverse(state vt.State, y int) bool {
	lines := state.Main.Lines
	if state.AltActive {
		lines = state.Alt.Lines
	}
	if y < 0 || y >= len(lines) || len(lines[y]) == 0 {
		return false
	}
	return lines[y][0].Flags&vt.Inverse != 0
}

// c12 qualifies the recognition of an interrupted Claude turn (03 rule 31;
// the probe saw no Interrupt hook from Claude Code): the user's Esc in the
// conversation during the turn of an automated dispatch ends the turn with no
// report, and the daemon must journal the dispatch as interrupted from that
// alone, so that it is nudged again rather than counted done.
func (h *Harness) c12(ctx context.Context, f *Factory, r *Result) error {
	leader, err := h.coordinationReady(ctx, f)
	if err != nil {
		return err
	}
	before, err := factoryJournal(f)
	if err != nil {
		return err
	}
	baseline := lastEntry(before)
	if err := h.registerSetupRepository(ctx, f); err != nil {
		return err
	}
	dispatch, err := h.awaitNudge(ctx, f, leader.Agent, baseline)
	if err != nil {
		return err
	}
	// The user interrupts the turn.
	if err := f.Keys("\x1b"); err != nil {
		return err
	}
	// Claude Code reports nothing for the interrupted turn, so the first sign
	// that it ended is the Stop of the next turn, under another prompt: the
	// user's next message.
	number := time.Now().UnixNano()
	if h.Number != nil {
		number = h.Number()
	}
	if err := f.Submit(fmt.Sprintf("Qualification exercise %d. Reply with the single word ok and run no tool.", number)); err != nil {
		return err
	}
	if err := h.awaitJournal(ctx, f, fmt.Sprintf("dispatch %d to be journaled as interrupted", dispatch), func(entries []store.Entry) (bool, error) {
		for _, e := range entriesAfter(entries, baseline, store.KindDispatchChanged) {
			var d struct {
				ID          int64 `json:"id"`
				Interrupted bool  `json:"interrupted"`
			}
			if json.Unmarshal(e.Data, &d) == nil && d.ID == dispatch && d.Interrupted {
				return true, nil
			}
		}
		return false, nil
	}); err != nil {
		return fmt.Errorf("the user's Esc during the turn was not recognised as an interruption: %w", err)
	}
	r.observe("the user's Esc during the turn of dispatch %d ended it with no Interrupt hook, and the Stop of the user's next turn, under another prompt, had the daemon journal the dispatch as interrupted", dispatch)
	return nil
}

// c14 qualifies terminal recording per dispatch and its replay (03 rules 41
// to 43): the dispatch the leader is nudged with is recorded, and `asmai
// replay` plays that recording on a terminal, with what the agent showed in it.
func (h *Harness) c14(ctx context.Context, f *Factory, r *Result) error {
	e, err := h.exerciseHandoff(ctx, f, r)
	if err != nil {
		return err
	}
	if err := h.awaitJournal(ctx, f, fmt.Sprintf("dispatch %d's recording to finish", e.dispatch), func(entries []store.Entry) (bool, error) {
		for _, en := range entriesAfter(entries, 0, store.KindRecordingFinished) {
			var fin struct {
				Dispatch int64 `json:"dispatch"`
			}
			if json.Unmarshal(en.Data, &fin) == nil && fin.Dispatch == e.dispatch {
				return true, nil
			}
		}
		return false, nil
	}); err != nil {
		return err
	}
	resp, err := daemon.Call(f.paths.Socket, daemon.Request{Command: daemon.CommandReplay, Agent: engineeringLeader, Dispatch: e.dispatch})
	if err != nil {
		return err
	}
	if resp.Error != "" || resp.Recording == nil {
		return fmt.Errorf("the daemon has no recording of dispatch %d to replay: %s", e.dispatch, resp.Error)
	}
	recorded := *resp.Recording
	info, err := os.Stat(recorded.Path)
	if err != nil || info.Size() == 0 {
		return fmt.Errorf("the recording of dispatch %d at %s is missing or empty (%v)", e.dispatch, recorded.Path, err)
	}
	r.observe("dispatch %d of %s was recorded to %s (%d bytes)", e.dispatch, engineeringLeader, filepath.Base(recorded.Path), info.Size())

	// The replay is played at the pace of the recording, each pause at most
	// two seconds, so it may take a while.
	replayCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	stdout, stderr, code, err := f.Run(replayCtx, "", "replay", engineeringLeader, "--dispatch", fmt.Sprint(e.dispatch))
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("asmai replay exited %d: %s", code, strings.TrimSpace(stderr))
	}
	if !strings.Contains(stderr, "End of the recording.") {
		return fmt.Errorf("asmai replay did not reach the end of the recording: %q", stderr)
	}
	if !strings.Contains(stdout, fmt.Sprintf("asmai inbox --dispatch %d", e.dispatch)) {
		return fmt.Errorf("the replay of dispatch %d does not show the nudge the agent was typed (%d bytes played)", e.dispatch, len(stdout))
	}
	r.observe("`asmai replay` played the recording to its end, and it shows the nudge typed for the dispatch")
	return nil
}

// c8 qualifies taking an agent mid-turn and with an unsent automated draft
// (03 rules 32 to 35): taking waits for the turn's boundary, or with
// --interrupt interrupts the turn; the taker never types into an automated
// draft, which is recorded and cleared.
func (h *Harness) c8(ctx context.Context, f *Factory, r *Result) error {
	if _, err := h.exerciseHandoff(ctx, f, r); err != nil {
		return err
	}
	engineering := engineeringLeader
	waiting := func(args ...string) (*session.Session, error) {
		env := envValue(f.env, "TERM", "xterm-256color")
		return session.Start(session.Spec{Path: f.asmai, Args: args, Env: env, Dir: "/", Columns: 100, Rows: 24})
	}
	taken := func(interrupt bool, baseline int64) (store.Entry, error) {
		var found store.Entry
		err := h.awaitJournal(ctx, f, "the take to be journaled", func(entries []store.Entry) (bool, error) {
			for _, en := range entriesAfter(entries, baseline, store.KindInputTaken) {
				var t store.InputTake
				if json.Unmarshal(en.Data, &t) == nil && t.Agent == engineering && t.Interrupted == interrupt {
					found = en
					return true, nil
				}
			}
			return false, nil
		})
		return found, err
	}
	release := func() error {
		stdout, stderr, code, err := f.Run(ctx, "", "release", engineering)
		if err != nil {
			return err
		}
		if code != 0 {
			return fmt.Errorf("asmai release exited %d: %s%s", code, stdout, stderr)
		}
		return nil
	}

	// Taking waits for the end of a turn that runs: the handoff's turn may be
	// over by now, in which case the boundary is at once.
	before, err := factoryJournal(f)
	if err != nil {
		return err
	}
	baseline := lastEntry(before)
	s, err := waiting("take", engineering)
	if err != nil {
		return err
	}
	entry, err := taken(false, baseline)
	if err != nil {
		s.Stop(5 * time.Second)
		return err
	}
	r.observe("the user took %s's input once its turn had ended (journal entry %d)", engineering, entry.ID)
	s.Stop(5 * time.Second)
	if err := release(); err != nil {
		return err
	}

	// A second turn, and a take that interrupts it.
	before, err = factoryJournal(f)
	if err != nil {
		return err
	}
	baseline = lastEntry(before)
	if err := f.Submit(h.followUpPrompt()); err != nil {
		return err
	}
	if err := h.awaitJournal(ctx, f, "Engineering's second turn to start", func(entries []store.Entry) (bool, error) {
		for _, en := range entriesAfter(entries, baseline, store.KindObservation) {
			var d struct {
				Agent string `json:"agent"`
				Event string `json:"event"`
			}
			if json.Unmarshal(en.Data, &d) == nil && d.Agent == engineering && d.Event == "UserPromptSubmit" {
				return true, nil
			}
		}
		return false, nil
	}); err != nil {
		return err
	}
	s, err = waiting("take", engineering, "--interrupt")
	if err != nil {
		return err
	}
	defer s.Stop(5 * time.Second)
	entry, err = taken(true, baseline)
	if err != nil {
		return err
	}
	r.observe("`asmai take --interrupt` interrupted %s's running turn and took its input (journal entry %d)", engineering, entry.ID)

	// An automated draft cannot be left in a real session's input box without
	// racing the daemon's own typing; the box is empty when the user takes it.
	state, err := f.State(engineering)
	if err != nil {
		return err
	}
	if text, readable := providers.ClaudeCodeUnsentText(state); readable && text != "" {
		return fmt.Errorf("after the take the input box holds %q, which the user would type into", text)
	}
	r.observe("the box the user types into holds no automated text after the take")
	r.limit("an unsent automated draft in a real session", "the draft's recording and clearing are exercised by the daemon's development tests with the scripted provider; a real Claude Code session cannot be made to hold one without racing the daemon's own typing")
	return release()
}

// followUpPrompt is the request that has Coordination send Engineering one
// more handoff for the case's job, so that Engineering has a second turn.
func (h *Harness) followUpPrompt() string {
	number := time.Now().UnixNano()
	if h.Number != nil {
		number = h.Number() + 1
	}
	return fmt.Sprintf("Qualification exercise %d. Send Engineering one more handoff for the job you opened, to acknowledge it, using 'none' for decisions, constraints and permissions and citing this message as evidence. Execute the asmai command now.", number)
}

// stateLines returns the rows of text state shows.
func stateLines(state vt.State) ([]string, error) {
	t, err := vt.FromState(state)
	if err != nil {
		return nil, err
	}
	return t.Lines(), nil
}
