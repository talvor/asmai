// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/talvor/asmai/internal/providers"
	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/store"
)

// recoverWork is what every start does with the work the previous daemon left,
// before it restores the leaders (02 rules 7 and 11). It only reads and makes
// records, so it runs before any agent does.
//
// After a clean stop, each worker whose dispatch stopped at a boundary
// continues its assignment, in a new dispatch that resumes the one that
// stopped; one whose turn was cut off, or whose native session cannot be
// resumed, has its assignment held for its owning leader to reconcile, and
// the leader is told. After an unclean stop, which is reconciled in full
// later, no worker is resumed.
//
// Every leader with open work is then told, in a message of its own, to load
// the brief of each job it has it in, which is how a leader loads its briefs
// at every start (02 rule 22).
func (d *daemon) recoverWork(clean bool) {
	if clean {
		d.recoverWorkers()
	}
	d.restoreBriefs()
}

// recoverWorkers continues or holds the assignment of each worker the
// previous daemon left carrying one that is active, or that is being
// cancelled, in assignment order.
func (d *daemon) recoverWorkers() {
	workers, err := d.store.WorkersInState(store.AssignmentActive, store.AssignmentCancelling)
	if err != nil {
		d.log.Error("reading the workers the previous daemon left", "error", err.Error())
		return
	}
	for _, w := range workers {
		switch {
		case !w.Fetched:
			// The worker never fetched what it was given, so nothing of it
			// was begun: it is started for it as for any message waiting.
		case w.Dispatch.State == store.DispatchStopped && w.Report == store.ReportBlocked && w.Assignment.State == store.AssignmentActive:
			// It reported that it cannot go on, which waits for its owning
			// leader's decision, not for the worker.
		case w.Dispatch.State == store.DispatchStopped:
			d.resumeWorker(w)
		default:
			d.holdForReconciliation(w, fmt.Sprintf("its dispatch %d was %s when the factory stopped, so what %s did in it is not known",
				w.Dispatch.ID, w.Dispatch.State, w.Assignment.Worker))
		}
	}
}

// resumeWorker continues the assignment of the worker w, whose dispatch
// stopped at a boundary, with a new dispatch that resumes it in the provider's
// session where the worker was. If that session cannot be resumed, the
// assignment needs reconciliation instead. The worker is started for the
// dispatch with the other agents that have messages waiting.
func (d *daemon) resumeWorker(w store.WorkerDispatch) {
	a := w.Assignment
	native, err := d.store.ResumableSession(a.ID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		d.holdForReconciliation(w, fmt.Sprintf("the native session of %s cannot be resumed: the provider reported none for its assignment", a.Worker))
		return
	case err != nil:
		d.log.Error("reading the native session of a stopped worker", "assignment", a.ID, "error", err.Error())
		return
	case native.Provider != providers.ClaudeCode:
		d.holdForReconciliation(w, fmt.Sprintf("the native session of %s cannot be resumed: it ran on %s, which this factory does not run workers on", a.Worker, native.Provider))
		return
	}
	if _, err := os.Stat(a.Workspace.Path); err != nil {
		d.holdForReconciliation(w, fmt.Sprintf("%s cannot be run again: the workspace %s of its assignment is gone", a.Worker, a.Workspace.Path))
		return
	}
	if native.Transcript != "" {
		if _, err := os.Stat(native.Transcript); err != nil {
			d.holdForReconciliation(w, fmt.Sprintf("the native session of %s cannot be resumed: the provider's transcript %s of it is gone", a.Worker, native.Transcript))
			return
		}
	}
	body := fmt.Sprintf("The factory was stopped and started again, and your session resumes where it was. Assignment %d of job %d is still yours and active: your dispatch %d stopped at a boundary, and this dispatch resumes it.\n"+
		"Load `asmai brief %d`, look at your workspace and at the effects you have recorded, and continue the assignment from where you stopped. Never repeat an effect you already made.",
		a.ID, a.Job, w.Dispatch.ID, a.Job)
	if a.State == store.AssignmentCancelling {
		body = fmt.Sprintf("The factory was stopped and started again, and your session resumes where it was. Your owning leader has cancelled assignment %d of job %d: your dispatch %d stopped at a boundary before the cancellation was complete, and this dispatch resumes it.\n"+
			"Do not submit a result. Look at your workspace and at the effects you have recorded, commit whatever you have, push your assignment branch with `git push origin %s` if it is not pushed, record the push with `asmai effect push`, and stop. Never repeat an effect you already made.",
			a.ID, a.Job, w.Dispatch.ID, a.Workspace.Branch)
	}
	dispatch, err := d.store.AssignmentResumed(a.ID, w.Dispatch.ID, native.ID, body, time.Now())
	if err != nil {
		d.log.Error("resuming an assignment", "assignment", a.ID, "error", err.Error())
		return
	}
	d.log.Info("assignment resumed", "assignment", a.ID, "job", a.Job, "worker", a.Worker, "resumes", w.Dispatch.ID, "dispatch", dispatch.ID, "native_session", native.ID)
}

// holdForReconciliation marks the assignment of the worker w as needing
// reconciliation, for why, and tells its owning leader. Recording a
// reconciliation is not yet possible: the assignment is held as it is.
func (d *daemon) holdForReconciliation(w store.WorkerDispatch, why string) {
	a := w.Assignment
	body := fmt.Sprintf("Assignment %d of job %d needs reconciliation: %s.\n"+
		"It is held: %s keeps its workspace %s on branch %s, and nothing is retried. Look at `asmai brief %d`, the effects its worker recorded, and the workspace's branch on origin to see what was done.",
		a.ID, a.Job, why, a.Worker, a.Workspace.Path, a.Workspace.Branch, a.Job)
	told, err := d.store.AssignmentNeedsReconciliation(a.ID, w.Dispatch.ID, why, body, time.Now())
	if err != nil {
		d.log.Error("holding an assignment for reconciliation", "assignment", a.ID, "error", err.Error())
		return
	}
	d.log.Warn("assignment needs reconciliation", "assignment", a.ID, "job", a.Job, "worker", a.Worker, "owner", a.Owner, "dispatch", w.Dispatch.ID, "why", why, "told", told.ID)
}

// cannotResume holds the assignment of l, a worker whose session resumed the
// provider's session and ended without coming up, for reconciliation, and
// starts its owning leader to tell it. d.mu is held.
func (d *daemon) cannotResume(l *leader, exit string) {
	w, err := d.store.WorkerDispatch(l.assignment)
	if err != nil {
		d.log.Error("reading the dispatch of a worker that could not resume", "agent", l.address.String(), "assignment", l.assignment, "error", err.Error())
		return
	}
	if w.Assignment.State != store.AssignmentActive && w.Assignment.State != store.AssignmentCancelling {
		return
	}
	d.holdForReconciliation(w, fmt.Sprintf("the native session of %s cannot be resumed: its session ended before it came up (%s)", w.Assignment.Worker, exit))
	if owner, err := roles.ParseAddress(w.Assignment.Owner); err == nil {
		if err := d.ensureAgent(owner); err != nil {
			d.log.Error("starting the owning leader to tell it", "agent", w.Assignment.Owner, "error", err.Error())
		}
	}
}

// restoreBriefs tells each leader with open work to load the brief of each
// job it has it in, in one message to it that waits in its inbox like any
// other, about the first of them.
func (d *daemon) restoreBriefs() {
	work, err := d.store.OpenLeaderWork()
	if err != nil {
		d.log.Error("reading the open work of the leaders", "error", err.Error())
		return
	}
	jobs := map[string][]int64{}
	var leaders []string
	for _, w := range work {
		if _, ok := jobs[w.Leader]; !ok {
			leaders = append(leaders, w.Leader)
		}
		jobs[w.Leader] = append(jobs[w.Leader], w.Job)
	}
	for _, leader := range leaders {
		numbers := jobs[leader]
		dispatch, made, err := d.store.LeaderRestored(leader, numbers[0], restorationBody(leader, numbers), time.Now())
		if err != nil {
			d.log.Error("telling a restored leader to load its briefs", "agent", leader, "jobs", numbers, "error", err.Error())
			continue
		}
		if made {
			d.log.Info("restored leader told to load its briefs", "agent", leader, "jobs", numbers, "dispatch", dispatch.ID)
		}
	}
}

// restorationBody is what a restored leader is told: to load the brief of
// each of its jobs with open work, and to fetch what waits for it.
func restorationBody(leader string, jobs []int64) string {
	var briefs []string
	for _, job := range jobs {
		briefs = append(briefs, fmt.Sprintf("`asmai brief %d`", job))
	}
	which := fmt.Sprintf("Load %s for job %d", briefs[0], jobs[0])
	if len(jobs) > 1 {
		which = fmt.Sprintf("Load the brief of each of your jobs with open work, %s", strings.Join(briefs, ", "))
	}
	return fmt.Sprintf("The factory was stopped and started again, so you are a new session of %s and start without what you knew.\n"+
		"%s, and run `asmai inbox` for everything waiting for you, then continue the open work you have in each. Nothing you did before the stop is lost: the briefs and the journal hold it.",
		leader, which)
}

// restoreLeaders starts every leader that has a message waiting, other than
// Coordination's, whose start the checks report: the leaders whose role has
// open work, as the previous daemon left it.
func (d *daemon) restoreLeaders() {
	pending, err := d.store.PendingLeaders()
	if err != nil {
		d.log.Error("reading pending leaders", "error", err)
		return
	}
	for _, agent := range pending {
		address, err := roles.ParseAddress(agent)
		if err != nil || address == roles.LeaderOf(roles.Coordination) {
			continue
		}
		d.mu.Lock()
		if d.stopping {
			d.mu.Unlock()
			return
		}
		if err = d.ensureAgent(address); err != nil {
			d.log.Error("restoring receiving agent", "agent", agent, "error", err)
		}
		d.mu.Unlock()
	}
}
