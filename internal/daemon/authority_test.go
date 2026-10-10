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
	engineering := roles.LeaderOf(roles.Engineering)
	quality := roles.LeaderOf(roles.Quality)
	worker := roles.Address{Name: "worker-1", Role: roles.Coordination}
	d := &daemon{
		leaders: map[string]*leader{
			coordination.String(): {address: coordination, session: &session.Session{}, generation: 2},
			engineering.String():  {address: engineering, session: &session.Session{}, generation: 1},
			quality.String():      {address: quality, session: &session.Session{}, generation: 1},
			worker.String():       {address: worker, session: &session.Session{}, generation: 1},
		},
		sessions: map[string]sessionRef{
			"current":     {address: coordination, generation: 2},
			"old":         {address: coordination, generation: 1},
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
		{"quality", CommandAssign, ""},
		{"engineering", CommandAccept, ""},
		{"quality", CommandAccept, ""},
		{"quality", CommandReject, ""},
		{"quality", CommandCancel, ""},
		{"current", CommandAccept, "only leader@engineering or leader@quality"},
		{"worker", CommandCancel, "only leader@engineering or leader@quality"},
		{"quality", CommandResult, "only a worker"},
		{"quality", CommandHandoffSend, ""},
		{"current", CommandAssign, "only leader@engineering"},
		{"worker", CommandAssign, "only leader@engineering"},
		{"worker", CommandEffect, ""},
		{"engineering", CommandEffect, ""},
		{"current", CommandEffect, "only a worker, or the delivery owner"},
		{"worker", CommandResult, ""},
		{"worker", CommandBlocked, ""},
		{"engineering", CommandResult, "only a worker"},
		{"current", CommandBlocked, "only a worker"},
		{"old", CommandEffect, "unknown or superseded"},
		{"", CommandAssign, "asmai assign is an agent command"},
		{"", CommandEffect, "asmai effect is an agent command"},
		{"", CommandResult, "asmai result is an agent command"},
		{"", CommandBlocked, "asmai blocked is an agent command"},
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
