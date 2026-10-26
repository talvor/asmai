// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/statedir"
	"github.com/talvor/asmai/internal/store"
)

type criteriaFlags []string

func (c *criteriaFlags) String() string         { return strings.Join(*c, "; ") }
func (c *criteriaFlags) Set(value string) error { *c = append(*c, value); return nil }

func jobNumber(value string) (int64, error) {
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%q is not a job number; `asmai jobs` lists the jobs", value)
	}
	return n, nil
}

func jobOpen(o *output, paths statedir.Paths, message, repository, reading, mandate string, criteria []string) int {
	if message == "" {
		return o.fail(fmt.Errorf("job open needs --message latest or --message <witnessed-id>"))
	}
	var witness int64
	var err error
	if message != "latest" {
		witness, err = jobNumber(message)
		if err != nil {
			return o.fail(fmt.Errorf("--message: %w", err))
		}
	}
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandJobOpen, Witness: witness, LatestWitness: message == "latest", Repository: repository, Reading: reading, Mandate: mandate, Criteria: criteria})
	if err != nil {
		return o.fail(err)
	}
	if o.structured() {
		return o.printData(map[string]any{"job": resp.Job})
	}
	if resp.Job.Repository == "" {
		fmt.Fprintf(o.stdout, "Opened job %d, which has no repository: its assignments work in scratch workspaces.\n", resp.Job.Number)
	} else {
		fmt.Fprintf(o.stdout, "Opened job %d for %s.\n", resp.Job.Number, resp.Job.Repository)
	}
	fmt.Fprintf(o.stdout, "Read its brief with `asmai brief %d`.\n", resp.Job.Number)
	return 0
}

// jobLink links the follow-up job to the job it follows. The daemon has the
// follow-up continue its predecessor's job branch and pull request where the
// pull request is still open, and otherwise start from the base.
func jobLink(o *output, paths statedir.Paths, job, follows int64) int {
	if job <= 0 || follows <= 0 {
		return o.fail(fmt.Errorf("job link needs --job <the follow-up's number> and --follows <the number of the job it follows>; `asmai jobs` lists the jobs"))
	}
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandJobLink, Job: job, Follows: follows})
	if err != nil {
		return o.fail(err)
	}
	if o.structured() {
		return o.printData(map[string]any{"job": resp.Job, "job_branch": resp.JobBranch, "continues": resp.Continues, "starts": resp.Starts})
	}
	fmt.Fprintf(o.stdout, "Job %d is linked as a follow-up to job %d.\n", job, follows)
	switch {
	case resp.Continues != nil:
		fmt.Fprintf(o.stdout, "It continues job %d's job branch %s and its open pull request #%d %s, starting from origin's copy at %s.\n", follows, resp.JobBranch.Name, resp.Continues.Number, resp.Continues.URL, resp.JobBranch.StartedFrom)
	case resp.Starts != "":
		fmt.Fprintf(o.stdout, "It starts afresh: %s.\n", resp.Starts)
	case resp.Job.Repository == "":
		fmt.Fprintln(o.stdout, "It has no repository, so there is no job branch to continue.")
	}
	fmt.Fprintf(o.stdout, "Read its brief with `asmai brief %d`.\n", job)
	return 0
}

// jobCancel cancels a job at the user's word, for Coordination's leader: the
// witnessed message message names holds the user's own words, the reason.
func jobCancel(o *output, paths statedir.Paths, number, message string) int {
	n, err := jobNumber(number)
	if err != nil {
		return o.fail(err)
	}
	if message == "" {
		return o.fail(fmt.Errorf("job cancel needs the user's own words: --message latest or --message <witnessed-id>"))
	}
	var witness int64
	if message != "latest" {
		if witness, err = jobNumber(message); err != nil {
			return o.fail(fmt.Errorf("--message: %w", err))
		}
	}
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandJobCancel, Job: n, Witness: witness, LatestWitness: message == "latest"})
	if err != nil {
		return o.fail(err)
	}
	c := resp.Cancellation
	if o.structured() {
		return o.printData(map[string]any{"cancellation": c})
	}
	fmt.Fprintf(o.stdout, "Job %d is cancelled at the user's word, in witnessed message %d: %s\n", n, c.Job.CancelWitness, c.Job.CancelReason)
	if c.Closed != nil {
		fmt.Fprintf(o.stdout, "Its pull request %s is closed with a comment giving the reason (effect %d).\n", c.Closed.Ref, c.Closed.ID)
	} else {
		fmt.Fprintln(o.stdout, "It had no open pull request to close.")
	}
	for _, a := range c.Cancelling {
		fmt.Fprintf(o.stdout, "Assignment %d is cancelling: %s pushes its assignment branch and stops, and it ends as cancelled once the daemon confirms the push.\n", a.ID, a.Worker)
	}
	if len(c.Discarded) > 0 {
		fmt.Fprintf(o.stdout, "Unreviewed results discarded: reports %v.\n", c.Discarded)
	}
	if len(c.Held) > 0 {
		fmt.Fprintf(o.stdout, "Assignments %v need reconciliation and stay held with their owning leaders.\n", c.Held)
	}
	for _, r := range c.Withdrawn {
		fmt.Fprintf(o.stdout, "Decision request %d (version %d) is withdrawn, and its page ends.\n", r.Request, r.Version)
	}
	fmt.Fprintln(o.stdout, "Every branch stays on origin.")
	return 0
}

func jobs(o *output, paths statedir.Paths) int {
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandJobs})
	if err != nil {
		return o.fail(err)
	}
	list := resp.Jobs
	if list == nil {
		list = []store.Job{}
	}
	if o.structured() {
		return o.printData(map[string]any{"jobs": list})
	}
	if len(list) == 0 {
		fmt.Fprintln(o.stdout, "No jobs have been opened.")
		return 0
	}
	rows := [][]string{{"JOB", "STATE", "ROLE", "REPOSITORY", "READING"}}
	for _, j := range list {
		rows = append(rows, []string{strconv.FormatInt(j.Number, 10), j.State, j.Role, repositoryText(j), j.Reading})
	}
	o.table(rows)
	return 0
}

func jobShow(o *output, paths statedir.Paths, number string) int {
	n, err := jobNumber(number)
	if err != nil {
		return o.fail(err)
	}
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandJob, Job: n})
	if err != nil {
		return o.fail(err)
	}
	if o.structured() {
		return o.printData(map[string]any{"job": resp.Job})
	}
	jobRows(o, *resp.Job)
	return 0
}

// jobClean removes the scratch workspaces of a job with no repository that
// has ended, which are kept until the user asks. The daemon refuses it while
// the job is open.
func jobClean(o *output, paths statedir.Paths, number string) int {
	n, err := jobNumber(number)
	if err != nil {
		return o.fail(err)
	}
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandJobClean, Job: n})
	if err != nil {
		return o.fail(err)
	}
	if o.structured() {
		return o.printData(map[string]any{"job": n, "removed": append([]string{}, resp.Removed...)})
	}
	if len(resp.Removed) == 0 {
		fmt.Fprintf(o.stdout, "Job %d's scratch workspaces are already removed.\n", n)
		return 0
	}
	fmt.Fprintf(o.stdout, "Removed job %d's scratch workspaces and the results in them: %s\n", n, strings.Join(resp.Removed, ", "))
	return 0
}

func jobBrief(o *output, paths statedir.Paths, number string) int {
	n, err := jobNumber(number)
	if err != nil {
		return o.fail(err)
	}
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandBrief, Job: n})
	if err != nil {
		return o.fail(err)
	}
	if o.structured() {
		return o.printData(map[string]any{"job": resp.Job, "brief": resp.Brief})
	}
	fmt.Fprint(o.stdout, resp.Brief)
	return 0
}

func jobRows(o *output, j store.Job) {
	rows := [][]string{{"job", strconv.FormatInt(j.Number, 10)}, {"state", j.State}, {"role", j.Role}, {"repository", repositoryText(j)}, {"witnessed message", strconv.FormatInt(j.Witness, 10)}, {"user's words", j.Words}, {"Coordination's reading", j.Reading}, {"mandate", j.Mandate}}
	for i, criterion := range j.Criteria {
		rows = append(rows, []string{fmt.Sprintf("criterion %d", i+1), criterion})
	}
	o.table(rows)
}

// repositoryText is the repository a job targets, or none.
func repositoryText(j store.Job) string {
	if j.Repository == "" {
		return "none"
	}
	return j.Repository
}
