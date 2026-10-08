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
	text, err := LeaderInstructions(Coordination)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Coordination's leader", "leader@coordination", "`asmai`", "never", "the user"} {
		if !strings.Contains(text, want) {
			t.Errorf("Coordination's instructions do not say %q", want)
		}
	}
	if _, err := LeaderInstructions("nowhere"); err == nil {
		t.Error("a role with no instructions has some")
	}
}
