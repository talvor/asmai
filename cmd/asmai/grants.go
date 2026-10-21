// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/statedir"
	"github.com/talvor/asmai/internal/store"
)

// grantFlags are the values of `asmai decision record --grant`'s flags that
// say what the grant is.
type grantFlags struct {
	grant                               bool
	scope, repository, action, approved string
	amount, currency                    string
	conditions                          listFlags
	answer                              int64
}

// grantRecord has a leader record a grant the user gave in words, at its
// scope, citing them: Coordination's leader one given in the conversation,
// with --cites, and the leader that asked one given in an answer, with
// --answer, the decision record that holds the answer.
func grantRecord(o *output, paths statedir.Paths, f decisionFlags, g grantFlags, cites int64, latest bool) int {
	conditions, err := explicit("condition", g.conditions)
	if err != nil {
		return o.fail(err)
	}
	in := store.GrantInput{Scope: g.scope, Job: f.job, Repository: g.repository, Action: g.action, Approved: g.approved, Currency: g.currency, Conditions: conditions, Cites: cites, Answer: g.answer}
	if g.scope != store.ScopeJob {
		// Only a job's own scope names it.
		in.Job = 0
	}
	if strings.TrimSpace(g.amount) != "" {
		if in.Amount, err = store.ParseAmount(g.amount); err != nil {
			return o.fail(fmt.Errorf("--amount: %w", err))
		}
	}
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandDecisionRecord, Session: os.Getenv(daemon.SessionCredential), Grant: &in, LatestWitness: latest})
	if err != nil {
		return o.fail(err)
	}
	if o.structured() {
		return o.printData(resp)
	}
	grant := resp.Grant
	fmt.Fprint(o.stdout, "recorded ", grant.Text("", "  "))
	fmt.Fprintf(o.stdout, "next: its scope and conditions hold it in force; every assignment and handoff it covers carries it, and every agent runs `asmai authorize` before each consequential action it covers. The user lists grants with `asmai grants` and revokes one with `asmai grant revoke %d`\n", grant.ID)
	return 0
}

// authorizeFlags are the values of `asmai authorize`'s flags.
type authorizeFlags struct {
	job              int64
	action, what     string
	amount, currency string
}

// authorize asks the daemon whether a grant covers the consequential action
// before the agent takes it.
func authorize(o *output, paths statedir.Paths, f authorizeFlags) int {
	in := store.ActionRequest{Job: f.job, Action: f.action, What: f.what, Currency: f.currency}
	if strings.TrimSpace(f.amount) != "" {
		var err error
		if in.Amount, err = store.ParseAmount(f.amount); err != nil {
			return o.fail(fmt.Errorf("--amount: %w", err))
		}
	}
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandAuthorize, Session: os.Getenv(daemon.SessionCredential), Action: &in})
	if err != nil {
		return o.fail(err)
	}
	if o.structured() {
		return o.printData(resp)
	}
	a := resp.Authorization
	fmt.Fprintf(o.stdout, "authorized %s%s in job %d by grant %d (authorization %d): %s\n", a.Action, a.AmountText(), a.Job, a.Grant, a.ID, a.What)
	fmt.Fprintf(o.stdout, "next: take only the action you named; check again before each further consequential action, because a revoked or invalidated grant, or a changed scope, authorizes nothing, and record the effect with `asmai effect %s <what you did>`, once for this authorization\n", a.Action)
	return 0
}

// listGrants prints every grant with its state and what it has authorized.
// It only reads: the user revokes one with `asmai grant revoke`.
func listGrants(o *output, paths statedir.Paths) int {
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandGrants})
	if err != nil {
		return o.fail(err)
	}
	list := resp.Grants
	if list == nil {
		list = []store.Grant{}
	}
	if o.structured() {
		return o.printData(map[string]any{"grants": list})
	}
	if len(list) == 0 {
		fmt.Fprintln(o.stdout, "No grants are recorded. You give one in words, in the conversation or in an answer, and a leader records it.")
		return 0
	}
	rows := [][]string{{"ID", "SCOPE", "ACTION", "APPROVED", "AMOUNT", "STATE", "YOUR WORDS"}}
	for _, g := range list {
		amount := "-"
		if g.Action == store.ActionSpend {
			amount = fmt.Sprintf("%s of %s", store.FormatAmount(g.Spent, g.Currency), store.FormatAmount(g.Amount, g.Currency))
		}
		state := g.State
		if g.State != store.GrantActive {
			state = g.State + " " + g.EndedAt.Local().Format("2006-01-02 15:04")
		}
		rows = append(rows, []string{strconv.FormatInt(g.ID, 10), g.ScopeText(), g.Action, g.Approved, amount, state, strconv.Quote(g.Words)})
	}
	o.table(rows)
	fmt.Fprintln(o.stdout, "\nRevoke one with `asmai grant revoke <id>`: every later action it covers is refused, and what it already authorized is reported as it stands, never undone.")
	return 0
}

// grantEnd revokes a grant, as the user, or reports that a leader's grant
// condition no longer holds.
func grantEnd(o *output, paths statedir.Paths, command string, id int64, why string) int {
	if id <= 0 {
		return o.fail(errors.New("name the grant, as `asmai grants` lists them"))
	}
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: command, Session: os.Getenv(daemon.SessionCredential), GrantID: id, Why: why})
	if err != nil {
		return o.fail(err)
	}
	if o.structured() {
		return o.printData(resp)
	}
	printGrantEnding(o.stdout, *resp.GrantEnding)
	return 0
}

// printGrantEnding reports a grant that ended: what stops, what already
// happened under it as it stands, and who was told.
func printGrantEnding(w io.Writer, e store.GrantEnding) {
	g := e.Grant
	if g.State == store.GrantRevoked {
		fmt.Fprintf(w, "grant %d revoked: every later action it covers is refused\n", g.ID)
	} else {
		fmt.Fprintf(w, "grant %d invalidated: %s; a new decision of the user's is needed before its actions go on\n", g.ID, g.EndedReason)
	}
	if len(e.Earlier) == 0 {
		fmt.Fprintln(w, "it authorized nothing before it ended")
	} else {
		fmt.Fprintln(w, "what it authorized before it ended stands as it is; nothing is undone, and only your decision authorizes a way to recover:")
		for _, a := range e.Earlier {
			fmt.Fprintf(w, "  authorization %d | %s%s | job %d | %s | by %s at %s\n", a.ID, a.Action, a.AmountText(), a.Job, a.What, a.Agent, a.At.Format(time.RFC3339))
		}
	}
	if len(e.Affected) == 0 {
		fmt.Fprintln(w, "no assignment in flight carried it")
		return
	}
	fmt.Fprintln(w, "assignments in flight that carried it, whose owning leaders were told to stop or revise them:")
	for _, a := range e.Affected {
		fmt.Fprintf(w, "  assignment %d | job %d | %s | %s\n", a.ID, a.Job, a.Owner, a.State)
	}
}

// printGrants writes the grants in force that an assignment or a handoff
// carries.
func printGrants(w io.Writer, title string, grants []store.Grant) {
	if len(grants) == 0 {
		return
	}
	fmt.Fprintf(w, "%s:\n", title)
	for _, g := range grants {
		fmt.Fprint(w, g.Text("  ", "  "))
	}
}

// parseGrantID reads a grant's ID, or 0 when it is not one.
func parseGrantID(value string) int64 {
	id, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || id <= 0 {
		return 0
	}
	return id
}
