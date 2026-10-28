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

func assign(o *output, paths statedir.Paths, job int64, outcome string, criteria []string, readOnly bool, commit, skill string, narrow []string, approved int64, records []string, of int64) int {
	if job <= 0 || strings.TrimSpace(outcome) == "" || len(criteria) == 0 {
		return o.fail(errors.New("assign needs --job, --outcome and --criterion"))
	}
	if readOnly && strings.TrimSpace(commit) == "" {
		return o.fail(errors.New("a read-only assignment is fixed at the job branch's exact head: give --commit"))
	}
	if !readOnly && commit != "" {
		return o.fail(errors.New("--commit belongs to --read-only: a writing assignment is made from the job branch's tip"))
	}
	ids, err := recordIDs(records)
	if err != nil {
		return o.fail(err)
	}
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandAssign, Job: job, Outcome: outcome, Criteria: criteria, ReadOnly: readOnly, Commit: commit, Skill: skill, Skills: narrow, Approved: approved, Records: ids, Assignment: of})
	if err != nil {
		return o.fail(err)
	}
	if o.structured() {
		return o.printData(resp)
	}
	a := resp.Assignment
	fmt.Fprintf(o.stdout, "assignment %d of job %d given to %s; dispatch %d\n", a.ID, a.Job, a.Worker, resp.Dispatch.ID)
	if a.Skill != "" {
		fmt.Fprintf(o.stdout, "skill named: %s\n", a.Skill)
	}
	if list := listOf(*a); len(list) > 0 {
		fmt.Fprintf(o.stdout, "skill list: %s\n", strings.Join(list, ", "))
	}
	if len(a.Records) > 0 {
		fmt.Fprintf(o.stdout, "decision records passed on: %s\n", strings.Join(idList(a.Records), ", "))
	}
	if a.Scratch {
		fmt.Fprintf(o.stdout, "scratch workspace, empty, with no repository, no branch and no commit\nworkspace: %s (slot %d)\n", a.Workspace.Path, a.Workspace.Slot)
		fmt.Fprintf(o.stdout, "next: %s is started in its workspace and nudged; its result or blocked report comes back to you through `asmai inbox`\n", a.Worker)
		return 0
	}
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
	if o.structured() {
		return o.printData(resp)
	}
	e := resp.Effect
	fmt.Fprintf(o.stdout, "effect %d recorded: %s %s (job %d, dispatch %d)\n", e.ID, e.Kind, e.Ref, e.Job, e.Dispatch)
	if p := resp.PullRequest; p != nil {
		fmt.Fprintf(o.stdout, "pull request #%d of job %d recorded, as GitHub has it: %s, from %s at %s; the daemon follows it\n", p.Number, p.Job, daemon.PullRequestState(*p), p.Branch, p.Head)
		if !p.Draft && !p.NotReady {
			fmt.Fprintln(o.stdout, `note: a pull request opens as a draft, or with "[not ready]" at the start of its title when GitHub refuses a draft`)
		}
	}
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
	// skills are the skills of the worker's list that it ran.
	skills listFlags
	// records and grants are the user's decision records and the grants that
	// the human steps of those skills relied on.
	records, grants listFlags
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
	for _, name := range f.skills {
		if name = strings.TrimSpace(name); name != "" && name != none {
			input.SkillsRun = append(input.SkillsRun, name)
		}
	}
	if input.RecordsRelied, err = idFlag("record", "decision record", f.records); err != nil {
		return o.fail(err)
	}
	if input.GrantsRelied, err = idFlag("grant", "grant", f.grants); err != nil {
		return o.fail(err)
	}
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
	// The daemon refuses a result with no PR section, except one of a worker
	// in a scratch workspace, which has no pull request: it alone knows which.
	// The tests and checks of a result with none are the daemon's to ask for
	// too, and a scratch workspace has no tests or checks to name.
	input.Artifacts, input.PRSection = f.artifacts, f.prSection
	noPR := strings.TrimSpace(f.prSection) == ""
	if len(f.tests) > 0 || !noPR {
		if input.Tests, err = explicit("test", f.tests); err != nil {
			return o.fail(err)
		}
	}
	if len(f.checks) > 0 || !noPR {
		checks, err := explicit("check", f.checks)
		if err != nil {
			return o.fail(err)
		}
		if input.Checks, err = parseChecks(checks); err != nil {
			return o.fail(err)
		}
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

// leaderBlocked files a leader's blocked report to Coordination's leader: the
// job it cannot go on with, why, what it needs, and the assignments of its
// own that depend on it, which are only named.
func leaderBlocked(o *output, paths statedir.Paths, job int64, reason, needs string, blocks []string) int {
	if job <= 0 {
		return o.fail(errors.New("--blocks goes with a leader's blocked report: give --job, the job you cannot go on with"))
	}
	if strings.TrimSpace(reason) == "" {
		return o.fail(errors.New("a blocked report needs --reason: why you cannot go on"))
	}
	if strings.TrimSpace(needs) == "" {
		return o.fail(errors.New("a blocked report needs --needs: what would let you go on"))
	}
	var ids []int64
	for _, text := range blocks {
		id, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
		if err != nil || id <= 0 {
			return o.fail(fmt.Errorf("--blocks is an assignment ID, not %q", text))
		}
		ids = append(ids, id)
	}
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandBlockedReport, Job: job, Report: &store.ReportInput{Reason: reason, Needs: needs}, Blocks: ids})
	if err != nil {
		return o.fail(err)
	}
	if o.structured() {
		return o.printData(resp)
	}
	b := resp.BlockedReport
	fmt.Fprintf(o.stdout, "blocked report %d recorded for job %d; sent to %s as dispatch %d\n", b.ID, b.Job, resp.Dispatch.Agent, resp.Dispatch.ID)
	if len(b.Blocks) > 0 {
		fmt.Fprintf(o.stdout, "it names assignments %v as depending on it; nothing is held\n", b.Blocks)
	}
	return 0
}

// blockedResolve is Coordination's leader resolving a leader's blocked report
// with a note of what changed, which goes to the leader that filed it.
func blockedResolve(o *output, paths statedir.Paths, id, note string) int {
	report, err := strconv.ParseInt(strings.TrimSpace(id), 10, 64)
	if err != nil || report <= 0 {
		return o.fail(fmt.Errorf("name the blocked report by its ID, as `asmai brief <number>` lists it, not %q", id))
	}
	if strings.TrimSpace(note) == "" {
		return o.fail(errors.New("resolving a blocked report needs --note: what changed"))
	}
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandBlockedResolve, BlockedReport: report, Note: note})
	if err != nil {
		return o.fail(err)
	}
	if o.structured() {
		return o.printData(resp)
	}
	fmt.Fprintf(o.stdout, "blocked report %d of job %d resolved; the note sent to %s as dispatch %d\n", resp.BlockedReport.ID, resp.BlockedReport.Job, resp.Dispatch.Agent, resp.Dispatch.ID)
	return 0
}

// printBlockedReport writes a leader's blocked report, as its message carries
// it.
func printBlockedReport(w io.Writer, b store.BlockedReport) {
	fmt.Fprintf(w, "blocked report %d | job %d | %s | %s\nReason: %s\nNeeds: %s\n", b.ID, b.Job, b.Leader, b.State, b.Reason, b.Needs)
	if len(b.Blocks) > 0 {
		fmt.Fprintf(w, "Assignments of %s that depend on it: %v; nothing is held\n", b.Leader, b.Blocks)
	}
}

func report(o *output, paths statedir.Paths, command string, input store.ReportInput) int {
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: command, Report: &input})
	if err != nil {
		return o.fail(err)
	}
	if o.structured() {
		return o.printData(resp)
	}
	r := resp.Report
	if r.Commit == "" {
		fmt.Fprintf(o.stdout, "%s recorded for assignment %d in its scratch workspace; sent to %s as dispatch %d\n", r.Kind, r.Assignment, resp.Dispatch.Agent, resp.Dispatch.ID)
		return 0
	}
	if r.Validation != nil {
		fmt.Fprintf(o.stdout, "validation report recorded for assignment %d at commit %s; sent to %s as dispatch %d\n", r.Assignment, r.Commit, resp.Dispatch.Agent, resp.Dispatch.ID)
		return 0
	}
	fmt.Fprintf(o.stdout, "%s recorded for assignment %d at commit %s; sent to %s as dispatch %d\n", r.Kind, r.Assignment, r.Commit, resp.Dispatch.Agent, resp.Dispatch.ID)
	if r.Kind == store.ReportResult && !r.TookInTip {
		fmt.Fprintf(o.stdout, "note: this commit does not have the job branch's tip %s in its history; take the tip in and submit again\n", r.JobTip)
	}
	if r.Kind == store.ReportResult && !slices.ContainsFunc(r.Effects, func(e store.Effect) bool { return e.Kind == "push" }) {
		fmt.Fprintln(o.stdout, "note: no push was recorded in this dispatch; push your assignment branch and record it with `asmai effect push <ref>`")
	}
	return 0
}

// decide makes the owning leader's decision on an assignment: to accept or
// reject its result, to cancel it, or to send it to its worker again.
func decide(o *output, paths statedir.Paths, command string, assignment int64, reasons []string, endsJob, prototype bool) int {
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
	if endsJob && command != daemon.CommandAccept {
		return o.fail(fmt.Errorf("%s takes no --ends-job: only an acceptance is the leader accepting the job's outcome", command))
	}
	if prototype && command != daemon.CommandAccept {
		return o.fail(fmt.Errorf("%s takes no --prototype: only an acceptance can leave a prototype on its own branch", command))
	}
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: command, Assignment: assignment, Reasons: reasons, EndsJob: endsJob, Prototype: prototype})
	if err != nil {
		return o.fail(err)
	}
	if o.structured() {
		return o.printData(resp)
	}
	a, d := resp.Assignment, resp.Decision
	if resp.Returned {
		fmt.Fprintf(o.stdout, "assignment %d: not accepted and not rejected: job branch %s moved to %s after the result was submitted, so the work is returned to %s, in dispatch %d, to take in the new tip and submit again\n", a.ID, resp.JobBranch.Name, resp.JobBranch.Tip, a.Worker, resp.Dispatch.ID)
		fmt.Fprintf(o.stdout, "next: %s is nudged, and its next result or blocked report comes back to you through `asmai inbox`\n", a.Worker)
		return 0
	}
	switch command {
	case daemon.CommandAccept:
		if a.ReadOnly {
			fmt.Fprintf(o.stdout, "validation %d accepted by %s at commit %s; the assignment has ended\n", a.ID, d.Leader, d.Commit)
			fmt.Fprintf(o.stdout, "the report and its findings are delivered to %s as dispatch %d; nothing about the job branch changed\n", resp.Dispatch.Agent, resp.Dispatch.ID)
			fmt.Fprintf(o.stdout, "next: the read-only workspace is removed and %s stopped\n", a.Worker)
			return 0
		}
		if a.Scratch {
			fmt.Fprintf(o.stdout, "assignment %d accepted by %s; the assignment has ended, and nothing else moved: its job has no repository\n", a.ID, d.Leader)
			fmt.Fprintf(o.stdout, "next: %s is stopped; the results stay in its scratch workspace %s\n", a.Worker, a.Workspace.Path)
			if resp.Dispatch != nil {
				fmt.Fprintf(o.stdout, "job %d has ended on your acceptance of its outcome: %s is told where the results are, in dispatch %d, and the scratch workspaces stay until the user runs `asmai job clean %d`\n", a.Job, resp.Dispatch.Agent, resp.Dispatch.ID, a.Job)
			}
			return 0
		}
		b := resp.JobBranch
		if p := resp.Prototype; p != nil {
			fmt.Fprintf(o.stdout, "assignment %d accepted by %s as a prototype that stays on its own branch %s at commit %s on origin; the assignment has ended\n", a.ID, d.Leader, p.Branch, p.Commit)
			fmt.Fprintf(o.stdout, "job branch %s did not move: it is at %s, and the prototype is not in the job's pull request\n", b.Name, b.Tip)
			fmt.Fprintf(o.stdout, "next: the workspace is removed and %s stopped; its verdict is a note on the job branch, written by a new assignment that names branch %s and commit %s\n", a.Worker, p.Branch, p.Commit)
			return 0
		}
		fmt.Fprintf(o.stdout, "assignment %d accepted by %s at commit %s; the assignment has ended\n", a.ID, d.Leader, d.Commit)
		fmt.Fprintf(o.stdout, "job branch %s fast-forwarded to %s\nread-only view of the job refreshed: %s\n", b.Name, b.Tip, b.View)
		fmt.Fprintf(o.stdout, "next: the workspace is removed and %s stopped; later work is a new assignment\n", a.Worker)
	case daemon.CommandReject:
		fmt.Fprintf(o.stdout, "assignment %d: result rejected by %s; it is active again with %s, in dispatch %d\n", a.ID, d.Leader, a.Worker, resp.Dispatch.ID)
		fmt.Fprintf(o.stdout, "next: %s is nudged, and its next result or blocked report comes back to you through `asmai inbox`\n", a.Worker)
	case daemon.CommandRedispatch:
		fmt.Fprintf(o.stdout, "assignment %d: sent to %s again by %s; it is active, in dispatch %d, a new dispatch that never reuses an earlier one\n", a.ID, a.Worker, a.Owner, resp.Dispatch.ID)
		fmt.Fprintf(o.stdout, "next: %s is started if it is not running and nudged, and its next result or blocked report comes back to you through `asmai inbox`\n", a.Worker)
	default:
		if a.Scratch {
			fmt.Fprintf(o.stdout, "assignment %d: cancellation requested by %s; %s is told to stop, in dispatch %d\n", a.ID, d.Leader, a.Worker, resp.Dispatch.ID)
			fmt.Fprintln(o.stdout, "next: the assignment is cancelled once the worker has stopped, and what it left stays in its scratch workspace")
			return 0
		}
		fmt.Fprintf(o.stdout, "assignment %d: cancellation requested by %s; %s is told to push %s and stop, in dispatch %d\n", a.ID, d.Leader, a.Worker, a.Workspace.Branch, resp.Dispatch.ID)
		fmt.Fprintln(o.stdout, "next: the assignment is cancelled once the worker has stopped")
	}
	return 0
}

// recoverAnswer runs answer redeliver or answer retire: the owning leader
// recovers the decision answer of dispatch, which its worker's session exited
// before acknowledging, by delivering it again or retiring it, for reasons.
func recoverAnswer(o *output, paths statedir.Paths, name string, dispatch int64, reasons []string) int {
	if dispatch <= 0 {
		return o.fail(fmt.Errorf("asmai %s needs --dispatch: the decision answer's dispatch, as the reconciliation message names it", name))
	}
	if len(reasons) == 0 {
		return o.fail(fmt.Errorf("asmai %s needs --reason: why the answer is recovered this way", name))
	}
	for _, reason := range reasons {
		if strings.TrimSpace(reason) == "" {
			return o.fail(fmt.Errorf("asmai %s --reason cannot be blank", name))
		}
	}
	command := daemon.CommandAnswerRedeliver
	if name == "answer retire" {
		command = daemon.CommandAnswerRetire
	}
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: command, Dispatch: dispatch, Reasons: reasons})
	if err != nil {
		return o.fail(err)
	}
	if o.structured() {
		return o.printData(resp)
	}
	a := resp.Assignment
	if command == daemon.CommandAnswerRetire {
		fmt.Fprintf(o.stdout, "decision answer dispatch %d retired by %s, journaled with the answer; it is never delivered to %s\n", dispatch, a.Owner, a.Worker)
		fmt.Fprintf(o.stdout, "next: assignment %d still needs reconciliation; send it to its worker again with `asmai redispatch --assignment %[1]d --reason <why>`\n", a.ID)
		return 0
	}
	fmt.Fprintf(o.stdout, "decision answer dispatch %d delivered to %s again by %s, in dispatch %d, a new dispatch; assignment %d is active again\n", dispatch, a.Worker, a.Owner, resp.Dispatch.ID, a.ID)
	fmt.Fprintf(o.stdout, "next: %s is started if it is not running and nudged, and its next result or blocked report comes back to you through `asmai inbox`\n", a.Worker)
	return 0
}

// listOf returns the skill list of a's worker: the daemon's, or a's
// narrowing or its role's default list from a daemon that gives none.
func listOf(a store.Assignment) []string {
	if a.List != nil {
		return a.List
	}
	return a.EffectiveSkills()
}

// printAssignment writes the assignment a worker is given, or a leader's
// result is about.
func printAssignment(w io.Writer, a store.Assignment) {
	fmt.Fprintf(w, "assignment %d | job %d | %s -> %s | %s\nOutcome: %s\nAcceptance criteria:\n", a.ID, a.Job, a.Owner, a.Worker, a.State, a.Outcome)
	for _, c := range a.Criteria {
		fmt.Fprintf(w, "- %s\n", c)
	}
	if a.Skill != "" {
		fmt.Fprintf(w, "Skill to run: %s\n", a.Skill)
	}
	printRecords(w, "Decision records passed to you with this assignment", a.DecisionRecords)
	printGrants(w, "Grants in force for this job, carried with the assignment; reuse one while its scope and conditions hold, and run `asmai authorize` before each consequential action it covers", a.Grants)
	if list := listOf(a); len(list) > 0 {
		fmt.Fprintf(w, "Your skill list for this assignment: %s\n", strings.Join(list, ", "))
	}
	if a.ReadOnly {
		fixed := fmt.Sprintf("the head of the job branch %s", a.JobBranch)
		if a.Review == store.ReviewEarly {
			fixed = fmt.Sprintf("the result of assignment %d, which %s asked Quality to review earlier than the finished job branch", a.Reviews, a.Requester)
		}
		fmt.Fprintf(w, "Read-only workspace: %s (slot %d; ASMAI_SLOT and TMPDIR %s are set for you)\nFixed at commit: %s, %s; it has no branch, and you never change the work\n", a.Workspace.Path, a.Workspace.Slot, a.Workspace.Tmp, a.Commit, fixed)
		printReviewScope(w, a)
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
	if a.Scratch {
		fmt.Fprintf(w, "Scratch workspace: %s (slot %d; ASMAI_SLOT and TMPDIR %s are set for you)\nIt is empty: the job has no repository, so it has no clone, no branch and no commit. Write your findings in it.\n", a.Workspace.Path, a.Workspace.Slot, a.Workspace.Tmp)
		return
	}
	fmt.Fprintf(w, "Workspace: %s (slot %d; ASMAI_SLOT and TMPDIR %s are set for you)\nAssignment branch: %s, made from the job branch %s at %s\n", a.Workspace.Path, a.Workspace.Slot, a.Workspace.Tmp, a.Workspace.Branch, a.JobBranch, a.Workspace.Base)
}

// printReviewScope says how a validation goes, and so what its worker does.
func printReviewScope(w io.Writer, a store.Assignment) {
	switch a.Review {
	case store.ReviewFull:
		fmt.Fprintln(w, "Review: full. This is the first validation of the finished job branch: re-run the repository's checks at the commit, and review the whole change on both axes, Standards and Spec.")
	case store.ReviewChanges:
		fmt.Fprintf(w, "Review: changes since %s, the last commit Quality validated. Later commits make that evidence stale: re-run the repository's checks at this commit, review what changed since %s on both axes, Standards and Spec, and say for each earlier blocking finding below whether it is resolved at this commit.\n", a.Since, a.Since)
	case store.ReviewChecks:
		fmt.Fprintf(w, "Review: checks only. What changed since %s, the last commit Quality validated, is a clean merge of the moved base, which needs no review: re-run the repository's checks at this commit and report their outcomes.\n", a.Since)
	case store.ReviewEarly:
		fmt.Fprintf(w, "Review: early. Re-run the repository's checks at this commit and review the result of assignment %d on both axes, Standards and Spec. This validates no head of the job: the finished job branch is validated again later. Say which earlier blocking findings below your review resolves; any you leave out stay open.\n", a.Reviews)
	}
}

// reliedList says the records a result relied on, each by its ID.
func reliedList(r store.Report) []string {
	var list []string
	for _, id := range r.RecordsRelied {
		list = append(list, fmt.Sprintf("decision record %d", id))
	}
	for _, id := range r.GrantsRelied {
		list = append(list, fmt.Sprintf("grant %d", id))
	}
	return list
}

// printReport writes a worker's result or blocked report.
func printReport(w io.Writer, r store.Report) {
	if r.Commit == "" {
		fmt.Fprintf(w, "%s %d | assignment %d | %s | in a scratch workspace, no commit\n", r.Kind, r.ID, r.Assignment, r.Agent)
	} else {
		fmt.Fprintf(w, "%s %d | assignment %d | %s | commit %s\n", r.Kind, r.ID, r.Assignment, r.Agent, r.Commit)
	}
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
		list("Skills run", r.SkillsRun)
		list("Records relied on", reliedList(r))
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

// push asks the daemon to push the job branch of the job number names.
func push(o *output, paths statedir.Paths, number string) int {
	n, err := jobNumber(number)
	if err != nil {
		return o.fail(err)
	}
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandPush, Job: n})
	if err != nil {
		return o.fail(err)
	}
	if o.structured() {
		return o.printData(resp)
	}
	p := resp.Push
	fmt.Fprintf(o.stdout, "job %d: the daemon pushed %s at %s to origin with a plain push; effect %d recorded in dispatch %d\n", p.Job, p.Branch, p.Commit, p.Effect, p.Dispatch)
	return 0
}

// decisionFlags are the values of `asmai decision record`'s flags.
type decisionFlags struct {
	job                                   int64
	choice, reliedOn, rationale, evidence string
	assumptions, reopenWhen               listFlags
	source                                string
	// cites is the witnessed message's journal ID, or latest.
	cites string
	// request, version and reading make the record the user's answer in the
	// conversation to a version of a decision request, with Coordination's
	// reading of their words.
	request int64
	version int
	reading string
	// grant makes the record a grant the user gave in words, at its scope.
	grant grantFlags
}

// recordIDs reads the values of --record, decision record IDs.
func recordIDs(values []string) ([]int64, error) {
	return idFlag("record", "decision record", values)
}

// idFlag reads the values of --name, each the ID of a thing the flag names.
func idFlag(name, what string, values []string) ([]int64, error) {
	var ids []int64
	for _, v := range values {
		id, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("--%s %q is not a %s ID", name, v, what)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func idList(ids []int64) []string {
	list := make([]string, 0, len(ids))
	for _, id := range ids {
		list = append(list, strconv.FormatInt(id, 10))
	}
	return list
}

// printRecords writes the decision records passed on, each with its fields.
func printRecords(w io.Writer, title string, records []store.DecisionRecord) {
	if len(records) == 0 {
		return
	}
	fmt.Fprintf(w, "%s:\n", title)
	for _, r := range records {
		fmt.Fprint(w, r.Text("  ", "  "))
	}
}

// decisionRecord has a leader record a decision with the fields of rule 25.
// Engineering's leader's choice rerun-failed is its judgment on a head's red
// checks, on which the daemon re-runs their failed jobs once.
func decisionRecord(o *output, paths statedir.Paths, f decisionFlags) int {
	cites, latest, err := witnessedRef(f.cites)
	if err != nil {
		return o.fail(fmt.Errorf("--cites: %w", err))
	}
	if f.grant.grant {
		return grantRecord(o, paths, f, f.grant, cites, latest)
	}
	if f.request != 0 {
		return conversationAnswer(o, paths, f, cites, latest)
	}
	rerun := strings.TrimSpace(f.choice) == store.ChoiceRerunFailed
	switch {
	case f.job <= 0:
		return o.fail(errors.New("decision record needs --job"))
	case strings.TrimSpace(f.choice) == "":
		return o.fail(errors.New("decision record needs --choice: what the decision chose"))
	case !rerun && strings.TrimSpace(f.reliedOn) == "":
		return o.fail(errors.New("decision record needs --relied-on: the mandate or grant the decision relied on"))
	case strings.TrimSpace(f.rationale) == "":
		return o.fail(errors.New("decision record needs --rationale: why"))
	case strings.TrimSpace(f.evidence) == "":
		return o.fail(errors.New("decision record needs --evidence: what the decision rests on"))
	}
	var assumptions, reopenWhen []string
	if rerun {
		assumptions, reopenWhen = f.assumptions, f.reopenWhen
	} else {
		var err error
		if assumptions, err = explicit("assumption", f.assumptions); err != nil {
			return o.fail(err)
		}
		if reopenWhen, err = explicit("reopen-when", f.reopenWhen); err != nil {
			return o.fail(err)
		}
	}
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandDecisionRecord, Job: f.job, Choice: f.choice, ReliedOn: f.reliedOn, Rationale: f.rationale, Evidence: f.evidence,
		Assumptions: assumptions, ReopenWhen: reopenWhen, DecisionSource: f.source, Cites: cites, LatestWitness: latest})
	if err != nil {
		return o.fail(err)
	}
	if o.structured() {
		return o.printData(resp)
	}
	d, r := resp.DecisionRecord, resp.Rerun
	fmt.Fprintf(o.stdout, "job %d: decision %d recorded, %s by %s: %s\n", d.Job, d.ID, d.Source, d.Leader, d.Choice)
	if r == nil {
		fmt.Fprintf(o.stdout, "next: pass it on with --record %d on asmai assign or asmai handoff send where it matters; it is in the job's brief\n", d.ID)
		return 0
	}
	fmt.Fprintf(o.stdout, "the daemon re-ran the failed jobs of head %s once, in workflow runs %s: %s\n", r.Head, runList(r.Runs), store.CheckNames(r.Checks))
	fmt.Fprintln(o.stdout, "next: the daemon watches the head's checks again; a second re-run of this head is refused, so if they stay red assign a fix")
	return 0
}

// witnessedRef reads --cites: the journal ID of a witnessed message, or
// latest for the newest the user submitted to the caller's conversation, or
// nothing.
func witnessedRef(value string) (id int64, latest bool, err error) {
	switch value = strings.TrimSpace(value); value {
	case "":
		return 0, false, nil
	case "latest":
		return 0, true, nil
	}
	id, err = strconv.ParseInt(value, 10, 64)
	if err != nil || id <= 0 {
		return 0, false, fmt.Errorf("%q is not a witnessed message's ID or latest", value)
	}
	return id, false, nil
}

func runList(runs []int64) string {
	ids := make([]string, 0, len(runs))
	for _, run := range runs {
		ids = append(ids, strconv.FormatInt(run, 10))
	}
	return strings.Join(ids, ", ")
}
