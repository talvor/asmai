// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/talvor/asmai/internal/repos"
	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/store"
)

// handoffTo are the roles each role's leader may hand work to: Coordination
// hands a job to Planning, for its plan, to Research, for its findings, or to
// Engineering, and Engineering hands validation of the finished job branch to
// Quality. Any leader may ask Quality for an earlier review of an assignment
// (06 rule 5), which is the only handoff to Quality but Engineering's of the
// finished job branch.
var handoffTo = map[string][]string{
	roles.Coordination: {roles.Planning, roles.Research, roles.Engineering, roles.Quality},
	roles.Planning:     {roles.Quality},
	roles.Research:     {roles.Quality},
	roles.Engineering:  {roles.Quality},
}

// errNotALeader is the refusal of a handoff to anything but the leader of a
// role its sender may hand work to.
var errNotALeader = errors.New("send a handoff to another role's leader: Coordination hands a job to Planning, Research or Engineering, Engineering hands validation to Quality, and any leader asks Quality for an earlier review of an assignment")

// jobOrZero returns job number, or the zero job when it cannot be read: the
// handoff that names it is refused when it is recorded, so a job that does
// not exist never reaches checkHandoff's mandate check.
func (d *daemon) jobOrZero(number int64) store.Job {
	j, _ := d.store.Job(number)
	return j
}

// onlyRole is the role that a job of each notes mandate is handed to: a
// planning-only job is authorized only to produce its planning outcome, and a
// research-only job only its research outcome, so neither goes to Engineering,
// whose work changes code, and each goes only to its own role (01 rule 17).
var onlyRole = map[string]string{
	store.MandatePlanningOnly: roles.Planning,
	store.MandateResearchOnly: roles.Research,
	store.MandateSetup:        roles.Coordination,
}

// checkHandoff checks that the leader of the role sender may hand the job j to
// the leader of the role receiver, and that the receiver is the role the job's
// mandate authorizes.
func checkHandoff(sender, receiver string, j store.Job) error {
	if !slices.Contains(handoffTo[sender], receiver) {
		return errNotALeader
	}
	if j.Mandate == store.MandateSetup {
		return fmt.Errorf("job %d is the setup of repository %s, which authorizes only its setup: %s gives it to one of its own workers, never to another role", j.Number, j.Repository, roles.LeaderOf(roles.Coordination))
	}
	if only, ok := onlyRole[j.Mandate]; ok && receiver != only {
		outcome := strings.TrimSuffix(j.Mandate, "-only")
		if receiver == roles.Engineering {
			return fmt.Errorf("job %d has the %s mandate, which authorizes only its %s outcome, never code: hand it to %s, and ask the user for a new job if they want it built", j.Number, j.Mandate, outcome, roles.Title(only))
		}
		return fmt.Errorf("job %d has the %s mandate, which authorizes only its %s outcome: hand it to %s, and ask the user for a new job if they want more", j.Number, j.Mandate, outcome, roles.Title(only))
	}
	return nil
}

func (d *daemon) handoffCommand(conn *net.UnixConn, req Request) {
	var resp Response
	// Validation is handed over at the job branch's exact head, which is
	// read before the daemon's state is locked: it fetches from origin.
	var target validationTarget
	if req.Command == CommandHandoffSend {
		if address, err := roles.ParseAddress(req.Agent); err == nil && address == roles.LeaderOf(roles.Quality) {
			conn.SetDeadline(time.Now().Add(gitTimeout + time.Minute))
			var err error
			if target, err = d.validationTarget(req); err != nil {
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
		} else if address.Name != roles.Leader {
			err = errNotALeader
		} else if req.Assignment != 0 && address.Role != roles.Quality {
			err = errors.New("--assignment asks Quality for an earlier review of an assignment, so it goes only with --to quality")
		} else if err = checkHandoff(ref.address.Role, address.Role, d.jobOrZero(req.Job)); err == nil {
			h := store.Handoff{Job: req.Job, Sender: ref.address.String(), Receiver: address.String(), Outcome: req.Outcome, Decisions: req.Decisions, Evidence: req.Evidence, Constraints: req.Constraints, Permissions: req.Permissions, Criteria: req.Criteria, Head: target.head, Review: target.review, Since: target.since, Assignment: target.assignment, Records: req.Records}
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
		resp.Messages, err = d.store.Inbox(ref.address.String(), req.Dispatch, l.currentDispatch, time.Now())
		if err == nil {
			for i := range resp.Messages {
				if a := resp.Messages[i].AssignmentData; a != nil {
					if a.Worker == ref.address.String() {
						a.List = l.skills
					} else {
						d.listSkills(a)
					}
				}
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
// at once to working: a reply to a handoff, an assignment for its worker, a
// result or a blocked report for the assignment's owning leader, and a
// leader's blocked report for Coordination's leader and its resolution for
// the leader that filed it. Unlike a
// handoff, none of them is answered by a command that starts the work. A
// rejection and a cancellation are for the assignment's worker too, as is a
// resumption after a restart; a restored leader's brief to load and a
// reconciliation for an assignment's owning leader are work at once too.
func (d *daemon) workFetchedReply(l *leader, ref sessionRef, m *store.Message) error {
	switch m.Kind {
	case store.HandoffAccepted, store.HandoffClarified, store.HandoffDeclined, store.MessageAssignment, store.MessageRejection, store.MessageMovedTip, store.MessageCancellation, store.MessageValidation, store.ReportResult, store.ReportBlocked, store.MessageBlockedResolved,
		store.MessageResumption, store.MessageRedispatch, store.MessageStalled, store.MessageReconciliation, store.MessageRestoration, store.MessageDecisionRequest, store.MessageDecisionAnswer, store.MessageLavishSent, store.MessageSetup:
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

// validationTarget is what a handoff to Quality asks it to validate: the exact
// commit, and how the validation goes.
type validationTarget struct {
	head string
	// review is one of the store's reviews, since the last validated commit
	// a changes or checks review looks from, and assignment the assignment
	// an early review is of.
	review     string
	since      string
	assignment int64
}

// validationTarget is the exact commit that req hands to Quality for
// validation, and how the validation goes. Without req.Assignment it is the
// job branch's tip, once the branch is finished: the branch has taken in the
// latest base (AsmAI's clone fetches origin, and the base's tip is in the
// branch's history), and only Engineering's leader hands it over. If the base
// moved, nothing is handed over, and Engineering assigns a worker to merge it
// in, through the normal accept path. Then the review follows from what
// changed since Quality last validated the branch (06 rules 6 and 12). With
// req.Assignment, any leader asks for an earlier review of that assignment's
// result, at its commit, whatever the job branch is at.
func (d *daemon) validationTarget(req Request) (validationTarget, error) {
	engineering := roles.LeaderOf(roles.Engineering)
	d.mu.Lock()
	ref, _, err := d.caller(req)
	d.mu.Unlock()
	if err != nil {
		return validationTarget{}, err
	}
	early := req.Assignment != 0
	if !early && ref.address != engineering {
		return validationTarget{}, fmt.Errorf("only %s hands a job branch to Quality for validation; any leader asks for an earlier review of an assignment with --assignment", engineering)
	}
	if ref.address.Name != roles.Leader || ref.address.Role == roles.Quality {
		return validationTarget{}, fmt.Errorf("only the leader of another role asks Quality for an earlier review: Quality never asks itself")
	}
	d.assignMu.Lock()
	defer d.assignMu.Unlock()
	j, err := d.store.Job(req.Job)
	if errors.Is(err, sql.ErrNoRows) {
		return validationTarget{}, fmt.Errorf("job %d does not exist; `asmai jobs` lists the jobs", req.Job)
	}
	if err != nil {
		return validationTarget{}, err
	}
	if j.State != store.JobOpen {
		return validationTarget{}, fmt.Errorf("job %d is %s, so it takes no validation", j.Number, j.State)
	}
	branch, err := d.store.JobBranch(j.Number)
	if errors.Is(err, sql.ErrNoRows) {
		return validationTarget{}, fmt.Errorf("job %d has no job branch, so there is nothing for Quality to validate", j.Number)
	}
	if err != nil {
		return validationTarget{}, err
	}
	repository, err := d.store.Repository(j.Repository)
	if errors.Is(err, sql.ErrNoRows) {
		return validationTarget{}, fmt.Errorf("repository %q is not registered", j.Repository)
	}
	if err != nil {
		return validationTarget{}, err
	}
	ctx, cancel := context.WithTimeout(d.work, gitTimeout)
	defer cancel()
	if early {
		return d.earlyTarget(ctx, j, repository, req.Assignment)
	}
	current, err := repos.Commit(ctx, repository.Clone, "refs/heads/"+branch.Name)
	if err != nil {
		return validationTarget{}, fmt.Errorf("reading the job branch: %w", err)
	}
	if current != branch.Tip {
		return validationTarget{}, fmt.Errorf("the job branch %s is at %s, not at its recorded tip %s, so it is not handed over", branch.Name, current, branch.Tip)
	}
	if err := repos.Fetch(ctx, repository.Clone); err != nil {
		return validationTarget{}, err
	}
	base, err := repos.OriginTip(ctx, repository.Clone, repository.DefaultBranch)
	if err != nil {
		return validationTarget{}, err
	}
	taken, err := repos.Contains(ctx, repository.Clone, "refs/heads/"+branch.Name, base)
	if err != nil {
		return validationTarget{}, err
	}
	if !taken {
		return validationTarget{}, fmt.Errorf("the job branch %s has not taken in the latest base: origin's %s is at %s, which the branch's tip %s does not contain. Assign a worker to merge origin/%s into its assignment branch, accept its result, and hand validation over at the new tip", branch.Name, repository.DefaultBranch, base, branch.Tip, repository.DefaultBranch)
	}
	review, since, err := d.reviewPlan(ctx, j, repository, branch.Tip)
	if err != nil {
		return validationTarget{}, err
	}
	return validationTarget{head: branch.Tip, review: review, since: since}, nil
}

// earlyTarget is the target of an early review of assignment, a writing
// assignment of j with a result: the commit of its latest result, which AsmAI's
// clone has fetched from origin.
func (d *daemon) earlyTarget(ctx context.Context, j store.Job, repository store.Repository, assignment int64) (validationTarget, error) {
	a, err := d.store.Assignment(assignment)
	if errors.Is(err, sql.ErrNoRows) {
		return validationTarget{}, fmt.Errorf("assignment %d does not exist", assignment)
	}
	if err != nil {
		return validationTarget{}, err
	}
	switch {
	case a.Job != j.Number:
		return validationTarget{}, fmt.Errorf("assignment %d belongs to job %d, not job %d", a.ID, a.Job, j.Number)
	case a.ReadOnly:
		return validationTarget{}, fmt.Errorf("assignment %d is a validation, which has no result of its own to review", a.ID)
	}
	r, err := d.store.LatestResult(a.ID)
	if err != nil {
		return validationTarget{}, fmt.Errorf("assignment %d has no result to review yet: %w", a.ID, err)
	}
	if err := repos.Fetch(ctx, repository.Clone); err != nil {
		return validationTarget{}, err
	}
	if _, err := repos.Commit(ctx, repository.Clone, r.Commit); err != nil {
		return validationTarget{}, fmt.Errorf("AsmAI's clone does not have %s, the commit of assignment %d's result: %w", r.Commit, a.ID, err)
	}
	return validationTarget{head: r.Commit, review: store.ReviewEarly, assignment: a.ID}, nil
}

// reviewPlan decides how the validation of tip, the finished job branch of j,
// goes, and the last validated commit it looks from. The first validation is
// a full review. Any later one is at a head that makes the evidence stale: a
// fix, or a merge whose conflicts a worker resolved, is reviewed for what
// changed since the last validated commit, and confirms each earlier blocking
// finding, while a clean merge of a moved base needs only the checks, as long
// as no earlier blocking finding is left for Quality to confirm. A head that
// Quality already validated has nothing new to validate.
func (d *daemon) reviewPlan(ctx context.Context, j store.Job, repository store.Repository, tip string) (review, since string, err error) {
	since, err = d.store.LastValidated(j.Number)
	if err != nil {
		return "", "", err
	}
	if since == tip {
		return "", "", fmt.Errorf("Quality has already validated %s, the head of job %d, and nothing has changed since: validation follows a fix or a merge, which gives the job branch a new head", tip, j.Number)
	}
	if since != "" {
		in, err := repos.Contains(ctx, repository.Clone, tip, since)
		if err != nil {
			return "", "", err
		}
		if !in {
			since = ""
		}
	}
	if since == "" {
		return store.ReviewFull, "", nil
	}
	open, err := d.store.OpenFindings(j.Number)
	if err != nil {
		return "", "", err
	}
	if len(open) > 0 {
		return store.ReviewChanges, since, nil
	}
	base := "refs/remotes/origin/" + repository.DefaultBranch
	if inBase, err := repos.Contains(ctx, repository.Clone, base, tip); err != nil {
		return "", "", err
	} else if inBase {
		return store.ReviewChecks, since, nil
	}
	steps, err := repos.Steps(ctx, repository.Clone, since, tip)
	if err != nil {
		return "", "", err
	}
	for _, step := range steps {
		clean, err := repos.CleanMerge(ctx, repository.Clone, step)
		if err != nil {
			return "", "", err
		}
		if !clean {
			return store.ReviewChanges, since, nil
		}
		// A merge of the base, not of outside commits or of another branch.
		onBase, err := repos.Contains(ctx, repository.Clone, base, step.Parents[1])
		if err != nil {
			return "", "", err
		}
		if !onBase {
			return store.ReviewChanges, since, nil
		}
	}
	return store.ReviewChecks, since, nil
}

// ensureRoleLeader is called with d.mu held. One role address has one leader.
func (d *daemon) ensureRoleLeader(role string) error {
	l := d.leader(role)
	if l.session != nil {
		return nil
	}
	checks, cfg, install := d.checks()
	if failed(checks) {
		return d.held(roles.LeaderOf(role).String(), checks)
	}
	if l.restart != nil {
		l.restart.Stop()
		l.restart = nil
	}
	return d.startLeader(l, cfg, install)
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
	if promptID != "" && promptID == l.stoppedPromptID {
		// Claude Code reports a prompt typed while the Stop hook of a finished
		// turn runs under that turn's prompt, not its own. The turn that
		// takes it ends with a Stop under another prompt, which ends this
		// dispatch: matching the finished turn's prompt would never happen.
		l.currentPromptID = ""
	}
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
	if l == nil || l.session == nil || l.generation != ref.generation {
		return 0, nil
	}
	if promptID != "" {
		l.stoppedPromptID = promptID
	}
	if l.currentDispatch == 0 || promptID == "" {
		return 0, nil
	}
	// With no prompt recorded for the dispatch, the first Stop after its
	// submission is its own turn's.
	if l.currentPromptID != "" && promptID != l.currentPromptID {
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
