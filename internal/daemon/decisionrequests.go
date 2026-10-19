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
		in.Job, in.Recipient, in.Blocks = req.Job, store.ToUser, req.Blocks
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
	if req.Request <= 0 || req.Version <= 0 || strings.TrimSpace(req.Answer) == "" {
		return store.DecisionRequest{}, store.Dispatch{}, refuse("asmai decision answer needs the request, the version it answers and the answer")
	}
	latest, err := d.store.DecisionRequest(req.Request)
	if err != nil {
		return store.DecisionRequest{}, store.Dispatch{}, refuse("%v", err)
	}
	switch {
	case latest.Recipient == store.ToUser:
		return store.DecisionRequest{}, store.Dispatch{}, refuse("decision request %d goes to the user, whose answer no agent supplies; Coordination presents it and returns the user's own answer", latest.Request)
	case latest.Recipient != ref.address.String():
		return store.DecisionRequest{}, store.Dispatch{}, refuse("decision request %d goes to %s, not %s", latest.Request, latest.Recipient, ref.address)
	}
	r, dispatch, err := d.store.DecisionAnswered(store.DecisionAnswer{Request: req.Request, Version: req.Version, Answerer: ref.address.String(), Text: req.Answer, ReliedOn: req.ReliedOn}, time.Now())
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
