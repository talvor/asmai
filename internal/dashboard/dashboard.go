// SPDX-License-Identifier: Apache-2.0

// Package dashboard is the Dashboard (ADR 0015): a read-only terminal view of
// the factory, built on Bubble Tea and Lip Gloss. It never sends the daemon an
// action; it only asks whether the daemon answers.
package dashboard

import (
	"fmt"
	"io"
	"os"
	"os/exec"
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
	// zero means 3 seconds. Refresh is how often to re-read while it runs;
	// zero means 5 seconds.
	StoppedRetry, Refresh time.Duration
}

const (
	defaultStoppedRetry = 3 * time.Second
	defaultRefresh      = 5 * time.Second
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
	// readAt is when the daemon answered, which "since" is counted from.
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
	read       bool
	generation int
}

func newModel(socket string, o Options) model {
	return model{socket: socket, opts: o, width: o.Columns, height: o.Rows}
}

// readMsg is the daemon's answer to a read of everything the Dashboard shows.
type readMsg struct {
	factory    factory
	generation int
}

// attachedMsg says that `asmai attach` has exited and the Dashboard has the
// terminal again.
type attachedMsg struct{}

// retryMsg fires when it is time to read again.
type retryMsg struct{ generation int }

func (m model) Init() tea.Cmd { return read(m.socket, m.generation) }

// read asks the daemon for everything the Dashboard shows. A daemon that
// does not answer is the stopped factory.
func read(socket string, generation int) tea.Cmd {
	return func() tea.Msg {
		resp, err := daemon.Call(socket, daemon.Request{Command: daemon.CommandDashboardSnapshot})
		if err != nil || resp.Dashboard == nil {
			return readMsg{generation: generation}
		}
		return readMsg{factory: factory{running: true, agents: resp.Dashboard.Agents, readAt: time.Now()}, generation: generation}
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m = m.scrolled()
	case readMsg:
		if msg.generation != m.generation {
			return m, nil
		}
		m.read = true
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
		return m, tea.Tick(wait, func(time.Time) tea.Msg { return retryMsg{generation: m.generation} })
	case retryMsg:
		if msg.generation != m.generation {
			return m, nil
		}
		return m, read(m.socket, m.generation)
	case attachedMsg:
		// Bubble Tea has restored raw mode and the alternate screen. The
		// terminal may have changed size and the factory may have stopped
		// while attach had it, so ask for both again and draw it all anew.
		m.generation++
		return m, tea.Batch(tea.RequestWindowSize, tea.ClearScreen, read(m.socket, m.generation))
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
