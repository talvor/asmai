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

// agentsAt is the agents of a factory in every status, with the Since of each
// well inside a unit of age so that the screen holds still while a test runs.
func agentsAt(now time.Time) []daemon.DashboardAgent {
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	return []daemon.DashboardAgent{
		{Agent: "leader@coordination", Role: "coordination", Status: daemon.StatusWaitingOnYou, Input: daemon.InputUser, Since: ago(12*time.Minute + 30*time.Second), Doing: "you hold its input in /dev/pts/3"},
		{Agent: "leader@engineering", Role: "engineering", Status: daemon.StatusWorking, Job: 12, Input: daemon.InputAutomation, Since: ago(2*time.Minute + 30*time.Second), Doing: "handoff: Add the Agents tab to the dashboard so a user can see who is working and who waits on them"},
		{Agent: "leader@planning", Role: "planning", Status: daemon.StatusIdle, Input: daemon.InputAutomation, Since: ago(3*time.Hour + 30*time.Minute)},
		{Agent: "leader@quality", Role: "quality", Status: daemon.StatusIdle, DispatchUnknown: true, Input: daemon.InputAutomation, Since: ago(41*time.Minute + 30*time.Second), Doing: "dispatch state unknown"},
		{Agent: "leader@research", Role: "research", Status: daemon.StatusStopped, Since: ago(5*time.Hour + 30*time.Minute), Doing: "session ended: exit 1"},
		{Agent: "worker-2@quality", Role: "quality", Status: daemon.StatusWaitingOnYou, Job: 12, Input: daemon.InputAutomation, Since: ago(4*time.Minute + 30*time.Second), Doing: "native prompt: Bash `gh pr diff 98`"},
		{Agent: "worker-3@engineering", Role: "engineering", Status: daemon.StatusWorking, Job: 12, Input: daemon.InputAutomation, Since: ago(9*time.Minute + 30*time.Second), Doing: "assignment 32: add --after to the journal read"},
		{Agent: "worker-4@engineering", Role: "engineering", Status: daemon.StatusReconciliation, Job: 14, Input: daemon.InputAutomation, Since: ago(70 * time.Minute), Doing: "assignment 27: its dispatch's outcome is not known"},
	}
}

func TestDashboardAgentsGoldenScreens(t *testing.T) {
	for _, size := range dashboardSizes {
		for _, env := range [][]string{nil, {"NO_COLOR=1"}} {
			name := fmt.Sprintf("agents-%dx%d", size[0], size[1])
			if len(env) > 0 {
				name += "-no_color"
			}
			t.Run(name, func(t *testing.T) {
				fake := dashtest.NewFakeDaemon(t)
				fake.SetAgents(agentsAt(time.Now())...)
				d := dashboardIn(t, fake, size[0], size[1], env...)
				d.WaitForText("worker-2@quality")
				dashtest.Golden(t, name, d.Snapshot())
			})
		}
	}
}

// The columns drop as the width shrinks, and the doing column takes the rest.
func TestDashboardAgentsDropColumnsAsTheWidthShrinks(t *testing.T) {
	for _, tt := range []struct {
		columns int
		has     []string
		lacks   []string
	}{
		{160, []string{"AGENT", "STATUS", "JOB", "INPUT", "SINCE", "DOING"}, nil},
		{100, []string{"AGENT", "STATUS", "JOB", "INPUT", "SINCE", "DOING"}, nil},
		{90, []string{"AGENT", "STATUS", "JOB", "INPUT", "DOING"}, []string{"SINCE"}},
		{80, []string{"AGENT", "STATUS", "JOB", "DOING"}, []string{"SINCE", "INPUT"}},
	} {
		t.Run(fmt.Sprint(tt.columns), func(t *testing.T) {
			fake := dashtest.NewFakeDaemon(t)
			fake.SetAgents(agentsAt(time.Now())...)
			d := dashboardIn(t, fake, tt.columns, 30)
			heading := d.WaitForText("AGENT")
			var line string
			for _, l := range strings.Split(heading, "\n") {
				if strings.Contains(l, "AGENT") {
					line = l
				}
			}
			for _, name := range tt.has {
				if !strings.Contains(line, name) {
					t.Errorf("at %d columns the heading %q lacks %s", tt.columns, line, name)
				}
			}
			for _, name := range tt.lacks {
				if strings.Contains(line, name) {
					t.Errorf("at %d columns the heading %q still has %s", tt.columns, line, name)
				}
			}
		})
	}
}

// The doing column is cut with an ellipsis, and wider terminals give it room
// only up to about 160 columns.
func TestDashboardAgentsTruncateTheDoingColumn(t *testing.T) {
	long := strings.Repeat("a long task ", 30)
	fake := dashtest.NewFakeDaemon(t)
	fake.SetAgents(daemon.DashboardAgent{Agent: "worker-1@engineering", Status: daemon.StatusWorking, Job: 1, Doing: long})
	for columns, want := range map[int]int{80: 80, 120: 120, 160: 160, 200: 160} {
		d := dashboardIn(t, fake, columns, 30)
		screen := d.WaitForText("a long task")
		var row string
		for _, l := range strings.Split(screen, "\n") {
			if strings.Contains(l, "worker-1@engineering") {
				row = l
			}
		}
		if !strings.HasSuffix(row, "…") {
			t.Errorf("at %d columns the long doing text is %q, want it cut with an ellipsis", columns, row)
		}
		if got := len([]rune(row)); got != want {
			t.Errorf("at %d columns the row is %d cells, want %d", columns, got, want)
		}
	}
}

// The status is the daemon's: the Dashboard shows what it is told, whatever
// the other fields of the agent say.
func TestDashboardAgentsShowTheDaemonsStatusAndDeriveNone(t *testing.T) {
	fake := dashtest.NewFakeDaemon(t)
	fake.SetAgents(
		daemon.DashboardAgent{Agent: "a@engineering", Status: daemon.StatusStopped, Job: 9, Input: daemon.InputUser, Doing: "native prompt: Bash"},
		daemon.DashboardAgent{Agent: "b@engineering", Status: daemon.StatusIdle},
	)
	d := dashboardIn(t, fake, 100, 30)
	screen := d.WaitForText("b@engineering")
	for _, want := range []string{"■ Stopped", "○ Idle"} {
		if !strings.Contains(screen, want) {
			t.Errorf("the screen lacks %q:\n%s", want, screen)
		}
	}
	if strings.Contains(screen, "Waiting on you") {
		t.Errorf("the Dashboard derived a status of its own:\n%s", screen)
	}
}

func manyAgents(n int) []daemon.DashboardAgent {
	var agents []daemon.DashboardAgent
	for i := 1; i <= n; i++ {
		agents = append(agents, daemon.DashboardAgent{Agent: fmt.Sprintf("worker-%02d@engineering", i), Status: daemon.StatusIdle, Input: daemon.InputAutomation})
	}
	return agents
}

// The list scrolls with the cursor, and the cursor stays on its agent when
// the list is read again.
func TestDashboardAgentsScrollWithTheCursor(t *testing.T) {
	fake := dashtest.NewFakeDaemon(t)
	fake.SetAgents(manyAgents(40)...)
	d := dashboardIn(t, fake, 80, 24)
	d.WaitForText("worker-01@engineering")
	if screen := d.Screen(); strings.Contains(screen, "worker-20@engineering") {
		t.Fatalf("the first screen already shows the twentieth agent:\n%s", screen)
	}
	for range 25 {
		d.Send("j")
	}
	d.WaitForText("worker-26@engineering")
	screen := d.Screen()
	if strings.Contains(screen, "worker-01@engineering") || !strings.Contains(screen, " 26/40") {
		t.Errorf("after 25 moves down the screen is:\n%s", screen)
	}
	dashtest.Golden(t, "agents-scrolled-80x24", d.Snapshot())

	d.Send("G")
	d.WaitForText("worker-40@engineering")
	d.Send("g")
	d.WaitForText("worker-01@engineering")
	d.Send("\x1b[A")
	d.Send("\x1b[6~")
	d.WaitForText(" 20/40")
}

func TestDashboardAgentsKeepTheCursorOnItsAgentWhenReread(t *testing.T) {
	fake := dashtest.NewFakeDaemon(t)
	agents := manyAgents(5)
	fake.SetAgents(agents...)
	d := dashboardIn(t, fake, 100, 30)
	d.WaitForText("worker-01@engineering")
	d.Send("j")
	d.Send("j")
	d.WaitFor("the third agent selected", func(string) bool { return strings.Contains(d.Attributes(), "\n7 1-100: reverse\n") })
	// A new agent sorts first, as the daemon lists them.
	fake.SetAgents(append([]daemon.DashboardAgent{{Agent: "worker-00@engineering", Status: daemon.StatusIdle}}, agents...)...)
	d.WaitForText("worker-00@engineering")
	d.WaitFor("the third agent still selected", func(string) bool { return strings.Contains(d.Attributes(), "\n8 1-100: reverse\n") })
}

// Every command in the Dashboard's set has no subcommand of asmai.
func TestDashboardCommandsHaveNoCLISubcommand(t *testing.T) {
	for _, command := range daemon.DashboardCommands {
		attempts := [][]string{{command}, strings.Split(command, "."), {"dashboard", command}}
		for _, args := range attempts {
			var stdout, stderr strings.Builder
			code := run(args, strings.NewReader(""), &stdout, &stderr)
			if code == 0 {
				t.Errorf("asmai %s succeeded, want no such subcommand", strings.Join(args, " "))
			}
		}
		if strings.Contains(usage, command) {
			t.Errorf("the usage text lists %s", command)
		}
	}
}
