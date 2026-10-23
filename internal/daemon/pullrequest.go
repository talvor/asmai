// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/talvor/asmai/internal/github"
	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/store"
)

// ghTimeout is how long looking a pull request up with gh may take.
const ghTimeout = 2 * time.Minute

// delivery is a delivery owner's call about its job's pull request: who asks,
// the dispatch it works on, and the job it delivers with its branch and
// repository.
type delivery struct {
	ref        sessionRef
	dispatch   int64
	job        store.Job
	branch     store.JobBranch
	repository store.Repository
}

// deliveryCall returns the delivery req.Job's owner makes in its current
// dispatch, or why req's session may not: only the job's delivery owner, while
// it works on a dispatch, for an open job that has a job branch.
func (d *daemon) deliveryCall(req Request) (delivery, error) {
	d.mu.Lock()
	ref, l, err := d.caller(req)
	dispatch := int64(0)
	if err == nil {
		dispatch = l.currentDispatch
	}
	d.mu.Unlock()
	if err != nil {
		return delivery{}, err
	}
	if !isDeliveryOwner(ref.address) {
		return delivery{}, refuse("only %s, the delivery owner of a code job, %s or %s, the delivery owner of a notes job, or %s, the delivery owner of a repository's setup, may run asmai pr", roles.LeaderOf(roles.Engineering), roles.LeaderOf(roles.Planning), roles.LeaderOf(roles.Research), roles.LeaderOf(roles.Coordination))
	}
	if dispatch == 0 {
		return delivery{}, refuse("%s has no current dispatch to tag the pull request with; ask while working on a dispatch you fetched with `asmai inbox`", ref.address)
	}
	j, err := d.store.Job(req.Job)
	if errors.Is(err, sql.ErrNoRows) {
		return delivery{}, refuse("job %d does not exist; `asmai jobs` lists the jobs", req.Job)
	}
	if err != nil {
		return delivery{}, err
	}
	if j.State != store.JobOpen {
		return delivery{}, refuse("job %d is %s, so its pull request is not changed", j.Number, j.State)
	}
	if owner := deliveryOwner(j); ref.address != owner {
		return delivery{}, refuse("%s owns the delivery of job %d, a %s job, so %s does not change its pull request", owner, j.Number, j.Mandate, ref.address)
	}
	branch, err := d.store.JobBranch(j.Number)
	if errors.Is(err, sql.ErrNoRows) {
		return delivery{}, refuse("job %d has no job branch, so it has no pull request", j.Number)
	}
	if err != nil {
		return delivery{}, err
	}
	repository, err := d.store.Repository(j.Repository)
	if errors.Is(err, sql.ErrNoRows) {
		return delivery{}, notRegistered(j.Repository)
	}
	if err != nil {
		return delivery{}, err
	}
	return delivery{ref: ref, dispatch: dispatch, job: j, branch: branch, repository: repository}, nil
}

// pullRequestEffect records an effect of kind store.EffectPullRequest that
// req's session made in its current dispatch. Of the delivery owner of the
// job the dispatch is about, Engineering's leader for a code job and
// Planning's or Research's for a notes job, it is how the daemon learns which
// pull request to follow: the daemon looks the pull request req.Ref names up with gh, in the
// job's clone, and records it as GitHub has it, with its number and whether
// it is a draft or opened with "[not ready]" at the start of its title, in
// the same step as the effect. It must be open, in the job's repository, the
// clone's origin, and from the job branch. When it cannot be looked up,
// nothing is recorded. Anyone else's is an effect like any other.
func (d *daemon) pullRequestEffect(req Request) (store.Effect, *store.PullRequest, error) {
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
	if !isDeliveryOwner(ref.address) {
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
	// Another leader's effect about a job it does not deliver is an effect
	// like any other.
	if ref.address != deliveryOwner(j) {
		e, err := d.effect(req)
		return e, nil, err
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
	c := delivery{ref: ref, dispatch: dispatch, job: j, branch: branch, repository: repository}
	ctx, cancel := context.WithTimeout(d.work, ghTimeout)
	defer cancel()
	p, e, err := d.recordPullRequest(ctx, c, req.Ref)
	return e, p, err
}

// recordPullRequest looks the pull request ref names up with gh, in the job's
// clone, and records it for c as GitHub has it, with the effect that tells
// the daemon which pull request to follow. It refuses one that is not open,
// not in the job's repository or not from the job branch, and then records
// nothing.
func (d *daemon) recordPullRequest(ctx context.Context, c delivery, ref string) (*store.PullRequest, store.Effect, error) {
	found, err := github.ViewPullRequest(ctx, c.repository.Clone, ref)
	if err != nil {
		return nil, store.Effect{}, refuse("nothing is recorded: the daemon could not look up the pull request %s with gh: %v. Record it again with `asmai effect pull-request <the URL gh printed>`", ref, err)
	}
	origin, err := github.RepositoryURL(ctx, c.repository.Clone)
	if err != nil {
		return nil, store.Effect{}, refuse("nothing is recorded: the daemon could not look up job %d's repository with gh: %v", c.job.Number, err)
	}
	if !found.In(origin) {
		return nil, store.Effect{}, refuse("nothing is recorded: pull request %s is not from a branch of job %d's repository %s. Open the pull request in %s from the job branch and record that one", found.URL, c.job.Number, origin, origin)
	}
	if found.State != "OPEN" {
		return nil, store.Effect{}, refuse("nothing is recorded: pull request #%d is %s, not open. Open a pull request from the job branch and record that one", found.Number, found.State)
	}
	if found.HeadBranch != c.branch.Name {
		return nil, store.Effect{}, refuse("nothing is recorded: pull request #%d is from %s, not the job branch %s. Open the pull request from the job branch and record that one", found.Number, found.HeadBranch, c.branch.Name)
	}
	p := store.PullRequest{
		Job: c.job.Number, Number: found.Number, URL: found.URL, Title: found.Title,
		Draft: found.Draft, NotReady: found.IsNotReady(),
		Branch: found.HeadBranch, Head: found.HeadCommit, State: found.State,
		Leader: c.ref.address.String(), Dispatch: c.dispatch,
	}
	p, e, err := d.store.PullRequestRecorded(p, c.ref.generation, ref, time.Now())
	if err != nil {
		return nil, store.Effect{}, refuse("%v", err)
	}
	d.log.Info("pull request recorded", "job", c.job.Number, "number", p.Number, "draft", p.Draft, "not_ready", p.NotReady, "effect", e.ID)
	return &p, e, nil
}

// openPullRequest opens the pull request of req.Job for its delivery owner,
// from the job branch the daemon pushed to the repository's default branch,
// with the title and body the request carries, as a draft. If GitHub refuses
// a draft it opens normally with "[not ready]" at the start of its title. It
// then records the pull request as pullRequestEffect does, so there is no
// second step for the delivery owner. It never opens a second pull request:
// the job's own, one from its branch that GitHub already has, and one a
// follow-up continues are named in its refusal instead.
func (d *daemon) openPullRequest(req Request) (*store.PullRequest, *store.Effect, error) {
	c, err := d.deliveryCall(req)
	if err != nil {
		return nil, nil, err
	}
	title := strings.TrimSpace(req.Title)
	if title == "" || strings.TrimSpace(req.Body) == "" {
		return nil, nil, refuse("a pull request needs a title and a body: give --title and --body-file")
	}
	if c.branch.Continues != 0 {
		pr, err := d.continuedPullRequest(c.branch.Continues)
		if err != nil {
			return nil, nil, err
		}
		return nil, nil, refuse("job %d continues pull request #%d, %s, which is this job's: record it with `asmai effect pull-request %s` and update its body with `asmai pr update %d --body-file <file>`, and open no other", c.job.Number, pr.Number, pr.URL, pr.URL, c.job.Number)
	}
	recorded, err := d.store.PullRequests(c.job.Number)
	if err != nil {
		return nil, nil, err
	}
	if len(recorded) > 0 {
		last := recorded[len(recorded)-1]
		return nil, nil, refuse("job %d already has pull request #%d, %s: update its body with `asmai pr update %d --body-file <file>`, and open no other", c.job.Number, last.Number, last.URL, c.job.Number)
	}
	pushes, err := d.store.Pushes(c.job.Number)
	if err != nil {
		return nil, nil, err
	}
	if !slices.ContainsFunc(pushes, func(p store.Push) bool { return p.Result == store.PushPushed }) {
		return nil, nil, refuse("job %d's branch %s is not pushed, so there is nothing to open a pull request from: ask for the push with `asmai push %d` first", c.job.Number, c.branch.Name, c.job.Number)
	}
	ctx, cancel := context.WithTimeout(d.work, 3*ghTimeout)
	defer cancel()
	repository, err := github.Repository(ctx, c.repository.Clone)
	if err != nil {
		return nil, nil, refuse("the daemon could not look up job %d's repository with gh: %v", c.job.Number, err)
	}
	existing, err := github.PullRequestsFrom(ctx, c.repository.Clone, repository, c.branch.Name)
	if err != nil {
		return nil, nil, refuse("the daemon could not look for a pull request from %s with gh: %v. Nothing is opened", c.branch.Name, err)
	}
	if len(existing) > 0 {
		return nil, nil, refuse("%s already has pull request #%d, %s, which is %s: record it with `asmai effect pull-request %s` instead of opening another", c.branch.Name, existing[0].Number, existing[0].URL, existing[0].State, existing[0].URL)
	}
	create := github.Create{Repository: repository, Base: c.repository.DefaultBranch, Head: c.branch.Name, Title: title, Body: req.Body, Draft: true}
	url, err := github.CreatePullRequest(ctx, c.repository.Clone, create)
	if github.DraftsRefused(err) {
		create.Draft = false
		create.Title = github.NotReady + " " + title
		url, err = github.CreatePullRequest(ctx, c.repository.Clone, create)
	}
	if err != nil {
		return nil, nil, refuse("gh could not open the pull request: %v. Ask again: if it was opened after all, the daemon names it", err)
	}
	p, e, err := d.recordPullRequest(ctx, c, url)
	if err != nil {
		return nil, nil, fmt.Errorf("the pull request was opened, %s, but %w", url, err)
	}
	d.log.Info("pull request opened", "job", c.job.Number, "number", p.Number, "draft", p.Draft, "effect", e.ID)
	return p, &e, nil
}

// updatePullRequest replaces the body of the pull request the daemon follows
// for req.Job with req.Body, for the job's delivery owner, and records the
// pull request again. It changes the body and nothing else: the title and
// whether it is a draft are the daemon's to change when the checks are green.
func (d *daemon) updatePullRequest(req Request) (*store.PullRequest, *store.Effect, error) {
	c, err := d.deliveryCall(req)
	if err != nil {
		return nil, nil, err
	}
	if strings.TrimSpace(req.Body) == "" {
		return nil, nil, refuse("a pull request's body is not blank: give --body-file")
	}
	number, url := 0, ""
	if recorded, err := d.store.PullRequests(c.job.Number); err != nil {
		return nil, nil, err
	} else if len(recorded) > 0 {
		number, url = recorded[len(recorded)-1].Number, recorded[len(recorded)-1].URL
	} else if c.branch.Continues != 0 {
		pr, err := d.continuedPullRequest(c.branch.Continues)
		if err != nil {
			return nil, nil, err
		}
		number, url = pr.Number, pr.URL
	}
	if number == 0 {
		return nil, nil, refuse("job %d has no pull request recorded: open one with `asmai pr open %d --title <title> --body-file <file>`", c.job.Number, c.job.Number)
	}
	ctx, cancel := context.WithTimeout(d.work, 3*ghTimeout)
	defer cancel()
	repository, err := github.Repository(ctx, c.repository.Clone)
	if err != nil {
		return nil, nil, refuse("the daemon could not look up job %d's repository with gh: %v", c.job.Number, err)
	}
	if err := github.EditBody(ctx, c.repository.Clone, repository, number, req.Body); err != nil {
		return nil, nil, refuse("gh could not update the body of pull request #%d: %v. Nothing is recorded", number, err)
	}
	p, e, err := d.recordPullRequest(ctx, c, url)
	if err != nil {
		return nil, nil, fmt.Errorf("the body of pull request #%d was updated, but %w", number, err)
	}
	return p, &e, nil
}
