// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"database/sql"
	"errors"
	"net"
	"strings"
	"time"

	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/store"
)

// decisionRequestCommand serves decision.request, decision.answer and
// decisions.
func (d *daemon) decisionRequestCommand(conn *net.UnixConn, req Request) {
	var resp Response
	var err error
	var notify string
	switch req.Command {
	case CommandDecisionRequest:
		var r store.DecisionRequest
		var dispatch *store.Dispatch
		if r, dispatch, err = d.decisionRequest(req); err == nil {
			if r.HasPage() && d.pages != nil {
				// A version answered in Lavish is shown there at once, so that
				// Coordination has its link when it is told of the request.
				d.pages.sync(d.work)
				if opened, readErr := d.store.DecisionRequest(r.Request); readErr == nil {
					r = opened
				}
			}
			resp.DecisionRequest = &r
			if dispatch != nil {
				resp.Dispatch, notify = dispatch, dispatch.Agent
			}
		}
	case CommandDecisionAnswer:
		var r store.DecisionRequest
		var dispatch store.Dispatch
		if r, dispatch, err = d.decisionAnswer(req); err == nil {
			resp.DecisionRequest, resp.Dispatch, notify = &r, &dispatch, dispatch.Agent
		}
	case CommandDecisionPresent:
		var r store.DecisionRequest
		if r, err = d.decisionPresent(req); err == nil {
			resp.DecisionRequest = &r
		}
	case CommandDecisionRecord:
		var record store.DecisionRecord
		var r store.DecisionRequest
		var dispatch *store.Dispatch
		if record, r, dispatch, err = d.conversationAnswer(req); err == nil {
			resp.DecisionRecord, resp.DecisionRequest = &record, &r
			if dispatch != nil {
				resp.Dispatch, notify = dispatch, dispatch.Agent
			}
		}
	case CommandDecisions:
		// Listing is not answering: it reads and changes nothing.
		resp.DecisionRequests, err = d.store.PendingDecisions()
	}
	if err != nil {
		if !errors.As(err, new(*userError)) {
			d.log.Error("a decision command failed", "command", req.Command, "error", err.Error())
		}
		resp.Error = err.Error()
	}
	reply(conn, resp)
	if notify != "" {
		d.nudge(notify)
	}
}

// decisionRequest records the decision request req makes, or the new version
// of one it issues. A worker's request goes to its owning leader and never to
// the user, and the work of its assignment waits for the answer. A leader's
// request goes to the user through Coordination, which presents it, and
// holds the assignments of its own that the leader names. The returned
// dispatch carries the version to the agent that receives it, which is
// started if it is not running.
func (d *daemon) decisionRequest(req Request) (store.DecisionRequest, *store.Dispatch, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	ref, l, err := d.caller(req)
	if err != nil {
		return store.DecisionRequest{}, nil, err
	}
	in := store.DecisionRequestInput{
		Request: req.Request, Requester: ref.address.String(), Kind: req.Kind,
		Content: req.Content, Questions: req.Questions,
	}
	if ref.address.Name == roles.Leader {
		if req.Job <= 0 {
			return store.DecisionRequest{}, nil, refuse("a leader's decision request names its job: give --job")
		}
		in.Job, in.Recipient, in.Blocks, in.Carries = req.Job, store.ToUser, req.Blocks, req.Carries
		if coordination := roles.LeaderOf(roles.Coordination); ref.address != coordination {
			in.Notify = coordination.String()
		}
		for _, id := range req.Blocks {
			a, err := d.store.Assignment(id)
			if errors.Is(err, sql.ErrNoRows) {
				return store.DecisionRequest{}, nil, refuse("assignment %d does not exist", id)
			}
			if err != nil {
				return store.DecisionRequest{}, nil, err
			}
			if a.Owner != ref.address.String() {
				return store.DecisionRequest{}, nil, refuse("assignment %d is owned by %s, not %s: only its owning leader holds its work for a decision", a.ID, a.Owner, ref.address)
			}
		}
	} else {
		if len(req.Blocks) > 0 || req.Job != 0 {
			return store.DecisionRequest{}, nil, refuse("a worker's decision request is about its own assignment: it takes no --job and no --blocks")
		}
		if req.Carries != 0 {
			return store.DecisionRequest{}, nil, refuse("a worker's decision request carries nothing: only its owning leader carries it to the user")
		}
		if l.currentDispatch == 0 {
			return store.DecisionRequest{}, nil, refuse("%s has no current dispatch to ask about; ask about the assignment you fetched with `asmai inbox`", ref.address)
		}
		a, err := d.store.DispatchAssignment(l.currentDispatch)
		if errors.Is(err, sql.ErrNoRows) {
			return store.DecisionRequest{}, nil, refuse("dispatch %d is about no assignment, so there is nothing to ask a decision about", l.currentDispatch)
		}
		if err != nil {
			return store.DecisionRequest{}, nil, err
		}
		if a.Worker != ref.address.String() {
			return store.DecisionRequest{}, nil, refuse("assignment %d belongs to %s, not %s", a.ID, a.Worker, ref.address)
		}
		// The request goes to the assignment's owning leader, which answers
		// it from the records or escalates it through Coordination.
		in.Job, in.Assignment, in.Recipient, in.Notify, in.Blocks = a.Job, a.ID, a.Owner, a.Owner, []int64{a.ID}
	}
	r, dispatch, err := d.store.DecisionRequested(in, time.Now())
	if err != nil {
		return store.DecisionRequest{}, nil, refuse("%v", err)
	}
	d.log.Info("decision requested", "request", r.Request, "version", r.Version, "job", r.Job, "requester", r.Requester, "recipient", r.Recipient, "surface", r.Surface)
	if dispatch != nil {
		if to, err := roles.ParseAddress(dispatch.Agent); err == nil {
			if err := d.ensureAgent(to); err != nil {
				d.log.Error("starting the agent a decision request goes to", "agent", dispatch.Agent, "dispatch", dispatch.ID, "error", err.Error())
			}
		}
	}
	return r, dispatch, nil
}

// decisionAnswer records the answer of the owning leader to one version of a
// worker's decision request, from the records, and returns it to the worker.
// An answer applies only to the version it names. A request that goes to the
// user is answered by the user, not by an agent, and this refuses it.
func (d *daemon) decisionAnswer(req Request) (store.DecisionRequest, store.Dispatch, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	ref, _, err := d.caller(req)
	if err != nil {
		return store.DecisionRequest{}, store.Dispatch{}, err
	}
	if req.Request <= 0 || req.Version <= 0 {
		return store.DecisionRequest{}, store.Dispatch{}, refuse("asmai decision answer needs the request and the version it answers")
	}
	latest, err := d.store.DecisionRequest(req.Request)
	if err != nil {
		return store.DecisionRequest{}, store.Dispatch{}, refuse("%v", err)
	}
	switch {
	case latest.Recipient == store.ToUser:
		return store.DecisionRequest{}, store.Dispatch{}, refuse("decision request %d goes to the user, whose answer no agent supplies; Coordination presents it and records the user's own message with `asmai decision record --request %d --version %d --cites <witnessed message> --reading <text>`", latest.Request, latest.Request, latest.Version)
	case latest.Recipient != ref.address.String():
		return store.DecisionRequest{}, store.Dispatch{}, refuse("decision request %d goes to %s, not %s", latest.Request, latest.Recipient, ref.address)
	}
	r, dispatch, err := d.store.DecisionAnswered(store.DecisionAnswer{Request: req.Request, Version: req.Version, Answerer: ref.address.String(), Text: req.Answer, ReliedOn: req.ReliedOn, Carried: req.UserAnswer}, time.Now())
	if err != nil {
		return store.DecisionRequest{}, store.Dispatch{}, refuse("%v", err)
	}
	d.log.Info("decision answered", "request", r.Request, "version", r.Version, "job", r.Job, "answerer", ref.address.String(), "requester", r.Requester)
	if to, err := roles.ParseAddress(dispatch.Agent); err == nil {
		if err := d.ensureAgent(to); err != nil {
			d.log.Error("starting the requester of an answered decision", "agent", dispatch.Agent, "dispatch", dispatch.ID, "error", err.Error())
		}
	}
	return r, dispatch, nil
}

// decisionPresent records that Coordination's leader showed the user a version
// of a decision request in the conversation. A decision is presented once,
// with its answer surface, which was fixed when the version was made.
func (d *daemon) decisionPresent(req Request) (store.DecisionRequest, error) {
	d.mu.Lock()
	ref, _, err := d.caller(req)
	d.mu.Unlock()
	if err != nil {
		return store.DecisionRequest{}, err
	}
	if ref.address != roles.LeaderOf(roles.Coordination) {
		return store.DecisionRequest{}, refuse("only %s presents a decision to the user, in the conversation", roles.LeaderOf(roles.Coordination))
	}
	if req.Request <= 0 || req.Version <= 0 {
		return store.DecisionRequest{}, refuse("asmai decision present needs the request and the version shown")
	}
	r, err := d.store.DecisionPresented(req.Request, req.Version, ref.address.String(), time.Now())
	if err != nil {
		return store.DecisionRequest{}, refuse("%v", err)
	}
	d.log.Info("decision presented", "request", r.Request, "version", r.Version, "job", r.Job, "surface", r.Surface, "shown", r.Shown)
	return r, nil
}

// conversationAnswer records the user's answer to one version of a decision
// request that is answered in the conversation: a human decision, by
// Coordination's leader, citing the user's witnessed message given after the
// version was shown and carrying Coordination's reading of it beside their
// words. The owning leader that asked receives both, and the returned dispatch
// carries them to it; no agent supplies the answer. A record citing a nudge
// or a message typed against an older version is refused, and the refusal
// shows the current version to present again.
func (d *daemon) conversationAnswer(req Request) (store.DecisionRecord, store.DecisionRequest, *store.Dispatch, error) {
	d.mu.Lock()
	ref, l, err := d.caller(req)
	current := int64(0)
	if err == nil {
		current = l.currentDispatch
	}
	d.mu.Unlock()
	if err != nil {
		return store.DecisionRecord{}, store.DecisionRequest{}, nil, err
	}
	if ref.address != roles.LeaderOf(roles.Coordination) {
		return store.DecisionRecord{}, store.DecisionRequest{}, nil, refuse("only %s records the user's answer in the conversation: other agents never supply the user's side of a decision", roles.LeaderOf(roles.Coordination))
	}
	if req.DecisionSource != "" && req.DecisionSource != store.SourceHuman {
		return store.DecisionRecord{}, store.DecisionRequest{}, nil, refuse("an answer to a decision request is the user's own: --source %s is refused", req.DecisionSource)
	}
	if req.LatestWitness {
		if req.Cites, err = d.store.LatestWitnessed(ref.address.String()); err != nil {
			return store.DecisionRecord{}, store.DecisionRequest{}, nil, refuse("%v", err)
		}
	}
	if req.Job <= 0 {
		asked, err := d.store.DecisionRequest(req.Request)
		if err != nil {
			return store.DecisionRecord{}, store.DecisionRequest{}, nil, refuse("%v", err)
		}
		req.Job = asked.Job
	}
	record := store.DecisionRecord{
		Job: req.Job, Leader: ref.address.String(), Source: store.SourceHuman, Cites: req.Cites, ReliedOn: req.ReliedOn,
		Choice: strings.TrimSpace(req.Choice), Rationale: req.Rationale, Evidence: req.Evidence,
		Assumptions: req.Assumptions, ReopenWhen: req.ReopenWhen, Dispatch: current,
		Request: req.Request, Version: req.Version, Reading: req.Reading,
	}
	record, r, dispatch, err := d.store.ConversationAnswerRecorded(record, ref.generation, time.Now())
	if err != nil {
		var refused *store.AnswerRefused
		if errors.As(err, &refused) {
			d.log.Info("an answer was refused", "request", req.Request, "version", req.Version, "cites", req.Cites, "reason", refused.Reason)
		}
		return store.DecisionRecord{}, store.DecisionRequest{}, nil, refuse("%v", err)
	}
	d.log.Info("decision answered in the conversation", "request", r.Request, "version", r.Version, "job", r.Job, "record", record.ID, "cites", record.Cites, "requester", r.Requester)
	if dispatch != nil {
		if to, err := roles.ParseAddress(dispatch.Agent); err == nil {
			if err := d.ensureAgent(to); err != nil {
				d.log.Error("starting the requester of an answered decision", "agent", dispatch.Agent, "dispatch", dispatch.ID, "error", err.Error())
			}
		}
	}
	return record, r, dispatch, nil
}
