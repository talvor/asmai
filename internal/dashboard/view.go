// SPDX-License-Identifier: Apache-2.0

package dashboard

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
)

type tab int

const (
	tabAgents tab = iota
	tabJobs
	tabLog
)

var tabNames = [...]string{"1 Agents", "2 Jobs", "3 Log"}

// Styles never rely on colour alone: selection is reverse video, and the
// colour profile Bubble Tea detects (NO_COLOR, a monochrome TERM) strips the
// rest.
var (
	barStyle      = lipgloss.NewStyle().Reverse(true)
	selectedStyle = lipgloss.NewStyle().Reverse(true)
	dimStyle      = lipgloss.NewStyle().Faint(true)
)

const footerKeys = "1/2/3 tab · ↑/↓ move · q quit"

// render draws the whole screen: exactly height lines of at most width cells.
func (m model) render() string {
	if m.width <= 0 || m.height <= 0 {
		return ""
	}
	if m.width < MinColumns || m.height < MinRows {
		return m.frame(m.tooSmall())
	}
	if !m.read {
		return m.frame(nil)
	}
	if !m.factory.running {
		return m.frame(m.stopped())
	}
	return m.shell()
}

// frame puts body above the footer, padded to the height of the screen.
func (m model) frame(body []string) string {
	rows := make([]string, 0, m.height)
	rows = append(rows, body...)
	for len(rows) < m.height-1 {
		rows = append(rows, "")
	}
	rows = append(rows[:m.height-1], m.footer())
	return strings.Join(rows, "\n")
}

func (m model) footer() string {
	keys := footerKeys
	switch {
	case m.tab == tabAgents:
		keys = "1/2/3 tab · ↑/↓ move · enter attach · q quit"
	case m.tab == tabJobs && m.detail.open:
		keys = m.detailFooter()
	case m.tab == tabJobs:
		keys = "1/2/3 tab · ↑/↓ move · enter open · q quit"
	case m.tab == tabLog && m.log.open:
		keys = "1/2/3 tab · ↑/↓ scroll · esc back · q quit"
	case m.tab == tabLog && m.log.full:
		keys = "1/2/3 tab · ↑/↓ move · enter open · t traffic · f filter · q quit"
	case m.tab == tabLog:
		keys = "1/2/3 tab · ↑/↓ move · enter open · t journal · f filter · q quit"
	}
	return bar(" "+keys, m.width)
}

// tooSmall is the only thing shown below the minimum size.
func (m model) tooSmall() []string {
	return []string{clip(fmt.Sprintf("terminal too small: needs %dx%d, has %dx%d", MinColumns, MinRows, m.width, m.height), m.width)}
}

// stopped is the only thing shown while the daemon is down: no stale data.
func (m model) stopped() []string {
	rows := []string{bar(" asmai dashboard  "+"factory stopped", m.width), ""}
	rows = append(rows,
		pad("The factory is not running.", m.width),
		pad("Nothing is shown. Start it with `asmai start`.", m.width),
		pad("Looking again every few seconds.", m.width),
	)
	return rows
}

// shell is the four chrome rows around the selected tab's body: the header,
// the tabs, the Waiting on you banner and the footer of keys.
func (m model) shell() string {
	body := []string{m.header(), m.tabs(), m.banner()}
	switch m.tab {
	case tabAgents:
		body = append(body, m.agentsBody()...)
	case tabJobs:
		if m.detail.open {
			body = append(body, m.detailBody()...)
			break
		}
		body = append(body, m.jobsBody()...)
	case tabLog:
		body = append(body, m.logBody()...)
	}
	return m.frame(body)
}

func (m model) header() string {
	left := " asmai dashboard  factory running"
	if m.failed {
		// Only a failed refresh says how old the screen is.
		left += "  ·  refresh failed, last updated " + m.factory.readAt.Format("15:04:05")
	}
	return bar(left, m.width)
}

func (m model) tabs() string {
	var b strings.Builder
	b.WriteString(" ")
	for i, name := range tabNames {
		cell := " " + name + " "
		if tab(i) == m.tab {
			cell = selectedStyle.Render(cell)
		} else {
			cell = dimStyle.Render(cell)
		}
		b.WriteString(cell)
		b.WriteString(" ")
	}
	return b.String()
}

// bannerStyle is the colour of the banner's text; its glyph and words carry
// the meaning without it.
var bannerStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("3"))

// banner is the Waiting on you row: the first thing the factory cannot
// continue without the user, and "+N more" for the rest. It is empty when
// nothing waits.
func (m model) banner() string {
	items := m.factory.waiting
	if len(items) == 0 {
		return ""
	}
	more := ""
	if len(items) > 1 {
		more = fmt.Sprintf(" · +%d more", len(items)-1)
	}
	text := clip(" ▲ Waiting on you: "+items[0].Text, max(m.width-lipgloss.Width(more), 0))
	return bannerStyle.Render(pad(text+more, m.width))
}

// bar draws s across the whole width in reverse video.
func bar(s string, width int) string { return barStyle.Render(pad(s, width)) }

// pad clips s to width cells and fills the rest with spaces.
func pad(s string, width int) string {
	s = clip(s, width)
	if w := lipgloss.Width(s); w < width {
		s += strings.Repeat(" ", width-w)
	}
	return s
}

// clip cuts s to width cells with an ellipsis.
func clip(s string, width int) string {
	if lipgloss.Width(s) <= width {
		return s
	}
	runes := []rune(s)
	for len(runes) > 0 && lipgloss.Width(string(runes))+1 > width {
		runes = runes[:len(runes)-1]
	}
	return string(runes) + "…"
}
