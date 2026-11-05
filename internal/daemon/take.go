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
		if l.generation == generation {
			l.taking--
		}
		d.mu.Unlock()
		l.takeMu.Unlock()
		reply(conn, Response{Error: fmt.Sprintf("%s's session ended before its input could be taken", address)})
		return
	}
	if _, err := d.store.InputTaken(take, time.Now()); err != nil {
		d.log.Error("journaling the user taking an agent's input", "agent", address.String(), "error", err.Error())
		l.taking--
		d.mu.Unlock()
		l.takeMu.Unlock()
		d.nudge(address.String())
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
		case l.taken != nil || interrupt || l.atBoundary():
			c = &conversation{leader: l, session: s, generation: l.generation, terminal: terminal, takenOver: make(chan string, 1)}
			if l.taken != nil {
				moved = true
			} else if !l.atBoundary() {
				interrupted = true
				dispatch = l.currentDispatch
				if err := s.Input([]byte(interruptKey)); err != nil {
					d.mu.Unlock()
					l.takeMu.Unlock()
					return nil, false, false, 0, fmt.Errorf("interrupting %s's turn: %w", l.address, err)
				}
				if dispatch != 0 {
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
	if err := d.store.DraftRecorded(c.leader.address.String(), c.generation, c.terminal, draft, time.Now()); err != nil {
		return "", err
	}
	if err := c.session.Input([]byte(providers.ClaudeCodeClearInput)); err != nil {
		d.log.Error("clearing an automated draft for the user taking the input", "agent", c.leader.address.String(), "error", err.Error())
		return "", err
	}
	c.session.Settle(settleQuiet, settleLimit)
	return draft, nil
}
