// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"slices"
	"time"

	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/store"
)

func (d *daemon) handoffCommand(conn *net.UnixConn, req Request) {
	var resp Response
	d.mu.Lock()
	ref, ok := d.sessions[req.Session]
	if !ok || d.leaders[ref.address.String()] == nil || d.leaders[ref.address.String()].session == nil || d.leaders[ref.address.String()].generation != ref.generation || d.stopping {
		d.mu.Unlock()
		reply(conn, Response{Error: "unknown or superseded agent session credential"})
		return
	}
	var err error
	switch req.Command {
	case CommandHandoffSend:
		address, e := roles.ParseAddress(req.Agent)
		if e != nil {
			err = e
		} else if address.Name != roles.Leader || address.Role == ref.address.Role || !slices.Contains(roles.Staffed, address.Role) {
			err = errors.New("send a handoff to another role's leader")
		} else {
			h := store.Handoff{Job: req.Job, Sender: ref.address.String(), Receiver: address.String(), Outcome: req.Outcome, Decisions: req.Decisions, Evidence: req.Evidence, Constraints: req.Constraints, Permissions: req.Permissions, Criteria: req.Criteria}
			var dispatch store.Dispatch
			h, dispatch, err = d.store.SendHandoff(h, time.Now())
			if err == nil {
				resp.Handoff = &h
				resp.Dispatch = &dispatch
				if startErr := d.ensureRoleLeader(address.Role); startErr != nil {
					d.log.Error("starting receiving leader", "agent", address.String(), "dispatch", dispatch.ID, "error", startErr)
				}
			}
		}
	case CommandHandoffAccept, CommandHandoffClarify, CommandHandoffDecline:
		state := store.HandoffAccepted
		if req.Command == CommandHandoffClarify {
			state = store.HandoffClarified
		}
		if req.Command == CommandHandoffDecline {
			state = store.HandoffDeclined
		}
		var h store.Handoff
		var dispatch store.Dispatch
		h, dispatch, err = d.store.AnswerHandoff(req.Handoff, ref.address.String(), state, req.Answer, time.Now())
		if err == nil {
			resp.Handoff = &h
			resp.Dispatch = &dispatch
			if startErr := d.ensureRoleLeader(roles.Coordination); startErr != nil {
				d.log.Error("starting receiving leader", "agent", roles.LeaderOf(roles.Coordination).String(), "dispatch", dispatch.ID, "error", startErr)
			}
		}
	case CommandInbox:
		resp.Messages, err = d.store.Inbox(ref.address.String(), req.Dispatch, time.Now())
	default:
		err = errors.New("unknown handoff command")
	}
	d.mu.Unlock()
	if err != nil {
		resp.Error = err.Error()
	}
	reply(conn, resp)
	if err == nil && (req.Command == CommandHandoffSend || req.Command == CommandHandoffAccept || req.Command == CommandHandoffClarify || req.Command == CommandHandoffDecline) && resp.Dispatch != nil {
		d.nudge(resp.Dispatch.Agent)
	}
}

// ensureRoleLeader is called with d.mu held. One role address has one leader.
func (d *daemon) ensureRoleLeader(role string) error {
	l := d.leader(role)
	if l.session != nil {
		return nil
	}
	checks, cfg, install := d.checks()
	if failed(checks) {
		return fmt.Errorf("cannot start %s's leader: factory checks failed", role)
	}
	if l.restart != nil {
		l.restart.Stop()
		l.restart = nil
	}
	return d.startLeader(l, cfg.Roles[role], install)
}

// boundary is the only time automation gains permission to try a nudge.
func (d *daemon) boundary(ref sessionRef, transcript string) {
	d.mu.Lock()
	l := d.leaders[ref.address.String()]
	if l == nil || l.session == nil || l.generation != ref.generation {
		d.mu.Unlock()
		return
	}
	if transcript != "" {
		l.transcript = transcript
	}
	if !l.inputUnknown && l.pendingDispatch == 0 {
		l.ready = true
	}
	d.mu.Unlock()
	d.nudge(ref.address.String())
}

// SessionStart is emitted before Claude Code draws its editor. Wait for the
// terminal's explicit input modes after that hook, rather than treating the
// hook or screen text alone as an input-ready boundary.
func (d *daemon) initialBoundary(ref sessionRef, transcript string) {
	d.mu.Lock()
	l := d.leaders[ref.address.String()]
	if l == nil || l.session == nil || l.generation != ref.generation {
		d.mu.Unlock()
		return
	}
	s := l.session
	d.mu.Unlock()
	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			state := s.Screen()
			if state.Modes.BracketedPaste && !state.Modes.HiddenCursor {
				d.boundary(ref, transcript)
				return
			}
			select {
			case <-s.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (d *daemon) nudge(agent string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	l := d.leaders[agent]
	if l == nil || l.session == nil || l.userInput || !l.ready || l.inputUnknown || l.pendingDispatch != 0 || d.stopping {
		return
	}
	dispatch, err := d.store.NextDispatch(agent)
	if errors.Is(err, sql.ErrNoRows) {
		return
	}
	if err != nil {
		d.log.Error("reading pending dispatch", "agent", agent, "error", err)
		return
	}
	prompt := fmt.Sprintf("asmai inbox --dispatch %d", dispatch.ID)
	// Record uncertainty before the PTY write. A crash or failed write cannot
	// create a false submission, and the same dispatch will never be retyped.
	if err = d.store.ChangeDispatch(dispatch.ID, l.generation, store.DispatchCreated, store.DispatchUnknown, l.transcript, time.Now()); err != nil {
		d.log.Error("recording nudge attempt", "error", err)
		return
	}
	l.ready = false
	l.inputUnknown = true
	l.pendingDispatch = dispatch.ID
	l.pendingPrompt = prompt
	if err = l.session.Input([]byte(prompt + "\r")); err != nil {
		d.log.Error("typing nudge", "dispatch", dispatch.ID, "error", err)
	}
}

func (d *daemon) automatedSubmitted(ref sessionRef, prompt, promptID, transcript string, payload json.RawMessage) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	l := d.leaders[ref.address.String()]
	if l == nil || l.session == nil || l.generation != ref.generation || l.pendingDispatch == 0 || prompt != l.pendingPrompt {
		return false, nil
	}
	if transcript != "" {
		l.transcript = transcript
	}
	err := d.store.ObservedAutomated(store.Dispatch{ID: l.pendingDispatch, Agent: ref.address.String(), Generation: ref.generation}, ref.address.Role, payload, l.transcript, time.Now())
	if err != nil {
		return true, err
	}
	l.currentDispatch = l.pendingDispatch
	l.currentPromptID = promptID
	l.pendingDispatch = 0
	l.pendingPrompt = ""
	l.inputUnknown = false
	return true, nil
}

func (d *daemon) stopCurrentDispatch(ref sessionRef, promptID, transcript string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	l := d.leaders[ref.address.String()]
	if l == nil || l.session == nil || l.generation != ref.generation || l.currentDispatch == 0 || promptID == "" || promptID != l.currentPromptID {
		return nil
	}
	id := l.currentDispatch
	l.currentDispatch = 0
	l.currentPromptID = ""
	return d.store.StopDispatchIfWorking(id, ref.generation, transcript, time.Now())
}
