// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"database/sql"
	"errors"
	"slices"

	"github.com/talvor/asmai/internal/skills"
	"github.com/talvor/asmai/internal/store"
)

// calledFor are the skills that start only on request which each mandate
// calls for: a tested-PR mandate covers to-spec, to-tickets, implement and
// code-review, and a planning-only one the skills that make its outcome,
// notes that plan (08 rule 14).
var calledFor = map[string][]string{
	store.MandateTestedPR:     {"to-spec", "to-tickets", "implement", "code-review"},
	store.MandatePlanningOnly: {"wayfinder", "grill-me", "grill-with-docs", "to-spec", "to-tickets"},
}

// checkInvocation checks that the skill an assignment of job j names may
// start. A skill that starts only on request starts only when the user's
// request or the job's mandate calls for that kind of outcome: a skill the
// mandate covers starts, and any other starts only with the user's own
// decision of the job, whose record is approved. Otherwise it is proposed to
// the user as a decision, which Coordination presents (08 rule 14). It
// returns the record that approves the skill, or nil when none was needed.
func (d *daemon) checkInvocation(j store.Job, list []skills.Given, skill string, approved int64) (*store.DecisionRecord, error) {
	if skill == "" {
		return nil, nil
	}
	i := slices.IndexFunc(list, func(g skills.Given) bool { return g.Name == skill })
	if i < 0 || !list[i].UserInvoked() {
		return nil, nil
	}
	if slices.Contains(calledFor[j.Mandate], skill) && approved == 0 {
		return nil, nil
	}
	if approved == 0 {
		return nil, refuse("%s starts only on request, and job %d's %s mandate does not call for it: propose it to the user as a decision, with one single-choice decision request that Coordination presents, and name it again with --approved <the ID of the decision record of the user's own answer>", skill, j.Number, j.Mandate)
	}
	r, err := d.store.DecisionRecord(approved)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, refuse("decision record %d does not exist; `asmai brief %d` lists the job's decisions", approved, j.Number)
	case err != nil:
		return nil, err
	case r.Job != j.Number:
		return nil, refuse("decision record %d is of job %d, not job %d: a decision of the user's starts a skill only in the job it was made in", approved, r.Job, j.Number)
	case r.Source != store.SourceHuman:
		return nil, refuse("decision record %d is a delegated decision, not the user's own decision: no agent starts %s for the user", approved, skill)
	}
	return &r, nil
}
