// SPDX-License-Identifier: Apache-2.0

package dashboard

import (
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/dashboard/dashtest"
)

// syncBuffer is a terminal output a test can share with the program.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

// running shows the Dashboard against fake, reading every refresh, until the
// test ends.
func running(t *testing.T, fake *dashtest.FakeDaemon, refresh time.Duration) {
	t.Helper()
	in, out := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- Run(fake.Paths().Socket, Options{Input: in, Output: &syncBuffer{}, Environ: []string{"TERM=xterm"}, Columns: 100, Rows: 30, Refresh: refresh, StoppedRetry: 50 * time.Millisecond})
	}()
	t.Cleanup(func() {
		out.Write([]byte("q"))
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("the Dashboard did not quit")
		}
		in.Close()
	})
}

func snapshots(f *dashtest.FakeDaemon) int {
	n := 0
	for _, c := range f.Calls() {
		if c.Command == daemon.CommandDashboardSnapshot {
			n++
		}
	}
	return n
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// A burst of ticks is about four redraws a second, not one per tick.
func TestTicksAreCoalescedToAboutFourReadsASecond(t *testing.T) {
	fake := dashtest.NewFakeDaemon(t)
	running(t, fake, time.Hour)
	waitFor(t, "the subscription", func() bool { return fake.Subscribers() == 1 })
	// The first tick the subscription opens with is read in its turn.
	time.Sleep(600 * time.Millisecond)
	before := snapshots(fake)

	start := time.Now()
	for time.Since(start) < time.Second {
		fake.Tick()
		time.Sleep(5 * time.Millisecond)
	}
	// Let the read that holds back the last ticks go.
	time.Sleep(300 * time.Millisecond)
	reads := snapshots(fake) - before
	if reads < 3 || reads > 6 {
		t.Errorf("%d reads for a second of ticks, want about 4", reads)
	}
}

// A tick on a quiet screen is read at once, not after the coalescing delay.
func TestALoneTickIsReadAtOnce(t *testing.T) {
	fake := dashtest.NewFakeDaemon(t)
	running(t, fake, time.Hour)
	waitFor(t, "the subscription", func() bool { return fake.Subscribers() == 1 })
	time.Sleep(600 * time.Millisecond)
	before := snapshots(fake)
	fake.Tick()
	waitFor(t, "a read", func() bool { return snapshots(fake) > before })
}

// With no tick the Dashboard still reads at the refresh interval.
func TestWithoutTicksTheDashboardRefreshesAtTheInterval(t *testing.T) {
	fake := dashtest.NewFakeDaemon(t)
	fake.Handle(daemon.CommandDashboardSubscribe, func(daemon.Request) daemon.Response {
		return daemon.Response{Error: "no subscription in this test"}
	})
	running(t, fake, 100*time.Millisecond)
	waitFor(t, "five refreshes with no tick", func() bool { return snapshots(fake) >= 5 })
}

// The subscription comes back after the factory has been stopped and started.
func TestTheSubscriptionIsOpenedAgainWhenTheFactoryReturns(t *testing.T) {
	fake := dashtest.NewFakeDaemon(t)
	running(t, fake, time.Hour)
	waitFor(t, "the subscription", func() bool { return fake.Subscribers() == 1 })
	fake.Stop()
	waitFor(t, "the subscription to end", func() bool { return fake.Subscribers() == 0 })
	fake.Start()
	waitFor(t, "the subscription again", func() bool { return fake.Subscribers() == 1 })
}

func shownAt(m model) string {
	m.width, m.height = 100, 30
	return m.render()
}

// "Last updated" is shown only after a refresh failed with the factory
// running, keeps what was read, and goes with the next good read.
func TestLastUpdatedIsShownOnlyAfterAFailedRefresh(t *testing.T) {
	m := newModel("", Options{Refresh: time.Second, StoppedRetry: time.Second})
	good := readMsg{factory: factory{running: true, agents: []daemon.DashboardAgent{{Agent: "leader@coordination", Status: daemon.StatusIdle}}, readAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}}
	next, _ := m.Update(good)
	m = next.(model)
	if screen := shownAt(m); strings.Contains(screen, "last updated") {
		t.Errorf("a good read shows a last updated note:\n%s", screen)
	}

	m, _ = m.reading(time.Now())
	next, _ = m.Update(readMsg{err: errors.New("the daemon did not answer"), number: m.readNumber})
	m = next.(model)
	screen := shownAt(m)
	if !strings.Contains(screen, "last updated 03:04:05") || !strings.Contains(screen, "leader@coordination") {
		t.Errorf("a failed refresh shows:\n%s\nwant the note and the agents read before", screen)
	}

	m, _ = m.reading(time.Now())
	good.number = m.readNumber
	next, _ = m.Update(good)
	m = next.(model)
	if screen := shownAt(m); strings.Contains(screen, "last updated") {
		t.Errorf("the note outlived a good read:\n%s", screen)
	}

	// A daemon that is not running is the stopped factory, not a failed refresh.
	m, _ = m.reading(time.Now())
	next, _ = m.Update(readMsg{err: daemon.ErrNotRunning, number: m.readNumber})
	m = next.(model)
	if screen := shownAt(m); strings.Contains(screen, "last updated") || !strings.Contains(screen, "factory stopped") {
		t.Errorf("a daemon that is down shows:\n%s", screen)
	}
}

// A fallback timer from before a newer read does not start another read.
func TestAStaleRefreshTimerDoesNothing(t *testing.T) {
	m := newModel("", Options{Refresh: time.Second, StoppedRetry: time.Second})
	next, _ := m.Update(readMsg{factory: factory{running: true}})
	m = next.(model)
	m, _ = m.reading(time.Now())
	next, _ = m.Update(readMsg{factory: factory{running: true}, number: m.readNumber})
	m = next.(model)
	if _, cmd := m.Update(retryMsg{round: m.round - 1}); cmd != nil {
		t.Error("a stale timer started a read")
	}
	if _, cmd := m.Update(retryMsg{}); cmd != nil {
		t.Error("an unnumbered timer started a read")
	}
	if _, cmd := m.Update(retryMsg{round: m.round}); cmd == nil {
		t.Error("the current timer did not start a read")
	}
	oldRound := m.round
	m, _ = m.reading(time.Now())
	if _, cmd := m.Update(retryMsg{round: oldRound}); cmd != nil {
		t.Error("a timer fired while a newer read was pending")
	}
}

func TestSupersededSnapshotRepliesDoNotReplaceCurrentState(t *testing.T) {
	fresh := factory{running: true, agents: []daemon.DashboardAgent{{Agent: "leader@coordination"}}, readAt: time.Now()}
	stale := factory{running: true, agents: []daemon.DashboardAgent{{Agent: "worker-1@engineering"}}, readAt: time.Now().Add(-time.Hour)}
	for _, old := range []struct {
		name string
		msg  readMsg
	}{
		{"success", readMsg{factory: stale}},
		{"failure", readMsg{err: errors.New("old failure")}},
		{"stopped", readMsg{err: daemon.ErrNotRunning}},
	} {
		t.Run(old.name, func(t *testing.T) {
			m := newModel("", Options{Refresh: time.Second})
			m, _ = m.reading(time.Now())
			next, _ := m.Update(readMsg{factory: fresh, number: m.readNumber})
			m = next.(model)
			before := shownAt(m)
			next, cmd := m.Update(old.msg)
			m = next.(model)
			if cmd != nil || shownAt(m) != before {
				t.Errorf("superseded %s reply changed the current dashboard", old.name)
			}
		})
	}

	m := newModel("", Options{Refresh: time.Second})
	next, _ := m.Update(readMsg{factory: fresh})
	m = next.(model)
	m, _ = m.reading(time.Now())
	next, _ = m.Update(readMsg{err: errors.New("current failure"), number: m.readNumber})
	m = next.(model)
	before := shownAt(m)
	next, cmd := m.Update(readMsg{factory: stale})
	m = next.(model)
	if cmd != nil || shownAt(m) != before || !m.failed {
		t.Error("a superseded success cleared the current failure note")
	}
}

func TestSlowReadAdvancesBeforeANewerReadCompletes(t *testing.T) {
	m := newModel("", Options{Refresh: time.Second, StoppedRetry: time.Second})
	older := m.readNumber
	m, _ = m.reading(time.Now())
	newer := m.readNumber
	next, cmd := m.Update(readMsg{factory: factory{running: true, agents: []daemon.DashboardAgent{{Agent: "leader@coordination"}}}, number: older})
	m = next.(model)
	if cmd == nil || !m.factory.running || m.appliedReadNumber != older {
		t.Fatal("a completed read was discarded while a newer read was pending")
	}
	next, cmd = m.Update(readMsg{factory: factory{running: true, agents: []daemon.DashboardAgent{{Agent: "worker-1@engineering"}}}, number: newer})
	m = next.(model)
	if cmd == nil || m.appliedReadNumber != newer || len(m.factory.agents) != 1 || m.factory.agents[0].Agent != "worker-1@engineering" {
		t.Fatal("the newer read did not replace the earlier result")
	}
	next, cmd = m.Update(readMsg{err: daemon.ErrNotRunning, number: older})
	m = next.(model)
	if cmd != nil || !m.factory.running {
		t.Fatal("an older reply replaced the newer result")
	}
	for _, slow := range []struct {
		name string
		err  error
	}{
		{"failure", errors.New("snapshot failed")},
		{"stopped", daemon.ErrNotRunning},
	} {
		t.Run(slow.name, func(t *testing.T) {
			m, _ = m.reading(time.Now())
			older = m.readNumber
			m, _ = m.reading(time.Now())
			newer = m.readNumber
			next, _ = m.Update(readMsg{err: slow.err, number: older})
			m = next.(model)
			if slow.name == "failure" && !m.failed || slow.name == "stopped" && m.factory.running {
				t.Fatal("a completed slow read did not update the dashboard")
			}
			next, _ = m.Update(readMsg{factory: factory{running: true}, number: newer})
			m = next.(model)
			if !m.factory.running || m.failed {
				t.Fatal("the newer success did not restore the dashboard")
			}
		})
	}
}
