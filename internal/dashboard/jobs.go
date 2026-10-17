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
// shrinks, and the title is never narrower than minTitle.
const (
	jobNumberWidth = 5
	stageWidth     = 14
	assignWidth    = 28
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
}

var stageStyles = map[daemon.JobStage]lipgloss.Style{
	daemon.StageWorking:    lipgloss.NewStyle().Foreground(lipgloss.Color("4")),
	daemon.StageValidating: lipgloss.NewStyle().Foreground(lipgloss.Color("6")),
	daemon.StageDelivering: lipgloss.NewStyle().Foreground(lipgloss.Color("6")),
	daemon.StageNeedsYou:   lipgloss.NewStyle().Foreground(lipgloss.Color("3")),
	daemon.StageDelivered:  lipgloss.NewStyle().Faint(true),
}

// jobColumns says which optional columns fit in width.
type jobColumns struct {
	assignments, pr bool
	title           int
}

func (m model) jobColumns() jobColumns {
	width := min(m.width, maxWidth)
	var c jobColumns
	used := 1 + jobNumberWidth + 1 + stageWidth + 1 // gutter, job, stage
	// The optional columns come back in the order they are dropped in reverse.
	for _, col := range []struct {
		on    *bool
		width int
	}{{&c.assignments, assignWidth}, {&c.pr, prWidth}} {
		if used+col.width+1+minTitle <= width {
			*col.on = true
			used += col.width + 1
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
	}{{a.Active, "active"}, {a.Submitted, "submitted"}, {a.Reconciliation, "reconcile"}, {a.Cancelling, "cancelling"}, {a.Accepted, "accepted"}, {a.Cancelled, "cancelled"}} {
		if s.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", s.n, s.word))
		}
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, ", ")
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
	heading := m.jobRow(c, jobHeading, lipgloss.NewStyle())
	if len(jobs) > m.listRows() {
		count := fmt.Sprintf(" %d/%d", m.jobCursor+1, len(jobs))
		heading = clip(heading, width-lipgloss.Width(count))
		heading += strings.Repeat(" ", max(width-lipgloss.Width(heading)-lipgloss.Width(count), 0)) + count
	}
	rows := []string{dimStyle.Render(pad(heading, width))}
	end := min(m.jobOffset+m.listRows(), len(jobs))
	for i := m.jobOffset; i < end; i++ {
		j := jobs[i]
		cells := jobCells{job: fmt.Sprintf("#%d", j.Job), stage: stageGlyphs[j.Stage] + " " + j.Stage.Word(), assignments: assignmentWords(j.Assignments), pr: "-", title: j.Title}
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

// jobRow lays out one row in the columns that fit, with the stage cell in
// style, and pads it to the table's width.
func (m model) jobRow(c jobColumns, cells jobCells, stage lipgloss.Style) string {
	var b strings.Builder
	b.WriteString(" ")
	b.WriteString(pad(cells.job, jobNumberWidth))
	b.WriteString(" ")
	b.WriteString(stage.Render(pad(cells.stage, stageWidth)))
	b.WriteString(" ")
	if c.assignments {
		b.WriteString(pad(cells.assignments, assignWidth))
		b.WriteString(" ")
	}
	if c.pr {
		b.WriteString(pad(cells.pr, prWidth))
		b.WriteString(" ")
	}
	b.WriteString(clip(cells.title, c.title))
	return b.String()
}
