// SPDX-License-Identifier: Apache-2.0

package dashboard

import (
	"testing"

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
