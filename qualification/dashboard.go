// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/session"
	"github.com/talvor/asmai/internal/store"
	"github.com/talvor/asmai/internal/vt"
)

// The terminal the Dashboard cases run `asmai dashboard` in, and the size
// C52 gives it while attach has it. Both are above the Dashboard's 80 by 24
// minimum, and wide enough that no column of the Agents tab is dropped.
const (
	dashboardColumns = 140
	dashboardRows    = 40
	resizedColumns   = 120
	resizedRows      = 36
)

// dashboardGrace is how long the Dashboard may show an agent as something
// other than what the journal says it is. The Dashboard reads again within
// about a quarter of a second of a change and every five seconds without one,
// so a difference that lasts longer than this is a wrong status, not a late
// one.
const dashboardGrace = 3 * time.Second

// keyUp is the Up arrow key, as a terminal sends it.
const keyUp = "\x1b[A"

// detachKeyByte is Ctrl-], the key that detaches an attach client.
const detachKeyByte = "\x1d"

// The leaders the Dashboard lists, by address.
var (
	coordinationLeader = roles.LeaderOf(roles.Coordination).String()
	qualityLeader      = roles.LeaderOf(roles.Quality).String()
)

// dashboardTerminal is `asmai dashboard` running in a terminal of the harness's
// own, as the user's terminal runs it: the Dashboard draws on it, and the
// harness reads what it drew back as text, and types the keys the user would.
type dashboardTerminal struct {
	h *Harness
	s *session.Session
}

// startDashboard runs `asmai dashboard` of the commit under test for the
// factory f, in a terminal dashboardColumns by dashboardRows.
func (h *Harness) startDashboard(f *Factory) (*dashboardTerminal, error) {
	env := envValue(f.env, "TERM", "xterm-256color")
	s, err := session.Start(session.Spec{Path: f.asmai, Args: []string{"dashboard"}, Env: env, Dir: "/", Columns: dashboardColumns, Rows: dashboardRows})
	if err != nil {
		return nil, fmt.Errorf("running asmai dashboard: %w", err)
	}
	return &dashboardTerminal{h: h, s: s}, nil
}

func (d *dashboardTerminal) stop() { d.s.Stop(5 * time.Second) }

// dashboardRow is one agent's row of the Agents tab: its status as the word
// the tab shows and what it says the agent is doing.
type dashboardRow struct {
	Status string
	// Line is the whole row, for the columns after the status.
	Line string
}

// dashboardView is what the terminal shows, read as the Dashboard draws it.
type dashboardView struct {
	lines []string
	state vt.State
	// header is the first row.
	header string
	// running and stopped are the factory states the header names.
	running, stopped bool
	// banner is the Waiting on you banner's text after its label, with any
	// "+N more", and empty when nothing waits.
	banner string
	// order lists the agents the Agents tab shows, first to last.
	order  []string
	agents map[string]dashboardRow
}

var (
	// agentRowPattern is a row of the Agents tab: the agent, then its glyph
	// and status word.
	agentRowPattern = regexp.MustCompile(`^\s*(\S+@\S+)\s+[■▲◆●○] (Stopped|Waiting on you|Needs reconciliation|Working|Idle)(?:\s|$)`)
	bannerLabel     = "▲ Waiting on you: "
)

// view reads the terminal.
func (d *dashboardTerminal) view() (dashboardView, error) {
	state := d.s.Screen()
	screen, err := vt.FromState(state)
	if err != nil {
		return dashboardView{}, err
	}
	return parseDashboard(screen.Lines(), state), nil
}

// parseDashboard reads the rows of a terminal the Dashboard has drawn.
func parseDashboard(lines []string, state vt.State) dashboardView {
	v := dashboardView{lines: lines, state: state, agents: map[string]dashboardRow{}}
	if len(lines) > 0 {
		v.header = lines[0]
	}
	v.running = strings.Contains(v.header, "asmai dashboard") && strings.Contains(v.header, "factory running")
	v.stopped = strings.Contains(v.header, "asmai dashboard") && strings.Contains(v.header, "factory stopped")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if text, found := strings.CutPrefix(trimmed, bannerLabel); found {
			v.banner = text
			continue
		}
		if m := agentRowPattern.FindStringSubmatch(line); m != nil {
			v.agents[m[1]] = dashboardRow{Status: m[2], Line: line}
			v.order = append(v.order, m[1])
		}
	}
	return v
}

// shows reports whether any row of the terminal holds text.
func (v dashboardView) shows(text string) bool {
	for _, line := range v.lines {
		if strings.Contains(line, text) {
			return true
		}
	}
	return false
}

// inverse reports whether the cell at column x and row y is drawn in reverse
// video, as the selected tab and the bars are.
func (v dashboardView) inverse(x, y int) bool {
	screen := v.state.Main
	if v.state.AltActive {
		screen = v.state.Alt
	}
	if y < 0 || y >= len(screen.Lines) || x < 0 || x >= len(screen.Lines[y]) {
		return false
	}
	return screen.Lines[y][x].Flags&vt.Inverse != 0
}

// selectedTab returns the tab drawn in reverse video in the tabs row, such as
// "2 Jobs", or empty.
func (v dashboardView) selectedTab() string {
	if len(v.lines) < 2 {
		return ""
	}
	for _, tab := range []string{"1 Agents", "2 Jobs", "3 Log"} {
		if x := strings.Index(v.lines[1], tab); x >= 0 && v.inverse(x, 1) {
			return tab
		}
	}
	return ""
}

// screenOf describes the terminal for a failure: the Dashboard's own screen
// shows nothing of the signed-in account, but the screen of an agent that
// attach draws may, so that one is the operator's choice.
func (d *dashboardTerminal) screenOf(v dashboardView) string {
	if strings.Contains(v.header, "asmai dashboard") {
		return "\nthe dashboard's screen:\n" + strings.Join(v.lines, "\n")
	}
	return d.h.screenNote(v.lines)
}

// waitFor reads the terminal until ok is satisfied, and fails with why it
// never was if the Dashboard exits, or the harness's wait runs out.
func (d *dashboardTerminal) waitFor(ctx context.Context, what string, ok func(dashboardView) bool) (dashboardView, error) {
	deadline := time.Now().Add(d.h.Wait)
	for {
		if err := ctx.Err(); err != nil {
			return dashboardView{}, err
		}
		v, err := d.view()
		if err != nil {
			return v, err
		}
		if ok(v) {
			return v, nil
		}
		select {
		case <-d.s.Done():
			return v, fmt.Errorf("the dashboard exited (%v) while the harness waited for %s%s", d.s.Exit(), what, d.screenOf(v))
		default:
		}
		if time.Now().After(deadline) {
			return v, fmt.Errorf("the screen never showed %s within %s%s", what, d.h.Wait, d.screenOf(v))
		}
		time.Sleep(d.h.Poll)
	}
}

// children returns the command lines of the processes whose parent is pid.
func children(pid int) ([]string, error) {
	out, err := exec.Command("ps", "-A", "-o", "pid=,ppid=,command=").Output()
	if err != nil {
		return nil, fmt.Errorf("listing processes with ps: %w", err)
	}
	return childCommands(string(out), pid), nil
}

// childCommands returns the command lines of the processes whose parent is
// pid, from the output of `ps -A -o pid=,ppid=,command=`.
func childCommands(psOutput string, pid int) []string {
	var commands []string
	for line := range strings.Lines(psOutput) {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		if ppid, err := strconv.Atoi(fields[1]); err == nil && ppid == pid {
			commands = append(commands, strings.Join(fields[2:], " "))
		}
	}
	return commands
}

// attachChild reports whether the Dashboard has `asmai attach agent` running
// as its child process, which is how it hands the terminal over (ADR 0015).
func (d *dashboardTerminal) attachChild(agent string) (bool, error) {
	commands, err := children(d.s.PID())
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(commands, func(c string) bool { return strings.HasSuffix(c, " attach "+agent) }), nil
}

// c52 qualifies the Dashboard's hand-over of the terminal to attach and its
// return (ADR 0015): from the Agents tab, Enter runs `asmai attach` for the
// selected agent as a child process, which draws the agent's session on the
// terminal and, still only observing, takes no key to the agent; Ctrl-]
// detaches, and the Dashboard has the terminal back as it was, in raw mode
// and on the alternate screen, redrawn at the size the terminal now has.
// It does the round trip twice, with the terminal resized while attach has it
// the first time, and leaves the Dashboard with q, which leaves the terminal
// as it found it.
func (h *Harness) c52(ctx context.Context, f *Factory, r *Result) error {
	if _, err := h.ready(ctx, f); err != nil {
		return err
	}
	leader, ok, err := f.Leader()
	if err != nil {
		return err
	}
	if !ok || leader.State != store.AgentRunning {
		return fmt.Errorf("Coordination's leader is not running after asmai start%s", exitNote(leader.Exit))
	}
	if err := h.awaitSessionStart(ctx, f, leader); err != nil {
		return err
	}
	if err := h.awaitPromptSurface(ctx, f); err != nil {
		return err
	}
	before, err := factoryJournal(f)
	if err != nil {
		return err
	}

	d, err := h.startDashboard(f)
	if err != nil {
		return err
	}
	defer d.stop()
	v, err := d.waitFor(ctx, "the Agents tab with "+coordinationLeader+" first", func(v dashboardView) bool {
		return v.running && len(v.order) > 0 && v.order[0] == coordinationLeader
	})
	if err != nil {
		return err
	}
	if tab := v.selectedTab(); tab != "1 Agents" {
		return fmt.Errorf("the dashboard opened on tab %q, not 1 Agents%s", tab, d.screenOf(v))
	}
	if !v.state.AltActive {
		return fmt.Errorf("the dashboard draws on the main screen, not the alternate screen%s", d.screenOf(v))
	}
	r.observe("asmai dashboard (pid %d) drew the Agents tab on the alternate screen of a %dx%d terminal, with %s first and selected", d.s.PID(), dashboardColumns, dashboardRows, coordinationLeader)

	const typed = "zqobserve"
	columns, rows := dashboardColumns, dashboardRows
	for round := 1; round <= 2; round++ {
		resize := round == 1
		if err := d.s.Input([]byte("\r")); err != nil {
			return err
		}
		attached, err := d.waitFor(ctx, "attach's status line for "+coordinationLeader, func(v dashboardView) bool {
			return len(v.lines) > 0 && strings.Contains(v.lines[len(v.lines)-1], coordinationLeader+" · observing · Ctrl-] detaches")
		})
		if err != nil {
			return fmt.Errorf("round %d: Enter on %s did not hand the terminal to attach: %w", round, coordinationLeader, err)
		}
		if child, err := d.attachChild(coordinationLeader); err != nil {
			return err
		} else if !child {
			return fmt.Errorf("round %d: attach's screen is up, but `asmai attach %s` is not a child process of the dashboard (pid %d)%s", round, coordinationLeader, d.s.PID(), d.screenOf(attached))
		}
		if !promptSurface(attached.lines) {
			return fmt.Errorf("round %d: attach drew no input prompt of %s, though its session is at its prompt%s", round, coordinationLeader, d.screenOf(attached))
		}
		r.observe("round %d: Enter on %s ran `asmai attach %s` as a child process of the dashboard, and attach drew the leader's session with its status line", round, coordinationLeader, coordinationLeader)

		if resize {
			columns, rows = resizedColumns, resizedRows
			if err := d.s.Resize(columns, rows); err != nil {
				return err
			}
			if _, err := d.waitFor(ctx, fmt.Sprintf("attach's status line on the last row of a %dx%d terminal", columns, rows), func(v dashboardView) bool {
				return len(v.lines) == rows && strings.Contains(v.lines[rows-1], "observing · Ctrl-] detaches")
			}); err != nil {
				return fmt.Errorf("round %d: attach did not follow the terminal's new size: %w", round, err)
			}
			r.observe("round %d: attach followed the terminal to %dx%d while it had it", round, columns, rows)
		}

		// Attach only observes: no key typed here reaches the agent.
		if err := d.s.Input([]byte(typed)); err != nil {
			return err
		}
		time.Sleep(time.Second)
		if err := d.s.Input([]byte(detachKeyByte)); err != nil {
			return err
		}
		back, err := d.waitFor(ctx, fmt.Sprintf("the dashboard back after Ctrl-], drawn at %dx%d", columns, rows), func(v dashboardView) bool {
			return v.running && v.state.AltActive && len(v.lines) == rows && strings.Contains(v.lines[rows-1], "enter attach · q quit") && !v.shows("observing")
		})
		if err != nil {
			return fmt.Errorf("round %d: after Ctrl-] the dashboard did not return: %w", round, err)
		}
		if tab := back.selectedTab(); tab != "1 Agents" {
			return fmt.Errorf("round %d: the dashboard came back on tab %q, not 1 Agents%s", round, tab, d.screenOf(back))
		}
		if !back.inverse(columns-1, 0) || !back.inverse(columns-1, rows-1) {
			return fmt.Errorf("round %d: the dashboard's header and footer bars do not reach the last column of the %d-column terminal, so it did not redraw at its size%s", round, columns, d.screenOf(back))
		}
		if len(back.order) == 0 || back.order[0] != coordinationLeader {
			return fmt.Errorf("round %d: the dashboard came back without the agents' rows%s", round, d.screenOf(back))
		}
		for deadline := time.Now().Add(h.Wait); ; time.Sleep(h.Poll) {
			child, err := d.attachChild(coordinationLeader)
			if err != nil {
				return err
			}
			if !child {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("round %d: `asmai attach %s` is still a child of the dashboard after Ctrl-]", round, coordinationLeader)
			}
		}
		r.observe("round %d: Ctrl-] ended attach, and the dashboard redrew the Agents tab on the alternate screen at %dx%d, its bars to the last column", round, columns, rows)

		// Raw mode is back: keys act as they are typed, with no Enter.
		for _, tab := range []string{"2 Jobs", "1 Agents"} {
			if err := d.s.Input([]byte(tab[:1])); err != nil {
				return err
			}
			if _, err := d.waitFor(ctx, "tab "+tab+" after the key "+tab[:1], func(v dashboardView) bool { return v.selectedTab() == tab }); err != nil {
				return fmt.Errorf("round %d: the dashboard did not take a key as it was typed after attach: %w", round, err)
			}
		}
		r.observe("round %d: the keys 2 and 1, typed with no Enter, switched the tabs, so raw mode is back", round)

		// The agent saw nothing of the hand-over.
		current, ok, err := f.Leader()
		if err != nil {
			return err
		}
		if !ok || current.State != store.AgentRunning || current.Generation != leader.Generation || current.PID != leader.PID {
			return fmt.Errorf("round %d: %s is not the session it was before the hand-over (generation %d, pid %d)%s", round, coordinationLeader, leader.Generation, leader.PID, exitNote(current.Exit))
		}
		screen, err := f.Screen()
		if err != nil {
			return err
		}
		if screenShows(screen, typed) {
			return fmt.Errorf("round %d: %q, typed while attach had the terminal, reached %s: attach is meant to observe, not to type%s", round, typed, coordinationLeader, h.screenNote(screen))
		}
		entries, err := factoryJournal(f)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if e.ID > lastEntry(before) && e.Kind == store.KindMessageWitnessed {
				return fmt.Errorf("round %d: the journal holds a witnessed user message (entry %d) made during the hand-over: attach is meant to observe, not to type", round, e.ID)
			}
		}
		r.observe("round %d: %s stayed in its session (generation %d, pid %d), the keys typed in attach did not reach it, and no user message was journaled", round, coordinationLeader, leader.Generation, leader.PID)
	}

	if err := d.s.Input([]byte("q")); err != nil {
		return err
	}
	select {
	case <-d.s.Done():
	case <-time.After(h.Wait):
		return errors.New("the dashboard did not exit after q")
	}
	if err := d.s.Exit(); err != nil {
		return fmt.Errorf("the dashboard exited after q with %v", err)
	}
	final, err := d.view()
	if err != nil {
		return err
	}
	switch {
	case final.state.AltActive:
		return errors.New("the dashboard left the terminal on the alternate screen after q")
	case final.shows("asmai dashboard"):
		return fmt.Errorf("the dashboard's screen was left on the terminal after q%s", d.screenOf(final))
	case final.state.Modes.HiddenCursor:
		return errors.New("the dashboard left the cursor hidden after q")
	case final.state.Modes.BracketedPaste:
		return errors.New("the dashboard left bracketed paste on after q")
	}
	r.observe("q left the dashboard with exit status 0, the alternate screen left, the cursor shown and bracketed paste off")
	return nil
}

// journalStatuses is what the journal and the agent records say each agent is,
// in the words the Dashboard uses, without the daemon's derivation of them: an
// agent whose session is not running is Stopped; one whose session's latest
// event is a permission request is Waiting on you; one whose latest dispatch is
// being worked on is Working; and any other is Idle. It leaves out what a
// journal does not hold, such as the user's unsent message, which also makes
// an agent Waiting on you.
func journalStatuses(entries []store.Entry, agents []store.Agent) map[string]string {
	owner := map[int64]string{}
	latest := map[string]string{}
	lastEvent := map[string]string{}
	for _, e := range entries {
		switch e.Kind {
		case store.KindDispatchChanged:
			var d struct {
				ID    int64  `json:"id"`
				Agent string `json:"agent"`
				State string `json:"state"`
			}
			if json.Unmarshal(e.Data, &d) != nil {
				continue
			}
			if d.Agent != "" {
				owner[d.ID] = d.Agent
			}
			if agent := owner[d.ID]; agent != "" && d.State != "" {
				latest[agent] = d.State
			}
		case store.KindObservation:
			var d struct {
				Agent      string `json:"agent"`
				Generation int    `json:"generation"`
				Event      string `json:"event"`
			}
			if json.Unmarshal(e.Data, &d) == nil {
				lastEvent[d.Agent+"/"+strconv.Itoa(d.Generation)] = d.Event
			}
		}
	}
	statuses := map[string]string{}
	for _, role := range roles.Staffed {
		statuses[roles.LeaderOf(role).String()] = "Stopped"
	}
	for _, a := range agents {
		switch {
		case a.State != store.AgentRunning:
			statuses[a.Agent] = "Stopped"
		case lastEvent[a.Agent+"/"+strconv.Itoa(a.Generation)] == "PermissionRequest":
			statuses[a.Agent] = "Waiting on you"
		case latest[a.Agent] == store.DispatchWorking:
			statuses[a.Agent] = "Working"
		default:
			statuses[a.Agent] = "Idle"
		}
	}
	return statuses
}

// factoryWatch compares the Dashboard with the journal as a case goes on, and
// remembers the statuses it saw the two agree on.
type factoryWatch struct {
	f *Factory
	d *dashboardTerminal
	// seen holds, for each agent, the statuses the Dashboard showed while the
	// journal said the same.
	seen map[string]map[string]bool
	// truth is the journal's last statuses, and differing when the Dashboard
	// first differed from them since they last changed.
	truth     map[string]string
	differing map[string]time.Time
}

func newFactoryWatch(f *Factory, d *dashboardTerminal) *factoryWatch {
	return &factoryWatch{f: f, d: d, seen: map[string]map[string]bool{}, truth: map[string]string{}, differing: map[string]time.Time{}}
}

// sample reads the Dashboard and the journal once, and fails if the Dashboard
// has shown an agent as other than the journal says for longer than
// dashboardGrace. It compares nothing while the factory is stopped or the
// Dashboard does not show it running. It returns what it read.
func (w *factoryWatch) sample() (dashboardView, map[string]string, error) {
	v, err := w.d.view()
	if err != nil {
		return v, nil, err
	}
	entries, err := factoryJournal(w.f)
	if err != nil {
		// A factory that is stopping or starting has no daemon to ask.
		return v, nil, nil
	}
	agents, err := w.f.Agents()
	if err != nil || !v.running {
		return v, nil, nil
	}
	truth := journalStatuses(entries, agents)
	now := time.Now()
	for agent, want := range truth {
		if w.truth[agent] != want {
			delete(w.differing, agent)
		}
		w.truth[agent] = want
		if row, shown := v.agents[agent]; shown && row.Status == want {
			delete(w.differing, agent)
			if w.seen[agent] == nil {
				w.seen[agent] = map[string]bool{}
			}
			w.seen[agent][want] = true
			continue
		}
		if _, differing := w.differing[agent]; !differing {
			w.differing[agent] = now
		}
		if now.Sub(w.differing[agent]) > dashboardGrace {
			shown := "no row"
			if row, ok := v.agents[agent]; ok {
				shown = row.Status
			}
			return v, truth, fmt.Errorf("the dashboard shows %s as %s, but the journal says it is %s, and has for more than %s%s", agent, shown, want, dashboardGrace, w.d.screenOf(v))
		}
	}
	return v, truth, nil
}

// until samples the Dashboard and the journal until ok is satisfied, failing
// when they differ, the Dashboard exits, ok fails, or the harness's wait runs
// out.
func (w *factoryWatch) until(ctx context.Context, what string, ok func(v dashboardView, truth map[string]string) (bool, error)) (dashboardView, error) {
	deadline := time.Now().Add(w.d.h.Wait)
	for {
		if err := ctx.Err(); err != nil {
			return dashboardView{}, err
		}
		v, truth, err := w.sample()
		if err != nil {
			return v, err
		}
		if truth != nil {
			if done, err := ok(v, truth); err != nil {
				return v, err
			} else if done {
				return v, nil
			}
		}
		select {
		case <-w.d.s.Done():
			return v, fmt.Errorf("the dashboard exited (%v) while the harness waited for %s%s", w.d.s.Exit(), what, w.d.screenOf(v))
		default:
		}
		if time.Now().After(deadline) {
			return v, fmt.Errorf("never saw %s within %s%s", what, w.d.h.Wait, w.d.screenOf(v))
		}
		time.Sleep(w.d.h.Poll)
	}
}

// quiet waits until the journal says no leader is Working or Waiting on you,
// and nothing is on its way to Coordination's leader, for a few seconds in a
// row, with the Dashboard agreeing throughout.
func (w *factoryWatch) quiet(ctx context.Context, what string) (dashboardView, error) {
	var since time.Time
	return w.until(ctx, what, func(v dashboardView, truth map[string]string) (bool, error) {
		entries, err := factoryJournal(w.f)
		if err != nil {
			return false, nil
		}
		settled := !coordinationBusy(entries)
		for _, status := range truth {
			settled = settled && status != "Working" && status != "Waiting on you"
		}
		if !settled {
			since = time.Time{}
			return false, nil
		}
		if since.IsZero() {
			since = time.Now()
		}
		return time.Since(since) >= 4*time.Second, nil
	})
}

// seenStatuses lists the statuses the Dashboard showed for agent while the
// journal said the same, in the order the Dashboard names them.
func (w *factoryWatch) seenStatuses(agent string) []string {
	var seen []string
	for _, status := range []string{"Stopped", "Idle", "Working", "Waiting on you"} {
		if w.seen[agent][status] {
			seen = append(seen, status)
		}
	}
	return seen
}

// snapshotWaiting reads the Waiting on you set from the daemon's snapshot.
func snapshotWaiting(f *Factory) ([]daemon.WaitingItem, error) {
	resp, err := daemon.Call(f.paths.Socket, daemon.Request{Command: daemon.CommandDashboardSnapshot})
	if err != nil {
		return nil, err
	}
	if resp.Dashboard == nil {
		return nil, errors.New("the daemon answered without a snapshot")
	}
	return resp.Dashboard.Waiting, nil
}

func matchesSingleWaitingBanner(banner, text string) bool {
	if lipgloss.Width(" "+bannerLabel+text) <= dashboardColumns {
		return banner == text
	}
	runes := []rune(text)
	for lipgloss.Width(" "+bannerLabel+string(runes))+1 > dashboardColumns {
		runes = runes[:len(runes)-1]
	}
	return banner == string(runes)+"…"
}

// permissionRequestedSince reports whether agent's session in generation
// reported a permission request in a journal entry after baseline, and
// whether it has finished a turn (reported Stop) since without one.
func permissionRequestedSince(entries []store.Entry, baseline int64, agent string, generation int) (requested, finishedWithout bool) {
	for _, e := range entries {
		if e.ID <= baseline || e.Kind != store.KindObservation {
			continue
		}
		var d struct {
			Agent      string `json:"agent"`
			Generation int    `json:"generation"`
			Event      string `json:"event"`
		}
		if json.Unmarshal(e.Data, &d) != nil || d.Agent != agent || d.Generation != generation {
			continue
		}
		switch d.Event {
		case "PermissionRequest":
			requested = true
		case "Stop":
			finishedWithout = !requested
		}
	}
	return requested, finishedWithout
}

// lastEntry is the ID of the journal's last entry, or zero.
func lastEntry(entries []store.Entry) int64 {
	if len(entries) == 0 {
		return 0
	}
	return entries[len(entries)-1].ID
}

// nativePromptWrite is the request that has Coordination's leader raise a
// native permission prompt: its Edit and Write tools change no file without
// one (README, "Agents"), so asking it to write a file in its own directory
// with the Write tool does. The file is removed with the disposable state.
func nativePromptWrite(number int64) string {
	return fmt.Sprintf("Qualification exercise %d. This is a check of your permission prompts, not a job: do not open one or hand anything to another role. With the Write tool, and no other tool, create the file qualification-dashboard.txt in your working directory, holding the single line ok. If it is refused, do nothing else. Then reply with the single word done.", number)
}

// isAllowOnce reports whether option, the line a Claude Code permission menu
// has selected, is the choice that allows the tool once: "Yes", whatever its
// number, and not "Yes, allow all edits during this session" or any choice that
// remembers the answer.
func isAllowOnce(option string) bool {
	_, label, found := strings.Cut(option, ". ")
	if !found {
		label = option
	}
	return strings.TrimSpace(strings.ToLower(label)) == "yes"
}

// allowOnce answers the native permission prompt Coordination's leader waits
// at with its one-time "Yes", in the conversation, as the user would: it moves
// the selection up to it if Claude Code started elsewhere, and confirms only
// while it is selected, so it never chooses an answer that is remembered.
func (h *Harness) allowOnce(ctx context.Context, f *Factory) error {
	var lines []string
	for range 6 {
		if err := ctx.Err(); err != nil {
			return err
		}
		var err error
		if lines, err = f.Screen(); err != nil {
			return err
		}
		switch option := selectedOption(lines); {
		case isAllowOnce(option):
			return f.Keys("\r")
		case option == "":
			time.Sleep(time.Second)
		default:
			if err := f.Keys(keyUp); err != nil {
				return err
			}
			time.Sleep(time.Second)
		}
	}
	return fmt.Errorf("the leader's permission prompt never had its one-time Yes selected%s", h.screenNote(lines))
}

// c53 qualifies, on a real factory, the statuses the Dashboard shows for an
// agent (ADR 0014) and its Waiting on you set. Throughout, it reads the
// Dashboard's terminal and the journal alike, and requires the Dashboard to
// show each leader as the journal says it is. It then requires, step by step:
//
//   - at rest, Coordination's leader Idle, the leaders of Engineering and
//     Quality, which have not run, Stopped, and nothing waiting;
//   - the user holding the leader's input (a started message) to put the
//     leader in Waiting on you and in the banner, and leaving to clear both;
//   - a native permission prompt, raised by the real Claude Code when it is
//     asked to write a file, to put the leader in Waiting on you, in the
//     banner and in the daemon's Waiting on you set alone, and the user's
//     answer, and the turn after it, to clear all three;
//   - a job opened and handed to Engineering, and the factory stopped and
//     started with the job open, to show an agent Working at some time, as the
//     journal says it is working on a dispatch: Coordination's leader reading
//     the reply to the handoff, or the restoration it is told to load; and
//   - the leaders at rest again, with nothing waiting.
//
// An assignment that needs reconciliation is the one status it does not show:
// it takes a worker whose dispatch outcome is not known, which no real
// session causes safely, so the development tests with the fake provider
// cover it.
func (h *Harness) c53(ctx context.Context, f *Factory, r *Result) error {
	if _, err := h.ready(ctx, f); err != nil {
		return err
	}
	leader, ok, err := f.Leader()
	if err != nil {
		return err
	}
	if !ok || leader.State != store.AgentRunning {
		return fmt.Errorf("Coordination's leader is not running after asmai start%s", exitNote(leader.Exit))
	}
	if err := h.awaitSessionStart(ctx, f, leader); err != nil {
		return err
	}
	if err := h.prepareHandoffRepository(ctx, f); err != nil {
		return err
	}
	if err := h.awaitPromptSurface(ctx, f); err != nil {
		return err
	}
	if err := h.awaitSettled(ctx, f); err != nil {
		return err
	}

	d, err := h.startDashboard(f)
	if err != nil {
		return err
	}
	defer d.stop()
	w := newFactoryWatch(f, d)

	// At rest.
	if _, err := w.until(ctx, "the leaders at rest: Coordination's Idle, Engineering's and Quality's Stopped, nothing waiting", func(v dashboardView, truth map[string]string) (bool, error) {
		return v.banner == "" &&
			v.agents[coordinationLeader].Status == "Idle" && v.agents[engineeringLeader].Status == "Stopped" && v.agents[qualityLeader].Status == "Stopped" &&
			truth[coordinationLeader] == "Idle", nil
	}); err != nil {
		return err
	}
	r.observe("at rest the dashboard showed %s Idle, and %s and %s, which have not run, Stopped, with nothing in the Waiting on you banner", coordinationLeader, engineeringLeader, qualityLeader)

	// The user holds the leader's input.
	leave, err := f.HoldInput()
	if err != nil {
		return err
	}
	held, err := w.until(ctx, "Coordination's leader Waiting on you for the input the user holds", func(v dashboardView, truth map[string]string) (bool, error) {
		return v.agents[coordinationLeader].Status == "Waiting on you", nil
	})
	if err != nil {
		leave()
		return err
	}
	set, err := snapshotWaiting(f)
	if err != nil {
		leave()
		return err
	}
	if len(set) != 1 || set[0].Kind != daemon.WaitingInput || set[0].Agent != coordinationLeader || !strings.Contains(held.agents[coordinationLeader].Line, "you hold its input") || !matchesSingleWaitingBanner(held.banner, set[0].Text) {
		leave()
		return fmt.Errorf("with the user holding %s's input, the Waiting on you set is %+v and the banner %q, want that one item, shown as the banner%s", coordinationLeader, set, held.banner, d.screenOf(held))
	}
	if err := leave(); err != nil {
		return err
	}
	// The journal does not hold the input, so the Dashboard is compared with
	// the journal again only once the user has left.
	w.differing = map[string]time.Time{}
	if _, err := w.until(ctx, "the leader Idle again with nothing waiting once the user left", func(v dashboardView, truth map[string]string) (bool, error) {
		return v.banner == "" && v.agents[coordinationLeader].Status == "Idle", nil
	}); err != nil {
		return err
	}
	r.observe("while the user held %s's input in the conversation, the dashboard showed it Waiting on you (%q) and the daemon's Waiting on you set was that one item; leaving the conversation cleared both", coordinationLeader, held.banner)

	// A native permission prompt.
	before, err := factoryJournal(f)
	if err != nil {
		return err
	}
	baseline := lastEntry(before)
	number := time.Now().UnixNano()
	if h.Number != nil {
		number = h.Number()
	}
	if err := f.Submit(nativePromptWrite(number)); err != nil {
		return err
	}
	prompted, err := w.until(ctx, "Coordination's leader Waiting on you at a native permission prompt", func(v dashboardView, truth map[string]string) (bool, error) {
		entries, err := factoryJournal(f)
		if err != nil {
			return false, nil
		}
		requested, finished := permissionRequestedSince(entries, baseline, coordinationLeader, leader.Generation)
		if finished {
			return false, errors.New("inconclusive: the leader finished its turn without raising a native permission prompt, so the harness cannot show how the dashboard shows one")
		}
		return requested && v.agents[coordinationLeader].Status == "Waiting on you" && strings.Contains(v.agents[coordinationLeader].Line, "native prompt"), nil
	})
	if err != nil {
		return err
	}
	row := prompted.agents[coordinationLeader]
	set, err = snapshotWaiting(f)
	if err != nil {
		return err
	}
	if len(set) != 1 || set[0].Kind != daemon.WaitingNativePrompt || set[0].Agent != coordinationLeader || !matchesSingleWaitingBanner(prompted.banner, set[0].Text) {
		return fmt.Errorf("with %s at a native prompt, the Waiting on you set is %+v and the banner %q, want that one item, shown as the banner%s", coordinationLeader, set, prompted.banner, d.screenOf(prompted))
	}
	about := row.Line
	if i := strings.Index(about, "native prompt"); i >= 0 {
		about = about[i:]
	}
	r.observe("when asked to write a file, %s raised a native permission prompt, which its PermissionRequest hook reported; the dashboard showed it Waiting on you (%q), and the daemon's Waiting on you set was that one item, shown as the banner %q", coordinationLeader, strings.TrimSpace(about), prompted.banner)

	if err := h.allowOnce(ctx, f); err != nil {
		return err
	}
	if _, err := w.until(ctx, "the prompt answered and the leader Idle again with nothing waiting", func(v dashboardView, truth map[string]string) (bool, error) {
		return v.banner == "" && v.agents[coordinationLeader].Status == "Idle" && truth[coordinationLeader] == "Idle", nil
	}); err != nil {
		return err
	}
	r.observe("once the user answered the prompt and the leader's turn finished, the dashboard showed %s Idle and the banner empty", coordinationLeader)

	// A job handed to Engineering, and a restart with the job open.
	before, err = factoryJournal(f)
	if err != nil {
		return err
	}
	baseline = lastEntry(before)
	prompt := h.exercisePrompt()
	if err := f.Submit(prompt); err != nil {
		return err
	}
	var evidence handoffEvidence
	if _, err := w.until(ctx, "the job opened and its handoff fetched by Engineering's leader", func(v dashboardView, truth map[string]string) (bool, error) {
		entries, err := factoryJournal(f)
		if err != nil {
			return false, nil
		}
		evidence = handoffEvidence{}
		for _, e := range entries {
			if e.ID > baseline {
				evidence.record(e, prompt)
			}
		}
		return evidence.witness != "" && evidence.dispatch > 0 && evidence.fetched, nil
	}); err != nil {
		return err
	}
	if _, err := w.quiet(ctx, "the leaders at rest after the handoff"); err != nil {
		return err
	}
	r.observe("with a job opened and handed to Engineering (dispatch %d), Engineering's leader was started, and the dashboard followed %s and %s through the handoff", evidence.dispatch, coordinationLeader, engineeringLeader)

	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	err = f.Stop(stopCtx)
	cancel()
	if err != nil {
		return err
	}
	if _, err := d.waitFor(ctx, "the stopped message", func(v dashboardView) bool { return v.stopped }); err != nil {
		return err
	}
	if res, code, err := f.Start(ctx); err != nil {
		return err
	} else if code != 0 {
		return fmt.Errorf("asmai start failed its checks: %s", failedChecks(res.Checks))
	}
	restored, ok, err := f.Leader()
	if err != nil {
		return err
	}
	if !ok || restored.State != store.AgentRunning || restored.Generation == leader.Generation {
		return fmt.Errorf("Coordination's leader was not restored in a new session after asmai start%s", exitNote(restored.Exit))
	}
	if _, err := w.quiet(ctx, "the leaders at rest after the restoration"); err != nil {
		return err
	}

	leaders := []string{coordinationLeader, engineeringLeader, qualityLeader}
	if !slices.ContainsFunc(leaders, func(agent string) bool { return w.seen[agent]["Working"] }) {
		return errors.New("inconclusive: no leader was seen Working while the journal said it worked on a dispatch, so the harness cannot show how the dashboard shows a Working agent")
	}
	for _, agent := range leaders {
		r.observe("%s: the dashboard and the journal agreed on %s", agent, strings.Join(w.seenStatuses(agent), ", "))
	}
	for _, status := range []string{"Stopped", "Idle", "Waiting on you"} {
		if !slices.ContainsFunc(leaders, func(agent string) bool { return w.seen[agent][status] }) {
			return fmt.Errorf("no leader was seen %s while the journal said the same", status)
		}
	}
	r.observe("Needs reconciliation is not exercised: it takes an assignment whose dispatch outcome is not known, which no real session causes safely; the development tests with the fake provider cover it")
	return nil
}

// c54 qualifies the Dashboard's factory-stopped state: when the factory stops
// the Dashboard clears everything it showed and shows only the stopped
// message, still running, and when the factory is restored it clears that
// message and reads everything again, from the new daemon. It does so twice, so
// that the Dashboard is shown to keep looking, not to find the factory once.
func (h *Harness) c54(ctx context.Context, f *Factory, r *Result) error {
	if _, err := h.ready(ctx, f); err != nil {
		return err
	}
	leader, ok, err := f.Leader()
	if err != nil {
		return err
	}
	if !ok || leader.State != store.AgentRunning {
		return fmt.Errorf("Coordination's leader is not running after asmai start%s", exitNote(leader.Exit))
	}
	if err := h.awaitSessionStart(ctx, f, leader); err != nil {
		return err
	}
	if err := h.awaitPromptSurface(ctx, f); err != nil {
		return err
	}

	d, err := h.startDashboard(f)
	if err != nil {
		return err
	}
	defer d.stop()
	running := func(v dashboardView) bool {
		return v.running && !v.stopped && !v.shows("The factory is not running.") && len(v.order) == len(roles.Staffed) && v.agents[coordinationLeader].Status != "Stopped"
	}
	v, err := d.waitFor(ctx, "the running factory's agents", running)
	if err != nil {
		return err
	}
	r.observe("the dashboard showed the running factory: %s", strings.Join(v.order, ", "))

	for round := 1; round <= 2; round++ {
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		err := f.Stop(stopCtx)
		cancel()
		if err != nil {
			return err
		}
		v, err := d.waitFor(ctx, "only the stopped message", func(v dashboardView) bool {
			return leftOnStoppedScreen(v) == ""
		})
		if err != nil {
			return fmt.Errorf("round %d: %w", round, err)
		}
		stoppedAt := time.Now()
		// Nothing of the factory outlives its daemon, and the Dashboard keeps
		// looking, which it does every few seconds, so the screen is read
		// again after a retry or two.
		for {
			if left := leftOnStoppedScreen(v); left != "" {
				return fmt.Errorf("round %d: the dashboard shows %s while the factory is stopped%s", round, left, d.screenOf(v))
			}
			if time.Since(stoppedAt) >= 8*time.Second {
				break
			}
			time.Sleep(h.Poll)
			if v, err = d.view(); err != nil {
				return err
			}
		}
		select {
		case <-d.s.Done():
			return fmt.Errorf("round %d: the dashboard exited (%v) while the factory was stopped", round, d.s.Exit())
		default:
		}
		r.observe("round %d: asmai stop cleared the dashboard to the stopped message alone, and it stayed so for %s while the dashboard looked again", round, time.Since(stoppedAt).Round(time.Second))

		res, code, err := f.Start(ctx)
		if err != nil {
			return err
		}
		if code != 0 {
			return fmt.Errorf("round %d: asmai start failed its checks: %s", round, failedChecks(res.Checks))
		}
		startedAt := time.Now()
		if _, err := d.waitFor(ctx, "the stopped message cleared and the factory read again", running); err != nil {
			return fmt.Errorf("round %d: %w", round, err)
		}
		restored, ok, err := f.Leader()
		if err != nil {
			return err
		}
		if !ok || restored.State != store.AgentRunning || restored.Generation == leader.Generation {
			return fmt.Errorf("round %d: Coordination's leader was not restored in a new session after asmai start%s", round, exitNote(restored.Exit))
		}
		leader = restored
		r.observe("round %d: within %s of asmai start returning, the dashboard cleared the stopped message and showed the restored factory (Coordination's leader in generation %d)", round, time.Since(startedAt).Round(100*time.Millisecond), restored.Generation)
		if err := h.awaitSessionStart(ctx, f, restored); err != nil {
			return err
		}
	}
	return nil
}

// leftOnStoppedScreen names what a terminal shows of the factory that the
// stopped screen must not: an agent's row, a tab, a banner, or a job, and
// returns empty when there is none.
func leftOnStoppedScreen(v dashboardView) string {
	switch {
	case !v.stopped:
		return "no stopped header"
	case len(v.order) > 0:
		return "the agent " + v.order[0]
	case v.banner != "":
		return "the banner " + strconv.Quote(v.banner)
	case v.shows("1 Agents"):
		return "the tabs"
	case v.shows("leader@") || v.shows("worker"):
		return "an agent's name"
	case !v.shows("The factory is not running."):
		return "no stopped message"
	}
	return ""
}
