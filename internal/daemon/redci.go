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

	"github.com/talvor/asmai/internal/github"
	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/store"
)

// tellRed tells the delivery owner, Engineering's leader, that the checks of
// o, on the head of j's pull request the daemon pushed, are red, naming the
// failing ones, once each time they turn red. The leader is started if it is
// not running.
func (d *daemon) tellRed(j store.Job, o store.ChecksObservation) {
	owner := roles.LeaderOf(roles.Engineering)
	earlier, err := d.store.RerunOf(j.Number, o.Head)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		d.log.Error("reading the re-runs of a job's head", "job", j.Number, "error", err.Error())
		return
	}
	var rerun *store.Rerun
	if err == nil {
		rerun = &earlier
	}
	dispatch, told, err := d.store.ChecksRed(o, owner.String(), redBody(j, o, rerun), time.Now())
	if err != nil {
		d.log.Error("telling the delivery owner the checks are red", "job", j.Number, "error", err.Error())
		return
	}
	if !told {
		return
	}
	d.log.Info("checks red; the delivery owner is told", "job", j.Number, "number", o.Number, "head", o.Head, "failed", store.CheckNames(store.FailedChecks(o.Checks)), "dispatch", dispatch.ID)
	d.mu.Lock()
	if err := d.ensureAgent(owner); err != nil {
		d.log.Error("starting the delivery owner", "agent", owner.String(), "dispatch", dispatch.ID, "error", err.Error())
	}
	d.mu.Unlock()
	d.nudge(owner.String())
}

// redBody is what the delivery owner is told of the red checks of o: the
// failing checks on the head, and what it may do. When rerun is the head's
// one re-run, a second is refused, and the fix is all that is left.
func redBody(j store.Job, o store.ChecksObservation, rerun *store.Rerun) string {
	var failing []string
	for _, c := range store.FailedChecks(o.Checks) {
		if c.URL != "" {
			failing = append(failing, c.Name+" ("+c.URL+")")
		} else {
			failing = append(failing, c.Name)
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "The checks on head %s of job %d's pull request #%d are red. Failing: %s. ", o.Head, j.Number, o.Number, strings.Join(failing, ", "))
	if rerun != nil {
		fmt.Fprintf(&b, "Their failed jobs were re-run once already, on decision %d, and failed again: a second re-run of this head is refused. Assign", rerun.Decision)
	} else {
		fmt.Fprintf(&b, "If you judge the failure unrelated to the change, you may re-run the failed jobs once for this head, recording your judgment: `asmai decision record --job %d --choice %s --rationale <why the failure is unrelated to the change> --evidence <what shows it>`. Otherwise, assign", j.Number, store.ChoiceRerunFailed)
	}
	b.WriteString(" a fix as a new writing assignment naming the failing checks; accept its result, hand validation of the new head to Quality, and once Quality passes it ask for the push with `asmai push`. The daemon then watches the new head and drops the wait on this one. ")
	b.WriteString("Update the pull request body with the CI results, and record each update with `asmai effect pull-request <url>`.")
	return b.String()
}

// decisionRecordCommand serves decision.record.
func (d *daemon) decisionRecordCommand(conn *net.UnixConn, req Request) {
	conn.SetDeadline(time.Now().Add(ghTimeout + time.Minute))
	var resp Response
	record, rerun, err := d.recordDecision(req)
	if record.ID != 0 {
		resp.DecisionRecord = &record
		if rerun.ID != 0 {
			resp.Rerun = &rerun
		}
	}
	if err != nil {
		if !errors.As(err, new(*userError)) {
			d.log.Error("recording a decision failed", "job", req.Job, "error", err.Error())
		}
		resp.Error = err.Error()
	}
	reply(conn, resp)
}

// recordDecision records the decision of req, a leader's, about req.Job, with
// the fields of rule 25: the leader, the job, the mandate or grant relied on,
// the choice, its rationale and evidence, its material assumptions and the
// conditions that would reopen it, and its source. Any choice is recorded as
// it is, tagged with the dispatch the leader is working on, if it is working
// on one, and a record is made only when the leader chooses to make one:
// routine edits need none. The choice rerun-failed is Engineering's leader's
// judged re-run of a head's red checks, one kind of decision, on which the
// daemon also re-runs the failed jobs.
func (d *daemon) recordDecision(req Request) (store.DecisionRecord, store.Rerun, error) {
	d.mu.Lock()
	ref, l, err := d.caller(req)
	dispatch := int64(0)
	if err == nil {
		dispatch = l.currentDispatch
	}
	d.mu.Unlock()
	if err != nil {
		return store.DecisionRecord{}, store.Rerun{}, err
	}
	if ref.address.Name != roles.Leader {
		return store.DecisionRecord{}, store.Rerun{}, refuse("only a role's leader may run asmai decision record: a worker's decision goes to its owning leader")
	}
	if req.LatestWitness {
		if req.Cites, err = d.store.LatestWitnessed(ref.address.String()); err != nil {
			return store.DecisionRecord{}, store.Rerun{}, refuse("%v", err)
		}
	}
	record := store.DecisionRecord{
		Job: req.Job, Leader: ref.address.String(), Source: req.DecisionSource, Cites: req.Cites, ReliedOn: req.ReliedOn,
		Choice: strings.TrimSpace(req.Choice), Rationale: req.Rationale, Evidence: req.Evidence,
		Assumptions: req.Assumptions, ReopenWhen: req.ReopenWhen, Dispatch: dispatch,
	}
	if record.Source == "" {
		record.Source = store.SourceDelegated
	}
	if record.Choice == store.ChoiceRerunFailed {
		if record.Source != store.SourceDelegated {
			return store.DecisionRecord{}, store.Rerun{}, refuse("a judged re-run is the leader's own, delegated decision: --choice %s takes no other source", store.ChoiceRerunFailed)
		}
		return d.recordRerun(req, ref, record)
	}
	if err := record.Check(); err != nil {
		return store.DecisionRecord{}, store.Rerun{}, refuse("%v", err)
	}
	record, err = d.store.DecisionRecorded(record, ref.generation, time.Now())
	if err != nil {
		return store.DecisionRecord{}, store.Rerun{}, refuse("%v", err)
	}
	d.log.Info("decision recorded", "job", record.Job, "decision", record.ID, "leader", record.Leader, "source", record.Source, "choice", record.Choice)
	return record, store.Rerun{}, nil
}

// recordRerun records record, Engineering's leader's judgment that the red
// checks on the head of req.Job's pull request the daemon pushed failed for a
// reason unrelated to the change, as a delegated decision. On it, the daemon
// re-runs the failed jobs of those checks with gh, in AsmAI's clone, and
// records the re-run with the decision. It re-runs a head's failed jobs once:
// a second re-run of the same head is refused, and nothing is recorded when
// nothing was re-run.
func (d *daemon) recordRerun(req Request, ref sessionRef, record store.DecisionRecord) (store.DecisionRecord, store.Rerun, error) {
	owner := roles.LeaderOf(roles.Engineering)
	dispatch := record.Dispatch
	if ref.address != owner {
		return store.DecisionRecord{}, store.Rerun{}, refuse("only %s, judging a red head's checks, may record --choice %s", owner, store.ChoiceRerunFailed)
	}
	if dispatch == 0 {
		return store.DecisionRecord{}, store.Rerun{}, refuse("%s has no current dispatch to tag the decision with; record it while working on a dispatch you fetched with `asmai inbox`", ref.address)
	}
	if strings.TrimSpace(req.Rationale) == "" || strings.TrimSpace(req.Evidence) == "" {
		return store.DecisionRecord{}, store.Rerun{}, refuse("a decision gives its --rationale and its --evidence")
	}

	// Holding assignMu, one re-run of a head is decided at a time.
	d.assignMu.Lock()
	defer d.assignMu.Unlock()
	j, err := d.store.Job(req.Job)
	if errors.Is(err, sql.ErrNoRows) {
		return store.DecisionRecord{}, store.Rerun{}, refuse("job %d does not exist; `asmai jobs` lists the jobs", req.Job)
	}
	if err != nil {
		return store.DecisionRecord{}, store.Rerun{}, err
	}
	if j.State != store.JobOpen {
		return store.DecisionRecord{}, store.Rerun{}, refuse("job %d is %s, so its checks are not re-run", j.Number, j.State)
	}
	if job, err := d.store.WorkingOn(dispatch, ref.address.String(), ref.generation); err != nil {
		return store.DecisionRecord{}, store.Rerun{}, refuse("%v", err)
	} else if job != j.Number {
		return store.DecisionRecord{}, store.Rerun{}, refuse("dispatch %d, the one %s is working on, is of job %d, not job %d; record the decision while working on a dispatch of job %d", dispatch, ref.address, job, j.Number, j.Number)
	}
	pr, head, ok := d.watched(j.Number)
	if !ok {
		return store.DecisionRecord{}, store.Rerun{}, refuse("job %d has no pushed head with a pull request whose checks the daemon watches, so there is nothing to re-run", j.Number)
	}
	if earlier, err := d.store.RerunOf(j.Number, head); err == nil {
		return store.DecisionRecord{}, store.Rerun{}, &userError{store.RerunRefused(earlier)}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return store.DecisionRecord{}, store.Rerun{}, err
	}
	o, err := d.store.LatestChecks(j.Number)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return store.DecisionRecord{}, store.Rerun{}, err
	}
	if err != nil || o.Head != head || o.Verdict != github.Red {
		return store.DecisionRecord{}, store.Rerun{}, refuse("the checks on %s, the head of job %d the daemon pushed, are not red, so there is nothing to re-run; `asmai brief %d` shows them", head, j.Number, j.Number)
	}
	failed := store.FailedChecks(o.Checks)
	checks := make([]github.Check, 0, len(failed))
	for _, c := range failed {
		checks = append(checks, github.Check(c))
	}
	runs := github.FailedRuns(checks)
	if len(runs) == 0 {
		return store.DecisionRecord{}, store.Rerun{}, refuse("none of the failed checks on %s (%s) is a GitHub Actions run the daemon can re-run; assign a fix", head, store.CheckNames(failed))
	}
	repository, err := d.store.Repository(j.Repository)
	if errors.Is(err, sql.ErrNoRows) {
		return store.DecisionRecord{}, store.Rerun{}, notRegistered(j.Repository)
	}
	if err != nil {
		return store.DecisionRecord{}, store.Rerun{}, err
	}
	ctx, cancel := context.WithTimeout(d.work, ghTimeout)
	defer cancel()
	rerun, ghErr := github.RerunFailed(ctx, repository.Clone, runs)
	if len(rerun) == 0 {
		return store.DecisionRecord{}, store.Rerun{}, refuse("nothing was re-run and nothing is recorded: %v", ghErr)
	}
	r := store.Rerun{Job: j.Number, Number: pr.Number, Head: head, Runs: rerun, Checks: failed}
	record, r, err = d.store.RerunDecided(record, r, ref.generation, time.Now())
	if err != nil {
		return store.DecisionRecord{}, store.Rerun{}, fmt.Errorf("gh re-ran runs %v of head %s, but the decision and its re-run were not recorded: %w", rerun, head, err)
	}
	d.log.Info("failed jobs re-run on a delegated decision", "job", j.Number, "head", head, "runs", fmt.Sprint(r.Runs), "decision", record.ID, "leader", record.Leader)
	d.lookSoon()
	if ghErr != nil {
		return record, r, refuse("the daemon re-ran runs %v of head %s on decision %d, but gh did not re-run the rest: %v. The head's one re-run is used: if its checks stay red, assign a fix", r.Runs, head, record.ID, ghErr)
	}
	return record, r, nil
}
