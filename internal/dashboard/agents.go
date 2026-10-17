// SPDX-License-Identifier: Apache-2.0

package dashboard

import (
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/talvor/asmai/internal/daemon"
)

// The Agents tab's columns. The agent and status always stay and the doing
// column takes what is left; Since, then Input, then Job drop as the width
// shrinks, and the doing column is never narrower than minDoing.
const (
	statusWidth = 22
	jobWidth    = 6
	inputWidth  = 10
	sinceWidth  = 5
	minDoing    = 24
	// maxWidth is the width the table stops growing at: extra width goes to
	// the doing column only up to about this many columns.
	maxWidth = 160
	// agentWidthMax caps the agent column, which is as wide as the longest
	// agent address up to it.
	agentWidthMax = 28
	agentWidthMin = 8
)

// statusGlyphs and statusStyles give every status its own glyph; the colour is
// only a second cue, so with no colour the glyph and the word still tell them
// apart.
var statusGlyphs = map[daemon.AgentStatus]string{
	daemon.StatusStopped:        "■",
	daemon.StatusWaitingOnYou:   "▲",
	daemon.StatusReconciliation: "◆",
	daemon.StatusWorking:        "●",
	daemon.StatusIdle:           "○",
}

var statusStyles = map[daemon.AgentStatus]lipgloss.Style{
	daemon.StatusStopped:        lipgloss.NewStyle().Faint(true),
	daemon.StatusWaitingOnYou:   lipgloss.NewStyle().Foreground(lipgloss.Color("3")),
	daemon.StatusReconciliation: lipgloss.NewStyle().Foreground(lipgloss.Color("5")),
	daemon.StatusWorking:        lipgloss.NewStyle().Foreground(lipgloss.Color("4")),
	daemon.StatusIdle:           lipgloss.NewStyle().Faint(true),
}

// agentColumns says which optional columns fit in width, and how wide the
// agent column is.
type agentColumns struct {
	agent             int
	job, input, since bool
	doing             int
}

func (m model) agentColumns() agentColumns {
	width := min(m.width, maxWidth)
	c := agentColumns{agent: agentWidthMin}
	for _, a := range m.factory.agents {
		c.agent = max(c.agent, min(lipgloss.Width(a.Agent), agentWidthMax))
	}
	used := 1 + c.agent + 1 + statusWidth + 1 // gutter, agent, status
	// The optional columns come back in the order they are dropped in reverse.
	for _, col := range []struct {
		on    *bool
		width int
	}{{&c.job, jobWidth}, {&c.input, inputWidth}, {&c.since, sinceWidth}} {
		if used+col.width+1+minDoing <= width {
			*col.on = true
			used += col.width + 1
		}
	}
	c.doing = max(width-used, 0)
	return c
}

// listRows is how many agents the list shows: the body less the column row.
func (m model) listRows() int { return max(m.height-chromeRows-1, 1) }

// chromeRows are the header, the tabs, the banner and the footer.
const chromeRows = 4

// agentCells are the texts of one row.
type agentCells struct{ agent, status, job, input, since, doing string }

var agentHeading = agentCells{"AGENT", "STATUS", "JOB", "INPUT", "SINCE", "DOING"}

// agentsBody draws the Agents tab: a row of column names and the agents the
// cursor's window shows.
func (m model) agentsBody() []string {
	agents := m.factory.agents
	if len(agents) == 0 {
		return []string{"", pad(" No agents.", m.width)}
	}
	c := m.agentColumns()
	width := min(m.width, maxWidth)
	heading := m.agentRow(c, agentHeading, lipgloss.NewStyle())
	if len(agents) > m.listRows() {
		count := fmt.Sprintf(" %d/%d", m.cursor+1, len(agents))
		heading = clip(heading, width-lipgloss.Width(count))
		heading += strings.Repeat(" ", max(width-lipgloss.Width(heading)-lipgloss.Width(count), 0)) + count
	}
	rows := []string{dimStyle.Render(pad(heading, width))}
	end := min(m.offset+m.listRows(), len(agents))
	for i := m.offset; i < end; i++ {
		a := agents[i]
		cells := agentCells{agent: a.Agent, status: statusGlyphs[a.Status] + " " + a.Status.Word(), job: "-", input: "-", since: "-", doing: a.Doing}
		if a.DispatchUnknown {
			cells.status += " (unknown)"
		}
		if a.Job != 0 {
			cells.job = fmt.Sprintf("#%d", a.Job)
		}
		if a.Input != "" {
			cells.input = a.Input
		}
		if !a.Since.IsZero() {
			cells.since = age(m.factory.readAt.Sub(a.Since))
		}
		if i == m.cursor {
			rows = append(rows, selectedStyle.Render(pad(m.agentRow(c, cells, lipgloss.NewStyle()), width)))
			continue
		}
		rows = append(rows, m.agentRow(c, cells, statusStyles[a.Status]))
	}
	return rows
}

// agentRow lays out one row in the columns that fit, with the status cell in
// style, and pads it to the table's width.
func (m model) agentRow(c agentColumns, cells agentCells, status lipgloss.Style) string {
	var b strings.Builder
	b.WriteString(" ")
	b.WriteString(pad(cells.agent, c.agent))
	b.WriteString(" ")
	b.WriteString(status.Render(pad(cells.status, statusWidth)))
	b.WriteString(" ")
	for _, col := range []struct {
		on    bool
		text  string
		width int
	}{{c.job, cells.job, jobWidth}, {c.input, cells.input, inputWidth}, {c.since, cells.since, sinceWidth}} {
		if col.on {
			b.WriteString(pad(col.text, col.width))
			b.WriteString(" ")
		}
	}
	b.WriteString(clip(cells.doing, c.doing))
	return b.String()
}

// age is how long ago a status began, in the coarsest unit that is not zero.
func age(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "<1m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
}
