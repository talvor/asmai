// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"fmt"
	"strings"

	"github.com/talvor/asmai/internal/roles"
)

// authorize fences a caller against the currently running session. This
// guard prevents agent mistakes; a process running as the user can use the
// user's own access, so it is not a security boundary.
func (d *daemon) authorize(req Request) error {
	if req.Session == "" {
		if isAgentCommand(req.Command) {
			return fmt.Errorf("asmai %s is an agent command", commandName(req.Command))
		}
		return nil
	}
	d.mu.Lock()
	ref, ok := d.sessions[req.Session]
	if ok && req.Command != CommandHook {
		l := d.leaders[ref.address.String()]
		ok = l != nil && l.session != nil && l.generation == ref.generation && !d.stopping
	}
	d.mu.Unlock()
	if !ok {
		return fmt.Errorf("unknown or superseded agent session credential; this session can no longer call asmai")
	}
	switch req.Command {
	case CommandStatus, CommandAgents, CommandJobs, CommandBrief, CommandHook, CommandInbox, CommandHandoffAccept, CommandHandoffClarify, CommandHandoffDecline, CommandTrailers:
		return nil
	case CommandHandoffSend:
		if ref.address.Name == roles.Leader {
			return nil
		}
		return fmt.Errorf("only a role's leader may send a handoff")
	case CommandJobOpen:
		if ref.address.Role == roles.Coordination && ref.address.Name == roles.Leader {
			return nil
		}
		return fmt.Errorf("only leader@coordination may run asmai job open")
	case CommandAssign:
		if ref.address == roles.LeaderOf(roles.Engineering) || ref.address == roles.LeaderOf(roles.Quality) {
			return nil
		}
		return fmt.Errorf("only leader@engineering, for writing work, or leader@quality, for validation, may run asmai assign")
	case CommandAccept, CommandReject, CommandCancel:
		if ref.address == roles.LeaderOf(roles.Engineering) || ref.address == roles.LeaderOf(roles.Quality) {
			return nil
		}
		return fmt.Errorf("only leader@engineering or leader@quality may run asmai %s", req.Command)
	case CommandEffect:
		if ref.address.Name != roles.Leader || ref.address == roles.LeaderOf(roles.Engineering) {
			return nil
		}
		return fmt.Errorf("only a worker, or the delivery owner of a job, may run asmai effect")
	case CommandResult, CommandBlocked:
		if ref.address.Name != roles.Leader {
			return nil
		}
		return fmt.Errorf("only a worker may run asmai %s: a leader's work goes through its workers", req.Command)
	default:
		if userCommand := commandName(req.Command); userCommand != "" {
			return fmt.Errorf("an agent cannot run this command; ask the user to run `asmai %s`", userCommand)
		}
		return fmt.Errorf("an agent cannot run %q; use an agent coordination command", req.Command)
	}
}

// isAgentCommand reports whether command is one only an agent's session may
// run, and so needs the session's credential.
func isAgentCommand(command string) bool {
	switch command {
	case CommandJobOpen, CommandHook, CommandBrief, CommandInbox,
		CommandHandoffSend, CommandHandoffAccept, CommandHandoffClarify, CommandHandoffDecline,
		CommandAssign, CommandEffect, CommandResult, CommandBlocked, CommandAccept, CommandReject, CommandCancel,
		CommandTrailers:
		return true
	}
	return false
}

func commandName(command string) string {
	switch command {
	case CommandStart, CommandStop, CommandStatus, CommandAgents, CommandExport, CommandAttach, CommandHook, CommandBrief, CommandJobs:
		return command
	case CommandConversation:
		return "chat"
	case CommandRepoAdd, CommandRepoList, CommandRepoShow, CommandRepoRemove:
		return "repo " + strings.TrimPrefix(command, "repo.")
	case CommandProviders, CommandProviderInstalled:
		return "providers install"
	case CommandJobOpen:
		return "job open"
	case CommandJob:
		return "job <number>"
	case CommandInbox:
		return "inbox"
	case CommandAssign, CommandEffect, CommandResult, CommandBlocked, CommandAccept, CommandReject, CommandCancel:
		return command
	case CommandHandoffSend, CommandHandoffAccept, CommandHandoffClarify, CommandHandoffDecline:
		return "handoff " + strings.TrimPrefix(command, "handoff.")
	default:
		return ""
	}
}
