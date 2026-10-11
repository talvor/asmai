// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/talvor/asmai/internal/github"
	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/store"
)

// ghTimeout is how long looking a pull request up with gh may take.
const ghTimeout = 2 * time.Minute

// pullRequestEffect records an effect of kind store.EffectPullRequest that
// req's session made in its current dispatch. Of the delivery owner of a code
// job, Engineering's leader, it is how the daemon learns which pull request to
// follow: the daemon looks the pull request req.Ref names up with gh, in the
// job's clone, and records it as GitHub has it, with its number and whether
// it is a draft or opened with "[not ready]" at the start of its title, in
// the same step as the effect. It must be open, in the job's repository, the
// clone's origin, and from the job branch. When it cannot be looked up,
// nothing is recorded. Anyone else's is an effect like any
// other.
func (d *daemon) pullRequestEffect(req Request) (store.Effect, *store.PullRequest, error) {
	owner := roles.LeaderOf(roles.Engineering)
	d.mu.Lock()
	ref, l, err := d.caller(req)
	dispatch := int64(0)
	if err == nil {
		dispatch = l.currentDispatch
	}
	d.mu.Unlock()
	if err != nil {
		return store.Effect{}, nil, err
	}
	if ref.address != owner {
		e, err := d.effect(req)
		return e, nil, err
	}
	if dispatch == 0 {
		return store.Effect{}, nil, refuse("%s has no current dispatch to tag the effect with; record effects while working on a dispatch you fetched with `asmai inbox`", ref.address)
	}
	job, err := d.store.DispatchJob(dispatch)
	if err != nil {
		return store.Effect{}, nil, err
	}
	j, err := d.store.Job(job)
	if err != nil {
		return store.Effect{}, nil, err
	}
	branch, err := d.store.JobBranch(j.Number)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Effect{}, nil, refuse("job %d has no job branch, so it has no pull request to record; nothing is recorded", j.Number)
	}
	if err != nil {
		return store.Effect{}, nil, err
	}
	repository, err := d.store.Repository(j.Repository)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Effect{}, nil, notRegistered(j.Repository)
	}
	if err != nil {
		return store.Effect{}, nil, err
	}
	ctx, cancel := context.WithTimeout(d.work, ghTimeout)
	defer cancel()
	found, err := github.ViewPullRequest(ctx, repository.Clone, req.Ref)
	if err != nil {
		return store.Effect{}, nil, refuse("nothing is recorded: the daemon could not look up the pull request %s with gh: %v. Record it again with `asmai effect pull-request <the URL gh printed>`", req.Ref, err)
	}
	origin, err := github.RepositoryURL(ctx, repository.Clone)
	if err != nil {
		return store.Effect{}, nil, refuse("nothing is recorded: the daemon could not look up job %d's repository with gh: %v", j.Number, err)
	}
	if !found.In(origin) {
		return store.Effect{}, nil, refuse("nothing is recorded: pull request %s is not from a branch of job %d's repository %s. Open the pull request in %s from the job branch and record that one", found.URL, j.Number, origin, origin)
	}
	if found.State != "OPEN" {
		return store.Effect{}, nil, refuse("nothing is recorded: pull request #%d is %s, not open. Open a pull request from the job branch and record that one", found.Number, found.State)
	}
	if found.HeadBranch != branch.Name {
		return store.Effect{}, nil, refuse("nothing is recorded: pull request #%d is from %s, not the job branch %s. Open the pull request from the job branch and record that one", found.Number, found.HeadBranch, branch.Name)
	}
	p := store.PullRequest{
		Job: j.Number, Number: found.Number, URL: found.URL, Title: found.Title,
		Draft: found.Draft, NotReady: found.IsNotReady(),
		Branch: found.HeadBranch, Head: found.HeadCommit, State: found.State,
		Leader: ref.address.String(), Dispatch: dispatch,
	}
	p, e, err := d.store.PullRequestRecorded(p, ref.generation, req.Ref, time.Now())
	if err != nil {
		return store.Effect{}, nil, refuse("%v", err)
	}
	d.log.Info("pull request recorded", "job", j.Number, "number", p.Number, "draft", p.Draft, "not_ready", p.NotReady, "effect", e.ID)
	return e, &p, nil
}
