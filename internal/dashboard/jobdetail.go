// SPDX-License-Identifier: Apache-2.0

package dashboard

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/talvor/asmai/internal/daemon"
)

// The job detail's layout. The header is the title, the stage progress and
// three rows of facts; the Work list and the selected item share the rest,
// side by side from splitColumns wide and stacked below that.
const (
	splitColumns = 100
	// detailHeaderRows are the rows above the Work list, with a blank one.
	detailHeaderRows = 6
	// minItemRows is the least rows the stacked layout gives the Work list.
	minItemRows = 2
	// itemAgeWidth is the column of the time an item last changed.
	itemAgeWidth = 4
	// labelWidthMin and labelWidthMax bound the column that says what sort
	// of item a row is.
	labelWidthMin = 8
	labelWidthMax = 20
)

// progress is the stages a job passes through, in order; the stage the daemon
// derived says how far it is.
var progress = []string{"Handoff", "Working", "Validating", "Delivering", "Delivered"}

// progressAt is the index in progress of the stage a job is at. A job that
// needs the user is still being worked on.
func progressAt(stage daemon.JobStage, handoffComplete bool) int {
	if !handoffComplete {
		return 0
	}
	switch stage {
	case daemon.StageValidating:
		return 2
	case daemon.StageDelivering:
		return 3
	case daemon.StageDelivered:
		return len(progress)
	default:
		return 1
	}
}

// jobDetail is the job detail: one job of the Jobs tab opened, with what the
// daemon last said of it.
type jobDetail struct {
	open bool
	job  int64
	// view numbers openings, seq reads, applied answers, and loaded good reads.
	view, seq, applied, loaded int
	// data is the daemon's answer, nil until it arrives; failure says why a
	// read failed.
	data    *daemon.DashboardJobDetail
	failure string
	// readAt is when data was read, which the time each item last changed is
	// counted back from.
	readAt time.Time
	// cursor is the selected item and offset the first one the list shows;
	// scroll is how far down the selected item is read.
	cursor, offset, scroll int
}

// jobMsg is the daemon's answer to a job detail read.
type jobMsg struct {
	view, seq int
	detail    daemon.DashboardJobDetail
	err       error
}

// readJob asks the daemon for the detail of job.
func readJob(socket string, view, seq int, job int64) tea.Cmd {
	return func() tea.Msg {
		resp, err := daemon.Call(socket, daemon.Request{Command: daemon.CommandDashboardJob, Job: job})
		if err == nil && resp.JobDetail == nil {
			err = fmt.Errorf("the daemon answered without a job detail")
		}
		if err != nil {
			return jobMsg{view: view, seq: seq, err: err}
		}
		return jobMsg{view: view, seq: seq, detail: *resp.JobDetail}
	}
}

// usable is whether the screen shows anything but the stopped and too-small
// messages.
func (m model) usable() bool {
	return m.read && m.factory.running && m.width >= MinColumns && m.height >= MinRows
}

// openDetail opens the job detail of the job the Jobs tab's cursor is on.
func (m model) openDetail() (model, tea.Cmd) {
	job := m.selectedJob()
	if m.tab != tabJobs || job == 0 || !m.usable() {
		return m, nil
	}
	m.detail = jobDetail{open: true, job: job, view: m.detail.view + 1, seq: 1}
	return m, readJob(m.socket, m.detail.view, m.detail.seq, job)
}

// refreshDetail reads the open job detail again.
func (m model) refreshDetail() (model, tea.Cmd) {
	if !m.detail.open || !m.read || !m.factory.running {
		return m, nil
	}
	m.detail.seq++
	return m, readJob(m.socket, m.detail.view, m.detail.seq, m.detail.job)
}

// closeDetail returns to the Jobs list.
func (m model) closeDetail() model {
	m.detail = jobDetail{view: m.detail.view}
	return m
}

// jobRead folds the daemon's answer into the job detail, keeping the cursor
// on its item.
func (m model) jobRead(msg jobMsg) model {
	d := &m.detail
	if !d.open || msg.view != d.view {
		return m
	}
	if msg.err != nil {
		if msg.seq <= d.applied {
			return m
		}
		d.applied = msg.seq
		d.failure = msg.err.Error()
		return m
	}
	if msg.seq <= d.loaded {
		return m
	}
	d.loaded = msg.seq
	if msg.seq >= d.applied {
		d.applied = msg.seq
		d.failure = ""
	}
	var kind string
	var id int64
	if item, ok := m.selectedItem(); ok {
		kind, id = item.Kind, item.ID
	}
	detail := msg.detail
	d.data, d.readAt = &detail, time.Now()
	for i, item := range detail.Work {
		if item.Kind == kind && item.ID == id {
			d.cursor = i
		}
	}
	if kind == "" {
		d.cursor = 0
	}
	return m.detailScrolled()
}

// selectedItem is the item the job detail's cursor is on, if any.
func (m model) selectedItem() (daemon.JobItem, bool) {
	d := m.detail
	if d.data != nil && d.cursor < len(d.data.Work) {
		return d.data.Work[d.cursor], true
	}
	return daemon.JobItem{}, false
}

// detailLayout is where the Work list and the selected item go.
type detailLayout struct {
	split bool
	// listRows is how many items the list shows, itemRows how many lines of
	// the selected item show, and listWidth and itemWidth their widths.
	listRows, itemRows   int
	listWidth, itemWidth int
}

func (m model) detailLayout() detailLayout {
	width := min(m.width, maxWidth)
	headerRows := detailHeaderRows
	if m.detail.data != nil && m.detail.failure != "" {
		headerRows++
	}
	area := max(m.height-chromeRows-headerRows, 1)
	if m.width >= splitColumns {
		listWidth := width * 11 / 20
		return detailLayout{split: true, listRows: max(area-1, 1), itemRows: max(area-1, 1), listWidth: listWidth, itemWidth: max(width-listWidth-1, 0)}
	}
	list := max((area-2)/2, minItemRows)
	return detailLayout{listRows: list, itemRows: max(area-2-list, 1), listWidth: width, itemWidth: width}
}

// detailScrolled puts the Work list's window where its cursor is in it.
func (m model) detailScrolled() model {
	d := &m.detail
	n := 0
	if d.data != nil {
		n = len(d.data.Work)
	}
	rows := m.detailLayout().listRows
	d.cursor = min(max(d.cursor, 0), max(n-1, 0))
	d.offset = min(max(d.offset, d.cursor-rows+1), d.cursor)
	d.offset = max(min(d.offset, n-rows), 0)
	lines := len(m.itemLines())
	d.scroll = min(max(d.scroll, 0), max(lines-m.detailLayout().itemRows, 0))
	return m
}

// detailKey handles a key while the job detail is open. It returns false for
// the keys the rest of the Dashboard handles, after leaving the detail for the
// tabs' own keys.
func (m model) detailKey(key string) (model, tea.Cmd, bool) {
	d := &m.detail
	n := 0
	if d.data != nil {
		n = len(d.data.Work)
	}
	move := func(delta int) model {
		before := d.cursor
		d.cursor = min(max(d.cursor+delta, 0), max(n-1, 0))
		if d.cursor != before {
			d.scroll = 0
		}
		return m.detailScrolled()
	}
	switch key {
	case "esc":
		return m.closeDetail(), nil, true
	case "1", "2", "3":
		return m.closeDetail(), nil, false
	case "up", "k":
		return move(-1), nil, true
	case "down", "j":
		return move(1), nil, true
	case "home", "g":
		return move(-n), nil, true
	case "end", "G":
		return move(n), nil, true
	case "pgup":
		d.scroll -= m.detailLayout().itemRows
		return m.detailScrolled(), nil, true
	case "pgdown":
		d.scroll += m.detailLayout().itemRows
		return m.detailScrolled(), nil, true
	case "enter":
		// The same hand-over as the Agents tab's, for the assignment's worker.
		if item, ok := m.selectedItem(); ok && item.Kind == daemon.ItemAssignment {
			return m, m.attachTo(item.Worker), true
		}
		return m, nil, true
	case "l":
		// The Log, filtered to the job.
		job := d.job
		m = m.closeDetail()
		m.tab = tabLog
		m.log.job, m.log.agent, m.log.open, m.log.want = job, "", false, 0
		m, cmd := m.newView()
		return m, cmd, true
	}
	return m, nil, false
}

// detailFooter is the keys of the job detail.
func (m model) detailFooter() string {
	keys := "1/2/3 tab · ↑/↓ move · pgup/pgdn read · l log · esc back · q quit"
	if item, ok := m.selectedItem(); ok && item.Kind == daemon.ItemAssignment {
		keys = "1/2/3 tab · ↑/↓ move · pgup/pgdn read · enter attach · l log · esc back · q quit"
	}
	return keys
}

// itemKinds give each kind of item the letter its ID is shown with.
var itemKinds = map[string]string{daemon.ItemFinding: "F", daemon.ItemAssignment: "A", daemon.ItemHandoff: "H"}

// itemID is how an item is named in the list.
func itemID(item daemon.JobItem) string { return fmt.Sprintf("%s%d", itemKinds[item.Kind], item.ID) }

// itemStyles colour the rows of the Work list. Colour is only a second cue:
// a needs-you finding also has its glyph and its word.
var needsStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))

// detailBody draws the job detail: the header, then the Work list and the
// selected item.
func (m model) detailBody() []string {
	d := m.detail
	width := min(m.width, maxWidth)
	if d.data == nil {
		if d.failure != "" {
			return []string{"", pad(" Could not read job #"+fmt.Sprint(d.job)+": "+d.failure, width)}
		}
		return nil
	}
	rows := m.detailHeader()
	if d.failure != "" {
		rows = append(rows, pad(fmt.Sprintf(" Refresh failed; last updated %s: %s", d.readAt.Format("15:04:05"), d.failure), width))
	}
	rows = append(rows, "")
	l := m.detailLayout()
	list, item := m.workRows(l), m.itemRows(l)
	if l.split {
		for i := range max(len(list), len(item)) {
			left, right := pad("", l.listWidth), pad("", l.itemWidth)
			if i < len(list) {
				left = list[i]
			}
			if i < len(item) {
				right = item[i]
			}
			rows = append(rows, left+dimStyle.Render("│")+right)
		}
		return rows
	}
	rows = append(rows, list...)
	return append(rows, item...)
}

// detailHeader is the job's title, the progress of its stage and the facts
// the daemon gave: stage, branch, head, pull request, CI and validation.
func (m model) detailHeader() []string {
	d := m.detail.data
	width := min(m.width, maxWidth)
	head := d.Head[:min(len(d.Head), 7)]
	none := func(s string) string {
		if s == "" {
			return "-"
		}
		return s
	}
	pr := "-"
	if d.PullRequest != 0 {
		pr = fmt.Sprintf("#%d %s", d.PullRequest, d.PullRequestState)
	}
	return []string{
		boldStyle.Render(pad(fmt.Sprintf(" #%d  %s", d.Job, d.Title), width)),
		pad(" "+m.progressWords(d.Stage, d.HandoffComplete), width),
		pad(fmt.Sprintf(" Stage %s %s · Branch %s · Head %s", stageGlyphs[d.Stage], d.Stage.Word(), none(d.Branch), none(head)), width),
		pad(fmt.Sprintf(" Pull request %s · CI %s", pr, d.CI), width),
		pad(" Validation "+d.Validation, width),
	}
}

var boldStyle = lipgloss.NewStyle().Bold(true)

// progressWords is the stage progress indicator: a mark for each stage a job
// has passed, the one it is at and those to come. With no colour the marks and
// the words still tell them apart.
func (m model) progressWords(stage daemon.JobStage, handoffComplete bool) string {
	at := progressAt(stage, handoffComplete)
	var parts []string
	for i, name := range progress {
		switch {
		case i < at:
			parts = append(parts, "✓ "+name)
		case i == at:
			mark := "●"
			if stage == daemon.StageNeedsYou && at > 0 {
				mark = stageGlyphs[stage]
			}
			parts = append(parts, stageStyles[stage].Render(mark+" "+name))
		default:
			parts = append(parts, dimStyle.Render("○ "+name))
		}
	}
	return strings.Join(parts, " ── ")
}

// workColumns are the widths of the Work list's columns.
type workColumns struct{ id, label, text int }

func (m model) workColumns(l detailLayout) workColumns {
	c := workColumns{id: 4, label: labelWidthMin}
	for _, item := range m.detail.data.Work {
		c.id = max(c.id, lipgloss.Width(itemID(item)))
		c.label = min(max(c.label, lipgloss.Width(itemLabel(item))), labelWidthMax)
	}
	c.text = max(l.listWidth-(1+c.id+1+c.label+1+itemAgeWidth+1), 0)
	return c
}

// itemLabel is what sort of item a row is; a needs-you finding has its glyph.
func itemLabel(item daemon.JobItem) string {
	if item.Needs {
		return "▲ " + item.Label
	}
	return item.Label
}

// workRow lays out one row of the Work list.
func (m model) workRow(c workColumns, id, label, text, age string) string {
	return " " + pad(id, c.id) + " " + pad(label, c.label) + " " + pad(text, c.text) + " " + pad(age, itemAgeWidth)
}

// workRows draws the Work list: its heading and the items its window shows.
func (m model) workRows(l detailLayout) []string {
	d := m.detail
	c := m.workColumns(l)
	work := "WORK"
	if n := len(d.data.Work); n > l.listRows {
		work += fmt.Sprintf(" (%d/%d)", d.cursor+1, n)
	}
	heading := m.workRow(c, "ITEM", "STATE", work, "AGE")
	rows := []string{dimStyle.Render(pad(heading, l.listWidth))}
	if len(d.data.Work) == 0 {
		rows = append(rows, pad(" Nothing recorded for this job yet.", l.listWidth))
	}
	end := min(d.offset+l.listRows, len(d.data.Work))
	for i := d.offset; i < end; i++ {
		item := d.data.Work[i]
		text, _, _ := strings.Cut(strings.TrimSpace(item.Text), "\n")
		row := pad(m.workRow(c, itemID(item), itemLabel(item), text, age(d.readAt.Sub(item.At))), l.listWidth)
		switch {
		case i == d.cursor:
			row = selectedStyle.Render(row)
		case item.Needs:
			row = needsStyle.Render(row)
		}
		rows = append(rows, row)
	}
	if !l.split {
		// The stacked layout keeps the selected item at one place.
		for len(rows) < 1+l.listRows {
			rows = append(rows, pad("", l.listWidth))
		}
	}
	return rows
}

// itemLines are the lines of the selected item in full, wrapped to the width
// they are shown in: what it is, then what it says.
func (m model) itemLines() []string {
	item, ok := m.selectedItem()
	if !ok {
		return nil
	}
	width := max(m.detailLayout().itemWidth-2, 1)
	meta := []string{item.Label}
	var sections [][2]string
	switch item.Kind {
	case daemon.ItemFinding:
		sections = append(sections, [2]string{"Says", item.Text})
	case daemon.ItemAssignment:
		meta = append(meta, "worker "+item.Worker, "owner "+item.From)
		if item.Dispatch != "" {
			meta = append(meta, "dispatch "+item.Dispatch)
		}
		sections = append(sections, [2]string{"Outcome", item.Text})
		if len(item.Criteria) > 0 {
			sections = append(sections, [2]string{"Acceptance", "- " + strings.Join(item.Criteria, "\n- ")})
		}
	case daemon.ItemHandoff:
		meta = append(meta, item.From+arrow+item.To)
		if item.Head != "" {
			meta = append(meta, "head "+item.Head)
		}
		sections = append(sections, [2]string{"Request", item.Text})
		for _, field := range [][2]string{{"Decisions", item.Decisions}, {"Evidence", item.Evidence}, {"Constraints", item.Constraints}, {"Permissions", item.Permissions}} {
			if field[1] != "" {
				sections = append(sections, field)
			}
		}
		if len(item.Criteria) > 0 {
			sections = append(sections, [2]string{"Acceptance", "- " + strings.Join(item.Criteria, "\n- ")})
		}
		if item.Answer != "" {
			sections = append(sections, [2]string{"Answer", item.Answer})
		}
	}
	meta = append(meta, "changed "+age(m.detail.readAt.Sub(item.At))+" ago")
	wrap := func(s string) []string {
		return strings.Split(strings.TrimRight(lipgloss.NewStyle().Width(width).Render(s), "\n"), "\n")
	}
	lines := fieldLines(meta, width)
	for _, s := range sections {
		lines = append(lines, "", s[0]+":")
		lines = append(lines, wrap(s[1])...)
	}
	return lines
}

// itemRows draws the selected item: its heading and the lines its window
// shows.
func (m model) itemRows(l detailLayout) []string {
	item, ok := m.selectedItem()
	title := " SELECTED"
	if ok {
		title = fmt.Sprintf(" %s %d", strings.ToUpper(item.Kind[:1])+item.Kind[1:], item.ID)
	}
	lines := m.itemLines()
	if len(lines) > l.itemRows {
		title += fmt.Sprintf(" · line %d/%d", m.detail.scroll+1, len(lines))
	}
	rows := []string{dimStyle.Render(pad(title, l.itemWidth))}
	end := min(m.detail.scroll+l.itemRows, len(lines))
	for _, line := range lines[min(m.detail.scroll, end):end] {
		rows = append(rows, pad(" "+line, l.itemWidth))
	}
	for len(rows) < 1+l.itemRows {
		rows = append(rows, pad("", l.itemWidth))
	}
	return rows
}

// fieldLines puts fields on lines of at most width cells, joined by dots, and
// never breaks a field across two lines unless it is wider than a line.
func fieldLines(fields []string, width int) []string {
	var lines []string
	line := ""
	for _, f := range fields {
		switch {
		case line == "":
			line = f
		case lipgloss.Width(line+" · "+f) <= width:
			line += " · " + f
		default:
			lines = append(lines, line)
			line = f
		}
	}
	if line != "" {
		lines = append(lines, line)
	}
	var out []string
	for _, l := range lines {
		wrapped := lipgloss.NewStyle().Width(width).Render(l)
		out = append(out, strings.Split(strings.TrimRight(wrapped, "\n"), "\n")...)
	}
	return out
}
