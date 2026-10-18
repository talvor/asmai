// SPDX-License-Identifier: Apache-2.0

package dashboard

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/talvor/asmai/internal/daemon"
)

// As the width shrinks the time drops first, then from-to collapses to the
// sender; the summary keeps at least its minimum.
func TestLogColumnsDropTheTimeThenCollapseFromTo(t *testing.T) {
	entries := []daemon.LogEntry{{From: "leader@engineering", To: "worker-3@engineering", Summary: "assignment 21"}}
	for _, tt := range []struct {
		width      int
		time, pair bool
	}{
		{160, true, true},
		{100, true, true},
		{80, true, true},
		{79, false, true},
		{71, false, true},
		{70, false, false},
		{40, false, false},
	} {
		m := model{width: tt.width, height: 30, log: logState{entries: entries}}
		c := m.logColumns()
		if c.time != tt.time || (c.to > 0) != tt.pair {
			t.Errorf("at %d columns: time %v from-to %v, want %v and %v", tt.width, c.time, c.to > 0, tt.time, tt.pair)
		}
		if c.summary < minSummary {
			t.Errorf("at %d columns the summary is %d wide, want at least %d", tt.width, c.summary, minSummary)
		}
	}
}

func TestLateLatestPageDoesNotReplaceOlderPage(t *testing.T) {
	m := model{tab: tabLog, width: 100, height: 30, log: logState{view: 1}}
	latest := logMsg{view: 1, mode: pageLatest, page: daemon.DashboardLog{Entries: []daemon.LogEntry{{ID: 3}, {ID: 4}}, Older: true}}
	m, _ = m.logPage(latest)
	m.log.cursor = 0
	m, _ = m.logPage(logMsg{view: 1, mode: pageOlder, page: daemon.DashboardLog{Entries: []daemon.LogEntry{{ID: 1}, {ID: 2}}}})
	m, _ = m.logPage(latest)
	if len(m.log.entries) != 4 || m.log.entries[0].ID != 1 || m.log.cursor != 2 || m.log.older {
		t.Errorf("late latest reply changed the paged view: %+v", m.log)
	}
}

func TestEntryReplyAfterLeavingLogDoesNotReopen(t *testing.T) {
	m := model{tab: tabLog, width: 100, height: 30, log: logState{want: 7, view: 1, loaded: true, entries: []daemon.LogEntry{{ID: 7}}}}
	for _, key := range []rune{'1', '2'} {
		m.tab, m.log.want = tabLog, 7
		updated, _ := m.Update(tea.KeyPressMsg(tea.Key{Code: key, Text: string(key)}))
		m = updated.(model)
		updated, _ = m.Update(tea.KeyPressMsg(tea.Key{Code: '3', Text: "3"}))
		m = updated.(model)
		updated, _ = m.Update(entryMsg{view: 1, id: 7, entry: daemon.DashboardEntry{LogEntry: daemon.LogEntry{ID: 7}}})
		m = updated.(model)
		if m.log.open || m.log.want != 0 {
			t.Errorf("reply reopened entry after tab %c: %+v", key, m.log)
		}
	}
	m.log.open, m.log.want = true, 7
	m = m.entryKey("esc")
	updated, _ := m.Update(entryMsg{view: 1, id: 7})
	m = updated.(model)
	if m.log.open {
		t.Error("reply reopened entry after Esc")
	}
}

func TestEmptyEntryShowsOnlyBodyPlaceholder(t *testing.T) {
	m := model{width: 100, height: 30, log: logState{entry: &daemon.DashboardEntry{}}}
	lines := m.entryLines()
	if len(lines) != 1 || strings.TrimSpace(lines[0]) != "(this entry has no message body)" {
		t.Errorf("empty body lines are %q", lines)
	}
}
