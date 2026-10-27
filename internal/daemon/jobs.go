// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/talvor/asmai/internal/store"
)

func (d *daemon) jobCommand(conn *net.UnixConn, req Request) {
	var resp Response
	var err error
	switch req.Command {
	case CommandJobOpen:
		var j store.Job
		// Keep the generation fixed until the job is committed. A session
		// ending while this request is in flight cannot turn a stale call
		// into a new job after its replacement starts.
		d.mu.Lock()
		ref, ok := d.sessions[req.Session]
		if !ok || ref.address.String() != "leader@coordination" || d.leaders[ref.address.String()] == nil || d.leaders[ref.address.String()].session == nil || d.leaders[ref.address.String()].generation != ref.generation || d.stopping {
			err = refuse("unknown or superseded agent session credential")
		} else {
			j, err = d.store.JobOpened(store.Job{Repository: req.Repository, Witness: req.Witness, Reading: req.Reading, Mandate: req.Mandate, Criteria: req.Criteria}, req.LatestWitness, time.Now())
		}
		d.mu.Unlock()
		if err == nil {
			resp.Job = &j
			// Making the branch may fetch from origin, which takes longer
			// than a request is usually given.
			conn.SetDeadline(time.Now().Add(gitTimeout + time.Minute))
			// A job with no repository has no job branch to make.
			if j.Repository != "" {
				d.makeJobBranch(j)
			}
		}
	case CommandJobLink:
		var j store.Job
		d.mu.Lock()
		if _, _, err = d.caller(req); err == nil {
			j, err = d.store.JobLinked(req.Job, req.Follows, time.Now())
		}
		d.mu.Unlock()
		if err != nil {
			err = refuse("%v", err)
		} else {
			resp.Job = &j
			conn.SetDeadline(time.Now().Add(2*(2*gitTimeout+ghTimeout) + time.Minute))
			if j.Repository != "" {
				err = d.settleFollowUp(j, &resp)
			}
		}
	case CommandJobCancel:
		// Closing the pull request goes through gh, and the workers'
		// workspaces are read for what they left.
		conn.SetDeadline(time.Now().Add(2*ghTimeout + gitTimeout + time.Minute))
		var c store.JobCancellation
		if c, err = d.cancelJob(req); err == nil {
			resp.Cancellation, resp.Job = &c, &c.Job
			defer func() {
				for _, dispatch := range c.Dispatches {
					d.nudge(dispatch.Agent)
				}
			}()
		}
	case CommandJobClean:
		resp.Removed, err = d.cleanScratch(req.Job)
	case CommandJobs:
		resp.Jobs, err = d.store.Jobs()
	case CommandJob, CommandBrief:
		var j store.Job
		j, err = d.store.Job(req.Job)
		if errors.Is(err, sql.ErrNoRows) {
			err = refuse("job %d does not exist; `asmai jobs` lists the jobs", req.Job)
		}
		if err == nil {
			resp.Job = &j
			if req.Command == CommandBrief {
				var repository store.Repository
				if j.Repository != "" {
					repository, err = d.store.Repository(j.Repository)
					if errors.Is(err, sql.ErrNoRows) {
						err = nil
					}
				}
				var entries []store.Entry
				if err == nil {
					entries, err = d.store.JobEntries(j.Number)
				}
				if err == nil {
					var handoffs []store.Handoff
					handoffs, err = d.store.HandoffsForJob(j.Number)
					var assignments []store.Assignment
					if err == nil {
						assignments, err = d.store.AssignmentsForJob(j.Number)
					}
					var findings []store.Finding
					if err == nil {
						findings, err = d.store.Findings(j.Number)
					}
					var pushes []store.Push
					if err == nil {
						pushes, err = d.store.Pushes(j.Number)
					}
					var prs []store.PullRequest
					if err == nil {
						prs, err = d.store.PullRequests(j.Number)
					}
					var branch *store.JobBranch
					if b, e := d.store.JobBranch(j.Number); e == nil {
						branch = &b
					} else if !errors.Is(e, sql.ErrNoRows) {
						err = e
					}
					var continued []store.PullRequest
					if branch != nil && branch.Continues != 0 && err == nil {
						var pr store.PullRequest
						pr, err = d.continuedPullRequest(branch.Continues)
						if err == nil {
							continued = []store.PullRequest{pr}
						}
					}
					var checks *store.ChecksObservation
					if o, e := d.store.LatestChecks(j.Number); e == nil {
						checks = &o
					} else if !errors.Is(e, sql.ErrNoRows) && err == nil {
						err = e
					}
					var delivery *store.Delivery
					if del, e := d.store.JobDelivery(j.Number); e == nil {
						delivery = &del
					} else if !errors.Is(e, sql.ErrNoRows) && err == nil {
						err = e
					}
					var reruns []store.Rerun
					if err == nil {
						reruns, err = d.store.Reruns(j.Number)
					}
					var records []store.DecisionRecord
					if err == nil {
						records, err = d.store.DecisionRecords(j.Number)
					}
					var requests []store.DecisionRequest
					if err == nil {
						requests, err = d.store.JobDecisionRequests(j.Number)
					}
					var grants []store.Grant
					if err == nil {
						grants, err = d.store.GrantsOf(j.Number)
					}
					var blocked []store.BlockedReport
					if err == nil {
						blocked, err = d.store.JobBlockedReports(j.Number)
					}
					var setup string
					if err == nil {
						setup, err = d.setupSection(j)
					}
					if err == nil {
						resp.Brief = brief(j, repository, entries, handoffs, branch, continued, assignments, findings, pushes, prs, checks, delivery, reruns, records, requests, blocked, grants, setup)
					}
				}
			}
		}
	}
	if err != nil {
		resp.Error = err.Error()
	}
	reply(conn, resp)
}

// cleanScratch removes the scratch workspaces of job, which has no repository
// and has ended, at the user's command, and journals it. A job's scratch
// workspaces are kept after it ends until then, and are refused while it is
// open (05 rule 44). It returns the paths it removed, none when they are
// already gone.
func (d *daemon) cleanScratch(job int64) ([]string, error) {
	d.assignMu.Lock()
	defer d.assignMu.Unlock()
	j, err := d.store.Job(job)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, refuse("job %d does not exist; `asmai jobs` lists the jobs", job)
	}
	if err != nil {
		return nil, err
	}
	switch {
	case j.Repository != "":
		return nil, refuse("job %d targets repository %s, whose workspaces the daemon removes itself when the job ends: only a job with no repository has scratch workspaces to clean", job, j.Repository)
	case !j.Ended():
		return nil, refuse("job %d is still open, so its scratch workspaces are kept: `asmai job clean %d` removes them once the job has ended", job, job)
	}
	if cleaned, err := d.store.CleanedScratch(job); err != nil || cleaned {
		return nil, err
	}
	dir := filepath.Join(d.cfg.Paths.Workspaces, fmt.Sprintf("job-%d", job))
	var removed []string
	if exists(dir) {
		if err := os.RemoveAll(dir); err != nil {
			return nil, fmt.Errorf("removing %s: %w", dir, err)
		}
		removed = append(removed, dir)
	}
	if err := d.store.ScratchCleaned(job, removed, time.Now()); err != nil {
		return nil, err
	}
	d.log.Info("job's scratch workspaces removed at the user's command", "job", job, "removed", len(removed))
	return removed, nil
}

// makeJobBranch makes a repository job's branch and the leaders' view of it
// as the job opens, so that the job's brief points to the code from the
// start. If it cannot, the job is open all the same, and its first
// assignment makes the branch again.
func (d *daemon) makeJobBranch(j store.Job) {
	repository, err := d.store.Repository(j.Repository)
	if err != nil {
		d.log.Error("reading the repository of a job just opened", "job", j.Number, "repository", j.Repository, "error", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(d.work, gitTimeout)
	defer cancel()
	d.assignMu.Lock()
	defer d.assignMu.Unlock()
	if _, err := d.ensureJobBranch(ctx, j, repository); err != nil {
		d.log.Warn("the job branch was not made as the job opened; its first assignment makes it", "job", j.Number, "error", err.Error())
	}
}

func brief(j store.Job, repository store.Repository, entries []store.Entry, handoffs []store.Handoff, branch *store.JobBranch, continued []store.PullRequest, assignments []store.Assignment, findings []store.Finding, pushes []store.Push, prs []store.PullRequest, checks *store.ChecksObservation, delivery *store.Delivery, reruns []store.Rerun, records []store.DecisionRecord, requests []store.DecisionRequest, blocked []store.BlockedReport, grants []store.Grant, setup string) string {
	var b strings.Builder
	repositoryName := j.Repository
	if repositoryName == "" {
		repositoryName = "none: its assignments work in scratch workspaces, which stay after the job ends until the user runs `asmai job clean " + strconv.FormatInt(j.Number, 10) + "`"
	}
	witnessed := "Witnessed message"
	if j.Mandate == store.MandateSetup {
		witnessed = "Registration the user ran (journal entry)"
	}
	fmt.Fprintf(&b, "Job %d | role: %s | state: %s\nRepository: %s\n%s: %d\nUser's words: %s\nCoordination's reading: %s\nMandate: %s\nAcceptance criteria:\n", j.Number, j.Role, j.State, repositoryName, witnessed, j.Witness, j.Words, j.Reading, j.Mandate)
	for _, criterion := range j.Criteria {
		fmt.Fprintf(&b, "- %s\n", criterion)
	}
	if j.Follows != 0 {
		fmt.Fprintf(&b, "Follow-up to job %d (linked with `asmai job link`)\n", j.Follows)
	}
	if j.State == store.JobCancelled {
		fmt.Fprintf(&b, "Cancelled at the user's word, in witnessed message %d: %s\nThe job takes no new work, and none of its results is accepted: its live assignments end as cancelled once their workers have pushed their branches and stopped.\n", j.CancelWitness, j.CancelReason)
	}
	if repository.Clone != "" {
		fmt.Fprintf(&b, "Repository origin: %s\nRepository clone: %s\nDefault branch (the base the job branch takes in): %s\n", repository.Origin, repository.Clone, repository.DefaultBranch)
	}
	switch {
	case branch != nil && j.State == store.JobEnded:
		fmt.Fprintf(&b, "Job branch: %s (started from %s, tip %s), kept on origin; the job ended, so its view and its branches in AsmAI's clone are removed\n", branch.Name, branch.StartedFrom, branch.Tip)
	case branch != nil && j.State == store.JobCancelled:
		fmt.Fprintf(&b, "Job branch: %s (started from %s, tip %s), and every assignment branch, kept on origin; the job was cancelled, so its view and its branches in AsmAI's clone are removed once its cancelling assignments have ended\n", branch.Name, branch.StartedFrom, branch.Tip)
	case branch != nil && branch.Continues != 0:
		fmt.Fprintf(&b, "Job branch: %s (continues job %d's job branch, started from origin's copy at %s, tip %s)\n", branch.Name, branch.Continues, branch.StartedFrom, branch.Tip)
		if len(continued) > 0 {
			p := continued[len(continued)-1]
			fmt.Fprintf(&b, "Continues job %d's pull request #%d %s, still open: it is this job's pull request, so never open another. Once the daemon has pushed the job branch, record it with `asmai effect pull-request %s` and update its body.\n", branch.Continues, p.Number, p.URL, p.URL)
		}
		fmt.Fprintf(&b, "Read-only view of the job at the job branch's tip: %s\nRead the job's code and the repository's instructions there. Never write in it: it is not a workspace.\n", branch.View)
	case branch != nil:
		fmt.Fprintf(&b, "Job branch: %s (started from %s, tip %s)\nRead-only view of the job at the job branch's tip: %s\nRead the job's code and the repository's instructions there. Never write in it: it is not a workspace.\n", branch.Name, branch.StartedFrom, branch.Tip, branch.View)
	case j.Repository != "":
		fmt.Fprintln(&b, "Job branch: not made yet; the daemon makes it, with a read-only view of the job, and this brief points to the view once it exists")
	}
	fmt.Fprintln(&b, "Handoffs:")
	if len(handoffs) == 0 {
		fmt.Fprintln(&b, "- none")
	}
	for _, h := range handoffs {
		fmt.Fprintf(&b, "- %d %s -> %s: %s", h.ID, h.Sender, h.Receiver, h.State)
		if h.Head != "" {
			fmt.Fprintf(&b, " (validate %s)", h.Head)
		}
		b.WriteString(passedOn(h.Records))
		fmt.Fprintln(&b)
	}
	fmt.Fprintln(&b, "Assignments:")
	if len(assignments) == 0 {
		fmt.Fprintln(&b, "- none")
	}
	for _, a := range assignments {
		if a.ReadOnly {
			fmt.Fprintf(&b, "- %d %s (owner %s) %s, read-only at %s: %s%s\n", a.ID, a.Worker, a.Owner, a.State, a.Commit, a.Outcome, passedOn(a.Records))
			continue
		}
		if a.Scratch {
			fmt.Fprintf(&b, "- %d %s (owner %s) %s in scratch workspace %s: %s%s\n", a.ID, a.Worker, a.Owner, a.State, a.Workspace.Path, a.Outcome, passedOn(a.Records))
			continue
		}
		if p := a.Prototype; p != nil {
			fmt.Fprintf(&b, "- %d %s (owner %s) %s as a prototype that stays on its own branch %s at %s on origin, not on the job branch: %s%s\n", a.ID, a.Worker, a.Owner, a.State, p.Branch, p.Commit, a.Outcome, passedOn(a.Records))
			continue
		}
		fmt.Fprintf(&b, "- %d %s (owner %s) %s on branch %s: %s%s\n", a.ID, a.Worker, a.Owner, a.State, a.Workspace.Branch, a.Outcome, passedOn(a.Records))
	}
	fmt.Fprintln(&b, "Findings:")
	if len(findings) == 0 {
		fmt.Fprintln(&b, "- none")
	}
	for _, f := range findings {
		state := ""
		switch {
		case f.Open():
			state = " (open)"
		case f.Kind == store.FindingBlocking && f.ClearedByDecision != 0:
			state = fmt.Sprintf(" (cleared by the user's decision %d)", f.ClearedByDecision)
		case f.Kind == store.FindingBlocking:
			state = fmt.Sprintf(" (cleared by report %d)", f.ClearedBy)
		}
		fmt.Fprintf(&b, "- %d %s%s, found at %s: %s\n", f.ID, f.Kind, state, f.Commit, f.Text)
	}
	fmt.Fprintln(&b, "Pushes of the job branch:")
	if len(pushes) == 0 {
		fmt.Fprintln(&b, "- none")
	}
	for _, p := range pushes {
		if p.Result == store.PushPushed {
			fmt.Fprintf(&b, "- %d pushed %s to origin, effect %d\n", p.ID, p.Commit, p.Effect)
			continue
		}
		merged := "not merged in yet"
		if p.MergedIn != "" {
			merged = "merged in by " + p.MergedIn
		}
		fmt.Fprintf(&b, "- %d refused at %s: outside change, %s has commits AsmAI did not make (%s), %s\n", p.ID, p.Commit, p.Found, strings.Join(p.Outside, ", "), merged)
	}
	fmt.Fprintln(&b, "Pull request (the latest the delivery owner recorded, which the daemon follows):")
	if len(prs) == 0 {
		fmt.Fprintln(&b, "- none")
	} else {
		p := prs[len(prs)-1]
		fmt.Fprintf(&b, "- #%d %s, %s, from %s at %s, %s on GitHub, effect %d\n", p.Number, p.URL, PullRequestState(p), p.Branch, p.Head, p.State, p.Effect)
	}
	fmt.Fprintln(&b, "Checks on the pull request's head (the daemon watches them through gh; a head with no checks is never green):")
	if checks == nil {
		fmt.Fprintln(&b, "- not read yet")
	} else {
		fmt.Fprintf(&b, "- %s at %s, %s\n", checks.Verdict, checks.Head, checks.At.Format(time.RFC3339))
		if len(checks.Checks) == 0 {
			fmt.Fprintln(&b, "  - no checks")
		}
		for _, c := range checks.Checks {
			fmt.Fprintf(&b, "  - %s: %s\n", c.Name, c.State)
		}
	}
	fmt.Fprintln(&b, "Judged re-runs of red checks (once per head):")
	if len(reruns) == 0 {
		fmt.Fprintln(&b, "- none")
	}
	for _, r := range reruns {
		fmt.Fprintf(&b, "- the failed jobs of %s re-run once on decision %d, %s: %s\n", r.Head, r.Decision, r.At.Format(time.RFC3339), store.CheckNames(r.Checks))
	}
	fmt.Fprintln(&b, "Delivery:")
	if delivery == nil {
		fmt.Fprintln(&b, "- not delivered")
	} else {
		ready := "with every check green"
		if delivery.NoCI {
			ready = "straight away, as the repository is declared as having no CI"
		}
		fmt.Fprintf(&b, "- pull request #%d %s marked ready for review at %s, %s, %s; the job ended. AsmAI never merges\n", delivery.Number, delivery.URL, delivery.Head, delivery.At.Format(time.RFC3339), ready)
	}
	fmt.Fprintln(&b, "Decisions:")
	if len(records) == 0 {
		fmt.Fprintln(&b, "- none")
	}
	for _, r := range records {
		b.WriteString(r.Text("- ", "  "))
	}
	fmt.Fprintln(&b, "Pending decisions:")
	pending := 0
	for _, r := range requests {
		if r.State != store.RequestPending {
			continue
		}
		pending++
		fmt.Fprintf(&b, "- request %d, version %d, from %s to %s, answered in %s: %s\n", r.Request, r.Version, r.Requester, r.Recipient, r.Surface, questionTitles(r.Questions))
		if r.Carries != 0 {
			fmt.Fprintf(&b, "  carries its worker's request %d, version %d, to the user: their own answer to it is the worker's answer\n", r.Carries, r.CarriesVersion)
		}
		if len(r.Blocks) > 0 {
			fmt.Fprintf(&b, "  the work of assignments %v waits for the answer; independent work goes on\n", r.Blocks)
		}
	}
	if pending == 0 {
		fmt.Fprintln(&b, "- none")
	}
	fmt.Fprintln(&b, "Open blocked reports (a leader that cannot go on, for Coordination to settle and resolve; none waits on the user, and none holds any work):")
	open := 0
	for _, r := range blocked {
		if r.State != store.BlockedOpen {
			continue
		}
		open++
		fmt.Fprintf(&b, "- blocked report %d from %s, %s\n  Reason: %s\n  Needs: %s\n", r.ID, r.Leader, r.At.Format(time.RFC3339), r.Reason, r.Needs)
		if len(r.Blocks) > 0 {
			fmt.Fprintf(&b, "  the assignments it names as depending on it: %v; nothing is held\n", r.Blocks)
		}
	}
	if open == 0 {
		fmt.Fprintln(&b, "- none")
	}
	fmt.Fprintln(&b, "Grants (the user's authority; reuse a grant while its scope and conditions hold, and check `asmai authorize` before each consequential action it covers):")
	for _, g := range grants {
		b.WriteString(g.Text("- ", "  "))
	}
	if len(grants) == 0 {
		fmt.Fprintln(&b, "- none")
	}
	b.WriteString(setup)
	fmt.Fprintln(&b, "Recent journal:")
	for _, entry := range entries {
		fmt.Fprintf(&b, "- %d %s\n", entry.ID, entry.Kind)
	}
	return b.String()
}

// passedOn says which decision records were passed on with a handoff or an
// assignment, or nothing when none was.
func passedOn(records []int64) string {
	if len(records) == 0 {
		return ""
	}
	ids := make([]string, 0, len(records))
	for _, id := range records {
		ids = append(ids, strconv.FormatInt(id, 10))
	}
	return " [decisions passed on: " + strings.Join(ids, ", ") + "]"
}

// PullRequestState says whether p is a draft, or opened with "[not ready]"
// at the start of its title, as GitHub had it when it was recorded.
func PullRequestState(p store.PullRequest) string {
	switch {
	case p.Draft:
		return "a draft"
	case p.NotReady:
		return `not a draft, with "[not ready]" at the start of its title`
	default:
		return `neither a draft nor marked "[not ready]"`
	}
}

// questionTitles is the titles of questions, for a line about a request.
func questionTitles(questions []store.Question) string {
	titles := make([]string, len(questions))
	for i, q := range questions {
		titles[i] = q.Title
	}
	return strings.Join(titles, "; ")
}
