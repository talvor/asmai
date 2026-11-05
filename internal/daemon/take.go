// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"fmt"
	"io"
	"net"
	"time"

	"github.com/talvor/asmai/internal/providers"
	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/session"
)

// How a take handed the agent's input to the user.
const (
	// TookAtBoundary is a take that waited for the agent's turn to finish.
	TookAtBoundary = "boundary"
	// TookByInterrupt is a take that interrupted the agent's turn.
	TookByInterrupt = "interrupt"
	// TookByMoving is a take that moved the hold of an earlier take, whose
	// handover had happened already.
	TookByMoving = "moved"
)

// takePoll is how often a take waiting for the turn boundary looks at the
// agent again.
const takePoll = 20 * time.Millisecond

// escape is the key that interrupts Claude Code's turn.
const escape = "\x1b"

// Taken is what a take did: the agent whose input the user holds now, the
// terminal they hold it from, how the handover happened, and the text the
// agent's input box held then, if any, which is saved in the journal and
// cleared.
type Taken struct {
	Agent     string `json:"agent"`
	Terminal  string `json:"terminal"`
	How       string `json:"how"`
	MovedFrom string `json:"movedFrom,omitempty"`
	Unsent    string `json:"unsent,omitempty"`
}

// working reports whether l is at a turn of its own rather than at a
// boundary, with an automated message submitted, being submitted or in an
// unknown state. d.mu is held.
func (l *leader) working() bool {
	return !l.ready || l.inputUnknown || l.pendingDispatch != 0 || l.currentDispatch != 0
}

// take makes the user the input owner of the agent the request addresses, and
// answers once it is. From the take on, nothing automated is typed into the
// agent: the messages for it wait in its inbox. By default it waits for the
// agent's turn boundary; with Interrupt it interrupts the turn. At the
// handover, the text the agent's input box holds is an automated draft, which
// the journal saves and Ctrl-L clears, so that the user starts with an empty
// box and the draft is never typed again. A newer take replaces an earlier
// one: if that one still waits for the boundary it ends, and otherwise the
// hold moves with the input box left as the user has it.
func (d *daemon) take(conn *net.UnixConn, req Request) {
	conn.SetDeadline(time.Time{})
	taken, err := d.takeInput(conn, req)
	if err != nil {
		reply(conn, Response{Error: err.Error()})
		return
	}
	reply(conn, Response{Taken: &taken})
}

func (d *daemon) takeInput(conn *net.UnixConn, req Request) (Taken, error) {
	address, err := roles.ParseAddress(req.Agent)
	if err != nil {
		return Taken{}, err
	}
	if address == roles.LeaderOf(roles.Coordination) {
		return Taken{}, fmt.Errorf("%s's input is the user's in the conversation already: use `asmai chat`", address)
	}
	terminal := req.Terminal
	if terminal == "" {
		terminal = "a terminal asmai could not name"
	}
	mine := &inputHold{terminal: terminal}

	d.mu.Lock()
	l := d.leaders[address.String()]
	if l == nil || l.session == nil {
		d.mu.Unlock()
		return Taken{}, fmt.Errorf("%s is not running; `asmai status` shows the leaders and `asmai agents` the agents", address)
	}
	if d.stopping {
		d.mu.Unlock()
		return Taken{}, fmt.Errorf("the factory is stopping")
	}
	s, generation := l.session, l.generation
	previous := l.held
	// Automation is paused from now on, whether or not the handover waits.
	l.held = mine
	moved := ""
	if previous != nil && previous.terminal != terminal {
		moved = previous.terminal
	}
	if previous != nil && previous.taken {
		// The hold moves, and the input box is the user's to leave as it is.
		mine.taken = true
		d.mu.Unlock()
		return d.handedOver(l, s, generation, mine, TookByMoving, moved, "")
	}
	working := l.working()
	if req.Interrupt && working && l.currentDispatch != 0 {
		l.interrupted = l.currentDispatch
	}
	d.mu.Unlock()

	how := TookAtBoundary
	if req.Interrupt {
		how = TookByInterrupt
		if working {
			if err := s.Input([]byte(escape)); err != nil {
				d.release(l, mine)
				return Taken{}, fmt.Errorf("interrupting %s: %w", address, err)
			}
		}
	} else {
		d.log.Info("a take waits for the agent's turn boundary", "agent", address.String(), "terminal", terminal)
		if err := d.awaitBoundary(conn, l, s, mine); err != nil {
			return Taken{}, err
		}
	}

	// The text is read once the keys typed last are drawn.
	s.Settle(settleQuiet, settleLimit)
	unsent, readable := providers.ClaudeCodeUnsentText(s.Screen())
	if readable && unsent != "" {
		if err := s.Input([]byte(providers.ClaudeCodeClearInput)); err != nil {
			d.release(l, mine)
			return Taken{}, fmt.Errorf("clearing %s's unsent draft: %w", address, err)
		}
		s.Settle(settleQuiet, settleLimit)
	} else {
		unsent = ""
	}

	d.mu.Lock()
	if l.held != mine || l.session != s {
		d.mu.Unlock()
		return Taken{}, d.displacedTake(l, address.String())
	}
	mine.taken = true
	d.mu.Unlock()
	return d.handedOver(l, s, generation, mine, how, moved, unsent)
}

// awaitBoundary waits until l's session is at a turn boundary, or the take
// ends first: a newer take replaced it, the session ended, the factory is
// stopping or the user's command went away.
func (d *daemon) awaitBoundary(conn *net.UnixConn, l *leader, s *session.Session, mine *inputHold) error {
	gone := make(chan struct{})
	go func() {
		io.Copy(io.Discard, conn)
		close(gone)
	}()
	tick := time.NewTicker(takePoll)
	defer tick.Stop()
	for {
		d.mu.Lock()
		switch {
		case l.held != mine:
			d.mu.Unlock()
			return d.displacedTake(l, l.address.String())
		case l.session != s || d.stopping:
			l.held = nil
			d.mu.Unlock()
			return fmt.Errorf("%s's session ended before its turn boundary", l.address)
		case !l.working():
			d.mu.Unlock()
			return nil
		}
		d.mu.Unlock()
		select {
		case <-gone:
			d.release(l, mine)
			return fmt.Errorf("the take was abandoned; %s is back under automation", l.address)
		case <-s.Done():
		case <-tick.C:
		}
	}
}

// displacedTake is the error of a take that a newer one replaced.
func (d *daemon) displacedTake(l *leader, agent string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if l.held == nil {
		return fmt.Errorf("%s's session ended before the handover", agent)
	}
	return fmt.Errorf("a newer take from %s holds %s's input now", l.held.terminal, agent)
}

// release drops hold, which nobody completed, and lets automation go on.
func (d *daemon) release(l *leader, hold *inputHold) {
	d.mu.Lock()
	if l.held == hold {
		l.held = nil
	}
	d.mu.Unlock()
	d.nudge(l.address.String())
}

// handedOver journals the take of l's session s and answers with it.
func (d *daemon) handedOver(l *leader, s *session.Session, generation int, hold *inputHold, how, movedFrom, unsent string) (Taken, error) {
	if err := d.store.InputTaken(l.address.String(), l.address.Role, generation, hold.terminal, how, movedFrom, unsent, time.Now()); err != nil {
		d.release(l, hold)
		return Taken{}, fmt.Errorf("journaling the take: %w", err)
	}
	d.log.Info("the user took an agent's input", "agent", l.address.String(), "generation", generation, "terminal", hold.terminal, "how", how, "unsent", unsent != "")
	return Taken{Agent: l.address.String(), Terminal: hold.terminal, How: how, MovedFrom: movedFrom, Unsent: unsent}, nil
}
