// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/store"
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
	case CommandJobLink:
		if ref.address.Role == roles.Coordination && ref.address.Name == roles.Leader {
			return nil
		}
		return fmt.Errorf("only leader@coordination may run asmai job link")
	case CommandAssign:
		if ownsWorkers(ref.address) {
			return nil
		}
		return fmt.Errorf("only leader@engineering, leader@planning, leader@research or leader@coordination, for writing work, or leader@quality, for validation, may run asmai assign")
	case CommandAccept, CommandReject, CommandCancel:
		if ownsWorkers(ref.address) {
			return nil
		}
		return fmt.Errorf("only leader@engineering, leader@planning, leader@research, leader@coordination or leader@quality may run asmai %s", req.Command)
	case CommandPush:
		if ref.address.Name != roles.Leader || isDeliveryOwner(ref.address) {
			return nil
		}
		return errors.New("only a worker, pushing its own assignment branch, leader@engineering, the delivery owner of a code job, leader@planning or leader@research, the delivery owner of a notes job, or leader@coordination, the delivery owner of a repository's setup, may run asmai push")
	case CommandCommit, CommandMerge:
		if ref.address.Name != roles.Leader {
			return nil
		}
		return fmt.Errorf("only a worker may run asmai %s: a leader's work goes through its workers", req.Command)
	case CommandPullRequestOpen, CommandPullRequestUpdate:
		if isDeliveryOwner(ref.address) {
			return nil
		}
		return fmt.Errorf("only the delivery owner of a job, leader@engineering, leader@planning, leader@research or leader@coordination, may run asmai %s", commandName(req.Command))
	case CommandDecisionRecord:
		if ref.address.Name == roles.Leader {
			return nil
		}
		return errors.New("only a role's leader may run asmai decision record: a worker's decision goes to its owning leader")
	case CommandEffect:
		if ref.address.Name != roles.Leader || isDeliveryOwner(ref.address) {
			return nil
		}
		return fmt.Errorf("only a worker, or the delivery owner of a job, may run asmai effect")
	case CommandDecisionRequest, CommandAuthorize:
		return nil
	case CommandGrantInvalidate:
		if ref.address.Name == roles.Leader {
			return nil
		}
		return errors.New("only a role's leader may run asmai grant invalidate: a worker tells its owning leader that a condition no longer holds")
	case CommandDecisionAnswer:
		if ref.address.Name == roles.Leader {
			return nil
		}
		return errors.New("only a leader may run asmai decision answer: a worker's request is answered by its owning leader")
	case CommandDecisionPresent:
		if ref.address == roles.LeaderOf(roles.Coordination) {
			return nil
		}
		return errors.New("only leader@coordination may run asmai decision present: it shows the user a decision in the conversation")
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
	case CommandJobOpen, CommandJobLink, CommandHook, CommandBrief, CommandInbox,
		CommandHandoffSend, CommandHandoffAccept, CommandHandoffClarify, CommandHandoffDecline,
		CommandAssign, CommandEffect, CommandResult, CommandBlocked, CommandAccept, CommandReject, CommandCancel,
		CommandPush, CommandCommit, CommandMerge, CommandPullRequestOpen, CommandPullRequestUpdate, CommandDecisionRecord, CommandDecisionRequest, CommandDecisionAnswer, CommandDecisionPresent, CommandTrailers, CommandAuthorize, CommandGrantInvalidate:
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
	case CommandConfigApply:
		return "config apply"
	case CommandProviders, CommandProviderInstalled:
		return "providers install"
	case CommandLavish:
		return "lavish"
	case CommandJobOpen:
		return "job open"
	case CommandJobLink:
		return "job link"
	case CommandJobClean:
		return "job clean"
	case CommandJob:
		return "job <number>"
	case CommandInbox:
		return "inbox"
	case CommandAssign, CommandEffect, CommandResult, CommandBlocked, CommandAccept, CommandReject, CommandCancel, CommandPush, CommandAuthorize, CommandCommit, CommandMerge:
		return command
	case CommandPullRequestOpen, CommandPullRequestUpdate:
		return "pr " + strings.TrimPrefix(command, "pr.")
	case CommandGrants:
		return "grants"
	case CommandGrantRevoke:
		return "grant revoke"
	case CommandGrantInvalidate:
		return "grant invalidate"
	case CommandDecisionRecord:
		return "decision record"
	case CommandDecisionRequest:
		return "decision request"
	case CommandDecisionAnswer:
		return "decision answer"
	case CommandDecisionPresent:
		return "decision present"
	case CommandDecisions:
		return "decisions"
	case CommandHandoffSend, CommandHandoffAccept, CommandHandoffClarify, CommandHandoffDecline:
		return "handoff " + strings.TrimPrefix(command, "handoff.")
	default:
		return ""
	}
}

// ownsWorkers reports whether address is a leader that gives workers
// assignments, and accepts, rejects and cancels them: Engineering's,
// Planning's and Research's for writing work, Coordination's for the setup of
// a repository, Quality's for validation.
func ownsWorkers(address roles.Address) bool {
	return slices.Contains([]roles.Address{roles.LeaderOf(roles.Engineering), roles.LeaderOf(roles.Planning), roles.LeaderOf(roles.Research), roles.LeaderOf(roles.Coordination), roles.LeaderOf(roles.Quality)}, address)
}

// isDeliveryOwner reports whether address is a leader that can own the delivery
// of a job: Engineering's, for a job that changes code, Planning's and
// Research's, for a job whose outcome is notes (06 rule 16), and
// Coordination's, for a repository's setup (08 rule 29). Which of them owns a
// given job is deliveryOwner's.
func isDeliveryOwner(address roles.Address) bool {
	return slices.Contains([]roles.Address{roles.LeaderOf(roles.Engineering), roles.LeaderOf(roles.Planning), roles.LeaderOf(roles.Research), roles.LeaderOf(roles.Coordination)}, address)
}

// deliveryOwner returns the leader that owns the delivery of j: the leader
// whose notes they are when its outcome is notes, Planning's for a
// planning-only job and Research's for a research-only job in a repository,
// Coordination's for the setup job of a repository, and Engineering's for
// any other (06 rule 16, 08 rule 29).
func deliveryOwner(j store.Job) roles.Address {
	if j.Notes() {
		return roles.LeaderOf(onlyRole[j.Mandate])
	}
	return roles.LeaderOf(roles.Engineering)
}
