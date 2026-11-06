// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"fmt"
	"slices"
	"time"

	"github.com/talvor/asmai/internal/providers"
	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/store"
)

// holdCheck is how often a held agent's screen is looked at for the input
// state it is waited at to end.
const holdCheck = 250 * time.Millisecond

// inputHold is an agent held at an input state its provider version's
// allow-list does not name (03 rules 24 and 25). Nothing is typed at it, its
// messages wait as the dispatches they are, and the hold ends only when the
// agent is at its input box again, or its session ends: the factory never
// cancels or retries what the agent was doing.
type inputHold struct {
	generation int
	since      time.Time
	screen     string
}

// modals is the allow-list of the qualified provider version agents run.
func (d *daemon) modals() []providers.Modal {
	if d.modalList != nil {
		return d.modalList
	}
	return providers.QualifiedModals(providers.ClaudeCode, d.cfg.Pins.ClaudeCode.Version)
}

// atKnownInput reports whether a nudge may be typed into l now. A session
// not at its input box is at an input state: one the allow-list names is
// answered with its safe default answer, which nothing is typed after, and
// any other holds the agent, and Coordination brings the hold to the user.
// Either way nothing is typed over the state. d.mu is held, and is held on
// return.
func (d *daemon) atKnownInput(l *leader) bool {
	screen := l.session.Screen()
	if providers.ClaudeCodeAtInput(screen) {
		d.liftHold(l, "the agent is at its input box")
		return true
	}
	pin := d.cfg.Pins.ClaudeCode.Version
	if m, ok := providers.MatchModal(d.modals(), screen); ok {
		if err := l.session.Input([]byte(m.Answer)); err != nil {
			d.log.Error("answering a qualified modal", "agent", l.address.String(), "modal", m.Name, "error", err.Error())
		} else {
			d.log.Info("a qualified modal is answered with its safe default", "agent", l.address.String(), "modal", m.Name)
			if err := d.store.ModalAnswered(store.ModalAnswered{Agent: l.address.String(), Generation: l.generation, Modal: m.Name, Provider: providers.ClaudeCode, Version: pin, Keys: string(m.Answer)}, time.Now()); err != nil {
				d.log.Error("journaling a qualified modal's answer", "agent", l.address.String(), "error", err.Error())
			}
		}
		d.watchHold(l)
		return false
	}
	if l.hold != nil && l.hold.generation == l.generation {
		return false
	}
	text := providers.ScreenText(screen)
	l.hold = &inputHold{generation: l.generation, since: time.Now(), screen: text}
	agent := l.address.String()
	d.log.Warn("an agent is held at an input state its provider version does not know; nothing is typed over it", "agent", agent, "generation", l.generation, "hidden_cursor", screen.Modes.HiddenCursor, "bracketed_paste", screen.Modes.BracketedPaste, "alt", screen.AltActive, "screen", text)
	hold := store.InputHold{Agent: agent, Generation: l.generation, Job: d.promptJob(l), Provider: providers.ClaudeCode, Version: pin, Screen: text}
	if next, err := d.store.NextDispatch(agent); err == nil {
		hold.Waiting = next.ID
		if hold.Job == 0 {
			if job, err := d.store.DispatchJob(next.ID); err == nil {
				hold.Job = job
			}
		}
	}
	recipient := ""
	if l.address != roles.LeaderOf(roles.Coordination) {
		recipient = roles.LeaderOf(roles.Coordination).String()
	}
	told, err := d.store.InputHeld(hold, recipient, time.Now())
	if err != nil {
		d.log.Error("journaling an agent's hold at an unknown input state", "agent", agent, "error", err.Error())
	} else if told.Agent != "" {
		go func() {
			if err := d.ensureAgent(roles.LeaderOf(roles.Coordination)); err != nil {
				d.log.Error("starting the agent an input hold is brought to", "error", err.Error())
			}
			d.nudge(told.Agent)
		}()
	}
	d.watchHold(l)
	return false
}

// liftHold ends l's hold, if it has one. d.mu is held.
func (d *daemon) liftHold(l *leader, why string) {
	if l.hold == nil {
		return
	}
	l.hold = nil
	d.log.Info("an agent is no longer held at an input state", "agent", l.address.String(), "why", why)
	if err := d.store.InputHoldLifted(store.InputHoldLifted{Agent: l.address.String(), Generation: l.generation, Why: why}, time.Now()); err != nil {
		d.log.Error("journaling the end of an input hold", "agent", l.address.String(), "error", err.Error())
	}
}

// watchHold looks at l's screen until the session is at its input box again
// or ends, and then tries the messages that waited. d.mu is held.
func (d *daemon) watchHold(l *leader) {
	s, generation, agent := l.session, l.generation, l.address.String()
	go func() {
		ticker := time.NewTicker(holdCheck)
		defer ticker.Stop()
		for {
			select {
			case <-s.Done():
				return
			case <-ticker.C:
			}
			if !providers.ClaudeCodeAtInput(s.Screen()) {
				continue
			}
			d.mu.Lock()
			cur := d.leaders[agent]
			if cur == nil || cur.session != s || cur.generation != generation {
				d.mu.Unlock()
				return
			}
			d.liftHold(cur, "the agent is at its input box")
			d.mu.Unlock()
			d.nudge(agent)
			return
		}
	}()
}

// inputHolds returns the status lines of the agents held at an input state
// the factory does not know, by agent. d.mu is not held.
func (d *daemon) inputHolds() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var lines []string
	for _, l := range d.leaders {
		if l.session != nil && l.hold != nil && l.hold.generation == l.generation {
			lines = append(lines, l.holdLine())
		}
	}
	slices.Sort(lines)
	return lines
}

func (l *leader) holdLine() string {
	return fmt.Sprintf("%s is held at an input state AsmAI does not know; nothing is typed at it", l.address.String())
}
