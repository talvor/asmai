// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/talvor/asmai/internal/github"
	"github.com/talvor/asmai/internal/repos"
	"github.com/talvor/asmai/internal/store"
)

// The CI watch's pace: each job's checks are read again after firstLook,
// doubling while nothing changes up to everyLook, and from firstLook again
// once something does.
const (
	firstLook = time.Second
	everyLook = 30 * time.Second
)

// watch is what the CI watch keeps for a job between looks.
type watch struct {
	head  string
	every time.Duration
	next  time.Time
	// unmet is why the latest green head was not delivered, logged once.
	unmet string
}

// watchCI watches CI for every open job with a pull request and a pushed
// head, through gh and with no agent, until the daemon stops. It runs no
// worker and changes nothing on GitHub but marking a pull request ready.
func (d *daemon) watchCI() {
	defer d.wg.Done()
	watches := map[int64]*watch{}
	tick := time.NewTicker(firstLook / 4)
	defer tick.Stop()
	for {
		select {
		case <-d.work.Done():
			return
		case <-tick.C:
		}
		jobs, err := d.store.Jobs()
		if err != nil {
			d.log.Error("reading the jobs whose CI the daemon watches", "error", err.Error())
			continue
		}
		watched := map[int64]bool{}
		for _, j := range jobs {
			if j.State != store.JobOpen {
				continue
			}
			pr, head, ok := d.watched(j.Number)
			if !ok {
				continue
			}
			watched[j.Number] = true
			w := watches[j.Number]
			if w == nil || w.head != head {
				w = &watch{head: head, every: firstLook, next: time.Now()}
				watches[j.Number] = w
			}
			if time.Now().Before(w.next) {
				continue
			}
			changed := d.lookAtCI(j, pr, w)
			if changed {
				w.every = firstLook
			} else {
				w.every = min(2*w.every, everyLook)
			}
			w.next = time.Now().Add(w.every)
		}
		// Nothing about a job that ended is watched.
		for job := range watches {
			if !watched[job] {
				delete(watches, job)
			}
		}
	}
}

// watched returns the pull request job's delivery owner last recorded, and
// the head the daemon last pushed, when the job has both: the head whose
// checks the daemon watches.
func (d *daemon) watched(job int64) (store.PullRequest, string, bool) {
	prs, err := d.store.PullRequests(job)
	if err != nil || len(prs) == 0 {
		return store.PullRequest{}, "", false
	}
	pushes, err := d.store.Pushes(job)
	if err != nil {
		return store.PullRequest{}, "", false
	}
	for i := len(pushes) - 1; i >= 0; i-- {
		if pushes[i].Result == store.PushPushed {
			return prs[len(prs)-1], pushes[i].Commit, true
		}
	}
	return store.PullRequest{}, "", false
}

// lookAtCI reads the checks on the head of j's pull request through gh, in
// AsmAI's clone, records each check's state when they changed, and delivers
// the job when every check on the head the daemon pushed is green. A head with
// no checks is never green: the daemon keeps waiting. It returns whether what
// it saw changed.
func (d *daemon) lookAtCI(j store.Job, pr store.PullRequest, w *watch) bool {
	repository, err := d.store.Repository(j.Repository)
	if err != nil {
		d.log.Error("reading the repository of a job whose CI the daemon watches", "job", j.Number, "error", err.Error())
		return false
	}
	ctx, cancel := context.WithTimeout(d.work, ghTimeout)
	defer cancel()
	found, err := github.ViewChecks(ctx, repository.Clone, pr.Number)
	if err != nil {
		if d.work.Err() == nil {
			d.log.Warn("the daemon could not read the checks of a job's pull request with gh; it looks again", "job", j.Number, "number", pr.Number, "error", err.Error())
		}
		return false
	}
	o := store.ChecksObservation{Job: j.Number, Number: found.Number, Head: found.HeadCommit, Verdict: found.Verdict()}
	for _, c := range found.Checks {
		o.Checks = append(o.Checks, store.CheckState(c))
	}
	o, changed, err := d.store.ChecksObserved(o, time.Now())
	if err != nil {
		d.log.Error("recording the checks of a job's pull request", "job", j.Number, "error", err.Error())
		return false
	}
	if changed {
		d.log.Info("checks observed", "job", j.Number, "number", o.Number, "head", o.Head, "verdict", o.Verdict, "checks", len(o.Checks))
	}
	switch {
	case found.State != "OPEN":
		return changed
	case found.HeadCommit != w.head:
		// GitHub does not have the pushed head on the pull request yet, or
		// has another: these checks are not the pushed head's.
		return changed
	case o.Verdict != github.Green:
		return changed
	}
	d.assignMu.Lock()
	if err := d.store.TestedPR(j.Number, w.head); err != nil {
		d.assignMu.Unlock()
		if why := err.Error(); why != w.unmet {
			w.unmet = why
			d.log.Warn("every check is green, but the head is not a tested pull request, so it is not marked ready", "job", j.Number, "head", w.head, "reason", why)
		}
		return changed
	}
	dispatch, ok := d.deliver(ctx, j, repository, found, o)
	d.assignMu.Unlock()
	if ok {
		d.nudge(dispatch.Agent)
		d.removeJobLocal(j.Number)
	}
	return true
}

// deliver marks the pull request found ready for review, a draft with gh pr
// ready and one opened with "[not ready]" by taking that out of its title,
// records the delivery of j with its pull request, head and time, and ends j
// with a dispatch telling Coordination's leader, which it returns with whether
// j was delivered. AsmAI never merges: ready never means merged. assignMu is
// held.
func (d *daemon) deliver(ctx context.Context, j store.Job, repository store.Repository, found github.Checks, o store.ChecksObservation) (store.Dispatch, bool) {
	if err := github.MarkReady(ctx, repository.Clone, found.PullRequest); err != nil {
		d.log.Error("marking a job's pull request ready; the daemon tries again", "job", j.Number, "number", found.Number, "error", err.Error())
		return store.Dispatch{}, false
	}
	ready, err := github.ViewPullRequest(ctx, repository.Clone, found.URL)
	if err != nil {
		d.log.Error("looking up a pull request just marked ready; the daemon looks again", "job", j.Number, "number", found.Number, "error", err.Error())
		return store.Dispatch{}, false
	}
	if ready.Draft || ready.IsNotReady() || ready.State != "OPEN" || ready.HeadCommit != found.HeadCommit {
		d.log.Error("a pull request just marked ready is not ready at the head", "job", j.Number, "number", ready.Number, "draft", ready.Draft, "title", ready.Title, "state", ready.State, "head", ready.HeadCommit)
		return store.Dispatch{}, false
	}
	delivery := store.Delivery{Job: j.Number, Number: ready.Number, URL: ready.URL, Head: ready.HeadCommit, Checks: o.Checks}
	delivery, dispatch, err := d.store.JobDelivered(delivery, deliveryBody(j, delivery), time.Now())
	if err != nil {
		d.log.Error("recording a job's delivery", "job", j.Number, "error", err.Error())
		return store.Dispatch{}, false
	}
	d.log.Info("job delivered", "job", j.Number, "number", delivery.Number, "url", delivery.URL, "head", delivery.Head, "dispatch", dispatch.ID)
	return dispatch, true
}

// deliveryBody is what Coordination's leader is told of a job's delivery.
func deliveryBody(j store.Job, p store.Delivery) string {
	return fmt.Sprintf("Job %d is delivered: its pull request #%d, %s, is ready for review at head %s, with every check green (%s). "+
		"The job has ended: its workspaces and view are removed, and its branches stay on origin. "+
		"AsmAI never merges: ready is not merged, and the user reviews and merges it. "+
		"Tell the user, in a line tagged with job %d, with the link.",
		j.Number, p.Number, p.URL, p.Head, store.CheckNames(p.Checks), j.Number)
}

// removeJobLocal removes everything local to job, which has ended: its view,
// any workspace of its assignments left, and its branches in AsmAI's clone.
// Its branches stay on origin. It journals what it removed, once.
func (d *daemon) removeJobLocal(job int64) {
	d.assignMu.Lock()
	defer d.assignMu.Unlock()
	if done, err := d.store.LocalRemoved(job); err != nil || done {
		return
	}
	j, err := d.store.Job(job)
	if err != nil || j.State != store.JobEnded {
		return
	}
	repository, err := d.store.Repository(j.Repository)
	if err != nil {
		d.log.Error("reading the repository of a job that ended", "job", job, "error", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(d.work), gitTimeout)
	defer cancel()
	var removed []string
	var failed error
	assignments, err := d.store.AssignmentsForJob(job)
	if err != nil {
		d.log.Error("reading the assignments of a job that ended", "job", job, "error", err.Error())
		return
	}
	for _, a := range assignments {
		if _, err := os.Lstat(a.Workspace.Path); err == nil {
			if err := repos.RemoveView(ctx, repository.Clone, a.Workspace.Path); err != nil {
				failed = errors.Join(failed, err)
			} else {
				removed = append(removed, a.Workspace.Path)
			}
		}
		if a.Workspace.Branch != "" {
			deleted, err := repos.DeleteBranch(ctx, repository.Clone, a.Workspace.Branch)
			failed = errors.Join(failed, err)
			if deleted {
				removed = append(removed, "refs/heads/"+a.Workspace.Branch)
			}
		}
	}
	if dir := filepath.Join(d.cfg.Paths.Workspaces, fmt.Sprintf("job-%d", job)); exists(dir) {
		if err := os.RemoveAll(dir); err != nil {
			failed = errors.Join(failed, err)
		} else {
			removed = append(removed, dir)
		}
	}
	branch, err := d.store.JobBranch(job)
	switch {
	case err == nil:
		if exists(branch.View) {
			if err := repos.RemoveView(ctx, repository.Clone, branch.View); err != nil {
				failed = errors.Join(failed, err)
			} else {
				removed = append(removed, branch.View)
			}
		}
		deleted, err := repos.DeleteBranch(ctx, repository.Clone, branch.Name)
		failed = errors.Join(failed, err)
		if deleted {
			removed = append(removed, "refs/heads/"+branch.Name)
		}
	case !errors.Is(err, sql.ErrNoRows):
		failed = errors.Join(failed, err)
	}
	if failed != nil {
		d.log.Error("removing what is local to a job that ended; the next start tries again", "job", job, "error", failed.Error())
		return
	}
	if err := d.store.JobLocalRemoved(job, removed, time.Now()); err != nil {
		d.log.Error("journaling the removal of what was local to a job that ended", "job", job, "error", err.Error())
		return
	}
	d.log.Info("job's local workspaces, view and branches removed; its branches stay on origin", "job", job, "removed", len(removed))
}

// removeEndedJobsLocal removes what is still local to each job that ended,
// as a stop may have left it.
func (d *daemon) removeEndedJobsLocal() {
	jobs, err := d.store.Jobs()
	if err != nil {
		d.log.Error("reading the jobs", "error", err.Error())
		return
	}
	for _, j := range jobs {
		if j.State == store.JobEnded {
			d.removeJobLocal(j.Number)
		}
	}
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
