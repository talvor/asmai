// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/store"
)

func TestTheBriefShowsTheChecksOnTheHeadAndTheDelivery(t *testing.T) {
	j := store.Job{Number: 1, Role: "coordination", State: store.JobOpen, Repository: "fixture"}
	branch := &store.JobBranch{Name: "asmai/job-1-add", StartedFrom: "aaa", Tip: "bbb", View: "/views/job-1"}
	at := time.Date(2026, 10, 12, 1, 2, 3, 0, time.UTC)

	got := brief(j, store.Repository{}, nil, nil, branch, nil, nil, nil, nil, nil, nil, nil, nil)
	for _, want := range []string{"Checks on the pull request's head (the daemon watches them through gh; a head with no checks is never green):\n- not read yet\n", "Delivery:\n- not delivered\n", "Read-only view of the job at the job branch's tip: /views/job-1"} {
		if !strings.Contains(got, want) {
			t.Errorf("with nothing read, the brief lacks %q:\n%s", want, got)
		}
	}

	none := &store.ChecksObservation{Head: "bbb", Verdict: "waiting", Checks: []store.CheckState{}, At: at}
	if got := brief(j, store.Repository{}, nil, nil, branch, nil, nil, nil, nil, none, nil, nil, nil); !strings.Contains(got, "- waiting at bbb, 2026-10-12T01:02:03Z\n  - no checks\n") {
		t.Errorf("with no checks, the brief is:\n%s", got)
	}

	j.State = store.JobEnded
	green := &store.ChecksObservation{Head: "bbb", Verdict: "green", Checks: []store.CheckState{{Name: "CI / test", State: "passed"}}, At: at}
	delivery := &store.Delivery{Job: 1, Number: 7, URL: "https://github.com/o/r/pull/7", Head: "bbb", At: at}
	got = brief(j, store.Repository{}, nil, nil, branch, nil, nil, nil, nil, green, delivery, nil, nil)
	for _, want := range []string{
		"- green at bbb, 2026-10-12T01:02:03Z\n  - CI / test: passed\n",
		"Delivery:\n- pull request #7 https://github.com/o/r/pull/7 marked ready for review at bbb, 2026-10-12T01:02:03Z, with every check green; the job ended. AsmAI never merges\n",
		"Job branch: asmai/job-1-add (started from aaa, tip bbb), kept on origin; the job ended",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("once delivered, the brief lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Read-only view") {
		t.Errorf("an ended job's brief points to its removed view:\n%s", got)
	}
}

func TestCoordinationIsToldTheLinkAndThatReadyIsNotMerged(t *testing.T) {
	body := deliveryBody(store.Job{Number: 3}, store.Delivery{Number: 7, URL: "https://github.com/o/r/pull/7", Head: "bbb", Checks: []store.CheckState{{Name: "CI / test"}, {Name: "lint"}}})
	for _, want := range []string{"Job 3 is delivered", "https://github.com/o/r/pull/7", "head bbb", "(CI / test, lint)", "The job has ended", "its branches stay on origin", "AsmAI never merges", "in a line tagged with job 3, with the link"} {
		if !strings.Contains(body, want) {
			t.Errorf("the delivery message lacks %q: %s", want, body)
		}
	}
}

func TestEngineeringIsToldTheFailingChecksOnTheHeadWithOneReRunOrAFix(t *testing.T) {
	o := store.ChecksObservation{Number: 7, Head: "bbb", Verdict: "red", Checks: []store.CheckState{
		{Name: "CI / test", State: "failed", URL: "https://github.com/o/r/actions/runs/41/job/1"},
		{Name: "CI / lint", State: "passed"},
		{Name: "deploy", State: "failed"},
	}}
	first := redBody(store.Job{Number: 3}, o, nil)
	for _, want := range []string{
		"The checks on head bbb of job 3's pull request #7 are red. Failing: CI / test (https://github.com/o/r/actions/runs/41/job/1), deploy.",
		"If you judge the failure unrelated to the change, you may re-run the failed jobs once for this head",
		"`asmai decision record --job 3 --choice rerun-failed --rationale <why the failure is unrelated to the change> --evidence <what shows it>`",
		"Otherwise, assign a fix as a new writing assignment naming the failing checks",
		"hand validation of the new head to Quality",
		"`asmai push`",
		"drops the wait on this one",
		"record each update with `asmai effect pull-request <url>`",
	} {
		if !strings.Contains(first, want) {
			t.Errorf("the red checks message lacks %q: %s", want, first)
		}
	}
	if strings.Contains(first, "CI / lint") {
		t.Errorf("the red checks message names a passing check: %s", first)
	}
	again := redBody(store.Job{Number: 3}, o, &store.Rerun{Decision: 4})
	for _, want := range []string{"re-run once already, on decision 4, and failed again: a second re-run of this head is refused. Assign a fix"} {
		if !strings.Contains(again, want) {
			t.Errorf("the red checks message after the re-run lacks %q: %s", want, again)
		}
	}
	if strings.Contains(again, "asmai decision record") {
		t.Errorf("the red checks message after the re-run offers another: %s", again)
	}
}

func TestTheBriefShowsTheJudgedReRunAndItsDecision(t *testing.T) {
	j := store.Job{Number: 1, Role: "coordination", State: store.JobOpen, Repository: "fixture"}
	at := time.Date(2026, 10, 12, 1, 2, 3, 0, time.UTC)
	reruns := []store.Rerun{{Head: "bbb", Decision: 4, Checks: []store.CheckState{{Name: "CI / test"}}, At: at}}
	records := []store.DecisionRecord{{ID: 4, Leader: "leader@engineering", Source: store.SourceDelegated, Choice: store.ChoiceRerunFailed, Rationale: "a runner fault", Evidence: "the log"}}
	got := brief(j, store.Repository{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, reruns, records)
	for _, want := range []string{
		"Judged re-runs of red checks (once per head):\n- the failed jobs of bbb re-run once on decision 4, 2026-10-12T01:02:03Z: CI / test\n",
		"Decisions:\n- 4 delegated by leader@engineering: rerun-failed. Rationale: a runner fault. Evidence: the log\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the brief lacks %q:\n%s", want, got)
		}
	}
	if got := brief(j, store.Repository{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil); !strings.Contains(got, "Judged re-runs of red checks (once per head):\n- none\n") || !strings.Contains(got, "Decisions:\n- none\n") {
		t.Errorf("with none, the brief is:\n%s", got)
	}
}
