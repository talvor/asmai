// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/dashboard/dashtest"
)

// jobDetailAt is the detail of job 12 as the daemon lists it: the needs-you
// finding first, then the other findings, the assignments and the handoffs,
// each with the time it last changed.
func jobDetailAt(now time.Time) daemon.DashboardJobDetail {
	ago := func(d time.Duration) time.Time { return now.Add(-d - 30*time.Second) }
	return daemon.DashboardJobDetail{
		Job: 12, Title: "Add a cursor read to the journal for the dashboard", Stage: daemon.StageNeedsYou, HandoffComplete: true,
		Branch: "asmai/job-12-journal-cursor-read", Head: "4f1c9a2b7d0e", PullRequest: 101, PullRequestState: "not ready",
		CI: "waiting: 3 of 5 passed, 2 pending", Validation: "accepted at 4f1c9a2, 1 blocking open",
		Work: []daemon.JobItem{
			{Kind: daemon.ItemFinding, ID: 3, Label: "needs-you", Needs: true, Text: "The change lets the dashboard's command set answer an agent's call without a credential, which ADR 0012 forbids.\nDecide whether the exception is wanted.", At: ago(9 * time.Minute)},
			{Kind: daemon.ItemFinding, ID: 5, Label: "blocking", Text: "The journal order test is flaky on macOS", At: ago(9 * time.Minute)},
			{Kind: daemon.ItemFinding, ID: 4, Label: "advisory", Text: "No doc line for the new read", At: ago(9 * time.Minute)},
			{Kind: daemon.ItemAssignment, ID: 31, Label: "accepted", Text: "Add the cursor read behind the dashboard's command set", From: "leader@engineering", Worker: "worker-3@engineering", Criteria: []string{"tests pass", "no CLI exposure"}, Dispatch: "stopped", At: ago(2 * time.Hour)},
			{Kind: daemon.ItemAssignment, ID: 32, Label: "active", Text: "Add --after to the journal read", From: "leader@engineering", Worker: "worker-3@engineering", Criteria: []string{"filters by job, agent and kind", "pages in both directions without reading an entry twice"}, Dispatch: "working", At: ago(14 * time.Minute)},
			{Kind: daemon.ItemAssignment, ID: 33, Label: "active", Text: "Validate 4f1c9a2 against the mandate and the repository's instructions", From: "leader@quality", Worker: "worker-2@quality", Criteria: []string{"blocking findings reported"}, Dispatch: "delivered", At: ago(4 * time.Minute)},
			{Kind: daemon.ItemHandoff, ID: 7, Label: "accepted", Text: "Add a cursor read to the journal for the dashboard", From: "leader@coordination", To: "leader@engineering", Decisions: "Use the journal", Evidence: "The log needs a cursor", Constraints: "Keep reads bounded", Permissions: "Read the repository", Criteria: []string{"No repeated entries"}, Answer: "Accepted", At: ago(3 * time.Hour)},
			{Kind: daemon.ItemHandoff, ID: 9, Label: "accepted", Text: "Validate the job branch at 4f1c9a2", From: "leader@engineering", To: "leader@quality", Head: "4f1c9a2b7d0e", At: ago(26 * time.Minute)},
		},
	}
}

func jobDetailFake(t *testing.T) *dashtest.FakeDaemon {
	t.Helper()
	fake := dashtest.NewFakeDaemon(t)
	fake.SetJobs(
		daemon.DashboardJob{Job: 12, Stage: daemon.StageNeedsYou, Assignments: daemon.AssignmentCounts{Active: 2, Accepted: 1}, PullRequest: 101, Title: "Add a cursor read to the journal for the dashboard"},
		daemon.DashboardJob{Job: 14, Stage: daemon.StageWorking, Title: "Make the log read take a cursor"},
	)
	fake.SetJobDetails(jobDetailAt(time.Now()), daemon.DashboardJobDetail{Job: 14, Title: "Make the log read take a cursor", Stage: daemon.StageWorking, CI: "no pull request", Validation: "not started", Work: []daemon.JobItem{}})
	fake.SetLog(nil, nil)
	return fake
}

func TestDashboardJobDetailGoldenScreens(t *testing.T) {
	for _, size := range dashboardSizes {
		for _, env := range [][]string{nil, {"NO_COLOR=1"}} {
			name := fmt.Sprintf("jobdetail-%dx%d", size[0], size[1])
			if len(env) > 0 {
				name += "-no_color"
			}
			t.Run(name, func(t *testing.T) {
				fake := jobDetailFake(t)
				d := dashboardIn(t, fake, size[0], size[1], env...)
				d.WaitForText("factory running")
				d.Send("2")
				d.WaitForText("Make the log read take a cursor")
				d.Send("\r")
				d.WaitForText("Validation accepted at 4f1c9a2")
				dashtest.Golden(t, name, d.Snapshot())
			})
		}
	}
}

func TestDashboardJobDetailNoHandoffGolden(t *testing.T) {
	fake := jobDetailFake(t)
	d := dashboardIn(t, fake, 100, 30, "NO_COLOR=1")
	d.WaitForText("factory running")
	d.Send("2")
	d.WaitForText("Make the log read take a cursor")
	d.Send("j\r")
	screen := d.WaitForText("Nothing recorded for this job yet.")
	if !strings.Contains(screen, "● Handoff ── ○ Working") || strings.Contains(screen, "✓ Handoff") {
		t.Errorf("an unhanded job has the wrong progress:\n%s", screen)
	}
	dashtest.Golden(t, "jobdetail-no-handoff-100x30-no_color", d.Snapshot())
}

// The selected item shows beside the Work list from 100 columns, and stacked
// below it under 100.
func TestDashboardJobDetailSplitsFromOneHundredColumnsAndStacksBelow(t *testing.T) {
	for _, tt := range []struct {
		columns int
		split   bool
	}{{99, false}, {100, true}} {
		t.Run(fmt.Sprint(tt.columns), func(t *testing.T) {
			fake := jobDetailFake(t)
			d := dashboardIn(t, fake, tt.columns, 30)
			d.WaitForText("factory running")
			d.Send("2")
			d.WaitForText("Make the log read take a cursor")
			d.Send("\r")
			screen := d.WaitForText("Validation accepted at 4f1c9a2")
			if got := strings.Contains(screen, "│"); got != tt.split {
				t.Errorf("at %d columns the divider is %v, want %v:\n%s", tt.columns, got, tt.split, screen)
			}
			var work, selected int
			for i, line := range strings.Split(screen, "\n") {
				if strings.Contains(line, "ITEM") {
					work = i
				}
				if strings.Contains(line, "Finding 3") {
					selected = i
				}
			}
			if sameRow := work == selected; sameRow != tt.split {
				t.Errorf("at %d columns the Work heading is on row %d and the selected item's on %d:\n%s", tt.columns, work, selected, screen)
			}
		})
	}
}

// Enter on a job opens its detail, which lists the needs-you finding first and
// the selected item in full; Esc returns to the Jobs list.
func TestDashboardJobDetailOpensOnEnterAndEscReturns(t *testing.T) {
	fake := jobDetailFake(t)
	d := dashboardIn(t, fake, 160, 40)
	d.WaitForText("factory running")
	d.Send("2")
	d.WaitForText("Make the log read take a cursor")
	d.Send("\r")
	screen := d.WaitForText("Validation accepted at 4f1c9a2")
	for _, want := range []string{
		"#12  Add a cursor read to the journal for the dashboard",
		"✓ Handoff ── ▲ Working ── ○ Validating ── ○ Delivering ── ○ Delivered",
		"Stage ▲ Needs you · Branch asmai/job-12-journal-cursor-read · Head 4f1c9a2",
		"Pull request #101 not ready · CI waiting: 3 of 5 passed, 2 pending",
		"F3", "▲ needs-you", "A32", "H9", "9m", "2h", "3h",
		"Finding 3", "which ADR 0012 forbids.", "Decide whether the exception is wanted.",
	} {
		if !strings.Contains(screen, want) {
			t.Errorf("the job detail lacks %q:\n%s", want, screen)
		}
	}
	rows := strings.Split(screen, "\n")
	first := -1
	for i, row := range rows {
		if strings.Contains(row, "F3 ") && first < 0 {
			first = i
		}
		if strings.Contains(row, "F5 ") && first < 0 {
			t.Errorf("the needs-you finding is not first:\n%s", screen)
		}
	}
	if !strings.Contains(screen, "esc back") {
		t.Errorf("the footer lacks Esc:\n%s", screen)
	}
	var asked []int64
	for _, call := range fake.Calls() {
		if call.Command == daemon.CommandDashboardJob {
			asked = append(asked, call.Job)
		}
	}
	if len(asked) == 0 || asked[0] != 12 {
		t.Errorf("the job detail was asked for jobs %v, want job 12", asked)
	}

	d.Send("\x1b")
	d.WaitFor("the Jobs list back", func(screen string) bool {
		return strings.Contains(screen, "ASSIGNMENTS") && !strings.Contains(screen, "Validation")
	})
}

func TestDashboardJobDetailMovesThroughTheWorkAndShowsEachItemInFull(t *testing.T) {
	fake := jobDetailFake(t)
	d := dashboardIn(t, fake, 100, 30)
	d.WaitForText("factory running")
	d.Send("2")
	d.WaitForText("Make the log read take a cursor")
	d.Send("\r")
	d.WaitForText("Finding 3")
	for _, step := range []struct {
		keys string
		want []string
	}{
		{"jjj", []string{"Assignment 31", "worker worker-3@engineering", "owner leader@engineering", "Acceptance:", "- no CLI exposure"}},
		{"j", []string{"Assignment 32", "dispatch working", "enter attach"}},
		{"jj", []string{"Handoff 7", "leader@coordination → leader@engineering", "Request:", "Decisions:", "Use the journal"}},
		{"G", []string{"Handoff 9", "head 4f1c9a2b7d0e"}},
		{"g", []string{"Finding 3"}},
	} {
		d.Send(step.keys)
		screen := d.WaitForText(step.want[0])
		for _, want := range step.want {
			if !strings.Contains(screen, want) {
				t.Errorf("after %q the screen lacks %q:\n%s", step.keys, want, screen)
			}
		}
	}
}

// Enter on an assignment hands the terminal to `asmai attach` for its worker,
// as the Agents tab does; Enter on anything else does not.
func TestDashboardJobDetailEnterOnAnAssignmentAttachesToItsWorker(t *testing.T) {
	fake := jobDetailFake(t)
	fake.ServeAttach("the worker's own screen")
	d := dashboardIn(t, fake, 100, 30)
	d.WaitForText("factory running")
	d.Send("2")
	d.WaitForText("Make the log read take a cursor")
	d.Send("\r")
	d.WaitForText("Finding 3")

	d.Send("\r") // a finding
	d.Send("jjjj")
	d.WaitForText("Assignment 32")
	for _, call := range fake.Calls() {
		if call.Command == daemon.CommandAttach {
			t.Fatalf("Enter on a finding attached to %s", call.Agent)
		}
	}
	d.Send("\r")
	d.WaitForText("worker-3@engineering · observing · Ctrl-] detaches")
	if screen := d.Screen(); !strings.Contains(screen, "the worker's own screen") || strings.Contains(screen, "asmai dashboard") {
		t.Errorf("while attached the screen is\n%s\nwant the worker's screen and none of the Dashboard's", screen)
	}
	d.Send("\x1d") // Ctrl-]
	screen := d.WaitForText("Assignment 32")
	if !strings.Contains(screen, "Validation accepted at 4f1c9a2") {
		t.Errorf("after attach the job detail is gone:\n%s", screen)
	}
	var attaches []string
	for _, call := range fake.Calls() {
		if call.Command == daemon.CommandAttach {
			attaches = append(attaches, call.Agent)
		}
	}
	if len(attaches) != 1 || attaches[0] != "worker-3@engineering" {
		t.Errorf("the daemon was asked to attach %q, want only worker-3@engineering", attaches)
	}
}

// l opens the Log filtered to the job.
func TestDashboardJobDetailLShowsTheLogFilteredToTheJob(t *testing.T) {
	fake := jobDetailFake(t)
	entries, bodies := logJournal()
	fake.SetLog(entries, bodies)
	d := dashboardIn(t, fake, 100, 30, logEnv...)
	d.WaitForText("factory running")
	d.Send("2")
	d.WaitForText("Make the log read take a cursor")
	d.Send("\r")
	d.WaitForText("Finding 3")
	d.Send("l")
	screen := d.WaitForText("agent traffic, job #12")
	if strings.Contains(screen, "Validation") {
		t.Errorf("the job detail is still on screen:\n%s", screen)
	}
	var queries []daemon.LogQuery
	for _, call := range fake.Calls() {
		if call.Command == daemon.CommandDashboardLog && call.Log != nil {
			queries = append(queries, *call.Log)
		}
	}
	if len(queries) == 0 || queries[0].Job != 12 || queries[0].Agent != "" {
		t.Errorf("the log was read with %+v, want the first read narrowed to job 12", queries)
	}
}

// A job with nothing recorded says so, and a job that is gone says why.
func TestDashboardJobDetailWithNoWorkOrNoJob(t *testing.T) {
	fake := jobDetailFake(t)
	d := dashboardIn(t, fake, 100, 30)
	d.WaitForText("factory running")
	d.Send("2")
	d.WaitForText("Make the log read take a cursor")
	d.Send("j\r")
	d.WaitForText("Nothing recorded for this job yet.")
	d.Send("\x1b")
	d.WaitFor("the Jobs list", func(screen string) bool { return strings.Contains(screen, "ASSIGNMENTS") })
	d.Send("k\r")
	d.WaitForText("Finding 3")

	fake.SetJobDetails()
	d.Send("\x1b")
	d.WaitFor("the Jobs list", func(screen string) bool { return strings.Contains(screen, "ASSIGNMENTS") })
	d.Send("\r")
	d.WaitForText("Could not read job #12: job does not exist")
}

// The job detail only reads: every call the Dashboard sent is one of its own.
func TestDashboardJobDetailSendsOnlyDashboardCommands(t *testing.T) {
	fake := jobDetailFake(t)
	d := dashboardIn(t, fake, 100, 30)
	d.WaitForText("factory running")
	d.Send("2")
	d.WaitForText("Make the log read take a cursor")
	d.Send("\r")
	d.WaitForText("Finding 3")
	d.Send("l")
	d.WaitForText("agent traffic, job #12")
	for _, call := range fake.Calls() {
		if call.Command != daemon.CommandStatus && !isDashboardCommand(call.Command) {
			t.Errorf("the Dashboard sent %s", call.Command)
		}
	}
}

func isDashboardCommand(command string) bool {
	for _, c := range daemon.DashboardCommands {
		if c == command {
			return true
		}
	}
	return false
}
