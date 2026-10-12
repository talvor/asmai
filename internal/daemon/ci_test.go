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

	got := brief(j, store.Repository{}, nil, nil, branch, nil, nil, nil, nil, nil, nil)
	for _, want := range []string{"Checks on the pull request's head (the daemon watches them through gh; a head with no checks is never green):\n- not read yet\n", "Delivery:\n- not delivered\n", "Read-only view of the job at the job branch's tip: /views/job-1"} {
		if !strings.Contains(got, want) {
			t.Errorf("with nothing read, the brief lacks %q:\n%s", want, got)
		}
	}

	none := &store.ChecksObservation{Head: "bbb", Verdict: "waiting", Checks: []store.CheckState{}, At: at}
	if got := brief(j, store.Repository{}, nil, nil, branch, nil, nil, nil, nil, none, nil); !strings.Contains(got, "- waiting at bbb, 2026-10-12T01:02:03Z\n  - no checks\n") {
		t.Errorf("with no checks, the brief is:\n%s", got)
	}

	j.State = store.JobEnded
	green := &store.ChecksObservation{Head: "bbb", Verdict: "green", Checks: []store.CheckState{{Name: "CI / test", State: "passed"}}, At: at}
	delivery := &store.Delivery{Job: 1, Number: 7, URL: "https://github.com/o/r/pull/7", Head: "bbb", At: at}
	got = brief(j, store.Repository{}, nil, nil, branch, nil, nil, nil, nil, green, delivery)
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
