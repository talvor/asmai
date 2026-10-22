// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// A skill's human step, such as the test seams of tdd and to-spec, the
// approval of to-tickets' breakdown, code-review's comparison point and spec,
// or wizard's stages, is satisfied by a recorded decision of the user's, or a
// grant, whose scope covers it: the job's own decisions, or a grant of the
// job's, its repository's or the whole factory's. This file is where the store
// checks that the owning leader's answer rests on such a record, and that the
// worker's result names the records it relied on.

// namesStep says whether text, a decision or a grant, names the skill as a
// whole word and the step.
func namesStep(text, skill, step string) bool {
	word := regexp.MustCompile(`(?i)(^|[^\w-])` + regexp.QuoteMeta(skill) + `($|[^\w-])`)
	name := strings.TrimSpace(strings.ToLower(step))
	for _, article := range []string{"the ", "a ", "an "} {
		name = strings.TrimPrefix(name, article)
	}
	return word.MatchString(text) && strings.Contains(strings.ToLower(text), name)
}

// stepCovered checks that the decision records and grants an owning leader
// answers the step request step from cover it, and returns the words that
// say what the answer rests on. Nothing but the user's own decision of the
// job, or an active grant of a decision whose scope covers the job, satisfies
// a step: a decision of another job, a delegated decision, a grant of a
// narrower scope or one that ended does not.
func stepCovered(q querier, step DecisionRequest, records, grants []int64) (string, error) {
	if len(records)+len(grants) == 0 {
		return "", fmt.Errorf("a skill's human step is satisfied by a recorded decision of the user's, or a grant, whose scope covers it: an answer to the %s step of decision request %d relies on a record, so name it with --record <the user's decision record of job %d> or --grant <a grant whose scope covers the job>; where none covers it, carry the step to the user with a request of your own (--carries %d) and return their answer", step.Skill, step.Request, step.Job, step.Request)
	}
	j, err := openedJob(q, step.Job)
	if err != nil {
		return "", err
	}
	var relied []string
	for i, id := range records {
		if slices.Contains(records[:i], id) {
			return "", fmt.Errorf("decision record %d is named twice", id)
		}
		found, err := scanDecisionRecords(q, `id = ?`, id)
		if err != nil {
			return "", err
		}
		if len(found) == 0 {
			return "", fmt.Errorf("decision record %d does not exist; `asmai brief %d` lists the job's decisions", id, step.Job)
		}
		r := found[0]
		if err := recordCoversStep(q, r, step); err != nil {
			return "", err
		}
		relied = append(relied, fmt.Sprintf("decision record %d, the user's own decision of job %d (%s)", r.ID, r.Job, r.Choice))
	}
	for i, id := range grants {
		if slices.Contains(grants[:i], id) {
			return "", fmt.Errorf("grant %d is named twice", id)
		}
		g, err := grantByID(q, id)
		if err != nil {
			return "", err
		}
		if err := grantCoversStep(g, j, step); err != nil {
			return "", err
		}
		relied = append(relied, fmt.Sprintf("grant %d, of %s, approving %q", g.ID, g.ScopeText(), g.Approved))
	}
	return strings.Join(relied, "; "), nil
}

// recordCoversStep checks that the decision record r is the user's own
// decision of the step's job, and names the step's skill, or is the user's
// answer to the request that carried the step to them.
func recordCoversStep(q querier, r DecisionRecord, step DecisionRequest) error {
	switch {
	case r.Job != step.Job:
		return fmt.Errorf("decision record %d is of job %d, not job %d: a decision of another job does not satisfy this job's step, which only the job's own decisions, or a grant whose scope covers the job, do", r.ID, r.Job, step.Job)
	case r.Source != SourceHuman:
		return fmt.Errorf("decision record %d is a delegated decision of %s, not the user's own decision: a skill's human step is satisfied only by the user's decision or a grant", r.ID, r.Leader)
	case namesStep(r.Choice, step.Skill, step.Step):
		return nil
	case r.Request != 0:
		asked, err := latestRequestVersion(q, r.Request)
		if err != nil {
			return err
		}
		if asked.Carries == step.Request {
			return nil
		}
	}
	return fmt.Errorf("decision record %d does not name %s and its %s step: it is the user's decision about something else (%q), so it does not satisfy the step; carry the step to the user and record their answer, naming the skill and the step in the choice", r.ID, step.Skill, step.Step, r.Choice)
}

// grantCoversStep checks that the grant g is an active grant of a decision
// that names the step's skill and step, and whose scope covers job j.
func grantCoversStep(g Grant, j Job, step DecisionRequest) error {
	switch {
	case !g.Covers(j):
		return fmt.Errorf("grant %d is of %s, which does not cover job %d: a grant of a narrower scope, or of another job, does not satisfy this job's step, and a changed scope needs a new decision", g.ID, g.ScopeText(), j.Number)
	case g.State == GrantRevoked:
		return fmt.Errorf("grant %d was revoked by %s", g.ID, g.EndedBy)
	case g.State == GrantInvalidated:
		return fmt.Errorf("grant %d was invalidated by %s: %s; a condition that no longer holds needs a new decision", g.ID, g.EndedBy, g.EndedReason)
	case g.Action != ActionDecision:
		return fmt.Errorf("grant %d approves %s, not a decision: only a grant of a decision satisfies a skill's human step", g.ID, g.Action)
	case !namesStep(g.Approved, step.Skill, step.Step):
		return fmt.Errorf("grant %d does not name %s and its %s step: it approves %q, which is not the user's grant of that step", g.ID, step.Skill, step.Step, g.Approved)
	}
	return nil
}

// stepAnswerRecorded records the user's own Lavish answer e to the request
// that carried the step to them as their decision of the step's job, which
// stands for the rest of the job: the next worker that asks the same step is
// answered from it.
func stepAnswerRecorded(tx *sql.Tx, step, e DecisionRequest, leader string, at time.Time) (DecisionRecord, error) {
	return insertDecisionRecord(tx, DecisionRecord{
		Job: step.Job, Leader: leader, Source: SourceHuman, Cites: e.Cites,
		ReliedOn:  fmt.Sprintf("the user's own answer to decision request %d, version %d, in Lavish (Lavish answer %d)", e.Request, e.Version, e.Cites),
		Choice:    fmt.Sprintf("%s, %s: %s", step.Skill, step.Step, e.Answer),
		Rationale: fmt.Sprintf("the user's own answer to the %s step that a worker asked in decision request %d", step.Skill, step.Request),
		Evidence:  e.Answer,
		// It answers the request that carried the step, as a paused job's
		// pending decisions stay answerable, and says whose words it holds.
		Request: e.Request, Version: e.Version,
		Reading: fmt.Sprintf("the user's own answer in Lavish to decision request %d, which %s carried to them for the %s step of decision request %d, returned as it was given", e.Request, leader, step.Skill, step.Request),
	}, 0, at)
}

// stepAnswerBody is the message a worker is given when its step is answered:
// the answer, and the records it rests on, which its result names.
func stepAnswerBody(r DecisionRequest, text string) string {
	if r.Kind != RequestStep || len(r.ReliedRecords)+len(r.ReliedGrants) == 0 {
		return text
	}
	var flags []string
	for _, id := range r.ReliedRecords {
		flags = append(flags, fmt.Sprintf("--record %d", id))
	}
	for _, id := range r.ReliedGrants {
		flags = append(flags, fmt.Sprintf("--grant %d", id))
	}
	return fmt.Sprintf("%s\n\nThis answers the %s step (%s) from the records: %s. Name them in your result: asmai result %s", text, r.Skill, r.Step, r.ReliedOn, strings.Join(flags, " "))
}

// checkRelied checks that the result in of assignment a names the records its
// steps relied on: every record and grant that satisfied a step the worker
// asked, and nothing else, so that a result never claims a record that no
// answer to its steps gave.
func checkRelied(q querier, a Assignment, in ReportInput) error {
	steps, err := decisionRequests(q, `assignment = ? AND kind = ? AND state = ?`, a.ID, RequestStep, RequestAnswered)
	if err != nil {
		return err
	}
	var records, grants []int64
	for _, step := range steps {
		records = append(records, step.ReliedRecords...)
		grants = append(grants, step.ReliedGrants...)
	}
	for i, id := range in.RecordsRelied {
		switch {
		case slices.Contains(in.RecordsRelied[:i], id):
			return fmt.Errorf("decision record %d is named twice", id)
		case !slices.Contains(records, id):
			return fmt.Errorf("decision record %d is not one your steps relied on: name only the records that the answers to your steps gave", id)
		}
	}
	for i, id := range in.GrantsRelied {
		switch {
		case slices.Contains(in.GrantsRelied[:i], id):
			return fmt.Errorf("grant %d is named twice", id)
		case !slices.Contains(grants, id):
			return fmt.Errorf("grant %d is not one your steps relied on: name only the grants that the answers to your steps gave", id)
		}
	}
	for _, step := range steps {
		for _, id := range step.ReliedRecords {
			if !slices.Contains(in.RecordsRelied, id) {
				return fmt.Errorf("your result names no record for the %s step (%s) that decision request %d asked: it relied on decision record %d, so name it with --record %d", step.Skill, step.Step, step.Request, id, id)
			}
		}
		for _, id := range step.ReliedGrants {
			if !slices.Contains(in.GrantsRelied, id) {
				return fmt.Errorf("your result names no grant for the %s step (%s) that decision request %d asked: it relied on grant %d, so name it with --grant %d", step.Skill, step.Step, step.Request, id, id)
			}
		}
	}
	return nil
}
