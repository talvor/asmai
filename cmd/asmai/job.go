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
	if o.json {
		return o.printJSON(map[string]any{"job": resp.Job})
	}
	fmt.Fprintf(o.stdout, "Opened job %d for %s.\n", resp.Job.Number, resp.Job.Repository)
	fmt.Fprintf(o.stdout, "Read its brief with `asmai brief %d`.\n", resp.Job.Number)
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
	if o.json {
		return o.printJSON(map[string]any{"jobs": list})
	}
	if len(list) == 0 {
		fmt.Fprintln(o.stdout, "No jobs have been opened.")
		return 0
	}
	rows := [][]string{{"JOB", "STATE", "ROLE", "REPOSITORY", "READING"}}
	for _, j := range list {
		rows = append(rows, []string{strconv.FormatInt(j.Number, 10), j.State, j.Role, j.Repository, j.Reading})
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
	if o.json {
		return o.printJSON(map[string]any{"job": resp.Job})
	}
	jobRows(o, *resp.Job)
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
	if o.json {
		return o.printJSON(map[string]any{"job": resp.Job, "brief": resp.Brief})
	}
	fmt.Fprint(o.stdout, resp.Brief)
	return 0
}

func jobRows(o *output, j store.Job) {
	rows := [][]string{{"job", strconv.FormatInt(j.Number, 10)}, {"state", j.State}, {"role", j.Role}, {"repository", j.Repository}, {"witnessed message", strconv.FormatInt(j.Witness, 10)}, {"user's words", j.Words}, {"Coordination's reading", j.Reading}, {"mandate", j.Mandate}}
	for i, criterion := range j.Criteria {
		rows = append(rows, []string{fmt.Sprintf("criterion %d", i+1), criterion})
	}
	o.table(rows)
}
