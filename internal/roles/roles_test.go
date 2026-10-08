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

// What the skeleton needs the writing worker's and Engineering's leader's
// instructions to say (08 rule 37).
func TestEngineeringsInstructionsSayWhatTheSkeletonNeeds(t *testing.T) {
	worker, err := WorkerInstructions(Engineering)
	if err != nil {
		t.Fatal(err)
	}
	for _, phrase := range []string{
		"one assignment for one owning leader",
		"Stay inside the workspace",
		"ASMAI_SLOT", "TMPDIR",
		"asmai inbox --dispatch",
		"Write tests along with the code",
		"Run the repository's checks",
		"Name every command you ran and its outcome",
		"Take in the job branch's tip before you submit",
		"Push your assignment branch",
		"commit anything uncommitted first",
		"asmai effect",
		"asmai result",
		"asmai blocked",
		"A message saying you are done is never a result",
		"Disclose every gap",
		"--pr-section",
		"before-and-after evidence",
	} {
		if !strings.Contains(worker, phrase) {
			t.Errorf("Engineering's worker instructions never say %q", phrase)
		}
	}
	if !strings.Contains(worker, "an Engineering worker") {
		t.Error("Engineering's worker instructions do not name the agent as an Engineering worker, as the fake provider tells it apart")
	}
	leader, err := LeaderInstructions(Engineering)
	if err != nil {
		t.Fatal(err)
	}
	for _, phrase := range []string{
		"only through worker assignments",
		"asmai assign",
		"an outcome and acceptance criteria",
		"read-only view",
		"Never write in it",
		"tests along with the code",
		"never a result",
		"asmai effect",
	} {
		if !strings.Contains(leader, phrase) {
			t.Errorf("Engineering's leader instructions never say %q", phrase)
		}
	}
	if _, err := WorkerInstructions("nowhere"); err == nil {
		t.Error("a role with no workers has instructions for them")
	}
}
