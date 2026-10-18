// SPDX-License-Identifier: Apache-2.0

// Package dashboard is the Dashboard (ADR 0015): a read-only terminal view of
// the factory, built on Bubble Tea and Lip Gloss. It never sends the daemon an
// action; it reads snapshots and change notifications from the daemon.
package dashboard

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/talvor/asmai/internal/daemon"
)

// Minimum terminal size: below it the Dashboard shows only a size message.
const (
	MinColumns = 80
	MinRows    = 24
)

// Options are what Run needs besides the daemon's socket. The zero value
// reads the real terminal and the process environment.
type Options struct {
	// Input and Output are the terminal; nil means standard input and output.
	Input  io.Reader
	Output io.Writer
	// Environ is the environment, for NO_COLOR and TERM; nil means the process's.
	Environ []string
	// Columns and Rows are the initial size, for when the terminal cannot say.
	Columns, Rows int
	// StoppedRetry is how often to look for the daemon while it is down;
	// zero means 3 seconds. Refresh is the fallback read interval while it
	// runs; zero means 5 seconds.
	StoppedRetry, Refresh time.Duration
}

const (
	defaultStoppedRetry = 3 * time.Second
	defaultRefresh      = 5 * time.Second
	// redrawEvery is the least time between two reads that ticks cause: about
	// four redraws a second however fast the factory changes.
	redrawEvery = 250 * time.Millisecond
)

// Run shows the Dashboard on the terminal until the user quits, asking the
// daemon on socket whether the factory is running.
func Run(socket string, o Options) error {
	if o.StoppedRetry <= 0 {
		o.StoppedRetry = defaultStoppedRetry
	}
	if o.Refresh <= 0 {
		o.Refresh = defaultRefresh
	}
	opts := []tea.ProgramOption{}
	if o.Input != nil {
		opts = append(opts, tea.WithInput(o.Input))
	}
	if o.Output != nil {
		opts = append(opts, tea.WithOutput(o.Output))
	}
	if o.Environ != nil {
		opts = append(opts, tea.WithEnvironment(o.Environ))
	}
	if o.Columns > 0 && o.Rows > 0 {
		opts = append(opts, tea.WithWindowSize(o.Columns, o.Rows))
	}
	m := newModel(socket, o)
	defer m.watch.close()
	if _, err := tea.NewProgram(m, opts...).Run(); err != nil {
		return fmt.Errorf("running the dashboard: %w", err)
	}
	return nil
}

// factory is what the Dashboard knows of the factory. It is replaced whole on
// every read, so nothing outlives the daemon.
type factory struct {
	running bool
	// agents are the agents with the status the daemon derived for each; the
	// Dashboard shows them and derives nothing itself.
	agents []daemon.DashboardAgent
	// jobs are the jobs with the stage the daemon derived for each.
	jobs []daemon.DashboardJob
	// waiting is the Waiting on you set, in the daemon's order.
	waiting []daemon.WaitingItem
	// readAt is when the daemon last answered a read, which "since" is counted
	// from and "last updated" names.
	readAt time.Time
}

type model struct {
	socket  string
	opts    Options
	width   int
	height  int
	tab     tab
	factory factory
	// cursor is the selected agent and offset the first one the list shows.
	cursor, offset int
	// jobCursor and jobOffset are the same for the Jobs tab.
	jobCursor, jobOffset int
	// log is the Log tab.
	log logState
	// read is false until the first answer, so the first frame does not claim
	// the factory is stopped before the daemon has been asked.
	read              bool
	generation        int
	readNumber        int
	appliedReadNumber int
	// failed is true while the last refresh failed with the daemon running;
	// the last successful read remains on screen.
	failed bool
	// watch holds the change subscription's connection, shared by every copy
	// of the model so that Run can close it.
	watch *watch
	// lastRead is when the latest read was sent; wait is true while a timer
	// holds back the read that ticks since then have asked for.
	lastRead time.Time
	wait     bool
	// round numbers the fallback timers, so a timer from before a newer read
	// does nothing.
	round int
}

func newModel(socket string, o Options) model {
	return model{socket: socket, opts: o, width: o.Columns, height: o.Rows, watch: &watch{}, appliedReadNumber: -1}
}

// attachedMsg says that `asmai attach` has exited and the Dashboard has the
// terminal again.
type attachedMsg struct{}

// readMsg is the daemon's answer to a read of everything the Dashboard shows:
// the factory, or why it could not be read. A daemon that is not running is
// the stopped factory.
type readMsg struct {
	factory    factory
	err        error
	generation int
	number     int
}

// retryMsg fires when it is time to read again, unless a read since has
// started a later round.
type retryMsg struct{ round, generation int }

// flushMsg fires when the read that ticks asked for may be sent.
type flushMsg struct{ generation int }

// subscribedMsg is the outcome of opening the change subscription: its first
// tick, or why there is none.
type subscribedMsg struct{ err error }

type changeMsg struct{}

// resubscribeMsg fires when it is time to open the subscription again.
type resubscribeMsg struct{}

func (m model) Init() tea.Cmd {
	return tea.Batch(read(m.socket, m.generation, m.readNumber), subscribe(m.socket, m.watch))
}

// read asks the daemon for everything the Dashboard shows.
func read(socket string, generation, number int) tea.Cmd {
	return func() tea.Msg {
		resp, err := daemon.Call(socket, daemon.Request{Command: daemon.CommandDashboardSnapshot})
		if err == nil && resp.Dashboard == nil {
			err = errors.New("the daemon answered without a snapshot")
		}
		if err != nil {
			return readMsg{err: err, generation: generation, number: number}
		}
		return readMsg{factory: factory{running: true, agents: resp.Dashboard.Agents, jobs: resp.Dashboard.Jobs, waiting: resp.Dashboard.Waiting, readAt: time.Now()}, generation: generation, number: number}
	}
}

// watch is the change subscription's connection.
type watch struct {
	mu     sync.Mutex
	conn   net.Conn
	closed bool
	// reader is what the daemon sends after its first tick.
	reader *bufio.Reader
}

// hold keeps conn to close, or closes it if the Dashboard has already quit.
func (w *watch) hold(conn net.Conn, r *bufio.Reader) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.conn != nil {
		w.conn.Close()
	}
	w.conn, w.reader = conn, r
	if w.closed {
		conn.Close()
	}
}

func (w *watch) close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	if w.conn != nil {
		w.conn.Close()
	}
}

// subscribe opens the change subscription, which answers with a tick at once.
func subscribe(socket string, w *watch) tea.Cmd {
	return func() tea.Msg {
		conn, r, resp, err := daemon.Open(socket, daemon.Request{Command: daemon.CommandDashboardSubscribe})
		if err == nil && resp.Change == nil {
			err = errors.New("the daemon answered without a tick")
		}
		if err != nil {
			if conn != nil {
				conn.Close()
			}
			return subscribedMsg{err: err}
		}
		w.hold(conn, r)
		return subscribedMsg{}
	}
}

// await waits for the subscription's next tick. The subscription ending is
// the same as it failing to open.
func await(w *watch) tea.Cmd {
	return func() tea.Msg {
		line, err := w.reader.ReadBytes('\n')
		var resp daemon.Response
		if err == nil {
			err = json.Unmarshal(line, &resp)
		}
		if err == nil && resp.Change == nil {
			err = errors.New("the daemon sent something that is not a tick")
		}
		if err != nil {
			return subscribedMsg{err: err}
		}
		return changeMsg{}
	}
}

// reading sends a read now.
func (m model) reading(now time.Time) (model, tea.Cmd) {
	m.lastRead = now
	m.readNumber++
	m.round++
	return m, read(m.socket, m.generation, m.readNumber)
}

// ticked asks for a read after a tick: at once when none was sent in the last
// quarter second, else when that quarter second is over. Ticks that come while
// it waits are that read.
func (m model) ticked(now time.Time) (model, tea.Cmd) {
	if m.wait {
		return m, nil
	}
	if left := redrawEvery - now.Sub(m.lastRead); left > 0 {
		m.wait = true
		generation := m.generation
		return m, tea.Tick(left, func(time.Time) tea.Msg { return flushMsg{generation} })
	}
	return m.reading(now)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m = m.scrolled()
	case subscribedMsg:
		if msg.err != nil {
			return m, tea.Tick(m.opts.StoppedRetry, func(time.Time) tea.Msg { return resubscribeMsg{} })
		}
		m, cmd := m.ticked(time.Now())
		return m, tea.Batch(cmd, await(m.watch))
	case changeMsg:
		m, cmd := m.ticked(time.Now())
		return m, tea.Batch(cmd, await(m.watch))
	case resubscribeMsg:
		return m, subscribe(m.socket, m.watch)
	case flushMsg:
		if msg.generation != m.generation {
			return m, nil
		}
		m.wait = false
		return m.reading(time.Now())
	case readMsg:
		if msg.generation != m.generation || msg.number <= m.appliedReadNumber {
			return m, nil
		}
		m.appliedReadNumber = msg.number
		if msg.err != nil && !errors.Is(msg.err, daemon.ErrNotRunning) && m.factory.running {
			// The daemon runs but did not answer: keep what was read and say
			// how old it is.
			m.failed = true
			m.round++
			round := m.round
			generation := m.generation
			return m, tea.Tick(m.opts.Refresh, func(time.Time) tea.Msg { return retryMsg{round: round, generation: generation} })
		}
		m.read, m.failed = true, false
		selected, selectedJob := m.selected(), m.selectedJob()
		m.factory = msg.factory
		if !m.factory.running {
			// Nothing outlives the daemon, the log included.
			m.log = logState{full: m.log.full, job: m.log.job, agent: m.log.agent, view: m.log.view + 1}
		}
		m.cursor, m.jobCursor = 0, 0
		for i, a := range m.factory.agents {
			if a.Agent == selected {
				m.cursor = i
			}
		}
		for i, j := range m.factory.jobs {
			if j.Job == selectedJob {
				m.jobCursor = i
			}
		}
		m = m.scrolled()
		wait := m.opts.Refresh
		if !m.factory.running {
			wait = m.opts.StoppedRetry
		}
		m.round++
		round := m.round
		generation := m.generation
		return m, tea.Batch(m.refreshLog(), tea.Tick(wait, func(time.Time) tea.Msg { return retryMsg{round: round, generation: generation} }))
	case retryMsg:
		if msg.generation != m.generation || msg.round != m.round {
			return m, nil
		}
		return m.reading(time.Now())
	case attachedMsg:
		// Bubble Tea has restored raw mode and the alternate screen. The
		// terminal may have changed size and the factory may have stopped
		// while attach had it, so ask for both again and draw it all anew.
		m.generation++
		m.wait = false
		fresh, cmd := m.reading(time.Now())
		return fresh, tea.Batch(tea.RequestWindowSize, tea.ClearScreen, cmd)
	case logMsg:
		return m.logPage(msg)
	case entryMsg:
		if m.tab != tabLog || msg.view != m.log.view || msg.id != m.log.want || m.log.open {
			return m, nil
		}
		m.log.open, m.log.id, m.log.entry, m.log.failure, m.log.scroll = true, msg.id, nil, "", 0
		if msg.err != nil {
			m.log.failure = msg.err.Error()
		} else {
			m.log.entry = &msg.entry
		}
		return m, nil
	case tea.KeyPressMsg:
		key := msg.String()
		if m.tab == tabLog && m.factory.running && m.width >= MinColumns && m.height >= MinRows {
			if m.log.open {
				if key != "q" && key != "ctrl+c" && key != "1" && key != "2" && key != "3" {
					return m.entryKey(key), nil
				}
			} else if m, cmd, handled := m.logKey(key); handled {
				return m, cmd
			}
		}
		switch key {
		case "enter":
			return m, m.attach()
		case "q", "ctrl+c":
			return m, tea.Quit
		case "1":
			m.tab, m.log.open, m.log.want = tabAgents, false, 0
		case "2":
			m.tab, m.log.open, m.log.want = tabJobs, false, 0
		case "3":
			m.tab = tabLog
			return m, m.refreshLog()
		case "up", "k":
			m = m.moved(-1)
		case "down", "j":
			m = m.moved(1)
		case "pgup":
			m = m.moved(-m.listRows())
		case "pgdown":
			m = m.moved(m.listRows())
		case "home", "g":
			m = m.moved(-m.listLen())
		case "end", "G":
			m = m.moved(m.listLen())
		}
		return m.pageOlder()
	}
	return m, nil
}

// attach hands the terminal to `asmai attach` for the selected agent, as a
// child process (ADR 0015): Bubble Tea releases the terminal first and takes
// it back when attach exits. Attach is unchanged and only observes.
func (m model) attach() tea.Cmd {
	agent := m.selected()
	if m.tab != tabAgents || !m.read || !m.factory.running || agent == "" || m.width < MinColumns || m.height < MinRows {
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return nil
	}
	c := exec.Command(exe, "attach", agent)
	c.Env = m.opts.Environ
	return tea.ExecProcess(c, func(error) tea.Msg { return attachedMsg{} })
}

func (m model) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	return v
}

// selected is the agent the cursor is on, or empty.
func (m model) selected() string {
	if m.cursor < len(m.factory.agents) {
		return m.factory.agents[m.cursor].Agent
	}
	return ""
}

// selectedJob is the job the cursor of the Jobs tab is on, or zero.
func (m model) selectedJob() int64 {
	if m.jobCursor < len(m.factory.jobs) {
		return m.factory.jobs[m.jobCursor].Job
	}
	return 0
}

// listLen is how many rows the selected tab's list has.
func (m model) listLen() int {
	switch m.tab {
	case tabAgents:
		return len(m.factory.agents)
	case tabJobs:
		return len(m.factory.jobs)
	case tabLog:
		return len(m.log.entries)
	}
	return 0
}

// moved moves the cursor of the selected tab by delta, within its list.
func (m model) moved(delta int) model {
	n := m.listLen()
	if n == 0 {
		return m
	}
	cursor, _ := m.position()
	*cursor = min(max(*cursor+delta, 0), n-1)
	return m.scrolled()
}

// position is where the selected tab's cursor and window are.
func (m *model) position() (cursor, offset *int) {
	switch m.tab {
	case tabJobs:
		return &m.jobCursor, &m.jobOffset
	case tabLog:
		return &m.log.cursor, &m.log.offset
	}
	return &m.cursor, &m.offset
}

// scrolled puts the lists' windows where their cursors are in them.
func (m model) scrolled() model {
	rows := m.listRows()
	for _, l := range []struct {
		cursor, offset *int
		n              int
	}{{&m.cursor, &m.offset, len(m.factory.agents)}, {&m.jobCursor, &m.jobOffset, len(m.factory.jobs)}, {&m.log.cursor, &m.log.offset, len(m.log.entries)}} {
		*l.cursor = min(max(*l.cursor, 0), max(l.n-1, 0))
		*l.offset = min(max(*l.offset, *l.cursor-rows+1), *l.cursor)
		*l.offset = max(min(*l.offset, l.n-rows), 0)
	}
	return m
}
