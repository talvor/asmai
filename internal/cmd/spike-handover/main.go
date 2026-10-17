// SPDX-License-Identifier: Apache-2.0

// Command spike-handover is a THROWAWAY spike for ADR 0015 (issue #115): it
// proves a Bubble Tea program can hand the terminal to `asmai attach` through
// tea.ExecProcess and get raw mode, the alternate screen and the size back.
// It is not part of the shipped CLI (it is not under cmd/asmai); delete it once the Dashboard
// owns the hand-over. Run:
//
//	go build -trimpath -o bin/spike-handover ./internal/cmd/spike-handover
//	bin/spike-handover [--asmai bin/asmai] <agent>
//
// Press a to attach to <agent>, q to quit. After attach returns the screen
// shows the state the program observed before and after, and the terminal's
// real tty state (stty -a) so restore quirks can be read off.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

type returned struct{ err error }

type model struct {
	asmai, agent  string
	width, height int
	before, after string
	attached      int
	lastErr       error
	resizes       int
}

// ttyState is stty's view of the controlling terminal, one line per flag group.
func ttyState() string {
	cmd := exec.Command("stty", "-a")
	cmd.Stdin = os.Stdin
	out, err := cmd.Output()
	if err != nil {
		return "stty: " + err.Error()
	}
	return strings.TrimSpace(string(out))
}

func (m model) Init() tea.Cmd { return nil }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.resizes++
	case tea.KeyPressMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "a":
			m.before = ttyState()
			cmd := exec.Command(m.asmai, "attach", m.agent)
			return m, tea.ExecProcess(cmd, func(err error) tea.Msg { return returned{err} })
		}
	case returned:
		m.attached++
		m.lastErr = msg.err
		m.after = ttyState()
		// Ask for the size again: a restore that lost it shows as no message.
		return m, tea.RequestWindowSize
	}
	return m, nil
}

func (m model) View() tea.View {
	title := lipgloss.NewStyle().Bold(true).Render("spike-handover (throwaway, issue #115)")
	same := "n/a"
	if m.after != "" {
		same = fmt.Sprint(m.before == m.after)
	}
	s := fmt.Sprintf("%s\n\nsize %dx%d (size messages: %d)\nattaches returned: %d, last error: %v\ntty state identical before/after: %s\n\n[a] attach %s  [q] quit\n",
		title, m.width, m.height, m.resizes, m.attached, m.lastErr, same, m.agent)
	v := tea.NewView(s)
	v.AltScreen = true
	return v
}

func main() {
	asmai := flag.String("asmai", "bin/asmai", "the asmai executable to run `attach` with")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: spike-handover [--asmai path] <agent>")
		os.Exit(2)
	}
	m := model{asmai: *asmai, agent: flag.Arg(0)}
	final, err := tea.NewProgram(m).Run()
	if err != nil {
		fmt.Fprintln(os.Stderr, "spike-handover:", err)
		os.Exit(1)
	}
	fm := final.(model)
	fmt.Printf("attaches returned: %d\nlast error: %v\nbefore:\n%s\n\nafter:\n%s\n", fm.attached, fm.lastErr, fm.before, fm.after)
}
