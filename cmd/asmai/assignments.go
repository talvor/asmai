// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
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

func assign(o *output, paths statedir.Paths, job int64, outcome string, criteria []string, readOnly bool, commit string) int {
	if job <= 0 || strings.TrimSpace(outcome) == "" || len(criteria) == 0 {
		return o.fail(errors.New("assign needs --job, --outcome and --criterion"))
	}
	if readOnly && strings.TrimSpace(commit) == "" {
		return o.fail(errors.New("a read-only assignment is fixed at the job branch's exact head: give --commit"))
	}
	if !readOnly && commit != "" {
		return o.fail(errors.New("--commit belongs to --read-only: a writing assignment is made from the job branch's tip"))
	}
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandAssign, Job: job, Outcome: outcome, Criteria: criteria, ReadOnly: readOnly, Commit: commit})
	if err != nil {
		return o.fail(err)
	}
	if o.json {
		return o.printJSON(resp)
	}
	a := resp.Assignment
	fmt.Fprintf(o.stdout, "assignment %d of job %d given to %s; dispatch %d\n", a.ID, a.Job, a.Worker, resp.Dispatch.ID)
	if a.ReadOnly {
		fmt.Fprintf(o.stdout, "read-only workspace fixed at %s of %s, with no branch\nworkspace: %s (slot %d)\n", a.Commit, a.JobBranch, a.Workspace.Path, a.Workspace.Slot)
		fmt.Fprintf(o.stdout, "next: %s is started in its workspace and nudged; its validation report comes back to you through `asmai inbox`\n", a.Worker)
		return 0
	}
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
	// findings, resolved and unresolved make the result a validation
	// report: a Quality worker's findings, and the earlier blocking findings
	// it says are resolved or still open.
	findings, resolved, unresolved listFlags
}

// validationFinding reads the value of --finding, '<kind>: <text>'.
func validationFinding(value string) (store.FindingInput, error) {
	kind, text, found := strings.Cut(value, ":")
	f := store.FindingInput{Kind: strings.ToLower(strings.TrimSpace(kind)), Text: strings.TrimSpace(text)}
	if !found || f.Text == "" || !slices.Contains(store.FindingKinds, f.Kind) {
		return store.FindingInput{}, fmt.Errorf("--finding %q is not '<%s>: <text>'", value, strings.Join(store.FindingKinds, "|"))
	}
	return f, nil
}

// findingIDs reads the values of --resolved or --unresolved, finding IDs.
func findingIDs(name string, values []string) ([]int64, error) {
	var ids []int64
	for _, v := range values {
		id, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("--%s %q is not a finding ID, as the assignment lists it", name, v)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// validationResult reads a validation report: the checks re-run, the
// findings, and the earlier blocking findings each resolved or not.
func validationResult(f resultFlags, input store.ReportInput) (store.ReportInput, error) {
	var err error
	if f.prSection != "" || len(f.tests) > 0 || len(f.artifacts) > 0 {
		return input, errors.New("a validation report has findings, not tests or a PR section: it changes nothing")
	}
	checks, err := explicit("check", f.checks)
	if err != nil {
		return input, err
	}
	if input.Checks, err = parseChecks(checks); err != nil {
		return input, err
	}
	if input.Gaps, err = explicit("gap", f.gaps); err != nil {
		return input, err
	}
	v := &store.ValidationInput{Findings: []store.FindingInput{}}
	findings, err := explicit("finding", f.findings)
	if err != nil {
		return input, err
	}
	for _, value := range findings {
		finding, err := validationFinding(value)
		if err != nil {
			return input, err
		}
		v.Findings = append(v.Findings, finding)
	}
	if v.Resolved, err = findingIDs("resolved", f.resolved); err != nil {
		return input, err
	}
	if v.StillOpen, err = findingIDs("unresolved", f.unresolved); err != nil {
		return input, err
	}
	input.Validation = v
	return input, nil
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
	if len(f.findings) > 0 || len(f.resolved) > 0 || len(f.unresolved) > 0 {
		if input, err = validationResult(f, input); err != nil {
			return o.fail(err)
		}
		return report(o, paths, daemon.CommandResult, input)
	}
	if strings.TrimSpace(f.prSection) == "" {
		return o.fail(errors.New("result needs --pr-section: your part of the pull request, what changed with before-and-after evidence; a Quality worker's validation report gives --finding instead"))
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
	if r.Validation != nil {
		fmt.Fprintf(o.stdout, "validation report recorded for assignment %d at commit %s; sent to %s as dispatch %d\n", r.Assignment, r.Commit, resp.Dispatch.Agent, resp.Dispatch.ID)
		return 0
	}
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
		if a.ReadOnly {
			fmt.Fprintf(o.stdout, "validation %d accepted by %s at commit %s; the assignment has ended\n", a.ID, d.Leader, d.Commit)
			fmt.Fprintf(o.stdout, "the report and its findings are delivered to %s as dispatch %d; nothing about the job branch changed\n", resp.Dispatch.Agent, resp.Dispatch.ID)
			fmt.Fprintf(o.stdout, "next: the read-only workspace is removed and %s stopped\n", a.Worker)
			return 0
		}
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
	if a.ReadOnly {
		fmt.Fprintf(w, "Read-only workspace: %s (slot %d; ASMAI_SLOT and TMPDIR %s are set for you)\nFixed at commit: %s, the head of the job branch %s; it has no branch, and you never change the work\n", a.Workspace.Path, a.Workspace.Slot, a.Workspace.Tmp, a.Commit, a.JobBranch)
		if i := a.Intent; i != nil {
			fmt.Fprintf(w, "Job intent:\nUser's words: %s\nCoordination's reading: %s\nMandate: %s\nJob acceptance criteria:\n", i.Words, i.Reading, i.Mandate)
			for _, c := range i.Criteria {
				fmt.Fprintf(w, "- %s\n", c)
			}
		}
		fmt.Fprintln(w, "Earlier blocking findings still open, which your report says are resolved or not:")
		if len(a.OpenFindings) == 0 {
			fmt.Fprintln(w, "- none")
		}
		for _, f := range a.OpenFindings {
			fmt.Fprintf(w, "- finding %d, found at %s: %s\n", f.ID, f.Commit, f.Text)
		}
		return
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
	if r.Kind == store.ReportResult && r.Validation != nil {
		fmt.Fprintf(w, "Validation report at commit %s\n", r.Commit)
	}
	if r.Kind == store.ReportResult {
		list("Artifacts", r.Artifacts)
		list("Evidence", r.Evidence)
		if r.Validation == nil {
			list("Tests added", r.Tests)
		}
		var checks []string
		for _, c := range r.Checks {
			checks = append(checks, c.Command+checkSeparator+c.Outcome+" (at "+r.Commit+")")
		}
		list("Checks run", checks)
		list("Gaps", r.Gaps)
	}
	if v := r.Validation; v != nil {
		var found []string
		switch {
		case len(r.Findings) > 0:
			for _, f := range r.Findings {
				found = append(found, fmt.Sprintf("finding %d | %s | %s", f.ID, f.Kind, f.Text))
			}
		default:
			for _, f := range v.Findings {
				found = append(found, fmt.Sprintf("%s | %s", f.Kind, f.Text))
			}
		}
		list("Findings", found)
		var earlier []string
		for _, id := range v.Resolved {
			earlier = append(earlier, fmt.Sprintf("finding %d resolved", id))
		}
		for _, id := range v.StillOpen {
			earlier = append(earlier, fmt.Sprintf("finding %d still open", id))
		}
		list("Earlier blocking findings", earlier)
	}
	var effects []string
	for _, e := range r.Effects {
		effects = append(effects, fmt.Sprintf("%d %s %s", e.ID, e.Kind, e.Ref))
	}
	list("Effects", effects)
	fmt.Fprintf(w, "Job branch tip when reported: %s (taken in: %t)\n", r.JobTip, r.TookInTip)
	if r.Kind == store.ReportResult && r.Validation == nil {
		fmt.Fprintf(w, "PR section:\n%s\n", r.PRSection)
	}
}
