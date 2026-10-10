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

func handoffSend(o *output, paths statedir.Paths, job int64, to, outcome, decisions, evidence, constraints, permissions string, criteria []string) int {
	if job <= 0 || strings.TrimSpace(to) == "" || strings.TrimSpace(outcome) == "" || len(criteria) == 0 {
		return o.fail(fmt.Errorf("handoff send needs --job, --to, --outcome and --criterion"))
	}
	if strings.TrimSpace(decisions) == "" || strings.TrimSpace(evidence) == "" || strings.TrimSpace(constraints) == "" || strings.TrimSpace(permissions) == "" {
		return o.fail(fmt.Errorf("handoff send needs --decisions, --evidence, --constraints and --permissions; use 'none' when empty"))
	}
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandHandoffSend, Session: os.Getenv(daemon.SessionCredential), Job: job, Agent: to, Outcome: outcome, Decisions: decisions, Evidence: evidence, Constraints: constraints, Permissions: permissions, Criteria: criteria})
	if err != nil {
		return o.fail(err)
	}
	if resp.Error != "" {
		return o.fail(fmt.Errorf("%s", resp.Error))
	}
	if o.json {
		return o.printJSON(resp)
	}
	fmt.Fprintf(o.stdout, "handoff %d sent to %s for job %d; dispatch %d\n", resp.Handoff.ID, resp.Handoff.Receiver, job, resp.Dispatch.ID)
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
	if o.json {
		return o.printJSON(resp)
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
	if o.json {
		return o.printJSON(resp.Messages)
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
		if a := m.AssignmentData; a != nil {
			printAssignment(o.stdout, *a)
		}
		if r := m.ReportData; r != nil {
			printReport(o.stdout, *r)
		}
	}
	return 0
}
