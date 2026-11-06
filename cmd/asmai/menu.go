// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/roles"
)

// menuAct is what choosing a menu entry does.
type menuAct int

const (
	// actDetach detaches, and leaves the conversation when this terminal is
	// in it (03 rule 30; 04 rule 15).
	actDetach menuAct = iota
	// actTake takes the input of the agent shown, actRelease gives it back
	// to automation, and actInterrupt interrupts its turn.
	actTake
	actRelease
	actInterrupt
	// actConversation and actAgent move this terminal to the conversation
	// or to an agent it only observes.
	actConversation
	actAgent
)

// menuItem is one entry of the Ctrl-] menu.
type menuItem struct {
	act    menuAct
	agent  string
	label  string
	detail string
	// waiting marks an entry that waits on the user.
	waiting bool
}

// menu is the Ctrl-] menu an attach client has open: the actions on the
// agent shown, then the conversation and every agent to move to, those
// waiting on the user first (03 rule 30).
type menu struct {
	actions []menuItem
	targets []menuItem
	loading bool
	// selected indexes actions followed by targets.
	selected int
	// problem is why the last choice did not work.
	problem string
}

func (m *menu) items() []menuItem {
	return slices.Concat(m.actions, m.targets)
}

// menuActions are the menu's actions on the agent shown.
func menuActions() []menuItem {
	return []menuItem{
		{act: actDetach, label: "Detach"},
		{act: actTake, label: "Take"},
		{act: actRelease, label: "Release"},
		{act: actInterrupt, label: "Interrupt"},
	}
}

// typing reports whether this terminal types into the agent it shows: it holds
// the conversation, or took the agent's input and still holds it. c.mu is
// held.
func (c *attachClient) typing() bool {
	return c.input() && c.takenOver == ""
}

// unavailable says why the menu entry cannot be chosen now, or "" when it
// can. c.mu is held.
func (c *attachClient) unavailable(it menuItem) string {
	coordination := roles.LeaderOf(roles.Coordination).String()
	switch it.act {
	case actDetach:
		return ""
	case actTake:
		switch {
		case c.agent == coordination:
			return "the conversation is spoken in, not taken"
		case c.typing():
			return "you already type into " + c.agent
		}
	case actRelease:
		if !c.taking || c.takenOver != "" {
			if !slices.Contains(c.held, "you hold "+c.agent+"'s input") {
				return "you do not hold " + c.agent + "'s input"
			}
		}
	case actInterrupt:
		if !c.typing() && c.agent == coordination {
			return "the conversation's turn is interrupted from the conversation"
		}
	case actConversation:
		if c.conversation && c.takenOver == "" {
			return "you are here"
		}
	case actAgent:
		if !c.conversation && c.agent == it.agent {
			return "you are here"
		}
		if it.detail == stoppedDetail {
			return it.agent + " is not running"
		}
	}
	return ""
}

// stoppedDetail is what the menu says of an agent that is not running.
const stoppedDetail = "stopped"

// openMenu opens the menu, with the agents it lists read from the daemon
// meanwhile.
func (c *attachClient) openMenu() {
	c.mu.Lock()
	c.menu = &menu{actions: menuActions(), loading: true}
	c.renderer.Invalidate()
	c.mu.Unlock()
	c.markDirty()
	go c.loadTargets()
}

// loadTargets lists the conversation and every agent as the daemon's snapshot
// derived them, those waiting on the user first.
func (c *attachClient) loadTargets() {
	resp, err := daemon.Call(c.paths.Socket, daemon.Request{Command: daemon.CommandDashboardSnapshot})
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.menu == nil {
		return
	}
	c.menu.loading = false
	if err != nil || resp.Dashboard == nil {
		c.menu.problem = "could not list the agents: the factory did not answer"
		c.markDirty()
		return
	}
	coordination := roles.LeaderOf(roles.Coordination).String()
	conversation := menuItem{act: actConversation, label: "Conversation", detail: "Coordination's leader", waiting: len(c.decisions) > 0}
	for _, w := range resp.Dashboard.Waiting {
		if w.Kind == daemon.WaitingDecision || w.Kind == daemon.WaitingFinding {
			conversation.waiting = true
		}
	}
	var agents []menuItem
	for _, a := range resp.Dashboard.Agents {
		if a.Agent == coordination {
			conversation.waiting = conversation.waiting || a.Status == daemon.StatusWaitingOnYou
			continue
		}
		it := menuItem{act: actAgent, agent: a.Agent, label: a.Agent, waiting: a.Status == daemon.StatusWaitingOnYou}
		switch {
		case a.Status == daemon.StatusStopped:
			it.detail = stoppedDetail
		case a.Doing != "":
			it.detail = a.Status.Word() + ": " + a.Doing
		default:
			it.detail = a.Status.Word()
		}
		agents = append(agents, it)
	}
	if conversation.waiting {
		conversation.detail = "Waiting on you: " + conversation.detail
	}
	targets := append([]menuItem{conversation}, agents...)
	// Those waiting on the user come first, otherwise in the order listed.
	slices.SortStableFunc(targets, func(a, b menuItem) int {
		switch {
		case a.waiting && !b.waiting:
			return -1
		case b.waiting && !a.waiting:
			return 1
		}
		return 0
	})
	c.menu.targets = targets
	c.markDirty()
}

// closeMenu closes the menu and has the agent's screen drawn again. c.mu is
// held.
func (c *attachClient) closeMenu() {
	c.menu = nil
	c.renderer.Invalidate()
}

// menuKeys acts on keys the user typed while the menu is open, none of which
// reaches the agent, and reports whether the client ended.
func (c *attachClient) menuKeys(keys []byte) bool {
	for len(keys) > 0 {
		c.mu.Lock()
		m := c.menu
		if m == nil {
			c.mu.Unlock()
			return false
		}
		n := len(m.items())
		var choose bool
		switch {
		case bytes.HasPrefix(keys, []byte("\x1b[A")), bytes.HasPrefix(keys, []byte("\x1bOA")):
			m.selected, keys = max(m.selected-1, 0), keys[3:]
			m.problem = ""
		case bytes.HasPrefix(keys, []byte("\x1b[B")), bytes.HasPrefix(keys, []byte("\x1bOB")):
			m.selected, keys = min(m.selected+1, max(n-1, 0)), keys[3:]
			m.problem = ""
		case keys[0] == 'k':
			m.selected, keys = max(m.selected-1, 0), keys[1:]
			m.problem = ""
		case keys[0] == 'j':
			m.selected, keys = min(m.selected+1, max(n-1, 0)), keys[1:]
			m.problem = ""
		case keys[0] == '\r' || keys[0] == '\n':
			choose, keys = true, keys[1:]
		case keys[0] == 0x1b || keys[0] == detachKey:
			// Esc and a second Ctrl-] close the menu; so does the start of
			// a sequence the menu has no use for.
			c.closeMenu()
			c.mu.Unlock()
			c.markDirty()
			return false
		default:
			_, size := utf8.DecodeRune(keys)
			keys = keys[size:]
		}
		if !choose {
			c.mu.Unlock()
			c.markDirty()
			continue
		}
		items := m.items()
		if m.selected >= len(items) {
			c.mu.Unlock()
			continue
		}
		it := items[m.selected]
		if why := c.unavailable(it); why != "" {
			m.problem = why
			c.mu.Unlock()
			c.markDirty()
			continue
		}
		c.mu.Unlock()
		if c.choose(it) {
			return true
		}
	}
	return false
}

// choose does what the menu entry it says, and reports whether the client
// ended.
func (c *attachClient) choose(it menuItem) bool {
	switch it.act {
	case actDetach:
		c.closeMenuNow()
		c.leave()
		return true
	case actInterrupt:
		c.mu.Lock()
		typing := c.typing()
		c.mu.Unlock()
		if typing {
			// Esc interrupts a turn, as typing it does.
			c.send(daemon.ClientMessage{Keys: []byte(interruptKey)})
			c.closeMenuNow()
			return false
		}
		return c.moveTo(daemon.Request{Command: daemon.CommandTake, Agent: c.agent, Interrupt: true, Terminal: c.terminal})
	case actTake:
		return c.moveTo(daemon.Request{Command: daemon.CommandTake, Agent: c.agent, Terminal: c.terminal})
	case actConversation:
		return c.moveTo(daemon.Request{Command: daemon.CommandConversation, Terminal: c.terminal})
	case actAgent:
		return c.moveTo(daemon.Request{Command: daemon.CommandAttach, Agent: it.agent})
	case actRelease:
		return c.releaseInput()
	}
	return false
}

// interruptKey is the Esc key, which interrupts a turn.
const interruptKey = "\x1b"

func (c *attachClient) closeMenuNow() {
	c.mu.Lock()
	c.closeMenu()
	c.mu.Unlock()
	c.markDirty()
}

// moveTo leaves what this terminal shows, which saves the text the user left
// unsent in the conversation as leaving does, and has the client show req
// next. It reports whether the client ended.
func (c *attachClient) moveTo(req daemon.Request) bool {
	c.mu.Lock()
	c.next = &req
	c.closeMenu()
	c.mu.Unlock()
	c.leave()
	return true
}

// releaseInput gives the input of the agent shown back to automation, and
// ends the client, as the release does.
func (c *attachClient) releaseInput() bool {
	resp, err := daemon.Call(c.paths.Socket, daemon.Request{Command: daemon.CommandRelease, Agent: c.agent, Terminal: c.terminal})
	if err != nil {
		c.mu.Lock()
		if c.menu != nil {
			c.menu.problem = err.Error()
		}
		c.mu.Unlock()
		c.markDirty()
		return false
	}
	c.closeMenuNow()
	why := fmt.Sprintf("Released %s: its input is back with automation.", c.agent)
	if r := resp.Released; r != nil && r.Intervention.Typed() {
		why += fmt.Sprintf(" You typed %d message(s): %s has the intervention record.", len(r.Intervention.Messages), r.Owner)
	}
	c.finish(why)
	return true
}

// menuOverlay draws the menu over the agent's screen, centred, clipped to
// the terminal above its status line, and hides the cursor. c.mu is held.
func (c *attachClient) menuOverlay() string {
	m := c.menu
	area := max(c.rows-1, 1)
	width := min(c.columns, 76)
	if width < 4 {
		return ""
	}
	inner := width - 2
	var lines []string
	var selectedLine int
	lines = append(lines, "Ctrl-] menu · ↑/↓ choose · Enter selects · Esc closes")
	items := m.items()
	for i, it := range items {
		if i == len(m.actions) {
			lines = append(lines, "Move this terminal to")
		}
		if i == m.selected {
			selectedLine = len(lines)
		}
		marker := "  "
		if i == m.selected {
			marker = "> "
		}
		text := marker + it.label
		switch it.act {
		case actAgent, actConversation:
			if it.detail != "" {
				text += " · " + it.detail
			}
			if c.unavailable(it) == "you are here" {
				text += " · you are here"
			}
		default:
			if why := c.unavailable(it); why != "" {
				text += " · unavailable: " + why
			}
		}
		lines = append(lines, text)
	}
	if m.loading {
		lines = append(lines, "  listing the agents…")
	}
	if m.problem != "" {
		lines = append(lines, "! "+m.problem)
	}
	// A long list scrolls to keep the selected entry in view.
	height := min(len(lines), area-2)
	if height < 1 {
		return ""
	}
	first := 0
	if len(lines) > height {
		first = min(max(selectedLine-height/2, 0), len(lines)-height)
	}
	visible := lines[first : first+height]
	top := (area-(height+2))/2 + 1
	left := (c.columns-width)/2 + 1
	var b strings.Builder
	b.WriteString("\x1b[?2026h\x1b7\x1b[0m")
	fmt.Fprintf(&b, "\x1b[%d;%dH┌%s┐", top, left, strings.Repeat("─", inner))
	for i, line := range visible {
		runes := []rune(strings.Map(printable, line))
		if len(runes) > inner {
			runes = append(runes[:inner-1], '…')
		}
		text := string(runes) + strings.Repeat(" ", inner-len(runes))
		if first+i == selectedLine {
			text = "\x1b[7m" + text + "\x1b[0m"
		}
		fmt.Fprintf(&b, "\x1b[%d;%dH│%s│", top+1+i, left, text)
	}
	fmt.Fprintf(&b, "\x1b[%d;%dH└%s┘", top+1+len(visible), left, strings.Repeat("─", inner))
	b.WriteString("\x1b8\x1b[?25l\x1b[?2026l")
	return b.String()
}
