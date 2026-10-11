// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/talvor/asmai/internal/repos"
	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/store"
)

// pushCommand serves push.
func (d *daemon) pushCommand(conn *net.UnixConn, req Request) {
	conn.SetDeadline(time.Now().Add(2*gitTimeout + time.Minute))
	var resp Response
	p, err := d.push(req)
	if err != nil {
		if !errors.As(err, new(*userError)) {
			d.log.Error("a push failed", "job", req.Job, "error", err.Error())
		}
		resp.Error = err.Error()
	} else {
		resp.Push = &p
	}
	reply(conn, resp)
}

// push pushes the job branch of req.Job to origin for its delivery owner,
// Engineering's leader, who asks while working on a dispatch. The daemon
// first fetches the branch from origin. If origin's branch has commits AsmAI
// did not make, nothing is pushed: the refusal is recorded as an outside
// change, for a worker to merge in. Otherwise the branch is pushed with a
// plain push, never forced, and the push is recorded as the daemon's own
// effect, tagged with the leader's dispatch, with the earlier outside changes
// it took in. Nothing the repository says changes how the branch is pushed.
func (d *daemon) push(req Request) (store.Push, error) {
	owner := roles.LeaderOf(roles.Engineering)
	d.mu.Lock()
	ref, l, err := d.caller(req)
	dispatch := int64(0)
	if err == nil {
		dispatch = l.currentDispatch
	}
	d.mu.Unlock()
	if err != nil {
		return store.Push{}, err
	}
	if ref.address != owner {
		return store.Push{}, refuse("only %s, the delivery owner of a code job, may run asmai push", owner)
	}
	if dispatch == 0 {
		return store.Push{}, refuse("%s has no current dispatch to tag the push with; ask for the push while working on a dispatch you fetched with `asmai inbox`", ref.address)
	}

	// Holding assignMu, no acceptance moves the job branch while it is pushed.
	d.assignMu.Lock()
	defer d.assignMu.Unlock()
	j, err := d.store.Job(req.Job)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Push{}, refuse("job %d does not exist; `asmai jobs` lists the jobs", req.Job)
	}
	if err != nil {
		return store.Push{}, err
	}
	if j.State != store.JobOpen {
		return store.Push{}, refuse("job %d is %s, so its job branch is not pushed", j.Number, j.State)
	}
	branch, err := d.store.JobBranch(j.Number)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Push{}, refuse("job %d has no job branch, so there is nothing to push", j.Number)
	}
	if err != nil {
		return store.Push{}, err
	}
	repository, err := d.store.Repository(j.Repository)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Push{}, notRegistered(j.Repository)
	}
	if err != nil {
		return store.Push{}, err
	}
	ctx, cancel := context.WithTimeout(d.work, 2*gitTimeout)
	defer cancel()
	current, err := repos.Commit(ctx, repository.Clone, "refs/heads/"+branch.Name)
	if err != nil {
		return store.Push{}, fmt.Errorf("reading the job branch: %w", err)
	}
	if current != branch.Tip {
		return store.Push{}, fmt.Errorf("the job branch %s is at %s, not at its recorded tip %s, so it is not pushed", branch.Name, current, branch.Tip)
	}
	tookIn, err := d.outsideTakenIn(ctx, repository.Clone, j.Number, branch.Tip)
	if err != nil {
		return store.Push{}, err
	}
	p := store.Push{Job: j.Number, Branch: branch.Name, Commit: branch.Tip, Leader: ref.address.String(), Dispatch: dispatch}
	outside, err := repos.PushBranch(ctx, repository.Clone, branch.Name)
	if err != nil {
		return store.Push{}, fmt.Errorf("%w; nothing is recorded as pushed. If origin's branch moved since the fetch, ask for the push again", err)
	}
	if len(outside) > 0 {
		p.Outside = outside
		if p, err = d.store.OutsideChangeFound(p, ref.generation, time.Now()); err != nil {
			return store.Push{}, err
		}
		d.log.Info("push refused over outside commits", "job", j.Number, "branch", branch.Name, "outside", len(outside))
		return store.Push{}, refuse("nothing was pushed: %s has %d commit(s) AsmAI did not make, %s, recorded as outside change %d. Assign a worker to merge origin/%s into its assignment branch, keeping them, accept its result, have Quality validate the new head, and ask for the push again",
			p.Found, len(outside), strings.Join(outside, ", "), p.ID, branch.Name)
	}
	if p, err = d.store.JobBranchPushed(p, ref.generation, tookIn, time.Now()); err != nil {
		return store.Push{}, err
	}
	d.log.Info("job branch pushed", "job", j.Number, "branch", branch.Name, "commit", p.Commit, "effect", p.Effect)
	return p, nil
}

// outsideTakenIn returns the outside changes of job not yet taken in whose
// commits tip has in its history.
func (d *daemon) outsideTakenIn(ctx context.Context, clone string, job int64, tip string) ([]int64, error) {
	pushes, err := d.store.Pushes(job)
	if err != nil {
		return nil, err
	}
	var taken []int64
	for _, p := range pushes {
		if p.Result != store.PushRefused || p.MergedIn != "" {
			continue
		}
		all := true
		for _, commit := range p.Outside {
			has, err := repos.Contains(ctx, clone, tip, commit)
			if err != nil {
				return nil, err
			}
			all = all && has
		}
		if all {
			taken = append(taken, p.ID)
		}
	}
	return taken, nil
}
