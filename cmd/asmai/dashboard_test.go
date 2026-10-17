// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/dashboard/dashtest"
)

// dashboardIn runs `asmai dashboard` against the fake daemon in a terminal of
// columns by rows, with extra environment such as NO_COLOR=1.
func dashboardIn(t *testing.T, fake *dashtest.FakeDaemon, columns, rows int, extra ...string) *dashtest.Terminal {
	t.Helper()
	env := append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "ASMAI_STATE_DIR=" + fake.StateDir()}, extra...)
	return dashtest.Start(t, dashtest.Config{Columns: columns, Rows: rows, Env: env}, filepath.Join(asmaiBin, "asmai"), "dashboard")
}

var dashboardSizes = [][2]int{{80, 24}, {100, 30}, {160, 40}}

func TestDashboardShellGoldenScreens(t *testing.T) {
	for _, size := range dashboardSizes {
		for _, variant := range []struct{ suffix, env string }{{"", ""}, {"-no_color", "NO_COLOR=1"}, {"-vt100", "TERM=vt100"}} {
			name := fmt.Sprintf("shell-%dx%d%s", size[0], size[1], variant.suffix)
			var env []string
			if variant.env != "" {
				env = []string{variant.env}
			}
			t.Run(name, func(t *testing.T) {
				fake := dashtest.NewFakeDaemon(t)
				d := dashboardIn(t, fake, size[0], size[1], env...)
				d.WaitForText("factory running")
				dashtest.Golden(t, name, d.Snapshot())
			})
		}
	}
}

func TestDashboardTabsSwitchByNumber(t *testing.T) {
	fake := dashtest.NewFakeDaemon(t)
	d := dashboardIn(t, fake, 100, 30)
	d.WaitForText("factory running")
	// The selected tab is in reverse video.
	columns := map[string]string{"1": "2-11", "2": "13-20", "3": "22-28"}
	for _, key := range []string{"2", "3", "1"} {
		d.Send(key)
		d.WaitFor("tab "+key+" selected", func(string) bool { return strings.Contains(d.Attributes(), "\n2 "+columns[key]+": reverse\n") })
		dashtest.Golden(t, "tab-"+key, d.Snapshot())
	}
}

func TestDashboardFactoryStoppedGoldenScreens(t *testing.T) {
	for _, size := range dashboardSizes {
		for _, env := range [][]string{nil, {"NO_COLOR=1"}} {
			name := fmt.Sprintf("stopped-%dx%d", size[0], size[1])
			if len(env) > 0 {
				name += "-no_color"
			}
			t.Run(name, func(t *testing.T) {
				fake := dashtest.NewFakeDaemon(t)
				fake.Stop()
				d := dashboardIn(t, fake, size[0], size[1], env...)
				d.WaitForText("The factory is not running.")
				dashtest.Golden(t, name, d.Snapshot())
			})
		}
	}
}

// A factory that stops leaves nothing behind, and one that comes back is read
// again in full.
func TestDashboardFactoryStoppedClearsEverythingAndRecovers(t *testing.T) {
	fake := dashtest.NewFakeDaemon(t)
	d := dashboardIn(t, fake, 100, 30)
	d.WaitForText("factory running")

	fake.Stop()
	d.WaitForText("The factory is not running.")
	if screen := d.Screen(); strings.Contains(screen, "factory running") || strings.Contains(screen, "Agents") {
		t.Errorf("the stopped factory's screen still shows data:\n%s", screen)
	}

	fake.Start()
	d.WaitForText("factory running")
	if screen := d.Screen(); strings.Contains(screen, "not running") {
		t.Errorf("the restored factory's screen still shows the stopped message:\n%s", screen)
	}
}

func TestDashboardTooSmallGoldenScreens(t *testing.T) {
	for _, size := range [][2]int{{79, 24}, {80, 23}, {40, 10}} {
		for _, env := range [][]string{nil, {"NO_COLOR=1"}} {
			name := fmt.Sprintf("toosmall-%dx%d", size[0], size[1])
			if len(env) > 0 {
				name += "-no_color"
			}
			t.Run(name, func(t *testing.T) {
				fake := dashtest.NewFakeDaemon(t)
				d := dashboardIn(t, fake, size[0], size[1], env...)
				d.WaitForText("terminal too small")
				dashtest.Golden(t, name, d.Snapshot())
			})
		}
	}
}

func TestDashboardRedrawsOnResize(t *testing.T) {
	fake := dashtest.NewFakeDaemon(t)
	d := dashboardIn(t, fake, 100, 30)
	d.WaitForText("factory running")

	d.Resize(60, 20)
	d.WaitForText("terminal too small: needs 80x24, has 60x20")
	dashtest.Golden(t, "resize-too-small", d.Snapshot())

	d.Resize(160, 40)
	d.WaitForText("factory running")
	dashtest.Golden(t, "resize-restored", d.Snapshot())
}

func TestDashboardQuitsOnQ(t *testing.T) {
	fake := dashtest.NewFakeDaemon(t)
	d := dashboardIn(t, fake, 100, 30)
	d.WaitForText("factory running")
	d.Send("q")
	if err := d.WaitExit(); err != nil {
		t.Errorf("asmai dashboard exited with %v after q, want 0", err)
	}
}

// The Dashboard only reads: whatever it sends the daemon asks for status.
func TestDashboardNeverActsOnTheFactory(t *testing.T) {
	fake := dashtest.NewFakeDaemon(t)
	d := dashboardIn(t, fake, 100, 30)
	d.WaitForText("factory running")
	for _, key := range []string{"1", "2", "3", "\r", "x", "s"} {
		d.Send(key)
	}
	d.WaitForText("factory running")
	for _, call := range fake.Calls() {
		if call.Command != daemon.CommandStatus {
			t.Errorf("the Dashboard sent the daemon %q, want only status", call.Command)
		}
	}
}

func TestDashboardNeedsATerminal(t *testing.T) {
	var stdout, stderr strings.Builder
	if code := run([]string{"dashboard"}, strings.NewReader(""), &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "terminal") {
		t.Errorf("asmai dashboard without a terminal exited %d printing %q, want 1 and a request for a terminal", code, stderr.String())
	}
}

func TestAnAgentCannotRunTheDashboard(t *testing.T) {
	t.Setenv(daemon.SessionCredential, "credential")
	var stdout, stderr strings.Builder
	if code := run([]string{"dashboard"}, strings.NewReader(""), &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "an agent cannot") {
		t.Errorf("asmai dashboard with an agent credential exited %d printing %q, want 1 and a refusal", code, stderr.String())
	}
}
