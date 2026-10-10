// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/talvor/asmai/internal/repos"
	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/store"
)

// handoffTo is the role each role's leader may hand work to: Coordination
// hands a job to Engineering, and Engineering hands validation of the
// finished job branch to Quality.
var handoffTo = map[string]string{
	roles.Coordination: roles.Engineering,
	roles.Engineering:  roles.Quality,
}

func (d *daemon) handoffCommand(conn *net.UnixConn, req Request) {
	var resp Response
	// Validation is handed over at the job branch's exact head, which is
	// read before the daemon's state is locked: it fetches from origin.
	head := ""
	if req.Command == CommandHandoffSend {
		if address, err := roles.ParseAddress(req.Agent); err == nil && address == roles.LeaderOf(roles.Quality) {
			conn.SetDeadline(time.Now().Add(gitTimeout + time.Minute))
			var err error
			if head, err = d.validationHead(req); err != nil {
				reply(conn, Response{Error: err.Error()})
				return
			}
		}
	}
	d.mu.Lock()
	ref, ok := d.sessions[req.Session]
	l := d.leaders[ref.address.String()]
	if !ok || l == nil || l.session == nil || l.generation != ref.generation || d.stopping {
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
		} else if address.Name != roles.Leader || handoffTo[ref.address.Role] != address.Role {
			err = errors.New("send a handoff to another role's leader: Coordination hands a job to Engineering, and Engineering hands validation to Quality")
		} else {
			h := store.Handoff{Job: req.Job, Sender: ref.address.String(), Receiver: address.String(), Outcome: req.Outcome, Decisions: req.Decisions, Evidence: req.Evidence, Constraints: req.Constraints, Permissions: req.Permissions, Criteria: req.Criteria, Head: head}
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
		h, dispatch, err = d.store.AnswerHandoff(req.Handoff, ref.address.String(), l.currentDispatch, ref.generation, state, req.Answer, time.Now())
		if err == nil {
			resp.Handoff = &h
			resp.Dispatch = &dispatch
			// The answer goes back to the leader that sent the handoff.
			if sender, parseErr := roles.ParseAddress(h.Sender); parseErr == nil {
				if startErr := d.ensureRoleLeader(sender.Role); startErr != nil {
					d.log.Error("starting receiving leader", "agent", sender.String(), "dispatch", dispatch.ID, "error", startErr)
				}
			}
		}
	case CommandInbox:
		resp.Messages, err = d.store.Inbox(ref.address.String(), req.Dispatch, time.Now())
		if err == nil {
			for i := range resp.Messages {
				if err = d.workFetchedReply(l, ref, &resp.Messages[i]); err != nil {
					break
				}
			}
		}
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

// workFetchedReply moves the fetched dispatch of a message that is work to do
// at once to working: a reply to a handoff, an assignment for its worker, and
// a result or a blocked report for the assignment's owning leader. Unlike a
// handoff, none of them is answered by a command that starts the work. A
// rejection and a cancellation are for the assignment's worker too, as is a
// resumption after a restart; a restored leader's brief to load and a
// reconciliation for an assignment's owning leader are work at once too.
func (d *daemon) workFetchedReply(l *leader, ref sessionRef, m *store.Message) error {
	switch m.Kind {
	case store.HandoffAccepted, store.HandoffClarified, store.HandoffDeclined, store.MessageAssignment, store.MessageRejection, store.MessageCancellation, store.MessageValidation, store.ReportResult, store.ReportBlocked,
		store.MessageResumption, store.MessageReconciliation, store.MessageRestoration:
	default:
		return nil
	}
	if m.Dispatch != l.currentDispatch || m.State != store.DispatchDelivered {
		return nil
	}
	if err := d.store.WorkFetchedDispatch(m.Dispatch, ref.address.String(), ref.generation, time.Now()); err != nil {
		return err
	}
	m.State = store.DispatchWorking
	return nil
}

// validationHead is the exact commit of the job branch that req hands to
// Quality for validation, once the branch is finished. The branch has taken in
// the latest base: AsmAI's clone fetches origin, and the base's tip is in the
// branch's history. If the base moved, nothing is handed over, and Engineering
// assigns a worker to merge it in, through the normal accept path.
func (d *daemon) validationHead(req Request) (string, error) {
	engineering := roles.LeaderOf(roles.Engineering)
	d.mu.Lock()
	ref, _, err := d.caller(req)
	d.mu.Unlock()
	if err != nil {
		return "", err
	}
	if ref.address != engineering {
		return "", fmt.Errorf("only %s hands a job branch to Quality for validation", engineering)
	}
	d.assignMu.Lock()
	defer d.assignMu.Unlock()
	j, err := d.store.Job(req.Job)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("job %d does not exist; `asmai jobs` lists the jobs", req.Job)
	}
	if err != nil {
		return "", err
	}
	if j.State != store.JobOpen {
		return "", fmt.Errorf("job %d is %s, so it takes no validation", j.Number, j.State)
	}
	branch, err := d.store.JobBranch(j.Number)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("job %d has no job branch, so there is nothing for Quality to validate", j.Number)
	}
	if err != nil {
		return "", err
	}
	repository, err := d.store.Repository(j.Repository)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("repository %q is not registered", j.Repository)
	}
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(d.work, gitTimeout)
	defer cancel()
	current, err := repos.Commit(ctx, repository.Clone, "refs/heads/"+branch.Name)
	if err != nil {
		return "", fmt.Errorf("reading the job branch: %w", err)
	}
	if current != branch.Tip {
		return "", fmt.Errorf("the job branch %s is at %s, not at its recorded tip %s, so it is not handed over", branch.Name, current, branch.Tip)
	}
	if err := repos.Fetch(ctx, repository.Clone); err != nil {
		return "", err
	}
	base, err := repos.OriginTip(ctx, repository.Clone, repository.DefaultBranch)
	if err != nil {
		return "", err
	}
	taken, err := repos.Contains(ctx, repository.Clone, "refs/heads/"+branch.Name, base)
	if err != nil {
		return "", err
	}
	if !taken {
		return "", fmt.Errorf("the job branch %s has not taken in the latest base: origin's %s is at %s, which the branch's tip %s does not contain. Assign a worker to merge origin/%s into its assignment branch, accept its result, and hand validation over at the new tip", branch.Name, repository.DefaultBranch, base, branch.Tip, repository.DefaultBranch)
	}
	return branch.Tip, nil
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
	l.started = true
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
	if l == nil || l.session == nil || l.userInput || !l.ready || l.inputUnknown || l.pendingDispatch != 0 || l.currentDispatch != 0 || d.stopping {
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
	if err = d.store.ChangeDispatch(dispatch.ID, l.generation, dispatch.State, store.DispatchUnknown, l.transcript, time.Now()); err != nil {
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
	_, err := d.endCurrentDispatch(ref, promptID, transcript)
	return err
}

// endCurrentDispatch ends the dispatch ref's session finished a turn for, and
// returns it when it was working and is now stopped.
func (d *daemon) endCurrentDispatch(ref sessionRef, promptID, transcript string) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	l := d.leaders[ref.address.String()]
	if l == nil || l.session == nil || l.generation != ref.generation || l.currentDispatch == 0 || promptID == "" || promptID != l.currentPromptID {
		return 0, nil
	}
	id := l.currentDispatch
	l.currentPromptID = ""
	stopped, err := d.store.StopDispatchIfWorking(id, ref.generation, transcript, time.Now())
	// A correlated Stop ends this submitted prompt even when it did not fetch
	// the inbox. Keep the durable dispatch eligible for another nudge, but do
	// not leave the finished prompt as the leader's current dispatch.
	l.currentDispatch = 0
	if stopped {
		return id, err
	}
	return 0, err
}
