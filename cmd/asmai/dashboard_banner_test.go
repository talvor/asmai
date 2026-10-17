// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/dashboard/dashtest"
)

// waitingAt is a Waiting on you set of every kind the daemon lists today.
func waitingAt(now time.Time) []daemon.WaitingItem {
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	return []daemon.WaitingItem{
		{Kind: daemon.WaitingNativePrompt, Agent: "worker-2@quality", Job: 12, Text: "worker-2@quality native prompt: Bash `gh pr diff 98`", Since: ago(4 * time.Minute)},
		{Kind: daemon.WaitingInput, Agent: "leader@coordination", Text: "leader@coordination you hold its input in /dev/pts/3", Since: ago(12 * time.Minute)},
		{Kind: daemon.WaitingFinding, Job: 12, Finding: 7, Text: "job 12 finding 7: the migration drops a column in use", Since: ago(time.Hour)},
	}
}

// The banner names the first item and "+N more", and is empty when nothing
// waits.
func TestDashboardBannerGoldenScreens(t *testing.T) {
	cases := []struct {
		name  string
		items func(time.Time) []daemon.WaitingItem
		want  string
	}{
		{"zero", func(time.Time) []daemon.WaitingItem { return nil }, ""},
		{"one", func(now time.Time) []daemon.WaitingItem { return waitingAt(now)[:1] }, "▲ Waiting on you: worker-2@quality native prompt"},
		{"several", waitingAt, "+2 more"},
	}
	for _, c := range cases {
		for _, size := range dashboardSizes {
			for _, env := range [][]string{nil, {"NO_COLOR=1"}} {
				name := fmt.Sprintf("banner-%s-%dx%d", c.name, size[0], size[1])
				if len(env) > 0 {
					name += "-no_color"
				}
				t.Run(name, func(t *testing.T) {
					fake := dashtest.NewFakeDaemon(t)
					fake.SetAgents(agentsAt(time.Now())...)
					fake.SetWaiting(c.items(time.Now())...)
					d := dashboardIn(t, fake, size[0], size[1], env...)
					screen := d.WaitForText("worker-2@quality")
					if c.want != "" && !strings.Contains(screen, c.want) {
						t.Errorf("the banner lacks %q:\n%s", c.want, screen)
					}
					if c.want == "" && strings.Contains(screen, "Waiting on you:") {
						t.Errorf("the banner shows with nothing waiting:\n%s", screen)
					}
					dashtest.Golden(t, name, d.Snapshot())
				})
			}
		}
	}
}

// The Dashboard shows the first item of the set as the daemon ordered it, and
// orders nothing itself.
func TestDashboardBannerShowsTheFirstItemTheDaemonListed(t *testing.T) {
	fake := dashtest.NewFakeDaemon(t)
	fake.SetAgents(agentsAt(time.Now())...)
	items := waitingAt(time.Now())
	fake.SetWaiting(items[2], items[0])
	d := dashboardIn(t, fake, 100, 30)
	screen := d.WaitForText("Waiting on you: job 12 finding 7")
	if !strings.Contains(screen, "+1 more") {
		t.Errorf("the banner lacks \"+1 more\":\n%s", screen)
	}
}
