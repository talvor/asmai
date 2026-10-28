// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"strings"
	"testing"

	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/session"
)

func TestAgentAuthorityUsesSessionRoleKindAndGeneration(t *testing.T) {
	coordination := roles.LeaderOf(roles.Coordination)
	planning := roles.LeaderOf(roles.Planning)
	engineering := roles.LeaderOf(roles.Engineering)
	quality := roles.LeaderOf(roles.Quality)
	worker := roles.Address{Name: "worker-1", Role: roles.Coordination}
	d := &daemon{
		leaders: map[string]*leader{
			coordination.String(): {address: coordination, session: &session.Session{}, generation: 2},
			planning.String():     {address: planning, session: &session.Session{}, generation: 1},
			engineering.String():  {address: engineering, session: &session.Session{}, generation: 1},
			quality.String():      {address: quality, session: &session.Session{}, generation: 1},
			worker.String():       {address: worker, session: &session.Session{}, generation: 1},
		},
		sessions: map[string]sessionRef{
			"current":     {address: coordination, generation: 2},
			"old":         {address: coordination, generation: 1},
			"planning":    {address: planning, generation: 1},
			"engineering": {address: engineering, generation: 1},
			"quality":     {address: quality, generation: 1},
			"worker":      {address: worker, generation: 1},
		},
	}
	for _, tt := range []struct{ credential, command, want string }{
		{"current", CommandJobOpen, ""},
		{"current", CommandBrief, ""},
		{"current", CommandStatus, ""},
		{"current", CommandRepoAdd, "ask the user to run `asmai repo add`"},
		{"engineering", CommandJobOpen, "only leader@coordination"},
		{"worker", CommandJobOpen, "only leader@coordination"},
		{"old", CommandJobs, "unknown or superseded"},
		{"unknown", CommandJobs, "unknown or superseded"},
		{"", CommandJobOpen, "agent command"},
		{"engineering", CommandAssign, ""},
		{"planning", CommandAssign, ""},
		{"quality", CommandAssign, ""},
		{"planning", CommandAccept, ""},
		{"planning", CommandReject, ""},
		{"planning", CommandCancel, ""},
		{"planning", CommandRedispatch, ""},
		{"engineering", CommandRedispatch, ""},
		{"quality", CommandRedispatch, ""},
		{"current", CommandRedispatch, ""},
		{"worker", CommandRedispatch, "only leader@engineering, leader@planning, leader@research, leader@coordination or leader@quality"},
		{"", CommandRedispatch, "asmai redispatch is an agent command"},
		// The owning leader recovers a decision answer its worker's session
		// exited before acknowledging; the assignment's owner is checked
		// where it is recovered.
		{"engineering", CommandAnswerRedeliver, ""},
		{"engineering", CommandAnswerRetire, ""},
		{"planning", CommandAnswerRetire, ""},
		{"current", CommandAnswerRedeliver, ""},
		{"worker", CommandAnswerRedeliver, "only leader@engineering, leader@planning, leader@research, leader@coordination or leader@quality may run asmai answer redeliver"},
		{"worker", CommandAnswerRetire, "only leader@engineering, leader@planning, leader@research, leader@coordination or leader@quality may run asmai answer retire"},
		{"", CommandAnswerRedeliver, "asmai answer redeliver is an agent command"},
		{"", CommandAnswerRetire, "asmai answer retire is an agent command"},
		{"engineering", CommandAccept, ""},
		{"quality", CommandAccept, ""},
		{"quality", CommandReject, ""},
		{"quality", CommandCancel, ""},
		// Coordination's leader owns the workers that set a repository up, and
		// delivers its setup; the job it does so for is checked where it is
		// assigned.
		{"current", CommandAccept, ""},
		{"current", CommandReject, ""},
		{"current", CommandCancel, ""},
		{"current", CommandAssign, ""},
		{"current", CommandPush, ""},
		{"current", CommandEffect, ""},
		{"worker", CommandCancel, "only leader@engineering, leader@planning, leader@research, leader@coordination or leader@quality"},
		{"quality", CommandResult, "only a worker"},
		{"quality", CommandHandoffSend, ""},
		{"worker", CommandAssign, "only leader@engineering, leader@planning, leader@research or leader@coordination"},
		// The delivery owners push and record their pull requests: Engineering's
		// leader for a code job, and Planning's or Research's for a notes job.
		{"planning", CommandPush, ""},
		{"planning", CommandEffect, ""},
		{"planning", CommandJobOpen, "only leader@coordination"},
		{"planning", CommandHandoffSend, ""},
		{"planning", CommandDecisionRequest, ""},
		{"planning", CommandDecisionAnswer, ""},
		{"planning", CommandDecisionRecord, ""},
		{"planning", CommandDecisionPresent, "only leader@coordination may run asmai decision present"},
		{"worker", CommandEffect, ""},
		{"engineering", CommandEffect, ""},
		{"quality", CommandEffect, "only a worker, or the delivery owner"},
		{"worker", CommandResult, ""},
		{"worker", CommandBlocked, ""},
		{"engineering", CommandResult, "only a worker"},
		{"current", CommandBlocked, "only a worker"},
		{"old", CommandEffect, "unknown or superseded"},
		{"", CommandAssign, "asmai assign is an agent command"},
		{"", CommandEffect, "asmai effect is an agent command"},
		{"", CommandResult, "asmai result is an agent command"},
		{"", CommandBlocked, "asmai blocked is an agent command"},
		// A leader other than Coordination's files a blocked report to
		// Coordination, which alone resolves one; a worker's asmai blocked
		// goes to its owning leader, and Coordination files none.
		{"engineering", CommandBlockedReport, ""},
		{"planning", CommandBlockedReport, ""},
		{"quality", CommandBlockedReport, ""},
		{"current", CommandBlockedReport, "leader@coordination files no blocked report"},
		{"worker", CommandBlockedReport, "a worker that cannot go on runs `asmai blocked --reason <text> [--needs <text>]`, with no --job"},
		{"", CommandBlockedReport, "asmai blocked --job is an agent command"},
		{"engineering", CommandBlocked, "files a blocked report to Coordination with `asmai blocked --job"},
		{"current", CommandBlockedResolve, ""},
		{"engineering", CommandBlockedResolve, "only leader@coordination may run asmai blocked resolve"},
		{"worker", CommandBlockedResolve, "only leader@coordination may run asmai blocked resolve"},
		{"", CommandBlockedResolve, "asmai blocked resolve is an agent command"},
		{"engineering", CommandPush, ""},
		{"quality", CommandPush, "only leader@engineering, the delivery owner"},
		{"worker", CommandPush, "only leader@engineering, the delivery owner"},
		{"", CommandPush, "asmai push is an agent command"},
		{"engineering", CommandDecisionRecord, ""},
		{"quality", CommandDecisionRecord, ""},
		{"current", CommandDecisionRecord, ""},
		{"worker", CommandDecisionRecord, "only a role's leader may run asmai decision record"},
		{"", CommandDecisionRecord, "asmai decision record is an agent command"},
		// Any agent checks an action against the grants before it takes it, and
		// only a leader reports that a condition no longer holds; listing and
		// revoking grants are the user's.
		{"worker", CommandAuthorize, ""},
		{"engineering", CommandAuthorize, ""},
		{"", CommandAuthorize, "asmai authorize is an agent command"},
		{"quality", CommandGrantInvalidate, ""},
		{"worker", CommandGrantInvalidate, "only a role's leader may run asmai grant invalidate"},
		{"", CommandGrantInvalidate, "asmai grant invalidate is an agent command"},
		{"engineering", CommandGrants, "ask the user to run `asmai grants`"},
		{"current", CommandGrantRevoke, "ask the user to run `asmai grant revoke`"},
		{"", CommandGrants, ""},
		{"", CommandGrantRevoke, ""},
		// Any agent requests a decision; only a leader answers one; only the
		// user lists the pending decisions.
		{"worker", CommandDecisionRequest, ""},
		{"engineering", CommandDecisionRequest, ""},
		{"current", CommandDecisionRequest, ""},
		{"", CommandDecisionRequest, "asmai decision request is an agent command"},
		{"engineering", CommandDecisionAnswer, ""},
		{"worker", CommandDecisionAnswer, "only a leader may run asmai decision answer"},
		{"", CommandDecisionAnswer, "asmai decision answer is an agent command"},
		// Only Coordination shows the user a decision, in the conversation.
		{"current", CommandDecisionPresent, ""},
		{"engineering", CommandDecisionPresent, "only leader@coordination may run asmai decision present"},
		{"quality", CommandDecisionPresent, "only leader@coordination may run asmai decision present"},
		{"worker", CommandDecisionPresent, "only leader@coordination may run asmai decision present"},
		{"", CommandDecisionPresent, "asmai decision present is an agent command"},
		{"", CommandDecisions, ""},
		{"engineering", CommandDecisions, "ask the user to run `asmai decisions`"},
		{"worker", CommandDecisions, "ask the user to run `asmai decisions`"},
		// No agent runs Lavish or asks about its server (04 rule 22).
		{"current", CommandLavish, "ask the user to run `asmai lavish`"},
		{"engineering", CommandLavish, "ask the user to run `asmai lavish`"},
		{"worker", CommandLavish, "ask the user to run `asmai lavish`"},
		{"", CommandLavish, ""},
	} {
		err := d.authorize(Request{Command: tt.command, Session: tt.credential})
		if tt.want == "" && err != nil {
			t.Errorf("%s %s: %v", tt.credential, tt.command, err)
		}
		if tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)) {
			t.Errorf("%s %s: %v, want %s", tt.credential, tt.command, err, tt.want)
		}
	}
}
