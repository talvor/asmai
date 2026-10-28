// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"strings"
	"time"

	"github.com/talvor/asmai/internal/providers"
	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/store"
)

// recoverWork is what every start does with the work the previous daemon left,
// before it restores the leaders (02 rules 7 and 11). It makes records, and
// removes what is local to a cancelled job nothing holds any more, so it runs
// before any agent does.
//
// After a clean stop, each worker whose dispatch stopped at a boundary
// continues its assignment, in a new dispatch that resumes the one that
// stopped; one whose turn was cut off, or whose native session cannot be
// resumed, has its assignment held for its owning leader to reconcile, and
// the leader is told. A cancelled job's cancelling assignment is never held:
// its worker is started again in a new session for its cancellation, or, when
// its workspace is gone, it ends as cancelled with no push. After an unclean
// stop, which is reconciled in full later, no worker is resumed.
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
// cancelled, in assignment order. A cancelled job's assignment whose
// workspace is gone ends as cancelled first, whatever its worker's dispatch.
// An active assignment whose worker was typed a decision answer's nudge in
// any session and never acknowledged it is held against the answer, never
// resumed past it, since every session ended with the previous daemon.
func (d *daemon) recoverWorkers() {
	workers, err := d.store.WorkersInState(store.AssignmentActive, store.AssignmentCancelling)
	if err != nil {
		d.log.Error("reading the workers the previous daemon left", "error", err.Error())
		return
	}
	for _, w := range workers {
		if d.cancelledWithoutWorkspace(w.Assignment) {
			d.cancelledJobEnded(w.Assignment.Job)
			continue
		}
		switch {
		case w.Fetched && w.Dispatch.State != store.DispatchStopped:
			d.cannotContinue(w, fmt.Sprintf("its dispatch %d was %s when the factory stopped, so what %s did in it is not known",
				w.Dispatch.ID, w.Dispatch.State, w.Assignment.Worker))
		case d.holdAgainstUncertainAnswer(w.Assignment, math.MaxInt, func(answer int64) string {
			return fmt.Sprintf("the factory was stopped and started again after decision answer dispatch %d was typed to %s and before it acknowledged it, so whether it saw the answer is unknown", answer, w.Assignment.Worker)
		}):
			// Resuming the worker past an answer it may not have seen would
			// strand the answer, which NextDispatch never offers again.
		case !w.Fetched:
			// The worker never fetched what it was given, so nothing of it
			// was begun: it is started for it as for any message waiting.
		case w.Dispatch.State == store.DispatchStopped && w.Report == store.ReportBlocked && w.Assignment.State == store.AssignmentActive:
			// It reported that it cannot go on, which waits for its owning
			// leader's decision, not for the worker.
		default:
			d.resumeWorker(w)
		}
	}
}

// resumeWorker continues the assignment of the worker w, whose dispatch
// stopped at a boundary, with a new dispatch that resumes it in the provider's
// session where the worker was. If that session cannot be resumed, or its
// workspace is gone, cannotContinue handles the assignment instead. The
// worker is started for the dispatch with the other agents that have
// messages waiting.
func (d *daemon) resumeWorker(w store.WorkerDispatch) {
	a := w.Assignment
	native, err := d.store.ResumableSession(a.ID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		d.cannotContinue(w, fmt.Sprintf("the native session of %s cannot be resumed: the provider reported none for its assignment", a.Worker))
		return
	case err != nil:
		d.log.Error("reading the native session of a stopped worker", "assignment", a.ID, "error", err.Error())
		return
	case native.Provider != providers.ClaudeCode:
		d.cannotContinue(w, fmt.Sprintf("the native session of %s cannot be resumed: it ran on %s, which this factory does not run workers on", a.Worker, native.Provider))
		return
	}
	if _, err := os.Stat(a.Workspace.Path); err != nil {
		d.cannotContinue(w, fmt.Sprintf("%s cannot be run again: the workspace %s of its assignment is gone", a.Worker, a.Workspace.Path))
		return
	}
	if native.Transcript != "" {
		if _, err := os.Stat(native.Transcript); err != nil {
			d.cannotContinue(w, fmt.Sprintf("the native session of %s cannot be resumed: the provider's transcript %s of it is gone", a.Worker, native.Transcript))
			return
		}
	}
	body := fmt.Sprintf("The factory was stopped and started again, and your session resumes where it was. Assignment %d of job %d is still yours and active: your dispatch %d stopped at a boundary, and this dispatch resumes it.\n"+
		"Load `asmai brief %d`, look at your workspace and at the effects you have recorded, and continue the assignment from where you stopped. Never repeat an effect you already made.",
		a.ID, a.Job, w.Dispatch.ID, a.Job)
	if a.State == store.AssignmentCancelling {
		body = cancellationResumption(a, w.Dispatch.ID)
	}
	dispatch, err := d.store.AssignmentResumed(a.ID, w.Dispatch.ID, native.ID, body, time.Now())
	if err != nil {
		d.log.Error("resuming an assignment", "assignment", a.ID, "error", err.Error())
		return
	}
	d.log.Info("assignment resumed", "assignment", a.ID, "job", a.Job, "worker", a.Worker, "resumes", w.Dispatch.ID, "dispatch", dispatch.ID, "native_session", native.ID)
}

// recovery is what cannotContinue did with an assignment.
type recovery int

const (
	// recoveryHeld: the assignment needs reconciliation, and its owning
	// leader is told.
	recoveryHeld recovery = iota
	// recoveryRestarted: a cancelled job's assignment has a new cancellation
	// dispatch, for which its worker is to be started in a new session.
	recoveryRestarted
	// recoveryEnded: a cancelled job's assignment whose workspace is gone
	// ended as cancelled with no push, and nobody is started for it.
	recoveryEnded
	// recoveryFailed: recording what was to be done failed, so the
	// assignment stays as it was, and is looked at again at the next start.
	recoveryFailed
)

// cannotContinue handles the assignment of the worker w, whose session cannot
// be resumed, for why, and says what it did. A job cancel ends every
// assignment as cancelled, so none of a cancelled job is held: a cancelling
// assignment of a cancelled job whose workspace is gone ends as cancelled
// with no push, and one whose workspace is there gets a new cancellation
// dispatch, for which its worker is started in a new session. Any other
// assignment is held for reconciliation.
func (d *daemon) cannotContinue(w store.WorkerDispatch, why string) recovery {
	a := w.Assignment
	if a.State != store.AssignmentCancelling {
		d.holdForReconciliation(w, why)
		return recoveryHeld
	}
	j, err := d.store.Job(a.Job)
	if err != nil {
		d.log.Error("reading the job of a cancelling assignment failed; it stays cancelling until the next start", "assignment", a.ID, "job", a.Job, "error", err.Error())
		return recoveryFailed
	}
	if j.State != store.JobCancelled {
		d.holdForReconciliation(w, why)
		return recoveryHeld
	}
	if d.cancelledWithoutWorkspace(a) {
		return recoveryEnded
	}
	if _, err := os.Stat(a.Workspace.Path); err != nil {
		d.log.Error("reading the workspace of a cancelled job's assignment failed; it stays cancelling until the next start", "assignment", a.ID, "workspace", a.Workspace.Path, "error", err.Error())
		return recoveryFailed
	}
	dispatch, err := d.store.CancellationRestarted(a.ID, w.Dispatch.ID, cancellationRestart(j, a, why), time.Now())
	if err != nil {
		d.log.Error("starting a cancelled job's assignment's cancellation again failed; it stays cancelling until the next start", "assignment", a.ID, "error", err.Error())
		return recoveryFailed
	}
	d.log.Info("a cancelled job's assignment's cancellation started again in a new session", "assignment", a.ID, "job", a.Job, "worker", a.Worker, "previous", w.Dispatch.ID, "dispatch", dispatch.ID, "why", why)
	return recoveryRestarted
}

// cancelledWithoutWorkspace ends a, an assignment of a cancelled job that is
// cancelling or needs reconciliation, as cancelled with no push when its
// workspace is gone, and reports whether it did. Nothing can be pushed from
// a workspace that is gone, so its worker is never started for it, and its
// branch, if pushed earlier, stays on origin. The caller removes what is
// local to the job once nothing else holds it, with cancelledJobEnded.
func (d *daemon) cancelledWithoutWorkspace(a store.Assignment) bool {
	if a.State != store.AssignmentCancelling && a.State != store.AssignmentNeedsReconciliation {
		return false
	}
	if _, err := os.Stat(a.Workspace.Path); !errors.Is(err, fs.ErrNotExist) {
		return false
	}
	j, err := d.store.Job(a.Job)
	if err != nil {
		d.log.Error("reading the job of an assignment whose workspace is gone", "assignment", a.ID, "job", a.Job, "error", err.Error())
		return false
	}
	if j.State != store.JobCancelled {
		return false
	}
	why := fmt.Sprintf("its workspace %s is gone, so nothing can be pushed from it", a.Workspace.Path)
	if _, err := d.store.CancelledWithoutWorkspace(a.ID, why, time.Now()); err != nil {
		d.log.Error("ending a cancelled job's assignment whose workspace is gone", "assignment", a.ID, "error", err.Error())
		return false
	}
	d.log.Warn("a cancelled job's assignment ended as cancelled with no push: its workspace is gone", "assignment", a.ID, "job", a.Job, "worker", a.Worker, "workspace", a.Workspace.Path, "branch", a.Workspace.Branch)
	return true
}

// cancellationRestart is the message that starts the worker of assignment a
// of the cancelled job j again, in a new session, because why.
func cancellationRestart(j store.Job, a store.Assignment, why string) string {
	lead := fmt.Sprintf("The factory was stopped and started again, and your earlier session cannot be resumed: %s. Job %d is cancelled at the user's word, in witnessed message %d, so assignment %d is cancelled with it, and this dispatch continues its cancellation in a new session.\n",
		why, j.Number, j.CancelWitness, a.ID)
	if a.Scratch {
		return lead + "Do not submit a result. Your workspace is a scratch workspace with no branch, so there is nothing to commit or push: what you wrote stays in it. Stop."
	}
	return lead + fmt.Sprintf("Do not submit a result. Look at `asmai brief %d`, your workspace and the effects recorded for the assignment, commit whatever you have, push your assignment branch with `git push origin %s` if it is not pushed (as its own single Bash call, never chained, piped or after a `cd`), record the push with `asmai effect push`, and stop. Never repeat an effect already made.",
		j.Number, a.Workspace.Branch)
}

// holdForReconciliation marks the assignment of the worker w as needing
// reconciliation, for why, and tells its owning leader. Recording a
// reconciliation is not yet possible: the assignment is held as it is.
func (d *daemon) holdForReconciliation(w store.WorkerDispatch, why string) {
	a := w.Assignment
	body := fmt.Sprintf("Assignment %d of job %d needs reconciliation: %s.\n"+
		"It is held: %s keeps its workspace %s on branch %s, and nothing is retried. Look at `asmai brief %d`, the effects its worker recorded, and the workspace's branch on origin to see what was done.",
		a.ID, a.Job, why, a.Worker, a.Workspace.Path, a.Workspace.Branch, a.Job)
	if a.Scratch {
		body = fmt.Sprintf("Assignment %d of job %d needs reconciliation: %s.\n"+
			"It is held: %s keeps its scratch workspace %s, which has no branch, and nothing is retried. Look at `asmai brief %d`, the effects its worker recorded, and what is in the workspace to see what was done.",
			a.ID, a.Job, why, a.Worker, a.Workspace.Path, a.Job)
	}
	told, err := d.store.AssignmentNeedsReconciliation(a.ID, w.Dispatch.ID, why, body, time.Now())
	if err != nil {
		d.log.Error("holding an assignment for reconciliation", "assignment", a.ID, "error", err.Error())
		return
	}
	d.log.Warn("assignment needs reconciliation", "assignment", a.ID, "job", a.Job, "worker", a.Worker, "owner", a.Owner, "dispatch", w.Dispatch.ID, "why", why, "told", told.ID)
}

// holdAgainstUncertainAnswer holds a, an active assignment, for
// reconciliation against the decision answer to its worker whose nudge was
// typed in generation of its session, or in any earlier one, and never
// acknowledged, for why it is uncertain, and reports whether it did.
func (d *daemon) holdAgainstUncertainAnswer(a store.Assignment, generation int, why func(answer int64) string) bool {
	if a.State != store.AssignmentActive {
		return false
	}
	answer, err := d.store.UncertainAnswer(a.ID, generation)
	if errors.Is(err, sql.ErrNoRows) {
		return false
	}
	if err != nil {
		d.log.Error("reading a worker's uncertain decision answer", "agent", a.Worker, "assignment", a.ID, "error", err.Error())
		return false
	}
	d.holdAnswerForReconciliation(a, answer, why(answer.ID))
	return true
}

// holdAnswerForReconciliation marks assignment a as needing reconciliation
// against answer, a decision answer to its worker whose outcome is unknown,
// for why, and tells its owning leader how to recover it. The answer stays
// queued and unknown until the owner delivers it again or retires it.
func (d *daemon) holdAnswerForReconciliation(a store.Assignment, answer store.Dispatch, why string) {
	body := fmt.Sprintf("Assignment %[1]d of job %[2]d needs reconciliation: %[3]s.\n"+
		"It is held, and decision answer dispatch %[4]d is kept, never discarded: nothing is retried until you choose. Look at `asmai brief %[2]d` and the effects %[5]s recorded, then either deliver the answer again in a new dispatch with `asmai answer redeliver --dispatch %[4]d --reason <why>`, which starts %[5]s for it, or retire it with `asmai answer retire --dispatch %[4]d --reason <why>` and send the assignment again with `asmai redispatch --assignment %[1]d --reason <why>`.",
		a.ID, a.Job, why, answer.ID, a.Worker)
	told, err := d.store.AssignmentNeedsReconciliation(a.ID, answer.ID, why, body, time.Now())
	if err != nil {
		d.log.Error("holding an assignment for reconciliation", "assignment", a.ID, "error", err.Error())
		return
	}
	d.log.Warn("assignment needs reconciliation", "assignment", a.ID, "job", a.Job, "worker", a.Worker, "owner", a.Owner, "answer", answer.ID, "why", why, "told", told.ID)
}

// cannotResume handles the assignment of l, a worker whose session resumed
// the provider's session and ended without coming up, with cannotContinue.
// An assignment held for reconciliation has its owning leader started to
// tell it; a cancelled job's assignment whose cancellation is started again
// has its worker started in a new session; and one that ended as cancelled,
// its workspace gone, has what is local to its job removed once nothing
// else holds it. d.mu is held.
func (d *daemon) cannotResume(l *leader, exit string) {
	w, err := d.store.WorkerDispatch(l.assignment)
	if err != nil {
		d.log.Error("reading the dispatch of a worker that could not resume", "agent", l.address.String(), "assignment", l.assignment, "error", err.Error())
		return
	}
	if w.Assignment.State != store.AssignmentActive && w.Assignment.State != store.AssignmentCancelling {
		return
	}
	switch d.cannotContinue(w, fmt.Sprintf("the native session of %s cannot be resumed: its session ended before it came up (%s)", w.Assignment.Worker, exit)) {
	case recoveryRestarted:
		if err := d.ensureAgent(l.address); err != nil {
			d.log.Error("starting the worker of a cancelled job's assignment again", "agent", l.address.String(), "assignment", l.assignment, "error", err.Error())
		}
	case recoveryEnded:
		// Removing the job's local view takes assignMu, which is never
		// taken while d.mu is held.
		job := w.Assignment.Job
		if d.stopping {
			return
		}
		d.agents.Add(1)
		go func() {
			defer d.agents.Done()
			d.cancelledJobEnded(job)
		}()
	case recoveryHeld:
		if owner, err := roles.ParseAddress(w.Assignment.Owner); err == nil {
			if err := d.ensureAgent(owner); err != nil {
				d.log.Error("starting the owning leader to tell it", "agent", w.Assignment.Owner, "error", err.Error())
			}
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
		"%s, and run `asmai inbox` for everything waiting for you, each as its own command in its own Bash call, then continue the open work you have in each. Nothing you did before the stop is lost: the briefs and the journal hold it.",
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

// cancellationResumption is the message that resumes a worker whose
// assignment was cancelled before its dispatch finished the cancellation.
func cancellationResumption(a store.Assignment, dispatch int64) string {
	if a.Scratch {
		return fmt.Sprintf("The factory was stopped and started again, and your session resumes where it was. Your owning leader has cancelled assignment %d of job %d: your dispatch %d stopped at a boundary before the cancellation was complete, and this dispatch resumes it.\n"+
			"Do not submit a result. Your workspace is a scratch workspace with no branch, so there is nothing to commit or push: what you wrote stays in it. Stop. Never repeat an effect you already made.",
			a.ID, a.Job, dispatch)
	}
	return fmt.Sprintf("The factory was stopped and started again, and your session resumes where it was. Your owning leader has cancelled assignment %d of job %d: your dispatch %d stopped at a boundary before the cancellation was complete, and this dispatch resumes it.\n"+
		"Do not submit a result. Look at your workspace and at the effects you have recorded, commit whatever you have, push your assignment branch with `git push origin %s` if it is not pushed (as its own single Bash call, never chained, piped or after a `cd`), record the push with `asmai effect push`, and stop. Never repeat an effect you already made.",
		a.ID, a.Job, dispatch, a.Workspace.Branch)
}
