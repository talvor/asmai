// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"fmt"
	"maps"
	"slices"

	"github.com/talvor/asmai/internal/store"
)

// statusLine is what the status line of every attached terminal shows from
// the daemon's state (ADR 0004): what waits on the user, the native prompts
// agents wait at and the decision requests pending with them, the
// conversation's focused job, or zero before any job had the focus, and the
// jobs that run. An attach stream sends it with its first frame and whatever of it
// changes on its own, whether or not the attached agent draws anything, so it
// is current in the middle of a turn and costs no model tokens.
type statusLine struct {
	prompts   []string
	decisions []string
	focus     int64
	jobs      []int64
	// held are the agents whose input the user holds, with `asmai take`,
	// until they release it.
	held []string
}

// statusLine reads the status line as the daemon's state now has it. What it
// cannot read from the store it keeps from was, which is what the stream last
// sent. d.mu is not held.
func (d *daemon) statusLine(was statusLine) statusLine {
	now := statusLine{prompts: []string{}, decisions: was.decisions, focus: was.focus, jobs: was.jobs, held: d.heldInputs()}
	pending, err := d.store.PendingDecisions()
	if err != nil {
		d.log.Error("reading the pending decisions for the status line", "error", err.Error())
	}
	jobOf := map[int64]int64{}
	for _, r := range pending {
		jobOf[r.Request] = r.Job
	}
	// The prompts that raised a decision request already name it, with its
	// job.
	shown := map[int64]bool{}
	if p := d.coordinationPrompt(); p != nil {
		now.prompts = append(now.prompts, p.String())
	}
	for _, p := range d.waitingPrompts() {
		line := p.String()
		if job, ok := jobOf[p.Request]; ok {
			line = store.JobTag(job) + " " + line
		}
		now.prompts = append(now.prompts, line)
		shown[p.Request] = true
	}
	if err == nil {
		now.decisions = []string{}
		for _, r := range pending {
			if !shown[r.Request] {
				now.decisions = append(now.decisions, decisionLine(r))
			}
		}
	}
	if f, err := d.store.Focus(); err != nil {
		d.log.Error("reading the focused job for the status line", "error", err.Error())
	} else {
		now.focus = f.Job
	}
	if jobs, err := d.store.OpenJobs(); err != nil {
		d.log.Error("reading the running jobs for the status line", "error", err.Error())
	} else {
		now.jobs = jobs
	}
	return now
}

// heldInputs are the lines of the status line for the agents whose input the
// user holds, by agent.
func (d *daemon) heldInputs() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	held := []string{}
	for _, name := range slices.Sorted(maps.Keys(d.leaders)) {
		if l := d.leaders[name]; l.session != nil && l.taken != nil {
			held = append(held, fmt.Sprintf("you hold %s's input", name))
		}
	}
	return held
}

// decisionLine is how the status line shows a decision request pending with
// the user, from when it is made until it is answered: its job, its number
// and where the user answers it.
func decisionLine(r store.DecisionRequest) string {
	where := "in the conversation"
	if r.Surface == store.SurfaceLavish {
		where = "in Lavish"
	}
	return fmt.Sprintf("%s decision request %d waits on you %s", store.JobTag(r.Job), r.Request, where)
}

// frame returns the frame that tells a client of what of s differs from was,
// or of all of s when all is set, and whether there is any.
func (s statusLine) frame(was statusLine, all bool) (Frame, bool) {
	var f Frame
	if all || !slices.Equal(s.prompts, was.prompts) {
		f.Prompts = &s.prompts
	}
	if all || !slices.Equal(s.decisions, was.decisions) {
		f.Decisions = &s.decisions
	}
	if all || !slices.Equal(s.held, was.held) {
		held := slices.Clone(s.held)
		if held == nil {
			held = []string{}
		}
		f.Held = &held
	}
	if all || s.focus != was.focus {
		f.Focus = &s.focus
	}
	if all || !slices.Equal(s.jobs, was.jobs) {
		f.Jobs = &s.jobs
	}
	return f, f.Prompts != nil || f.Decisions != nil || f.Focus != nil || f.Jobs != nil || f.Held != nil
}
