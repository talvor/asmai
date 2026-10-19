// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"errors"
	"strings"
	"testing"

	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/session"
	"github.com/talvor/asmai/internal/store"
)

func TestALeaderRecordsADecisionAndRoutineWorkNeedsNone(t *testing.T) {
	f := newTwoResults(t)
	record := func() Request {
		return Request{
			Session: "credential", Command: CommandDecisionRecord, Job: f.job.Number,
			Choice: "keep the existing greeting library", ReliedOn: "the job's mandate", Rationale: "it meets the constraints",
			Evidence: "its tests pass", Assumptions: []string{"it stays maintained"}, ReopenWhen: []string{"an advisory names it"},
		}
	}

	// Accepting a result is routine work: it needs no decision record.
	if _, err := f.decide(CommandAccept, f.assignments[0].ID, "file-1.txt is there"); err != nil {
		t.Fatal(err)
	}
	if records, err := f.s.DecisionRecords(f.job.Number); err != nil || len(records) != 0 {
		t.Fatalf("routine work left decision records %+v (%v)", records, err)
	}

	// Only a leader records, and the refusals say what is missing.
	worker := roles.WorkerOf(roles.Engineering, 1)
	f.d.leaders[worker.String()] = &leader{address: worker, session: &session.Session{}, generation: 1}
	f.d.sessions["worker"] = sessionRef{address: worker, generation: 1}
	asWorker := record()
	asWorker.Session = "worker"
	for _, tt := range []struct {
		name string
		req  Request
		want string
	}{
		{"a worker", asWorker, "only a role's leader"},
		{"no choice", func() Request { r := record(); r.Choice = " "; return r }(), "names its choice"},
		{"no mandate or grant", func() Request { r := record(); r.ReliedOn = ""; return r }(), "mandate or grant"},
		{"no rationale", func() Request { r := record(); r.Rationale = ""; return r }(), "rationale"},
		{"no evidence", func() Request { r := record(); r.Evidence = ""; return r }(), "evidence"},
		{"a job that does not exist", func() Request { r := record(); r.Job = 99; return r }(), "does not exist"},
		{"a human decision citing nothing", func() Request { r := record(); r.DecisionSource = store.SourceHuman; return r }(), "cites the witnessed message"},
		{"a human judged re-run", func() Request { r := record(); r.Choice, r.DecisionSource, r.Cites = store.ChoiceRerunFailed, store.SourceHuman, 1; return r }(), "delegated decision"},
	} {
		_, _, err := f.d.recordDecision(tt.req)
		var refusal *userError
		if err == nil || !errors.As(err, &refusal) || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v, want a refusal naming %q", tt.name, err, tt.want)
		}
	}
	if records, err := f.s.DecisionRecords(f.job.Number); err != nil || len(records) != 0 {
		t.Fatalf("refused decisions left records %+v (%v)", records, err)
	}

	// A leader in no dispatch, as Coordination is in its conversation,
	// records a decision all the same; one working on a dispatch tags it.
	first, _, err := f.d.recordDecision(record())
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == 0 || first.Dispatch != 0 || first.Leader != "leader@engineering" || first.Source != store.SourceDelegated {
		t.Errorf("the first decision is %+v", first)
	}
	dispatch := f.leaderWorks(t)
	second, rerun, err := f.d.recordDecision(record())
	if err != nil || rerun.ID != 0 {
		t.Fatalf("the second decision: %v, %+v", err, rerun)
	}
	if second.Dispatch != dispatch || second.ReliedOn != "the job's mandate" || len(second.Assumptions) != 1 || len(second.ReopenWhen) != 1 {
		t.Errorf("the second decision is %+v", second)
	}

	// The job's brief carries both, with every field.
	records, err := f.s.DecisionRecords(f.job.Number)
	if err != nil || len(records) != 2 {
		t.Fatalf("the records are %+v (%v)", records, err)
	}
	text := brief(f.job, store.Repository{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, records, nil)
	for _, want := range []string{
		"Decisions:\n- decision 1 | job 1 | delegated | accountable leader leader@engineering | ",
		"- decision 2 | job 1 | delegated | accountable leader leader@engineering | ",
		"  Choice: keep the existing greeting library\n  Relied on: the job's mandate\n  Rationale: it meets the constraints\n  Evidence: its tests pass\n  Assumptions: it stays maintained\n  Reopen when: an advisory names it\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the brief lacks %q:\n%s", want, text)
		}
	}
}
