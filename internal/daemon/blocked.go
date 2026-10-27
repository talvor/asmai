// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"errors"
	"net"
	"time"

	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/store"
)

// blockedCommand serves a leader's blocked report and Coordination's
// resolution of it. A report goes to Coordination's leader as a message of
// kind blocked, and a resolution to the leader that filed it as a message of
// kind blocked.resolved, with the note of what changed. Either way the
// receiving leader is started if it is not running and nudged at its next
// input boundary. A blocked report is not a decision request: it has no
// version and no answer surface, it holds nothing, and it never waits on the
// user.
func (d *daemon) blockedCommand(conn *net.UnixConn, req Request) {
	var resp Response
	b, dispatch, err := d.blocked(req)
	if err == nil {
		resp.BlockedReport, resp.Dispatch = &b, &dispatch
	} else {
		if !errors.As(err, new(*userError)) {
			d.log.Error("a blocked report command failed", "command", req.Command, "error", err.Error())
		}
		resp.Error = err.Error()
	}
	reply(conn, resp)
	if err == nil {
		d.nudge(dispatch.Agent)
	}
}

// blocked records the blocked report or the resolution req makes, and starts
// the leader its message goes to unless it is running.
func (d *daemon) blocked(req Request) (store.BlockedReport, store.Dispatch, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	ref, _, err := d.caller(req)
	if err != nil {
		return store.BlockedReport{}, store.Dispatch{}, err
	}
	coordination := roles.LeaderOf(roles.Coordination)
	var b store.BlockedReport
	var dispatch store.Dispatch
	switch req.Command {
	case CommandBlockedReport:
		in := store.BlockedReportInput{Job: req.Job, Leader: ref.address.String(), Recipient: coordination.String(), Blocks: req.Blocks}
		if req.Report != nil {
			in.Reason, in.Needs = req.Report.Reason, req.Report.Needs
		}
		if b, dispatch, err = d.store.BlockedReported(in, time.Now()); err != nil {
			return store.BlockedReport{}, store.Dispatch{}, refuse("%v", err)
		}
		d.log.Info("blocked report filed", "blocked_report", b.ID, "job", b.Job, "leader", b.Leader, "dispatch", dispatch.ID)
	case CommandBlockedResolve:
		if req.BlockedReport <= 0 {
			return store.BlockedReport{}, store.Dispatch{}, refuse("name the blocked report to resolve, as `asmai brief <job>` lists it")
		}
		if b, dispatch, err = d.store.BlockedResolved(req.BlockedReport, ref.address.String(), req.Note, time.Now()); err != nil {
			return store.BlockedReport{}, store.Dispatch{}, refuse("%v", err)
		}
		d.log.Info("blocked report resolved", "blocked_report", b.ID, "job", b.Job, "leader", b.Leader, "dispatch", dispatch.ID)
	default:
		return store.BlockedReport{}, store.Dispatch{}, errors.New("unknown blocked report command")
	}
	if to, err := roles.ParseAddress(dispatch.Agent); err == nil {
		if err := d.ensureAgent(to); err != nil {
			d.log.Error("starting the leader a blocked report's message goes to", "agent", dispatch.Agent, "dispatch", dispatch.ID, "error", err.Error())
		}
	}
	return b, dispatch, nil
}
