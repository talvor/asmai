// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/dashboard/dashtest"
)

// jobsFactory is jobs in every stage.
func jobsFactory() []daemon.DashboardJob {
	return []daemon.DashboardJob{
		{Job: 9, Stage: daemon.StageDelivered, Assignments: daemon.AssignmentCounts{Accepted: 4}, PullRequest: 131, Title: "feat(daemon): add the journal cursor read"},
		{Job: 10, Stage: daemon.StageNeedsYou, Assignments: daemon.AssignmentCounts{Accepted: 2, Submitted: 1}, PullRequest: 140, Title: "fix(store): clear a blocking finding only by a later validation"},
		{Job: 11, Stage: daemon.StageDelivering, Assignments: daemon.AssignmentCounts{Accepted: 3}, PullRequest: 149, Title: "docs: record the dashboard's daemon calls"},
		{Job: 12, Stage: daemon.StageValidating, Assignments: daemon.AssignmentCounts{Active: 1, Accepted: 2}, Title: "Add the Jobs tab to the dashboard so a user can see how each job is progressing"},
		{Job: 14, Stage: daemon.StageWorking, Assignments: daemon.AssignmentCounts{Active: 2, Reconciliation: 1, Accepted: 1, Cancelled: 1}, Title: "Make the log read take a cursor in both directions"},
		{Job: 15, Stage: daemon.StageWorking, Title: "Plan the next release"},
	}
}

func TestDashboardJobsGoldenScreens(t *testing.T) {
	for _, size := range dashboardSizes {
		for _, env := range [][]string{nil, {"NO_COLOR=1"}} {
			name := fmt.Sprintf("jobs-%dx%d", size[0], size[1])
			if len(env) > 0 {
				name += "-no_color"
			}
			t.Run(name, func(t *testing.T) {
				fake := dashtest.NewFakeDaemon(t)
				fake.SetJobs(jobsFactory()...)
				d := dashboardIn(t, fake, size[0], size[1], env...)
				d.WaitForText("factory running")
				d.Send("2")
				d.WaitForText("Plan the next release")
				dashtest.Golden(t, name, d.Snapshot())
			})
		}
	}
}

// Pull request drops first and then Assignments as the width shrinks.
func TestDashboardJobsDropColumnsAsTheWidthShrinks(t *testing.T) {
	for _, tt := range []struct {
		columns int
		has     []string
		lacks   []string
	}{
		{100, []string{"JOB", "STAGE", "ASSIGNMENTS", "PR", "TITLE"}, nil},
		{90, []string{"JOB", "STAGE", "ASSIGNMENTS", "TITLE"}, []string{"PR "}},
		{80, []string{"JOB", "STAGE", "TITLE"}, []string{"ASSIGNMENTS", "PR "}},
	} {
		t.Run(fmt.Sprint(tt.columns), func(t *testing.T) {
			fake := dashtest.NewFakeDaemon(t)
			fake.SetJobs(jobsFactory()...)
			d := dashboardIn(t, fake, tt.columns, 30)
			d.WaitForText("factory running")
			d.Send("2")
			heading := d.WaitForText("STAGE")
			var line string
			for _, l := range strings.Split(heading, "\n") {
				if strings.Contains(l, "STAGE") {
					line = l
				}
			}
			for _, name := range tt.has {
				if !strings.Contains(line, name) {
					t.Errorf("at %d columns the heading %q lacks %s", tt.columns, line, name)
				}
			}
			for _, name := range tt.lacks {
				if strings.Contains(line, name) {
					t.Errorf("at %d columns the heading %q still has %s", tt.columns, line, name)
				}
			}
		})
	}
}

func TestDashboardJobsPreserveJobAndPullRequestNumbers(t *testing.T) {
	fake := dashtest.NewFakeDaemon(t)
	fake.SetJobs(
		daemon.DashboardJob{Job: 10000, Stage: daemon.StageWorking, PullRequest: 1234567, Title: "first"},
		daemon.DashboardJob{Job: 10001, Stage: daemon.StageWorking, PullRequest: 1234568, Title: "second"},
	)
	for _, width := range []int{80, 100, 160} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			d := dashboardIn(t, fake, width, 30)
			d.WaitForText("factory running")
			d.Send("2")
			screen := d.WaitForText("second")
			for _, number := range []string{"#10000", "#10001"} {
				if !strings.Contains(screen, number) {
					t.Errorf("the %d-column screen clips job %s:\n%s", width, number, screen)
				}
			}
			if width > 80 {
				for _, number := range []string{"#1234567", "#1234568"} {
					if !strings.Contains(screen, number) {
						t.Errorf("the %d-column screen clips pull request %s:\n%s", width, number, screen)
					}
				}
			}
		})
	}
}

// The title is cut with an ellipsis, and wider terminals give it room only up
// to about 160 columns.
func TestDashboardJobsTruncateTheTitle(t *testing.T) {
	fake := dashtest.NewFakeDaemon(t)
	fake.SetJobs(daemon.DashboardJob{Job: 1, Stage: daemon.StageWorking, Title: strings.Repeat("a long title ", 30)})
	for columns, want := range map[int]int{80: 80, 120: 120, 160: 160, 200: 160} {
		d := dashboardIn(t, fake, columns, 30)
		d.WaitForText("factory running")
		d.Send("2")
		screen := d.WaitForText("a long title")
		var row string
		for _, l := range strings.Split(screen, "\n") {
			if strings.Contains(l, "a long title") {
				row = l
			}
		}
		if !strings.HasSuffix(row, "…") {
			t.Errorf("at %d columns the long title is %q, want it cut with an ellipsis", columns, row)
		}
		if got := len([]rune(row)); got != want {
			t.Errorf("at %d columns the row is %d cells, want %d", columns, got, want)
		}
	}
}

// The stage is the daemon's: the Dashboard shows what it is told, whatever the
// assignment counts say.
func TestDashboardJobsShowTheDaemonsStageAndDeriveNone(t *testing.T) {
	fake := dashtest.NewFakeDaemon(t)
	fake.SetJobs(
		daemon.DashboardJob{Job: 1, Stage: daemon.StageDelivered, Assignments: daemon.AssignmentCounts{Active: 3}, Title: "one"},
		daemon.DashboardJob{Job: 2, Stage: daemon.StageWorking, Assignments: daemon.AssignmentCounts{Accepted: 3}, PullRequest: 5, Title: "two"},
	)
	d := dashboardIn(t, fake, 100, 30)
	d.WaitForText("factory running")
	d.Send("2")
	screen := d.WaitForText("two")
	for _, want := range []string{"✓ Delivered", "● Working", "act3", "acc3", "#5"} {
		if !strings.Contains(screen, want) {
			t.Errorf("the screen lacks %q:\n%s", want, screen)
		}
	}
}

func TestDashboardJobsShowEveryNonzeroAssignmentCount(t *testing.T) {
	fake := dashtest.NewFakeDaemon(t)
	fake.SetJobs(daemon.DashboardJob{Job: 1, Stage: daemon.StageWorking, Assignments: daemon.AssignmentCounts{
		Active: 2, Submitted: 3, Reconciliation: 4, Cancelling: 5, Accepted: 6, Cancelled: 7,
	}, Title: "all states with a title long enough to be truncated"})
	for _, size := range dashboardSizes {
		for _, env := range [][]string{nil, {"NO_COLOR=1"}} {
			name := fmt.Sprintf("%dx%d", size[0], size[1])
			if len(env) > 0 {
				name += "-no_color"
			}
			t.Run(name, func(t *testing.T) {
				d := dashboardIn(t, fake, size[0], size[1], env...)
				d.WaitForText("factory running")
				d.Send("2")
				screen := d.WaitForText("all states")
				if !strings.Contains(screen, "act2 sub3 rec4 cng5 acc6 cxl7") {
					t.Errorf("the assignment counts are incomplete:\n%s", screen)
				}
				if size[0] == 80 && !strings.Contains(screen, "… act2 sub3 rec4 cng5 acc6 cxl7") {
					t.Errorf("the 80-column title did not truncate before the counts:\n%s", screen)
				}
			})
		}
	}
}

func manyJobs(n int) []daemon.DashboardJob {
	var jobs []daemon.DashboardJob
	for i := 1; i <= n; i++ {
		jobs = append(jobs, daemon.DashboardJob{Job: int64(i), Stage: daemon.StageWorking, Title: fmt.Sprintf("job title %02d", i)})
	}
	return jobs
}

// The list scrolls with the cursor, which keeps its job when the list is read
// again, and the Agents tab's cursor is not moved.
func TestDashboardJobsScrollWithTheCursor(t *testing.T) {
	fake := dashtest.NewFakeDaemon(t)
	fake.SetJobs(manyJobs(40)...)
	d := dashboardIn(t, fake, 80, 24)
	d.WaitForText("factory running")
	d.Send("2")
	d.WaitForText("job title 01")
	if screen := d.Screen(); strings.Contains(screen, "job title 20") {
		t.Fatalf("the first screen already shows the twentieth job:\n%s", screen)
	}
	for range 25 {
		d.Send("j")
	}
	d.WaitForText("job title 26")
	screen := d.Screen()
	if strings.Contains(screen, "job title 01") || !strings.Contains(screen, "job title 26") || strings.Contains(screen, "26/40") {
		t.Errorf("after 25 moves down the screen is:\n%s", screen)
	}
	dashtest.Golden(t, "jobs-scrolled-80x24", d.Snapshot())

	d.Send("G")
	d.WaitForText("job title 40")
	d.Send("g")
	d.WaitForText("job title 01")
	d.Send("\x1b[A")
	d.Send("\x1b[6~")
	d.WaitForText("job title 20")
}

func TestDashboardJobsKeepTheCursorOnItsJobWhenReread(t *testing.T) {
	fake := dashtest.NewFakeDaemon(t)
	jobs := manyJobs(5)
	fake.SetJobs(jobs...)
	d := dashboardIn(t, fake, 100, 30)
	d.WaitForText("factory running")
	d.Send("2")
	d.WaitForText("job title 01")
	d.Send("j")
	d.Send("j")
	d.WaitFor("the third job selected", func(string) bool { return strings.Contains(d.Attributes(), "\n7 1-100: reverse\n") })
	// A new job sorts first here, as a test's fake lists it.
	fake.SetJobs(append([]daemon.DashboardJob{{Job: 0, Stage: daemon.StageWorking, Title: "job title 00"}}, jobs...)...)
	d.WaitForText("job title 00")
	d.WaitFor("the third job still selected", func(string) bool { return strings.Contains(d.Attributes(), "\n8 1-100: reverse\n") })
}
