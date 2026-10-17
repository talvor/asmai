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
		return readMsg{factory: factory{running: true, agents: resp.Dashboard.Agents, waiting: resp.Dashboard.Waiting, readAt: time.Now()}, generation: generation, number: number}
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
		selected := m.selected()
		m.factory = msg.factory
		m.cursor = 0
		for i, a := range m.factory.agents {
			if a.Agent == selected {
				m.cursor = i
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
		return m, tea.Tick(wait, func(time.Time) tea.Msg { return retryMsg{round: round, generation: generation} })
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
	case tea.KeyPressMsg:
		switch msg.String() {
		case "enter":
			return m, m.attach()
		case "q", "ctrl+c":
			return m, tea.Quit
		case "1":
			m.tab = tabAgents
		case "2":
			m.tab = tabJobs
		case "3":
			m.tab = tabLog
		case "up", "k":
			m = m.moved(-1)
		case "down", "j":
			m = m.moved(1)
		case "pgup":
			m = m.moved(-m.listRows())
		case "pgdown":
			m = m.moved(m.listRows())
		case "home", "g":
			m = m.moved(-len(m.factory.agents))
		case "end", "G":
			m = m.moved(len(m.factory.agents))
		}
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

// moved moves the cursor of the Agents tab by delta, within the list.
func (m model) moved(delta int) model {
	if m.tab != tabAgents || len(m.factory.agents) == 0 {
		return m
	}
	m.cursor = min(max(m.cursor+delta, 0), len(m.factory.agents)-1)
	return m.scrolled()
}

// scrolled puts the list's window where the cursor is in it.
func (m model) scrolled() model {
	rows := m.listRows()
	m.cursor = min(max(m.cursor, 0), max(len(m.factory.agents)-1, 0))
	m.offset = min(max(m.offset, m.cursor-rows+1), m.cursor)
	m.offset = max(min(m.offset, len(m.factory.agents)-rows), 0)
	return m
}
