// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"fmt"
	"slices"

	"github.com/talvor/asmai/internal/store"
)

// statusLine is what the status line of every attached terminal shows from
// the daemon's state (ADR 0004): what waits on the user, the native prompts
// agents wait at and the decision requests pending with them, and the jobs
// that run. An attach stream sends it with its first frame and whatever of it
// changes on its own, whether or not the attached agent draws anything, so it
// is current in the middle of a turn and costs no model tokens.
type statusLine struct {
	prompts   []string
	decisions []string
	jobs      []int64
}

// statusLine reads the status line as the daemon's state now has it. What it
// cannot read from the store it keeps from was, which is what the stream last
// sent. d.mu is not held.
func (d *daemon) statusLine(was statusLine) statusLine {
	now := statusLine{prompts: []string{}, decisions: was.decisions, jobs: was.jobs}
	// The prompts that raised a decision request already name it.
	shown := map[int64]bool{}
	if p := d.coordinationPrompt(); p != nil {
		now.prompts = append(now.prompts, p.String())
	}
	for _, p := range d.waitingPrompts() {
		now.prompts = append(now.prompts, p.String())
		shown[p.Request] = true
	}
	if pending, err := d.store.PendingDecisions(); err != nil {
		d.log.Error("reading the pending decisions for the status line", "error", err.Error())
	} else {
		now.decisions = []string{}
		for _, r := range pending {
			if !shown[r.Request] {
				now.decisions = append(now.decisions, decisionLine(r))
			}
		}
	}
	if jobs, err := d.store.OpenJobs(); err != nil {
		d.log.Error("reading the running jobs for the status line", "error", err.Error())
	} else {
		now.jobs = jobs
	}
	return now
}

// decisionLine is how the status line shows a decision request pending with
// the user: its job, its number and where the user answers it.
func decisionLine(r store.DecisionRequest) string {
	where := "in the conversation"
	if r.Surface == store.SurfaceLavish {
		where = "in Lavish"
	}
	return fmt.Sprintf("job %d: decision request %d waits on you %s", r.Job, r.Request, where)
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
	if all || !slices.Equal(s.jobs, was.jobs) {
		f.Jobs = &s.jobs
	}
	return f, f.Prompts != nil || f.Decisions != nil || f.Jobs != nil
}
