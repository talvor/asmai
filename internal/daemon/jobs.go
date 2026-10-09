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
			d.makeJobBranch(j)
		}
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
					var branch *store.JobBranch
					if b, e := d.store.JobBranch(j.Number); e == nil {
						branch = &b
					} else if !errors.Is(e, sql.ErrNoRows) {
						err = e
					}
					if err == nil {
						resp.Brief = brief(j, repository, entries, handoffs, branch, assignments)
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

func brief(j store.Job, repository store.Repository, entries []store.Entry, handoffs []store.Handoff, branch *store.JobBranch, assignments []store.Assignment) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Job %d | role: %s | state: %s\nRepository: %s\nWitnessed message: %d\nUser's words: %s\nCoordination's reading: %s\nMandate: %s\nAcceptance criteria:\n", j.Number, j.Role, j.State, j.Repository, j.Witness, j.Words, j.Reading, j.Mandate)
	for _, criterion := range j.Criteria {
		fmt.Fprintf(&b, "- %s\n", criterion)
	}
	if repository.Clone != "" {
		fmt.Fprintf(&b, "Repository clone: %s\n", repository.Clone)
	}
	switch {
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
		fmt.Fprintf(&b, "- %d %s -> %s: %s\n", h.ID, h.Sender, h.Receiver, h.State)
	}
	fmt.Fprintln(&b, "Assignments:")
	if len(assignments) == 0 {
		fmt.Fprintln(&b, "- none")
	}
	for _, a := range assignments {
		fmt.Fprintf(&b, "- %d %s (owner %s) %s on branch %s: %s\n", a.ID, a.Worker, a.Owner, a.State, a.Workspace.Branch, a.Outcome)
	}
	fmt.Fprintln(&b, "Pending decisions: none\nGrants: none\nRecent journal:")
	for _, entry := range entries {
		fmt.Fprintf(&b, "- %d %s\n", entry.ID, entry.Kind)
	}
	return b.String()
}
