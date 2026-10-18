// SPDX-License-Identifier: Apache-2.0

package dashboard

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/talvor/asmai/internal/daemon"
)

func TestJobDetailFailedRefreshShowsTheLastGoodRead(t *testing.T) {
	for _, columns := range []int{80, 100} {
		t.Run(fmt.Sprint(columns), func(t *testing.T) {
			m := newModel("", Options{})
			m.width, m.height = columns, 30
			m.read, m.factory.running, m.tab = true, true, tabJobs
			m.detail = jobDetail{open: true, job: 12, view: 1}
			good := daemon.DashboardJobDetail{
				Job: 12, Title: "Current job title", Stage: daemon.StageWorking,
				CI: "not observed", Validation: "not started",
				Work: []daemon.JobItem{{Kind: daemon.ItemFinding, ID: 3, Label: "needs-you", Text: "Current finding"}},
			}
			m = m.jobRead(jobMsg{view: 1, seq: 1, detail: good})
			readAt := m.detail.readAt
			m = m.jobRead(jobMsg{view: 1, seq: 2, err: errors.New("connection lost")})
			screen := m.render()
			for _, want := range []string{"Refresh failed; last updated " + readAt.Format("15:04:05"), "connection lost", "Current job title", "Current finding"} {
				if !strings.Contains(screen, want) {
					t.Errorf("the failed refresh hides %q:\n%s", want, screen)
				}
			}
			if !m.detail.readAt.Equal(readAt) {
				t.Error("a failed refresh changed the last successful read time")
			}
			m = m.jobRead(jobMsg{view: 1, seq: 3, detail: good})
			if screen := m.render(); strings.Contains(screen, "Refresh failed") {
				t.Errorf("a successful refresh left the failure visible:\n%s", screen)
			}
		})
	}
}
