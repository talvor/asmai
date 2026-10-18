// SPDX-License-Identifier: Apache-2.0

package dashboard

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/talvor/asmai/internal/daemon"
)

// The Log tab's columns. The summary always stays and takes what is left; the
// time drops first and then from-to collapses to the sender as the width
// shrinks.
const (
	timeWidth  = 8
	minSummary = 28
	// partyWidthMax caps each of the two agents of from-to, which is as wide
	// as the longest address loaded up to it.
	partyWidthMax = 24
	partyWidthMin = 6
	arrow         = " → "
)

const (
	// logLoad is how many entries the Log tab reads on open and for each
	// older page: the latest few hundred.
	logLoad = 300
	// pageMargin is how close to the oldest loaded entry the cursor gets
	// before an older page is read.
	pageMargin = 20
)

// logState is the Log tab: a window of the journal that grows at both ends,
// and the entry opened from it.
type logState struct {
	// entries are the loaded entries, oldest first, with whether more lie
	// before and after them.
	entries      []daemon.LogEntry
	older, newer bool
	// loaded is true once the first page of the current view has arrived.
	loaded bool
	// loading is true while a page of older entries is being read.
	loading bool
	// cursor is the selected entry and offset the first one the list shows.
	cursor, offset int
	// full shows the whole journal instead of agent traffic.
	full bool
	// job and agent narrow the view when set.
	job   int64
	agent string
	// view numbers the views (a mode or a filter), so a page read for an
	// earlier one is dropped.
	view int
	// want is the journal ID of the entry the user asked to open. The entry
	// opens when the daemon has answered, so the list gives way to it in one
	// redraw. While open, entry is what the daemon answered, failure why it
	// could not, and scroll how far down the body is.
	want    int64
	open    bool
	id      int64
	entry   *daemon.DashboardEntry
	failure string
	scroll  int
}

// pageMode says where a page of the journal goes.
type pageMode int

const (
	pageLatest pageMode = iota
	pageOlder
	pageNewer
)

// logMsg is the daemon's answer to a log read.
type logMsg struct {
	view int
	mode pageMode
	page daemon.DashboardLog
	err  error
}

// entryMsg is the daemon's answer to an entry read.
type entryMsg struct {
	view  int
	id    int64
	entry daemon.DashboardEntry
	err   error
}

// logQuery is what the current view reads.
func (m model) logQuery() daemon.LogQuery {
	q := daemon.LogQuery{Job: m.log.job, Agent: m.log.agent, Limit: logLoad}
	if !m.log.full {
		q.Kinds = daemon.TrafficKinds
	}
	return q
}

// readLog reads a page of the journal.
func readLog(socket string, view int, mode pageMode, q daemon.LogQuery) tea.Cmd {
	return func() tea.Msg {
		resp, err := daemon.Call(socket, daemon.Request{Command: daemon.CommandDashboardLog, Log: &q})
		if err == nil && resp.Log == nil {
			err = fmt.Errorf("the daemon answered without a log")
		}
		if err != nil {
			return logMsg{view: view, mode: mode, err: err}
		}
		return logMsg{view: view, mode: mode, page: *resp.Log}
	}
}

// readEntry reads one journal entry with its body.
func readEntry(socket string, view int, id int64) tea.Cmd {
	return func() tea.Msg {
		resp, err := daemon.Call(socket, daemon.Request{Command: daemon.CommandDashboardEntry, Entry: id})
		if err == nil && resp.LogEntry == nil {
			err = fmt.Errorf("the daemon answered without an entry")
		}
		if err != nil {
			return entryMsg{view: view, id: id, err: err}
		}
		return entryMsg{view: view, id: id, entry: *resp.LogEntry}
	}
}

// refreshLog reads what the Log tab lacks: the latest entries when it has
// none, else the entries newer than its newest.
func (m model) refreshLog() tea.Cmd {
	if m.tab != tabLog || !m.read || !m.factory.running {
		return nil
	}
	q := m.logQuery()
	if !m.log.loaded || len(m.log.entries) == 0 {
		return readLog(m.socket, m.log.view, pageLatest, q)
	}
	q.After = m.log.entries[len(m.log.entries)-1].ID
	return readLog(m.socket, m.log.view, pageNewer, q)
}

// newView starts the Log tab's view over, as its mode or filter changed, and
// reads the latest entries for it.
func (m model) newView() (model, tea.Cmd) {
	m.log = logState{full: m.log.full, job: m.log.job, agent: m.log.agent, view: m.log.view + 1}
	return m, m.refreshLog()
}

// pageOlder reads older entries when the cursor nears the oldest loaded.
func (m model) pageOlder() (model, tea.Cmd) {
	if m.tab != tabLog || !m.log.older || m.log.loading || len(m.log.entries) == 0 || m.log.cursor >= pageMargin {
		return m, nil
	}
	m.log.loading = true
	q := m.logQuery()
	q.Before = m.log.entries[0].ID
	return m, readLog(m.socket, m.log.view, pageOlder, q)
}

// logPage folds a page the daemon sent into the Log tab.
func (m model) logPage(msg logMsg) (model, tea.Cmd) {
	if msg.view != m.log.view {
		return m, nil
	}
	l := &m.log
	if msg.mode == pageOlder {
		l.loading = false
	}
	if msg.err != nil {
		return m, nil
	}
	page := msg.page
	switch msg.mode {
	case pageLatest:
		l.entries, l.older, l.newer, l.loaded = page.Entries, page.Older, page.Newer, true
		l.cursor = max(len(l.entries)-1, 0)
	case pageOlder:
		// Entries may have been read twice if two reads were in flight.
		fresh := page.Entries
		if len(l.entries) > 0 {
			fresh = slices.DeleteFunc(slices.Clone(fresh), func(e daemon.LogEntry) bool { return e.ID >= l.entries[0].ID })
		}
		l.entries = append(slices.Clone(fresh), l.entries...)
		l.cursor += len(fresh)
		l.offset += len(fresh)
		l.older = page.Older
	case pageNewer:
		fresh := page.Entries
		if len(l.entries) > 0 {
			fresh = slices.DeleteFunc(slices.Clone(fresh), func(e daemon.LogEntry) bool { return e.ID <= l.entries[len(l.entries)-1].ID })
		}
		following := l.cursor >= len(l.entries)-1
		l.entries = append(slices.Clone(l.entries), fresh...)
		l.newer = page.Newer
		if following {
			l.cursor = len(l.entries) - 1
		}
		m = m.scrolled()
		if page.Newer && len(fresh) > 0 {
			return m, m.refreshLog()
		}
	}
	m = m.scrolled()
	return m.pageOlder()
}

// selectedEntry is the entry the cursor is on, if any.
func (m model) selectedEntry() (daemon.LogEntry, bool) {
	if m.log.cursor < len(m.log.entries) {
		return m.log.entries[m.log.cursor], true
	}
	return daemon.LogEntry{}, false
}

// openEntry opens the selected entry with its body.
func (m model) openEntry() (model, tea.Cmd) {
	e, ok := m.selectedEntry()
	if !ok {
		return m, nil
	}
	m.log.want = e.ID
	return m, readEntry(m.socket, m.log.view, e.ID)
}

// filtered cycles the filter: to the selected entry's job, then its sender,
// then none.
func (m model) filtered() (model, tea.Cmd) {
	e, ok := m.selectedEntry()
	switch {
	case m.log.job != 0:
		m.log.job, m.log.agent = 0, ""
		if ok && e.From != "" {
			m.log.agent = e.From
		}
	case m.log.agent != "":
		m.log.job, m.log.agent = 0, ""
	case ok && e.Job != 0:
		m.log.job = e.Job
	case ok && e.From != "":
		m.log.agent = e.From
	default:
		return m, nil
	}
	return m.newView()
}

// logKey handles a key on the Log tab's list.
func (m model) logKey(key string) (model, tea.Cmd, bool) {
	switch key {
	case "enter":
		m, cmd := m.openEntry()
		return m, cmd, true
	case "t":
		m.log.full = !m.log.full
		m, cmd := m.newView()
		return m, cmd, true
	case "f":
		m, cmd := m.filtered()
		return m, cmd, true
	}
	return m, nil, false
}

// entryKey handles a key while an entry is open.
func (m model) entryKey(key string) model {
	rows := m.entryRows()
	switch key {
	case "esc":
		m.log.open = false
	case "up", "k":
		m.log.scroll--
	case "down", "j":
		m.log.scroll++
	case "pgup":
		m.log.scroll -= rows
	case "pgdown":
		m.log.scroll += rows
	case "home", "g":
		m.log.scroll = 0
	case "end", "G":
		m.log.scroll = len(m.entryLines())
	}
	m.log.scroll = min(max(m.log.scroll, 0), max(len(m.entryLines())-rows, 0))
	return m
}

// logColumns says which columns fit in width.
type logColumns struct {
	time bool
	// from and to are the widths of the two agents; to is zero when from-to
	// has collapsed to the sender.
	from, to int
	summary  int
}

func (m model) logColumns() logColumns {
	width := min(m.width, maxWidth)
	c := logColumns{from: partyWidthMin, to: partyWidthMin}
	for _, e := range m.log.entries {
		c.from = max(c.from, min(lipgloss.Width(e.From), partyWidthMax))
		c.to = max(c.to, min(lipgloss.Width(e.To), partyWidthMax))
	}
	used := 1 + c.from + lipgloss.Width(arrow) + c.to + 1 // gutter, from-to
	if used+timeWidth+1+minSummary <= width {
		c.time = true
		used += timeWidth + 1
	} else if used+minSummary > width {
		// From-to collapses to the sender, which then gives way to the summary.
		c.to = 0
		c.from = min(c.from, max(width-2-minSummary, partyWidthMin))
		used = 1 + c.from + 1
	}
	c.summary = max(width-used, 0)
	return c
}

// logRow lays out one entry in the columns that fit.
func (m model) logRow(c logColumns, at, from, to, summary string) string {
	var b strings.Builder
	b.WriteString(" ")
	if c.time {
		b.WriteString(pad(at, timeWidth))
		b.WriteString(" ")
	}
	if c.to > 0 {
		b.WriteString(pad(from, c.from))
		b.WriteString(arrow)
		b.WriteString(pad(to, c.to))
	} else {
		b.WriteString(pad(from, c.from))
	}
	b.WriteString(" ")
	b.WriteString(clip(summary, c.summary))
	return b.String()
}

// viewWords says what the Log tab shows: agent traffic or the whole journal,
// and the filter on it.
func (m model) viewWords() string {
	words := "agent traffic"
	if m.log.full {
		words = "full journal"
	}
	switch {
	case m.log.job != 0:
		words += fmt.Sprintf(", job #%d", m.log.job)
	case m.log.agent != "":
		words += ", " + m.log.agent
	}
	return words
}

// logBody draws the Log tab: a row of column names that says what is shown,
// and the entries the cursor's window shows.
func (m model) logBody() []string {
	if m.log.open {
		return m.entryBody()
	}
	c := m.logColumns()
	width := min(m.width, maxWidth)
	title := "SUMMARY (" + m.viewWords() + ")"
	if m.log.older {
		title += " ↑ older"
	}
	heading := m.logRow(c, "TIME", "FROM", "TO", title)
	if !m.log.loaded {
		// The columns are not known until the first page is.
		return nil
	}
	rows := []string{dimStyle.Render(pad(heading, width))}
	if len(m.log.entries) == 0 {
		empty := " No agent traffic."
		if m.log.full {
			empty = " No journal entries."
		}
		if m.log.job != 0 || m.log.agent != "" {
			empty = " No matching entries."
		}
		return append(rows, pad(empty, width))
	}
	end := min(m.log.offset+m.listRows(), len(m.log.entries))
	for i := m.log.offset; i < end; i++ {
		e := m.log.entries[i]
		from, to := e.From, e.To
		if from == "" {
			from = "-"
		}
		if to == "" {
			to = "-"
		}
		row := m.logRow(c, e.At.Local().Format("15:04:05"), from, to, e.Summary)
		if i == m.log.cursor {
			row = selectedStyle.Render(pad(row, width))
		}
		rows = append(rows, row)
	}
	return rows
}

// entryRows is how many lines of an entry's body show.
func (m model) entryRows() int { return max(m.height-chromeRows-3, 1) }

// entryLines are the lines of the open entry's body, wrapped to the width.
func (m model) entryLines() []string {
	switch {
	case m.log.failure != "":
		return []string{"Could not read the entry: " + m.log.failure}
	case m.log.entry == nil:
		return nil
	}
	text := m.log.entry.Body
	if strings.TrimSpace(text) == "" {
		text = "(this entry has no message body)\n\n" + dataLines(m.log.entry.Data)
	}
	wrapped := lipgloss.NewStyle().Width(max(min(m.width, maxWidth)-2, 1)).Render(text)
	return strings.Split(strings.TrimRight(wrapped, "\n"), "\n")
}

// dataLines shows an entry's data as one key and value to a line.
func dataLines(data json.RawMessage) string {
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		return string(data)
	}
	var b strings.Builder
	for _, k := range slices.Sorted(maps.Keys(fields)) {
		encoded, _ := json.Marshal(fields[k])
		if s, ok := fields[k].(string); ok {
			encoded = []byte(s)
		}
		fmt.Fprintf(&b, "%s: %s\n", k, encoded)
	}
	return b.String()
}

// entryBody draws the open entry: what it is, who it runs between, and its
// body scrolled to where the user has read.
func (m model) entryBody() []string {
	width := min(m.width, maxWidth)
	e := m.log.entry
	title, meta := fmt.Sprintf(" Entry #%d", m.log.id), ""
	if e != nil {
		title += " · " + e.Kind
		meta = " " + e.At.Local().Format("2006-01-02 15:04:05")
		if e.Job != 0 {
			meta += fmt.Sprintf(" · job #%d", e.Job)
		}
		switch {
		case e.From != "" && e.To != "":
			meta += " · " + e.From + arrow + e.To
		case e.From != "":
			meta += " · " + e.From
		}
	}
	rows := []string{dimStyle.Render(pad(title, width)), dimStyle.Render(pad(meta, width)), ""}
	lines := m.entryLines()
	end := min(m.log.scroll+m.entryRows(), len(lines))
	for _, line := range lines[min(m.log.scroll, end):end] {
		rows = append(rows, pad(" "+line, width))
	}
	return rows
}
