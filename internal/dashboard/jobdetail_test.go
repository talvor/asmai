// SPDX-License-Identifier: Apache-2.0

package dashboard

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/daemon"
)

func TestJobDetailFailedRefreshShowsTheLastGoodRead(t *testing.T) {
	for _, columns := range []int{80, 100} {
		for _, rows := range []int{24, 30} {
			t.Run(fmt.Sprintf("%dx%d", columns, rows), func(t *testing.T) {
				m := newModel("", Options{})
				m.width, m.height = columns, rows
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
}

func TestJobDetailKeepsLateSuccessfulReadsAfterNewerFailures(t *testing.T) {
	m := newModel("", Options{})
	m.width, m.height = 80, 24
	m.read, m.factory.running, m.tab = true, true, tabJobs
	m.detail = jobDetail{open: true, job: 12, view: 1, seq: 6}
	good := func(title string) daemon.DashboardJobDetail {
		return daemon.DashboardJobDetail{Job: 12, Title: title, Stage: daemon.StageWorking, CI: "not observed", Validation: "not started"}
	}
	m = m.jobRead(jobMsg{view: 1, seq: 2, err: errors.New("temporary failure")})
	m = m.jobRead(jobMsg{view: 1, seq: 1, detail: good("first read")})
	if m.detail.data == nil || m.detail.data.Title != "first read" || !strings.Contains(m.render(), "Refresh failed; last updated ") {
		t.Errorf("a late initial read did not keep the newer failure visible:\n%s", m.render())
	}
	m = m.jobRead(jobMsg{view: 1, seq: 3, detail: good("third read")})
	if m.detail.data == nil || m.detail.data.Title != "third read" || m.detail.failure != "" {
		t.Errorf("a newer successful read did not clear the failure: %+v", m.detail)
	}
	m = m.jobRead(jobMsg{view: 1, seq: 5, err: errors.New("another failure")})
	m = m.jobRead(jobMsg{view: 1, seq: 4, detail: good("fourth read")})
	if m.detail.data == nil || m.detail.data.Title != "fourth read" || !strings.Contains(m.render(), "another failure") {
		t.Errorf("a late refresh lost its data or hid the newer failure:\n%s", m.render())
	}
	m = m.jobRead(jobMsg{view: 1, seq: 3, detail: good("stale read")})
	if m.detail.data.Title != "fourth read" {
		t.Errorf("an older successful read replaced the detail: %+v", m.detail.data)
	}
	m = m.jobRead(jobMsg{view: 1, seq: 6, detail: good("current read")})
	if m.detail.data.Title != "current read" || m.detail.failure != "" {
		t.Errorf("the current read did not clear the failure: %+v", m.detail)
	}
}

func TestSelectedHandoffShowsItsStoredRecord(t *testing.T) {
	m := newModel("", Options{Columns: 160, Rows: 40})
	item := daemon.JobItem{
		Kind: daemon.ItemHandoff, ID: 7, Label: "accepted", Text: "Build the feature",
		From: "leader@coordination", To: "leader@engineering", Head: "1111111111111111111111111111111111111111",
		Decisions: "Use the fixture", Evidence: "The user asked for it", Constraints: "Stay on the branch",
		Permissions: "Read the repository", Criteria: []string{"tests pass", "no side effects"}, Answer: "Accepted",
	}
	m.detail = jobDetail{open: true, data: &daemon.DashboardJobDetail{Work: []daemon.JobItem{item}}, readAt: time.Now()}
	lines := strings.Join(m.itemLines(), "\n")
	for _, want := range []string{
		"leader@coordination", "leader@engineering", "1111111111111111111111111111111111111111",
		"Request:", "Build the feature", "Decisions:", "Use the fixture", "Evidence:", "The user asked for it",
		"Constraints:", "Stay on the branch", "Permissions:", "Read the repository", "Acceptance:",
		"- tests pass", "- no side effects", "Answer:", "Accepted",
	} {
		if !strings.Contains(lines, want) {
			t.Errorf("the selected handoff omitted %q:\n%s", want, lines)
		}
	}
}
