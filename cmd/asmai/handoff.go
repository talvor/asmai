// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/statedir"
	"github.com/talvor/asmai/internal/store"
)

func handoffSend(o *output, paths statedir.Paths, job int64, to, outcome, decisions, evidence, constraints, permissions string, criteria []string, records []string, assignment int64) int {
	if job <= 0 || strings.TrimSpace(to) == "" || strings.TrimSpace(outcome) == "" || len(criteria) == 0 {
		return o.fail(fmt.Errorf("handoff send needs --job, --to, --outcome and --criterion"))
	}
	if strings.TrimSpace(decisions) == "" || strings.TrimSpace(evidence) == "" || strings.TrimSpace(constraints) == "" || strings.TrimSpace(permissions) == "" {
		return o.fail(fmt.Errorf("handoff send needs --decisions, --evidence, --constraints and --permissions; use 'none' when empty"))
	}
	ids, err := recordIDs(records)
	if err != nil {
		return o.fail(err)
	}
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandHandoffSend, Session: os.Getenv(daemon.SessionCredential), Job: job, Agent: to, Outcome: outcome, Decisions: decisions, Evidence: evidence, Constraints: constraints, Permissions: permissions, Criteria: criteria, Records: ids, Assignment: assignment})
	if err != nil {
		return o.fail(err)
	}
	if resp.Error != "" {
		return o.fail(fmt.Errorf("%s", resp.Error))
	}
	if o.structured() {
		return o.printData(resp)
	}
	fmt.Fprintf(o.stdout, "handoff %d sent to %s for job %d; dispatch %d\n", resp.Handoff.ID, resp.Handoff.Receiver, job, resp.Dispatch.ID)
	if len(resp.Handoff.Records) > 0 {
		fmt.Fprintf(o.stdout, "decision records passed on: %s\n", strings.Join(idList(resp.Handoff.Records), ", "))
	}
	return 0
}

func handoffAnswer(o *output, paths statedir.Paths, command string, id int64, answer string) int {
	if id <= 0 {
		return o.fail(fmt.Errorf("%s needs --handoff", command))
	}
	kind := map[string]string{"handoff accept": daemon.CommandHandoffAccept, "handoff clarify": daemon.CommandHandoffClarify, "handoff decline": daemon.CommandHandoffDecline}[command]
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: kind, Session: os.Getenv(daemon.SessionCredential), Handoff: id, Answer: answer})
	if err != nil {
		return o.fail(err)
	}
	if resp.Error != "" {
		return o.fail(fmt.Errorf("%s", resp.Error))
	}
	if o.structured() {
		return o.printData(resp)
	}
	fmt.Fprintf(o.stdout, "handoff %d %s; reply dispatch %d\n", id, resp.Handoff.State, resp.Dispatch.ID)
	return 0
}

func inbox(o *output, paths statedir.Paths, dispatch int64) int {
	if dispatch < 0 {
		return o.fail(fmt.Errorf("dispatch ID must be positive"))
	}
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandInbox, Session: os.Getenv(daemon.SessionCredential), Dispatch: dispatch})
	if err != nil {
		return o.fail(err)
	}
	if resp.Error != "" {
		return o.fail(fmt.Errorf("%s", resp.Error))
	}
	if o.structured() {
		return o.printData(resp.Messages)
	}
	for _, m := range resp.Messages {
		fmt.Fprintf(o.stdout, "message %d | dispatch %d | job %d | %s from %s\n", m.ID, m.Dispatch, m.Job, m.Kind, m.Sender)
		if h := m.HandoffData; h != nil {
			fmt.Fprintf(o.stdout, "handoff %d | %s -> %s | %s\nOutcome: %s\nDecisions: %s\nEvidence: %s\nConstraints: %s\nPermissions: %s\nAcceptance criteria:\n", h.ID, h.Sender, h.Receiver, h.State, h.Outcome, h.Decisions, h.Evidence, h.Constraints, h.Permissions)
			for _, c := range h.Criteria {
				fmt.Fprintf(o.stdout, "- %s\n", c)
			}
			if h.Head != "" {
				fmt.Fprintf(o.stdout, "Head to validate: %s\n", h.Head)
			}
			switch h.Review {
			case store.ReviewChanges:
				fmt.Fprintf(o.stdout, "Review: changes since %s, the last commit Quality validated; later commits made that evidence stale\n", h.Since)
			case store.ReviewChecks:
				fmt.Fprintf(o.stdout, "Review: checks only; what changed since %s, the last commit Quality validated, is a clean merge of the moved base\n", h.Since)
			case store.ReviewEarly:
				fmt.Fprintf(o.stdout, "Review: early, of the result of assignment %d, which %s asked Quality to review before the job branch is finished\n", h.Assignment, h.Sender)
			case store.ReviewFull:
				fmt.Fprintln(o.stdout, "Review: full, the first validation of the finished job branch")
			}
			printRecords(o.stdout, "Decision records passed with the handoff", h.DecisionRecords)
			printGrants(o.stdout, "Grants in force for this job, carried with the handoff; reuse one while its scope and conditions hold, and run `asmai authorize` before each consequential action it covers", h.Grants)
		}
		if m.HandoffData != nil && m.Kind != "handoff" {
			fmt.Fprintf(o.stdout, "Answer: %s\n", m.Body)
		}
		if m.Kind == store.MessageValidation {
			fmt.Fprintf(o.stdout, "Validation delivered by %s: %s\n", m.Sender, m.Body)
		}
		if m.Kind == store.MessageRejection || m.Kind == store.MessageCancellation {
			fmt.Fprintf(o.stdout, "Reasons from %s:\n%s\n", m.Sender, m.Body)
		}
		if m.Kind == store.MessageMovedTip {
			fmt.Fprintf(o.stdout, "Your result is returned by %s, which is not a rejection:\n%s\n", m.Sender, m.Body)
		}
		if m.Kind == store.MessageResumption {
			fmt.Fprintf(o.stdout, "Resumes dispatch %d, which stopped at a boundary before the factory stopped.\n%s\n", m.Resumes, m.Body)
		}
		if m.Kind == store.MessageRedispatch {
			fmt.Fprintf(o.stdout, "%s\n", m.Body)
		}
		if m.Kind == store.MessageRestoration || m.Kind == store.MessageReconciliation || m.Kind == store.MessageStalled || m.Kind == store.MessageDelivery || m.Kind == store.MessageChecksRed || m.Kind == store.MessageGrantEnded || m.Kind == store.MessageSetup {
			fmt.Fprintf(o.stdout, "%s\n", m.Body)
		}
		if a := m.AssignmentData; a != nil {
			printAssignment(o.stdout, *a)
		}
		if r := m.ReportData; r != nil {
			printReport(o.stdout, *r)
		}
		if b := m.BlockedData; b != nil {
			if m.Kind == store.MessageBlockedResolved {
				fmt.Fprintf(o.stdout, "%s resolved your blocked report %d of job %d. What changed:\n%s\n", m.Sender, b.ID, b.Job, m.Body)
			}
			printBlockedReport(o.stdout, *b)
			if m.Kind == store.ReportBlocked {
				fmt.Fprintf(o.stdout, "Settle it from the records, hand the work to another role, or put a choice that is the user's to them with `asmai decision request`; once its cause is settled, run `asmai blocked resolve %d --note <what changed>`\n", b.ID)
			}
		}
		if r := m.RequestData; r != nil {
			if m.Kind == store.MessageDecisionAnswer {
				fmt.Fprintf(o.stdout, "Answer to version %d of decision request %d, from %s:\n%s\n", r.Version, r.Request, m.Sender, m.Body)
			} else if m.Kind == store.MessageLavishSent {
				fmt.Fprintf(o.stdout, "The user sent version %d of decision request %d from its Lavish page, which went to %s:\n%s\n", r.Version, r.Request, r.Requester, m.Body)
			} else {
				printRequest(o.stdout, *r)
			}
		}
	}
	return 0
}
