// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/talvor/asmai/internal/providers"
	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/session"
	"github.com/talvor/asmai/internal/store"
)

// takeCheck is how often a take that waits for the end of an agent's turn
// looks at the agent again, and at its client.
const takeCheck = 50 * time.Millisecond

// interruptKey is the Esc key, which interrupts Claude Code's turn.
const interruptKey = "\x1b"

// atBoundary reports whether l's session is at a boundary the user may take
// its input at: it has no turn running, no automated submission under way and
// no message of the user's yet to be confirmed, and is ready for input or waits
// at a native prompt, which counts as a boundary (03 rules 32 and 34). d.mu is
// held.
func (l *leader) atBoundary() bool {
	return l.pendingDispatch == 0 && l.interrupted == 0 && !l.userInput && (l.prompt != nil || l.ready && l.currentDispatch == 0)
}

// take makes the user the input owner of the agent the request names, from
// the terminal the request names, and streams the agent's terminal to it as
// attach does, with the keys the user types typed into it. Unless the request
// interrupts, a running turn is waited for: the answer comes at its boundary,
// and nothing is typed for the agent meanwhile. Any automated draft left in
// the agent's input box is recorded and cleared before the answer, so the
// user never types into it. Detaching leaves the input with the user, and a
// newer take of the agent moves the hold to its terminal, whose older one only
// observes (03 rules 32, 33, 35 and 39).
func (d *daemon) take(conn *net.UnixConn, r *bufio.Reader, req Request) {
	address, err := roles.ParseAddress(req.Agent)
	if err != nil {
		reply(conn, Response{Error: err.Error()})
		return
	}
	if address == roles.LeaderOf(roles.Coordination) {
		reply(conn, Response{Error: fmt.Sprintf("%s is the conversation; use `asmai chat` to speak with it", address)})
		return
	}
	d.mu.Lock()
	l := d.leaders[address.String()]
	if l == nil || l.session == nil || d.stopping {
		d.mu.Unlock()
		reply(conn, Response{Error: fmt.Sprintf("%s is not running; `asmai status` shows the leaders and `asmai agents` the agents", address)})
		return
	}
	s, generation := l.session, l.generation
	l.taking++
	l.takeSeq++
	seq := l.takeSeq
	d.mu.Unlock()

	terminal := req.Terminal
	if terminal == "" {
		terminal = "a terminal asmai could not name"
	}
	c, interrupted, moved, dispatch, err := d.awaitBoundary(conn, r, l, s, seq, terminal, req.Interrupt)
	if err != nil {
		d.mu.Lock()
		if l.generation == generation {
			l.taking--
		}
		d.mu.Unlock()
		// The nudges the take held back go on.
		d.nudge(address.String())
		if !errors.Is(err, errTakeAbandoned) {
			reply(conn, Response{Error: err.Error()})
		}
		return
	}

	take := store.InputTake{Agent: address.String(), Role: address.Role, Generation: generation, Terminal: terminal, Interrupted: interrupted, Moved: moved, Dispatch: dispatch}
	if !moved {
		take.Draft, err = d.clearDraft(c, interrupted)
		if err != nil {
			d.mu.Lock()
			if l.generation == generation {
				l.taking--
			}
			d.mu.Unlock()
			l.takeMu.Unlock()
			d.nudge(address.String())
			reply(conn, Response{Error: fmt.Sprintf("clearing the automated draft: %v", err)})
			return
		}
	}
	d.mu.Lock()
	if l.session != s {
		restoreErr := restoreDraft(c, take.Draft)
		if l.generation == generation {
			l.taking--
		}
		d.mu.Unlock()
		l.takeMu.Unlock()
		err := fmt.Errorf("%s's session ended before its input could be taken", address)
		if restoreErr != nil {
			err = errors.Join(err, fmt.Errorf("restoring the automated draft: %w", restoreErr))
		}
		reply(conn, Response{Error: err.Error()})
		return
	}
	if _, err := d.store.InputTaken(take, time.Now()); err != nil {
		d.log.Error("journaling the user taking an agent's input", "agent", address.String(), "error", err.Error())
		restoreErr := restoreDraft(c, take.Draft)
		l.taking--
		d.mu.Unlock()
		l.takeMu.Unlock()
		if take.Draft == "" {
			d.nudge(address.String())
		}
		if restoreErr != nil {
			err = errors.Join(err, fmt.Errorf("restoring the automated draft: %w", restoreErr))
		}
		reply(conn, Response{Error: fmt.Sprintf("journaling the take: %v", err)})
		return
	}
	if older := l.taken; older != nil {
		older.displaced = true
		older.takenOver <- terminal
	}
	l.taken = c
	l.taking--
	d.mu.Unlock()
	l.takeMu.Unlock()
	d.log.Info("the user took an agent's input", "agent", address.String(), "generation", generation, "terminal", terminal, "interrupted", interrupted, "moved", moved, "draft", take.Draft != "")

	model := d.modelOf(address.String(), generation)
	attached := &Leader{Agent: address.String(), State: store.AgentRunning, Generation: generation, PID: s.PID(), Input: InputUser, Taken: terminal, Model: model.Name, Effort: model.Effort}
	how := d.stream(conn, r, req, s, attached, c.takenOver, nil, func(keys []byte) { d.typed(c, keys) }, c)
	d.log.Info("the user detached from an agent they hold", "agent", address.String(), "generation", generation, "terminal", terminal, "how", how)
}

// errTakeAbandoned is a take whose client went before it was granted.
var errTakeAbandoned = errors.New("the client went before the take was granted")

// awaitBoundary waits until l's session s, which a take counted in l.taking
// waits on, may be taken, and takes it for terminal: the agent is at a
// boundary, or the user already holds its input, which the take then moves,
// or interrupt is set, which has the running turn interrupted. It returns the
// take, whether it interrupted a turn or moved a hold, and the dispatch that
// was interrupted.
func (d *daemon) awaitBoundary(conn *net.UnixConn, r *bufio.Reader, l *leader, s *session.Session, seq uint64, terminal string, interrupt bool) (c *conversation, interrupted, moved bool, dispatch int64, err error) {
	for {
		l.takeMu.Lock()
		d.mu.Lock()
		switch {
		case l.session != s:
			d.mu.Unlock()
			l.takeMu.Unlock()
			return nil, false, false, 0, fmt.Errorf("%s's session ended before its input could be taken", l.address)
		case seq != l.takeSeq:
			d.mu.Unlock()
			l.takeMu.Unlock()
			return nil, false, false, 0, errors.New("a newer take superseded this take")
		case d.stopping:
			d.mu.Unlock()
			l.takeMu.Unlock()
			return nil, false, false, 0, errors.New("the factory is stopping")
		case l.taken != nil || interrupt && (l.currentDispatch != 0 || l.pendingDispatch != 0) || l.atBoundary():
			c = &conversation{leader: l, session: s, generation: l.generation, terminal: terminal, takenOver: make(chan string, 1)}
			if l.taken != nil {
				moved = true
			} else if !l.atBoundary() {
				interrupted = true
				dispatch = l.currentDispatch
				pending := l.pendingDispatch
				if dispatch == 0 {
					dispatch = pending
				}
				if err := s.Input([]byte(interruptKey)); err != nil {
					d.mu.Unlock()
					l.takeMu.Unlock()
					return nil, false, false, 0, fmt.Errorf("interrupting %s's turn: %w", l.address, err)
				}
				if pending != 0 {
					if err := d.store.InterruptedDispatch(pending, l.address.String(), l.generation, l.transcript, time.Now()); err != nil {
						d.mu.Unlock()
						l.takeMu.Unlock()
						return nil, false, false, 0, fmt.Errorf("settling %s's interrupted submission: %w", l.address, err)
					}
					l.renudge = pending
					l.pendingDispatch = 0
					l.pendingPrompt = ""
					l.inputUnknown = false
					l.ready = true
				} else if dispatch != 0 {
					l.interrupted = dispatch
				}
			}
			d.mu.Unlock()
			return c, interrupted, moved, dispatch, nil
		}
		d.mu.Unlock()
		l.takeMu.Unlock()

		// The client goes, or the wait goes on.
		conn.SetReadDeadline(time.Now().Add(takeCheck))
		if _, err := r.Peek(1); err != nil {
			var timeout net.Error
			if !errors.As(err, &timeout) || !timeout.Timeout() {
				return nil, false, false, 0, errTakeAbandoned
			}
		}
		conn.SetReadDeadline(time.Time{})
	}
}

// clearDraft records and clears what automation left unsent in the input box
// of the session c names, now that nothing is typed for it, and returns it:
// the text that stands in the box, which the user did not type, is never
// theirs to send. When the turn was interrupted the box is read once it has
// settled.
func (d *daemon) clearDraft(c *conversation, interrupted bool) (string, error) {
	if interrupted {
		c.session.Settle(settleQuiet, settleLimit)
	}
	draft, readable := providers.ClaudeCodeUnsentText(c.session.Screen())
	if !readable || draft == "" {
		return "", nil
	}
	if err := c.session.Input([]byte(providers.ClaudeCodeClearInput)); err != nil {
		d.log.Error("clearing an automated draft for the user taking the input", "agent", c.leader.address.String(), "error", err.Error())
		return "", err
	}
	c.session.Settle(settleQuiet, settleLimit)
	return draft, nil
}

func restoreDraft(c *conversation, draft string) error {
	if draft == "" {
		return nil
	}
	return c.session.Input([]byte(pasteStart + draft + pasteEnd))
}

// releasedMark tells the stream of a take that the user released the agent's
// input, in place of the terminal a newer take moved the hold to.
const releasedMark = "\x00released"

// release returns the input of the agent the request names, which the user
// holds, to automation, with the request's note. Whatever the user left unsent
// in the input box is cleared and recorded, so that no nudge is typed over it.
// If the user only answered native prompts, the same dispatch continues;
// otherwise the dispatch is superseded and the owning leader is sent the
// intervention record (03 rules 36 to 38).
func (d *daemon) release(conn *net.UnixConn, req Request) {
	address, err := roles.ParseAddress(req.Agent)
	if err != nil {
		reply(conn, Response{Error: err.Error()})
		return
	}
	d.mu.Lock()
	l := d.leaders[address.String()]
	if l == nil || l.session == nil || l.taken == nil {
		d.mu.Unlock()
		reply(conn, Response{Error: fmt.Sprintf("you do not hold %s's input; `asmai take %s` makes you its input owner", address, req.Agent)})
		return
	}
	d.mu.Unlock()

	// No key is typed while the box is read and cleared.
	l.takeMu.Lock()
	defer l.takeMu.Unlock()
	d.mu.Lock()
	c, s, generation := l.taken, l.session, l.generation
	if c == nil || s == nil {
		d.mu.Unlock()
		reply(conn, Response{Error: fmt.Sprintf("you do not hold %s's input", address)})
		return
	}
	d.mu.Unlock()
	var unsent string
	if text, readable := providers.ClaudeCodeUnsentText(s.Screen()); readable && text != "" {
		if err := s.Input([]byte(providers.ClaudeCodeClearInput)); err != nil {
			reply(conn, Response{Error: fmt.Sprintf("clearing the text you left unsent: %v", err)})
			return
		}
		s.Settle(settleQuiet, settleLimit)
		unsent = text
	}

	fallback := roles.LeaderOf(roles.Coordination).String()
	if address.Name != roles.Leader {
		fallback = roles.LeaderOf(address.Role).String()
	}
	d.mu.Lock()
	if l.taken != c || l.session != s {
		d.mu.Unlock()
		reply(conn, Response{Error: fmt.Sprintf("%s's session ended before its input could be released", address)})
		return
	}
	released, err := d.store.InputReleased(store.Release{Agent: address.String(), Role: address.Role, Generation: generation, Terminal: terminalOr(req.Terminal), Note: req.Note, Unsent: unsent, Fallback: fallback}, time.Now())
	if err != nil {
		d.mu.Unlock()
		d.log.Error("journaling the user releasing an agent's input", "agent", address.String(), "error", err.Error())
		reply(conn, Response{Error: fmt.Sprintf("journaling the release: %v", err)})
		return
	}
	l.taken = nil
	l.userInput = false
	l.submits, l.earlier = 0, 0
	if released.Superseded != 0 && l.renudge == released.Superseded {
		l.renudge = 0
	}
	if released.Superseded != 0 && l.interrupted == released.Superseded {
		l.interrupted = 0
	}
	c.displaced = true
	select {
	case c.takenOver <- releasedMark:
	default:
	}
	d.mu.Unlock()
	d.log.Info("the user released an agent's input", "agent", address.String(), "generation", generation, "messages", len(released.Intervention.Messages), "answers", len(released.Intervention.Answers), "superseded", released.Superseded, "assignment", released.Assignment)

	// The nudges the hold kept back go on, and the owning leader reads the
	// record.
	d.nudge(address.String())
	if released.Told.Agent != "" {
		d.nudge(released.Told.Agent)
	}
	reply(conn, Response{Released: &released})
}

func terminalOr(terminal string) string {
	if terminal == "" {
		return "a terminal asmai could not name"
	}
	return terminal
}
