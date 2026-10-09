// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/statedir"
	"github.com/talvor/asmai/internal/store"
)

// listFlags is a flag that may be repeated, each time with one more value.
type listFlags []string

func (l *listFlags) String() string         { return strings.Join(*l, "; ") }
func (l *listFlags) Set(value string) error { *l = append(*l, value); return nil }

// none is what an agent says to report that a list it must give is empty, so
// that leaving it out is never taken for it.
const none = "none"

// explicit returns the values of the flag --name, which must be given: with
// "none" alone it means there are no values.
func explicit(name string, values []string) ([]string, error) {
	switch {
	case len(values) == 0:
		return nil, fmt.Errorf("give --%s, or --%s %s when there is nothing to give", name, name, none)
	case len(values) == 1 && strings.TrimSpace(values[0]) == none:
		return nil, nil
	}
	for _, v := range values {
		if strings.TrimSpace(v) == "" || strings.TrimSpace(v) == none {
			return nil, fmt.Errorf("--%s %q is not a value: give each value, or only --%s %s", name, v, name, none)
		}
	}
	return values, nil
}

// checkSeparator separates a check's command from its outcome in --check.
const checkSeparator = " -> "

// parseChecks reads the values of --check, each "<command> -> <outcome>".
func parseChecks(values []string) ([]store.Check, error) {
	var checks []store.Check
	for _, v := range values {
		i := strings.LastIndex(v, checkSeparator)
		command, outcome := "", ""
		if i >= 0 {
			command, outcome = strings.TrimSpace(v[:i]), strings.TrimSpace(v[i+len(checkSeparator):])
		}
		if command == "" || outcome == "" {
			return nil, fmt.Errorf("--check %q is not '<command>%s<outcome at your commit>'", v, checkSeparator)
		}
		checks = append(checks, store.Check{Command: command, Outcome: outcome})
	}
	return checks, nil
}

func assign(o *output, paths statedir.Paths, job int64, outcome string, criteria []string) int {
	if job <= 0 || strings.TrimSpace(outcome) == "" || len(criteria) == 0 {
		return o.fail(errors.New("assign needs --job, --outcome and --criterion"))
	}
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandAssign, Job: job, Outcome: outcome, Criteria: criteria})
	if err != nil {
		return o.fail(err)
	}
	if o.json {
		return o.printJSON(resp)
	}
	a := resp.Assignment
	fmt.Fprintf(o.stdout, "assignment %d of job %d given to %s; dispatch %d\n", a.ID, a.Job, a.Worker, resp.Dispatch.ID)
	fmt.Fprintf(o.stdout, "assignment branch: %s, made from %s at %s\nworkspace: %s (slot %d)\n", a.Workspace.Branch, a.JobBranch, a.Workspace.Base, a.Workspace.Path, a.Workspace.Slot)
	fmt.Fprintf(o.stdout, "next: %s is started in its workspace and nudged; its result or blocked report comes back to you through `asmai inbox`\n", a.Worker)
	return 0
}

func effect(o *output, paths statedir.Paths, kind, ref string) int {
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandEffect, Kind: kind, Ref: ref})
	if err != nil {
		return o.fail(err)
	}
	if o.json {
		return o.printJSON(resp)
	}
	e := resp.Effect
	fmt.Fprintf(o.stdout, "effect %d recorded: %s %s (job %d, dispatch %d)\n", e.ID, e.Kind, e.Ref, e.Job, e.Dispatch)
	return 0
}

// resultFlags are the values of `asmai result`'s flags.
type resultFlags struct {
	artifacts, evidence, tests, checks, gaps listFlags
	prSection                                string
}

func result(o *output, paths statedir.Paths, f resultFlags) int {
	var input store.ReportInput
	var err error
	if input.Evidence = f.evidence; len(input.Evidence) == 0 {
		return o.fail(errors.New("result needs --evidence: what shows the work does"))
	}
	for _, evidence := range input.Evidence {
		if strings.TrimSpace(evidence) == "" {
			return o.fail(errors.New("result evidence cannot be blank"))
		}
	}
	if strings.TrimSpace(f.prSection) == "" {
		return o.fail(errors.New("result needs --pr-section: your part of the pull request, what changed with before-and-after evidence"))
	}
	input.Artifacts, input.PRSection = f.artifacts, f.prSection
	if input.Tests, err = explicit("test", f.tests); err != nil {
		return o.fail(err)
	}
	checks, err := explicit("check", f.checks)
	if err != nil {
		return o.fail(err)
	}
	if input.Checks, err = parseChecks(checks); err != nil {
		return o.fail(err)
	}
	if input.Gaps, err = explicit("gap", f.gaps); err != nil {
		return o.fail(err)
	}
	return report(o, paths, daemon.CommandResult, input)
}

func blocked(o *output, paths statedir.Paths, reason, needs string) int {
	if strings.TrimSpace(reason) == "" {
		return o.fail(errors.New("blocked needs --reason: why you cannot go on"))
	}
	return report(o, paths, daemon.CommandBlocked, store.ReportInput{Reason: reason, Needs: needs})
}

func report(o *output, paths statedir.Paths, command string, input store.ReportInput) int {
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: command, Report: &input})
	if err != nil {
		return o.fail(err)
	}
	if o.json {
		return o.printJSON(resp)
	}
	r := resp.Report
	fmt.Fprintf(o.stdout, "%s recorded for assignment %d at commit %s; sent to %s as dispatch %d\n", r.Kind, r.Assignment, r.Commit, resp.Dispatch.Agent, resp.Dispatch.ID)
	if !r.TookInTip {
		fmt.Fprintf(o.stdout, "note: this commit does not have the job branch's tip %s in its history; take the tip in and submit again\n", r.JobTip)
	}
	if !slices.ContainsFunc(r.Effects, func(e store.Effect) bool { return e.Kind == "push" }) {
		fmt.Fprintln(o.stdout, "note: no push was recorded in this dispatch; push your assignment branch and record it with `asmai effect push <ref>`")
	}
	return 0
}

// decide makes the owning leader's decision on an assignment: to accept or
// reject its result, or to cancel it.
func decide(o *output, paths statedir.Paths, command string, assignment int64, reasons []string) int {
	if assignment <= 0 {
		return o.fail(fmt.Errorf("%s needs --assignment", command))
	}
	if len(reasons) == 0 {
		return o.fail(fmt.Errorf("%s needs --reason: why, against the assignment's acceptance criteria", command))
	}
	for _, reason := range reasons {
		if strings.TrimSpace(reason) == "" {
			return o.fail(fmt.Errorf("%s --reason cannot be blank", command))
		}
	}
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: command, Assignment: assignment, Reasons: reasons})
	if err != nil {
		return o.fail(err)
	}
	if o.json {
		return o.printJSON(resp)
	}
	a, d := resp.Assignment, resp.Decision
	switch command {
	case daemon.CommandAccept:
		b := resp.JobBranch
		fmt.Fprintf(o.stdout, "assignment %d accepted by %s at commit %s; the assignment has ended\n", a.ID, d.Leader, d.Commit)
		fmt.Fprintf(o.stdout, "job branch %s fast-forwarded to %s\nread-only view of the job refreshed: %s\n", b.Name, b.Tip, b.View)
		fmt.Fprintf(o.stdout, "next: the workspace is removed and %s stopped; later work is a new assignment\n", a.Worker)
	case daemon.CommandReject:
		fmt.Fprintf(o.stdout, "assignment %d: result rejected by %s; it is active again with %s, in dispatch %d\n", a.ID, d.Leader, a.Worker, resp.Dispatch.ID)
		fmt.Fprintf(o.stdout, "next: %s is nudged, and its next result or blocked report comes back to you through `asmai inbox`\n", a.Worker)
	default:
		fmt.Fprintf(o.stdout, "assignment %d: cancellation requested by %s; %s is told to push %s and stop, in dispatch %d\n", a.ID, d.Leader, a.Worker, a.Workspace.Branch, resp.Dispatch.ID)
		fmt.Fprintln(o.stdout, "next: the assignment is cancelled once the worker has stopped")
	}
	return 0
}

// printAssignment writes the assignment a worker is given, or a leader's
// result is about.
func printAssignment(w io.Writer, a store.Assignment) {
	fmt.Fprintf(w, "assignment %d | job %d | %s -> %s | %s\nOutcome: %s\nAcceptance criteria:\n", a.ID, a.Job, a.Owner, a.Worker, a.State, a.Outcome)
	for _, c := range a.Criteria {
		fmt.Fprintf(w, "- %s\n", c)
	}
	fmt.Fprintf(w, "Workspace: %s (slot %d; ASMAI_SLOT and TMPDIR %s are set for you)\nAssignment branch: %s, made from the job branch %s at %s\n", a.Workspace.Path, a.Workspace.Slot, a.Workspace.Tmp, a.Workspace.Branch, a.JobBranch, a.Workspace.Base)
}

// printReport writes a worker's result or blocked report.
func printReport(w io.Writer, r store.Report) {
	fmt.Fprintf(w, "%s %d | assignment %d | %s | commit %s\n", r.Kind, r.ID, r.Assignment, r.Agent, r.Commit)
	if r.Kind == store.ReportBlocked {
		fmt.Fprintf(w, "Reason: %s\n", r.Reason)
		if r.Needs != "" {
			fmt.Fprintf(w, "Needs: %s\n", r.Needs)
		}
	}
	list := func(title string, items []string) {
		fmt.Fprintf(w, "%s:", title)
		if len(items) == 0 {
			fmt.Fprintln(w, " none")
			return
		}
		fmt.Fprintln(w)
		for _, item := range items {
			fmt.Fprintf(w, "- %s\n", item)
		}
	}
	if r.Kind == store.ReportResult {
		list("Artifacts", r.Artifacts)
		list("Evidence", r.Evidence)
		list("Tests added", r.Tests)
		var checks []string
		for _, c := range r.Checks {
			checks = append(checks, c.Command+checkSeparator+c.Outcome+" (at "+r.Commit+")")
		}
		list("Checks run", checks)
		list("Gaps", r.Gaps)
	}
	var effects []string
	for _, e := range r.Effects {
		effects = append(effects, fmt.Sprintf("%d %s %s", e.ID, e.Kind, e.Ref))
	}
	list("Effects", effects)
	fmt.Fprintf(w, "Job branch tip when reported: %s (taken in: %t)\n", r.JobTip, r.TookInTip)
	if r.Kind == store.ReportResult {
		fmt.Fprintf(w, "PR section:\n%s\n", r.PRSection)
	}
}
