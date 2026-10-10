// SPDX-License-Identifier: Apache-2.0

package roles

import (
	"strings"
	"testing"
)

func TestAgentsAreAddressedNameAtRoleAndARoleMeansItsLeader(t *testing.T) {
	for in, want := range map[string]string{
		"coordination":        "leader@coordination",
		"leader@coordination": "leader@coordination",
		"quality":             "leader@quality",
		"worker1@engineering": "worker1@engineering",
	} {
		a, err := ParseAddress(in)
		if err != nil || a.String() != want {
			t.Errorf("ParseAddress(%q) = %v, %v; want %s", in, a, err, want)
		}
	}
	for _, in := range []string{"", "nobody", "leader@nowhere", "@coordination"} {
		if _, err := ParseAddress(in); err == nil || !strings.Contains(err.Error(), "names no agent") {
			t.Errorf("ParseAddress(%q) returned %v, want it to say it names no agent", in, err)
		}
	}
}

func TestCoordinationsLeaderHasItsInstructions(t *testing.T) {
	if _, err := LeaderInstructions(Coordination); err != nil {
		t.Fatal(err)
	}
	if _, err := LeaderInstructions("nowhere"); err == nil {
		t.Error("a role with no instructions has some")
	}
}

func TestWorkersAreNumberedFromOneAndNamedByAddress(t *testing.T) {
	if got := WorkerOf(Engineering, 3).String(); got != "worker3@engineering" {
		t.Errorf("worker 3 of Engineering is %s", got)
	}
	for in, want := range map[string]int{"worker1@engineering": 1, "worker12@quality": 12} {
		a, err := ParseAddress(in)
		if n, ok := a.WorkerNumber(); err != nil || !ok || n != want {
			t.Errorf("%s is worker %d, %v (%v), want %d", in, n, ok, err, want)
		}
	}
	for _, in := range []string{"leader@engineering", "worker@engineering", "worker0@engineering", "worker01@engineering", "worker1x@engineering", "worker-1@engineering"} {
		a, err := ParseAddress(in)
		if n, ok := a.WorkerNumber(); err != nil || ok {
			t.Errorf("%s is worker %d, %v (%v), want it to be no worker", in, n, ok, err)
		}
	}
}

func TestQualitysInstructionsAssignValidationReadOnlyAtTheExactHeadAndForbidFixing(t *testing.T) {
	leader, err := LeaderInstructions(Quality)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"You are Quality's leader",
		"asmai assign --read-only --commit",
		"exact head commit",
		"the job's mandate and acceptance criteria",
		"blocking",
		"advisory",
		"needs-you",
		"never fixes",
	} {
		if !strings.Contains(leader, want) {
			t.Errorf("Quality's leader's instructions lack %q", want)
		}
	}
	worker, err := WorkerInstructions(Quality)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"a Quality worker",
		"never fix, edit, commit",
		"no branch",
		"never push",
		"Re-run the repository's checks",
		"Standards",
		"Spec",
		"--finding",
		"blocking",
		"advisory",
		"needs-you",
	} {
		if !strings.Contains(worker, want) {
			t.Errorf("Quality's workers' instructions lack %q", want)
		}
	}
}

func TestEngineeringsInstructionsDecideReadinessHandValidationToQualityAndAssignFixes(t *testing.T) {
	leader, err := LeaderInstructions(Engineering)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Decide when the branch is ready",
		"Take in the latest base first",
		"asmai handoff send --job <number> --to quality",
		"Assign a fix for each blocking finding",
		"new writing assignment",
		"Only Quality's re-validation or the user's decision clears a blocking finding",
	} {
		if !strings.Contains(leader, want) {
			t.Errorf("Engineering's leader's instructions lack %q", want)
		}
	}
}
