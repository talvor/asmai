// SPDX-License-Identifier: Apache-2.0

package daemon

import (
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
					if err == nil {
						resp.Brief = brief(j, repository, entries, handoffs)
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

func brief(j store.Job, repository store.Repository, entries []store.Entry, handoffs []store.Handoff) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Job %d | role: %s | state: %s\nRepository: %s\nWitnessed message: %d\nUser's words: %s\nCoordination's reading: %s\nMandate: %s\nAcceptance criteria:\n", j.Number, j.Role, j.State, j.Repository, j.Witness, j.Words, j.Reading, j.Mandate)
	for _, criterion := range j.Criteria {
		fmt.Fprintf(&b, "- %s\n", criterion)
	}
	if repository.Clone != "" {
		fmt.Fprintf(&b, "Repository clone: %s\n", repository.Clone)
	}
	fmt.Fprintln(&b, "Handoffs:")
	if len(handoffs) == 0 {
		fmt.Fprintln(&b, "- none")
	}
	for _, h := range handoffs {
		fmt.Fprintf(&b, "- %d %s -> %s: %s\n", h.ID, h.Sender, h.Receiver, h.State)
	}
	fmt.Fprintln(&b, "Assignments: none\nPending decisions: none\nGrants: none\nRecent journal:")
	for _, entry := range entries {
		fmt.Fprintf(&b, "- %d %s\n", entry.ID, entry.Kind)
	}
	return b.String()
}
