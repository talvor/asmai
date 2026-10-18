// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/dashboard/dashtest"
	"github.com/talvor/asmai/internal/store"
)

var logStart = time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)

// logEntry is the entry with journal ID id, a minute after the one before.
func logEntry(id int64, kind string, job int64, from, to, summary string) daemon.LogEntry {
	return daemon.LogEntry{ID: id, At: logStart.Add(time.Duration(id) * time.Minute), Kind: kind, Job: job, From: from, To: to, Summary: summary}
}

// logJournal is a journal of agent traffic between agents whose addresses are
// long enough to make from-to collapse at 80 columns, with the entries around
// it that only the full journal shows.
func logJournal() ([]daemon.LogEntry, map[int64]string) {
	const (
		coordination = "leader@coordination"
		engineering  = "leader@engineering"
		worker       = "worker-3@engineering"
		reviewer     = "reviewer-12@quality-assurance"
	)
	entries := []daemon.LogEntry{
		logEntry(1, store.KindSessionStarted, 0, coordination, "", "session.started"),
		logEntry(2, store.KindHandoffSent, 7, coordination, engineering, "handoff: Add the log tab to the dashboard"),
		logEntry(3, store.KindMessageCreated, 7, coordination, engineering, "handoff message"),
		logEntry(4, store.KindDispatchChanged, 0, "", "", "dispatch.changed"),
		logEntry(5, store.KindAssignmentCreated, 7, engineering, worker, "assignment 21: Read the journal with a cursor in both directions"),
		logEntry(6, store.KindEffectRecorded, 7, worker, "", "effect.recorded"),
		logEntry(7, store.KindBlockedReported, 7, worker, engineering, "blocked on assignment 21: the store has no index on the journal's job"),
		logEntry(8, store.KindAssignmentCreated, 7, engineering, worker, "assignment 22: Read the journal with a cursor, indexed by job"),
		logEntry(9, store.KindResultSubmitted, 7, worker, engineering, "result of assignment 22"),
		logEntry(10, store.KindResultRejected, 7, engineering, worker, "rejected assignment 22: the cursor skips an entry at a page boundary"),
		logEntry(11, store.KindResultSubmitted, 7, worker, engineering, "result of assignment 22"),
		logEntry(12, store.KindResultAccepted, 7, engineering, worker, "accepted assignment 22: both directions page and no entry is read twice"),
		logEntry(13, store.KindHandoffSent, 7, engineering, reviewer, "handoff: Validate the log read at the job branch's tip"),
		logEntry(14, store.KindAssignmentCreated, 7, reviewer, "worker-5@quality-assurance", "assignment 23: Validate the log read"),
		logEntry(15, store.KindResultSubmitted, 7, "worker-5@quality-assurance", reviewer, "result of assignment 23"),
		logEntry(16, store.KindResultAccepted, 7, reviewer, "worker-5@quality-assurance", "accepted assignment 23: no blocking findings"),
		logEntry(17, store.KindHandoffSent, 8, coordination, engineering, "handoff: Fix the stopped message wrapping at 80 columns"),
		logEntry(18, store.KindAssignmentCreated, 8, engineering, worker, "assignment 24: Wrap the stopped message"),
		logEntry(19, store.KindBlockedReported, 8, worker, engineering, "blocked on assignment 24: the fixture repository is not cloned"),
		logEntry(20, store.KindSessionEnded, 0, worker, "", "session.ended"),
	}
	bodies := map[int64]string{
		2:  "Add the log tab to the dashboard.\nIt shows agent traffic by default, and the whole journal on request.",
		7:  "The store has no index on the journal's job, so a job filter reads every entry.\nNeeds: a migration that adds one.",
		12: "Both directions page and no entry is read twice.\nThe cursor test covers a page boundary.",
	}
	return entries, bodies
}

var logEnv = []string{"TZ=UTC"}

func logIn(t *testing.T, fake *dashtest.FakeDaemon, columns, rows int, extra ...string) *dashtest.Terminal {
	t.Helper()
	d := dashboardIn(t, fake, columns, rows, append(slices.Clone(logEnv), extra...)...)
	d.WaitForText("factory running")
	d.Send("3")
	return d
}

func TestDashboardLogGoldenScreens(t *testing.T) {
	for _, size := range dashboardSizes {
		for _, env := range [][]string{nil, {"NO_COLOR=1"}} {
			suffix := ""
			if len(env) > 0 {
				suffix = "-no_color"
			}
			t.Run(fmt.Sprintf("%dx%d%s", size[0], size[1], suffix), func(t *testing.T) {
				fake := dashtest.NewFakeDaemon(t)
				fake.SetLog(logJournal())
				d := logIn(t, fake, size[0], size[1], env...)
				d.WaitForText("accepted assignment 23")
				dashtest.Golden(t, fmt.Sprintf("log-%dx%d%s", size[0], size[1], suffix), d.Snapshot())

				// Enter opens the entry the cursor is on, the latest; Esc returns.
				for range 7 {
					d.Send("k") // back to the acceptance of assignment 22, which has a body
				}
				d.Send("\r")
				d.WaitForText("Entry #12")
				dashtest.Golden(t, fmt.Sprintf("log-entry-%dx%d%s", size[0], size[1], suffix), d.Snapshot())
				d.Send("\x1b")
				d.WaitFor("the log list", func(s string) bool { return strings.Contains(s, "SUMMARY") && !strings.Contains(s, "Entry #") })
				for range 4 {
					d.Send("j")
				}
				d.Send("\r")
				d.WaitForText("(this entry has no message body)")
				dashtest.Golden(t, fmt.Sprintf("log-entry-empty-%dx%d%s", size[0], size[1], suffix), d.Snapshot())
			})
		}
	}
}

func TestDashboardLogShowsAgentTrafficByDefaultAndTheJournalOnT(t *testing.T) {
	fake := dashtest.NewFakeDaemon(t)
	fake.SetLog(logJournal())
	d := logIn(t, fake, 100, 40)
	screen := d.WaitForText("accepted assignment 23")
	for _, hidden := range []string{"session.started", "dispatch.changed", "effect.recorded", "handoff message"} {
		if strings.Contains(screen, hidden) {
			t.Errorf("agent traffic shows %s:\n%s", hidden, screen)
		}
	}
	d.Send("t")
	screen = d.WaitForText("session.ended")
	for _, shown := range []string{"session.started", "dispatch.changed", "effect.recorded", "handoff message", "full journal"} {
		if !strings.Contains(screen, shown) {
			t.Errorf("the full journal lacks %s:\n%s", shown, screen)
		}
	}
	d.Send("t")
	d.WaitFor("agent traffic again", func(s string) bool {
		return strings.Contains(s, "agent traffic") && !strings.Contains(s, "session.ended")
	})
}

func TestDashboardLogFiltersToTheSelectedJobThenAgentThenNone(t *testing.T) {
	fake := dashtest.NewFakeDaemon(t)
	fake.SetLog(logJournal())
	d := logIn(t, fake, 100, 40)
	d.WaitForText("accepted assignment 23")
	// The cursor starts on the latest entry, a blocked report on job 8.
	d.Send("f")
	screen := d.WaitFor("job 8 only", func(s string) bool { return strings.Contains(s, "agent traffic, job #8") })
	if strings.Contains(screen, "assignment 21") || strings.Contains(screen, "assignment 23") {
		t.Errorf("the job 8 view shows entries of another job:\n%s", screen)
	}
	d.Send("f")
	d.WaitFor("an agent filter", func(s string) bool { return strings.Contains(s, "agent traffic, worker-3@engineering") })
	d.Send("f")
	d.WaitFor("no filter", func(s string) bool {
		return strings.Contains(s, "(agent traffic)") && strings.Contains(s, "accepted assignment 23")
	})
	var asked []daemon.LogQuery
	for _, c := range fake.Calls() {
		if c.Command == daemon.CommandDashboardLog && c.Log != nil {
			asked = append(asked, *c.Log)
		}
	}
	if !slices.ContainsFunc(asked, func(q daemon.LogQuery) bool { return q.Job == 8 }) || !slices.ContainsFunc(asked, func(q daemon.LogQuery) bool { return q.Agent == "worker-3@engineering" && q.Job == 0 }) {
		t.Errorf("the log reads were %+v, want one for job 8 and one for the agent", asked)
	}
}

func TestDashboardLogEntryShowsItsBodyAndEscReturns(t *testing.T) {
	fake := dashtest.NewFakeDaemon(t)
	fake.SetLog(logJournal())
	d := logIn(t, fake, 100, 30)
	d.WaitForText("accepted assignment 23")
	for range 7 {
		d.Send("k")
	}
	d.Send("\r")
	screen := d.WaitForText("The cursor test covers a page boundary.")
	for _, want := range []string{"Entry #12 · result.accepted", "job #7", "Both directions page and no entry is read twice."} {
		if !strings.Contains(screen, want) {
			t.Errorf("the entry view lacks %q:\n%s", want, screen)
		}
	}
	d.Send("\x1b")
	d.WaitFor("the list again", func(s string) bool { return strings.Contains(s, "SUMMARY") && !strings.Contains(s, "Entry #") })
	var entries []int64
	for _, c := range fake.Calls() {
		if c.Command == daemon.CommandDashboardEntry {
			entries = append(entries, c.Entry)
		}
	}
	if !slices.Equal(entries, []int64{12}) {
		t.Errorf("the entries read were %v, want only 12", entries)
	}
}

// The latest few hundred entries load on open and older ones page in as the
// user scrolls back.
func TestDashboardLogLoadsTheLatestAndPagesOlderOnScrollBack(t *testing.T) {
	var entries []daemon.LogEntry
	for id := int64(1); id <= 700; id++ {
		entries = append(entries, logEntry(id, store.KindHandoffSent, 7, "leader@coordination", "leader@engineering", fmt.Sprintf("handoff: number %d", id)))
	}
	fake := dashtest.NewFakeDaemon(t)
	fake.SetLog(entries, nil)
	d := logIn(t, fake, 100, 30)
	d.WaitForText("handoff: number 700")
	reads := func() (log []daemon.LogQuery) {
		for _, c := range fake.Calls() {
			if c.Command == daemon.CommandDashboardLog && c.Log != nil {
				log = append(log, *c.Log)
			}
		}
		return log
	}
	if first := reads()[0]; first.Before != 0 || first.After != 0 || first.Limit < 200 || first.Limit > 500 {
		t.Errorf("the first read was %+v, want the latest few hundred", first)
	}
	// Scroll back to the oldest loaded entry: 301, since 400 were not loaded.
	d.Send("g")
	d.WaitForText("handoff: number 401")
	d.WaitFor("an older page read", func(string) bool {
		return slices.ContainsFunc(reads(), func(q daemon.LogQuery) bool { return q.Before == 401 })
	})
	// The page that came in goes in above, and the cursor stays on its entry.
	d.Send("g")
	d.WaitForText("handoff: number 101")
	d.WaitFor("the oldest page read", func(string) bool {
		return slices.ContainsFunc(reads(), func(q daemon.LogQuery) bool { return q.Before == 101 })
	})
	d.Send("g")
	d.WaitFor("the first entry", func(s string) bool {
		return slices.ContainsFunc(strings.Split(s, "\n"), func(l string) bool { return strings.HasSuffix(strings.TrimRight(l, " "), "handoff: number 1") })
	})
}

func TestDashboardLogFollowsNewEntries(t *testing.T) {
	entries, bodies := logJournal()
	fake := dashtest.NewFakeDaemon(t)
	fake.SetLog(entries, bodies)
	d := logIn(t, fake, 100, 30)
	d.WaitForText("accepted assignment 23")
	entries = append(entries, logEntry(21, store.KindResultAccepted, 8, "leader@engineering", "worker-3@engineering", "accepted assignment 24: the fixture is cloned"))
	fake.SetLog(entries, bodies)
	fake.Tick()
	d.WaitForText("accepted assignment 24")
	d.Send("\r")
	d.WaitForText("Entry #21")
}

// Time drops first, then from-to collapses to the sender; the summary stays.
func TestDashboardLogDropsTheTimeThenCollapsesFromTo(t *testing.T) {
	entries, bodies := logJournal()
	fake := dashtest.NewFakeDaemon(t)
	fake.SetLog(entries, bodies)
	for _, tt := range []struct {
		columns  int
		has      []string
		lacks    []string
		sentence string
	}{
		{100, []string{"TIME", "FROM", "TO", "SUMMARY"}, nil, "accepted assignment 23"},
		{80, []string{"FROM", "SUMMARY"}, []string{"TIME", " TO "}, "accepted assignment 23: no blocking"},
	} {
		t.Run(fmt.Sprint(tt.columns), func(t *testing.T) {
			d := logIn(t, fake, tt.columns, 30)
			screen := d.WaitForText(tt.sentence)
			var heading string
			for _, l := range strings.Split(screen, "\n") {
				if strings.Contains(l, "SUMMARY") {
					heading = l
				}
			}
			for _, name := range tt.has {
				if !strings.Contains(heading, name) {
					t.Errorf("at %d columns the heading %q lacks %s", tt.columns, heading, name)
				}
			}
			for _, name := range tt.lacks {
				if strings.Contains(heading, name) {
					t.Errorf("at %d columns the heading %q still has %s", tt.columns, heading, name)
				}
			}
		})
	}
}

func TestDashboardLogOnAnEmptyJournal(t *testing.T) {
	fake := dashtest.NewFakeDaemon(t)
	fake.SetLog(nil, nil)
	d := logIn(t, fake, 100, 30)
	d.WaitForText("No agent traffic.")
	d.Send("t")
	d.WaitForText("No journal entries.")
}
