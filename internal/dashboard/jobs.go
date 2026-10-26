// SPDX-License-Identifier: Apache-2.0

package dashboard

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/talvor/asmai/internal/daemon"
)

// The Jobs tab's columns. The job, stage and title always stay and the title
// takes what is left; Pull request, then Assignments drop as the width
// shrinks.
const (
	jobNumberWidth = 5
	stageWidth     = 14
	minAssignWidth = 32
	prWidth        = 7
	minTitle       = 28
)

// stageGlyphs and stageStyles give every stage its own glyph; the colour is
// only a second cue, so with no colour the glyph and the word still tell them
// apart.
var stageGlyphs = map[daemon.JobStage]string{
	daemon.StageWorking:    "●",
	daemon.StageValidating: "◐",
	daemon.StageDelivering: "▶",
	daemon.StageNeedsYou:   "▲",
	daemon.StageDelivered:  "✓",
	daemon.StageCancelled:  "✗",
}

var stageStyles = map[daemon.JobStage]lipgloss.Style{
	daemon.StageWorking:    lipgloss.NewStyle().Foreground(lipgloss.Color("4")),
	daemon.StageValidating: lipgloss.NewStyle().Foreground(lipgloss.Color("6")),
	daemon.StageDelivering: lipgloss.NewStyle().Foreground(lipgloss.Color("6")),
	daemon.StageNeedsYou:   lipgloss.NewStyle().Foreground(lipgloss.Color("3")),
	daemon.StageDelivered:  lipgloss.NewStyle().Faint(true),
	daemon.StageCancelled:  lipgloss.NewStyle().Faint(true),
}

// jobTitle is the title a job's row shows: a cancelled job's leads with the
// user's own words it was cancelled for.
func jobTitle(j daemon.DashboardJob) string {
	if j.Stage == daemon.StageCancelled && j.Reason != "" {
		reason, _, _ := strings.Cut(strings.TrimSpace(j.Reason), "\n")
		return "cancelled: " + reason + " · " + j.Title
	}
	return j.Title
}

// jobColumns says which optional columns fit in width.
type jobColumns struct {
	assignments, pr                  bool
	job, assignWidth, prWidth, title int
}

func (m model) jobColumns() jobColumns {
	width := min(m.width, maxWidth)
	c := jobColumns{job: jobNumberWidth, assignWidth: minAssignWidth, prWidth: prWidth}
	for _, job := range m.factory.jobs {
		c.job = max(c.job, lipgloss.Width(fmt.Sprintf("#%d", job.Job)))
		c.assignWidth = max(c.assignWidth, lipgloss.Width(assignmentWords(job.Assignments)))
		if job.PullRequest != 0 {
			c.prWidth = max(c.prWidth, lipgloss.Width(fmt.Sprintf("#%d", job.PullRequest)))
		}
	}
	used := 1 + c.job + 1 + stageWidth + 1 // gutter, job, stage
	if used+c.assignWidth+1+minTitle <= width {
		c.assignments = true
		used += c.assignWidth + 1
		if used+c.prWidth+1+minTitle <= width {
			c.pr = true
			used += c.prWidth + 1
		}
	}
	c.title = max(width-used, 0)
	return c
}

// jobCells are the texts of one row.
type jobCells struct{ job, stage, assignments, pr, title string }

var jobHeading = jobCells{"JOB", "STAGE", "ASSIGNMENTS", "PR", "TITLE"}

// assignmentWords says how many assignments are in each state, leaving out the
// states with none.
func assignmentWords(a daemon.AssignmentCounts) string {
	var parts []string
	for _, s := range []struct {
		n    int
		word string
	}{{a.Active, "act"}, {a.Submitted, "sub"}, {a.Reconciliation, "rec"}, {a.Cancelling, "cng"}, {a.Accepted, "acc"}, {a.Cancelled, "cxl"}} {
		if s.n > 0 {
			parts = append(parts, fmt.Sprintf("%s%d", s.word, s.n))
		}
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, " ")
}

// jobsBody draws the Jobs tab: a row of column names and the jobs the
// cursor's window shows.
func (m model) jobsBody() []string {
	jobs := m.factory.jobs
	if len(jobs) == 0 {
		return []string{"", pad(" No jobs.", m.width)}
	}
	c := m.jobColumns()
	width := min(m.width, maxWidth)
	headingCells := jobHeading
	if !c.assignments {
		headingCells.assignments = ""
	}
	heading := m.jobRow(c, headingCells, lipgloss.NewStyle())
	rows := []string{dimStyle.Render(pad(heading, width))}
	end := min(m.jobOffset+m.listRows(), len(jobs))
	for i := m.jobOffset; i < end; i++ {
		j := jobs[i]
		cells := jobCells{job: fmt.Sprintf("#%d", j.Job), stage: stageGlyphs[j.Stage] + " " + j.Stage.Word(), assignments: assignmentWords(j.Assignments), pr: "-", title: jobTitle(j)}
		if j.PullRequest != 0 {
			cells.pr = fmt.Sprintf("#%d", j.PullRequest)
		}
		if i == m.jobCursor {
			rows = append(rows, selectedStyle.Render(pad(m.jobRow(c, cells, lipgloss.NewStyle()), width)))
			continue
		}
		rows = append(rows, m.jobRow(c, cells, stageStyles[j.Stage]))
	}
	return rows
}

// jobRow lays out one row in the columns that fit, styling the stage cell.
func (m model) jobRow(c jobColumns, cells jobCells, stage lipgloss.Style) string {
	var b strings.Builder
	b.WriteString(" ")
	b.WriteString(pad(cells.job, c.job))
	b.WriteString(" ")
	b.WriteString(stage.Render(pad(cells.stage, stageWidth)))
	b.WriteString(" ")
	if c.assignments {
		b.WriteString(pad(cells.assignments, c.assignWidth))
		b.WriteString(" ")
	}
	if c.pr {
		b.WriteString(pad(cells.pr, c.prWidth))
		b.WriteString(" ")
	}
	titleWidth := c.title
	inlineCounts := !c.assignments && cells.assignments != "" && cells.assignments != "-"
	if inlineCounts {
		titleWidth -= lipgloss.Width(cells.assignments) + 1
	}
	if titleWidth > 0 {
		b.WriteString(clip(cells.title, titleWidth))
	}
	if inlineCounts {
		b.WriteString(" ")
		b.WriteString(cells.assignments)
	}
	return b.String()
}
