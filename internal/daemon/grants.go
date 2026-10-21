// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"errors"
	"net"
	"time"

	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/store"
)

// grantCommand serves the commands about grants: a leader recording one with
// decision.record, and checking an action against them with authorize; a
// leader reporting that a condition of one no longer holds with
// grant.invalidate; and the user listing and revoking them with grants and
// grant.revoke.
func (d *daemon) grantCommand(conn *net.UnixConn, req Request) {
	var resp Response
	var err error
	var notify []store.Dispatch
	switch req.Command {
	case CommandDecisionRecord:
		var g store.Grant
		if g, err = d.recordGrant(req); err == nil {
			resp.Grant = &g
		}
	case CommandAuthorize:
		var a store.Authorization
		if a, err = d.authorizeAction(req); err == nil {
			resp.Authorization = &a
		}
	case CommandGrants:
		// Listing a grant is not revoking it: it reads and changes nothing.
		resp.Grants, err = d.store.Grants()
	case CommandGrantRevoke, CommandGrantInvalidate:
		var ending store.GrantEnding
		if ending, err = d.endGrant(req); err == nil {
			resp.GrantEnding = &ending
			notify = ending.Dispatches
		}
	}
	if err != nil {
		if !errors.As(err, new(*userError)) {
			d.log.Error("a grant command failed", "command", req.Command, "error", err.Error())
		}
		resp.Error = err.Error()
	}
	reply(conn, resp)
	for _, dispatch := range notify {
		d.nudge(dispatch.Agent)
	}
}

// recordGrant records the grant req carries, which a leader records at its
// scope citing the user's words: Coordination's leader one given in the
// conversation, citing the witnessed message, and the leader that asked one
// given in an answer to its decision request, citing the record of the
// answer. The daemon refuses a grant without a valid citation.
func (d *daemon) recordGrant(req Request) (store.Grant, error) {
	d.mu.Lock()
	ref, _, err := d.caller(req)
	d.mu.Unlock()
	if err != nil {
		return store.Grant{}, err
	}
	if ref.address.Name != roles.Leader {
		return store.Grant{}, refuse("only a role's leader may record a grant: a worker's request goes to its owning leader")
	}
	in := *req.Grant
	if req.LatestWitness {
		if in.Cites, err = d.store.LatestWitnessed(ref.address.String()); err != nil {
			return store.Grant{}, refuse("%v", err)
		}
	}
	g, err := d.store.GrantRecorded(in, ref.address.String(), time.Now())
	if err != nil {
		return store.Grant{}, refuse("%v", err)
	}
	d.log.Info("grant recorded", "grant", g.ID, "scope", g.Scope, "job", g.Job, "repository", g.Repository, "action", g.Action, "leader", g.Leader, "cites", g.Cites)
	return g, nil
}

// authorizeAction checks the consequential action req asks to take against
// the grants in force, before the agent takes it, and records the grant that
// covers it. A worker's action is of the job of the assignment in its current
// dispatch, and a leader names the job it acts for. Nothing a grant does not
// cover is authorized, and no grant covers a merge.
func (d *daemon) authorizeAction(req Request) (store.Authorization, error) {
	d.mu.Lock()
	ref, l, err := d.caller(req)
	current := int64(0)
	if err == nil {
		current = l.currentDispatch
	}
	d.mu.Unlock()
	if err != nil {
		return store.Authorization{}, err
	}
	if req.Action == nil {
		return store.Authorization{}, refuse("asmai authorize needs the action to check")
	}
	in := *req.Action
	in.Agent, in.Generation = ref.address.String(), ref.generation
	if ref.address.Name == roles.Leader {
		if in.Job <= 0 {
			return store.Authorization{}, refuse("a leader's action is of a job: give --job")
		}
		in.Dispatch = 0
	} else {
		if current == 0 {
			return store.Authorization{}, refuse("%s has no current dispatch to authorize an action in; check actions while working on an assignment you fetched with `asmai inbox`", ref.address)
		}
		in.Job, in.Dispatch = 0, current
	}
	a, err := d.store.ActionAuthorized(in, time.Now())
	if err != nil {
		return store.Authorization{}, refuse("%v", err)
	}
	d.log.Info("action authorized", "authorization", a.ID, "grant", a.Grant, "job", a.Job, "action", a.Action, "agent", a.Agent)
	return a, nil
}

// endGrant ends the grant req names: the user revoking it, or a leader
// reporting that one of its conditions no longer holds. The owning leaders of
// the assignments in flight that carried it are told, started if they are not
// running.
func (d *daemon) endGrant(req Request) (store.GrantEnding, error) {
	state, by, reason := store.GrantRevoked, "the user", ""
	if req.Command == CommandGrantInvalidate {
		d.mu.Lock()
		ref, _, err := d.caller(req)
		d.mu.Unlock()
		if err != nil {
			return store.GrantEnding{}, err
		}
		if ref.address.Name != roles.Leader {
			return store.GrantEnding{}, refuse("only a role's leader may report that a grant's condition no longer holds")
		}
		state, by, reason = store.GrantInvalidated, ref.address.String(), req.Why
	} else if req.Session != "" {
		return store.GrantEnding{}, refuse("an agent cannot revoke a grant; ask the user to run `asmai grant revoke %d`", req.GrantID)
	}
	if req.GrantID <= 0 {
		return store.GrantEnding{}, refuse("name the grant, as `asmai grants` lists them")
	}
	ending, err := d.store.GrantEnded(req.GrantID, state, by, reason, time.Now())
	if err != nil {
		return store.GrantEnding{}, refuse("%v", err)
	}
	d.log.Info("grant ended", "grant", ending.Grant.ID, "state", state, "by", by, "earlier", len(ending.Earlier), "affected", len(ending.Affected))
	for _, dispatch := range ending.Dispatches {
		to, err := roles.ParseAddress(dispatch.Agent)
		if err != nil {
			continue
		}
		d.mu.Lock()
		err = d.ensureAgent(to)
		d.mu.Unlock()
		if err != nil {
			d.log.Error("starting the owning leader of an assignment a grant's end affects", "agent", dispatch.Agent, "dispatch", dispatch.ID, "error", err.Error())
		}
	}
	return ending, nil
}

// grantedEffect checks the effect of kind that req's worker records against
// the actions that need a grant: a merge is never recorded, and a spend, a
// deployment or a destructive change is recorded only after the daemon
// authorized it in the same dispatch.
func (d *daemon) grantedEffect(kind string, dispatch int64) error {
	switch kind {
	case store.ActionMerge:
		return refuse("AsmAI never merges in v1, and no grant can authorize a merge: the user merges a tested pull request themselves")
	case store.ActionSpend, store.ActionDeploy, store.ActionDestructive:
		authorized, err := d.store.AuthorizedIn(dispatch, kind)
		if err != nil {
			return err
		}
		if len(authorized) == 0 {
			return refuse("a %s effect is recorded only after `asmai authorize --action %s` found a grant that covers it, in this dispatch; check first, and take no action a grant does not cover", kind, kind)
		}
	}
	return nil
}
