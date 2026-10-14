// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/talvor/asmai/internal/roles"
)

// permissionRequested is the Claude Code hook event a session reports as it
// raises a native permission prompt.
const permissionRequested = "PermissionRequest"

// promptCheck is how often an attach stream looks for a change in the native
// prompts agents wait at.
const promptCheck = time.Second

// WaitingPrompt is a native permission prompt an agent's session waits at
// that nobody can answer: in M1 only Coordination's terminal takes the user's
// keys. Every other session runs so that it is denied rather than prompted,
// so a prompt there is something its rules did not foresee.
type WaitingPrompt struct {
	Agent      string `json:"agent"`
	Generation int    `json:"generation"`
	// About is what the session asks to do, as its hook reported it: the
	// tool, and the command for a shell command.
	About string    `json:"about"`
	Since time.Time `json:"since"`
}

// String is how `asmai status` and the status line show the prompt.
func (p WaitingPrompt) String() string {
	text := p.Agent + " waits at a native prompt nobody can answer"
	if p.About != "" {
		text += ": " + p.About
	}
	return text
}

// promptAbout describes what a PermissionRequest payload asks to do.
func promptAbout(payload json.RawMessage) string {
	var fields struct {
		Tool  string `json:"tool_name"`
		Input struct {
			Command  string `json:"command"`
			FilePath string `json:"file_path"`
		} `json:"tool_input"`
	}
	json.Unmarshal(payload, &fields)
	about := fields.Tool
	if target := cmp.Or(fields.Input.Command, fields.Input.FilePath); target != "" {
		// The status line has one row.
		target, _, _ = strings.Cut(target, "\n")
		about = fmt.Sprintf("%s `%s`", about, target)
	}
	return strings.TrimSpace(about)
}

// promptObserved notes what an event ref's session reported says of the
// native prompts it waits at: a permission request from any session but
// Coordination's leader's waits at one nobody can answer, and any later
// event of the session shows it is past it. d.mu is not held.
func (d *daemon) promptObserved(ref sessionRef, event string, payload json.RawMessage) {
	d.mu.Lock()
	defer d.mu.Unlock()
	l := d.leaders[ref.address.String()]
	if l == nil || l.session == nil || l.generation != ref.generation {
		return
	}
	if event != permissionRequested || ref.address.Role == roles.Coordination {
		l.prompt = nil
		return
	}
	l.prompt = &WaitingPrompt{Agent: ref.address.String(), Generation: ref.generation, About: promptAbout(payload), Since: time.Now()}
	d.log.Warn("an agent waits at a native permission prompt nobody can answer", "agent", l.prompt.Agent, "generation", l.prompt.Generation, "about", l.prompt.About)
}

// waitingPrompts returns the native prompts running sessions wait at that
// nobody can answer, by agent. d.mu is not held.
func (d *daemon) waitingPrompts() []WaitingPrompt {
	d.mu.Lock()
	defer d.mu.Unlock()
	var prompts []WaitingPrompt
	for _, l := range d.leaders {
		if l.session != nil && l.prompt != nil && l.prompt.Generation == l.generation {
			prompts = append(prompts, *l.prompt)
		}
	}
	slices.SortFunc(prompts, func(a, b WaitingPrompt) int { return strings.Compare(a.Agent, b.Agent) })
	return prompts
}

// promptLines returns the waitingPrompts as the status line shows them.
func (d *daemon) promptLines() []string {
	lines := []string{}
	for _, p := range d.waitingPrompts() {
		lines = append(lines, p.String())
	}
	return lines
}
