// SPDX-License-Identifier: Apache-2.0

// Package dashboard is the Dashboard (ADR 0015): a read-only terminal view of
// the factory, built on Bubble Tea and Lip Gloss. It never sends the daemon an
// action; it only asks whether the daemon answers.
package dashboard

import (
	"fmt"
	"io"
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
	status  daemon.Status
}

type model struct {
	socket  string
	opts    Options
	width   int
	height  int
	tab     tab
	factory factory
	// read is false until the first answer, so the first frame does not claim
	// the factory is stopped before the daemon has been asked.
	read bool
}

func newModel(socket string, o Options) model {
	return model{socket: socket, opts: o, width: o.Columns, height: o.Rows}
}

// readMsg is the daemon's answer to a read of everything the Dashboard shows.
type readMsg struct{ factory factory }

// retryMsg fires when it is time to read again.
type retryMsg struct{}

func (m model) Init() tea.Cmd { return read(m.socket) }

// read asks the daemon for everything the Dashboard shows. A daemon that
// does not answer is the stopped factory.
func read(socket string) tea.Cmd {
	return func() tea.Msg {
		resp, err := daemon.Call(socket, daemon.Request{Command: daemon.CommandStatus})
		if err != nil || resp.Status == nil {
			return readMsg{}
		}
		return readMsg{factory{running: true, status: *resp.Status}}
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case readMsg:
		m.read = true
		m.factory = msg.factory
		wait := m.opts.Refresh
		if !m.factory.running {
			wait = m.opts.StoppedRetry
		}
		return m, tea.Tick(wait, func(time.Time) tea.Msg { return retryMsg{} })
	case retryMsg:
		return m, read(m.socket)
	case tea.KeyPressMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "1":
			m.tab = tabAgents
		case "2":
			m.tab = tabJobs
		case "3":
			m.tab = tabLog
		}
	}
	return m, nil
}

func (m model) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	return v
}
