// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// localPushFixture makes a bare repository with one commit on main, which
// stands in for the fixture repository on GitHub, and returns its path.
func localPushFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	origin := filepath.Join(dir, "fixture.git")
	seed := filepath.Join(dir, "seed")
	for _, step := range []struct {
		dir  string
		args []string
	}{
		{dir, []string{"init", "-q", "--bare", "-b", "main", origin}},
		{dir, []string{"init", "-q", "-b", "main", seed}},
		{seed, []string{"-c", "user.name=T", "-c", "user.email=t@example.test", "-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "-m", "first"}},
		{seed, []string{"push", "-q", origin, "main"}},
	} {
		cmd := exec.Command("git", step.args...)
		cmd.Dir = step.dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(step.args, " "), err, out)
		}
	}
	return origin
}

func usePushFixture(t *testing.T, origin string) {
	t.Helper()
	previous := pushFixture
	pushFixture = origin
	t.Cleanup(func() { pushFixture = previous })
}

func TestC41PassesWhenThePushReachesOriginIsRefusedOverAnOutsideCommitAndKeepsItOnceMerged(t *testing.T) {
	origin := localPushFixture(t)
	usePushFixture(t, origin)
	r := result(t, harness(t, signedIn, startsDirectly), "C41")
	if r.Outcome != Passed {
		t.Fatalf("C41 %s: %s", r.Outcome, r.Failure)
	}
	evidence := strings.Join(r.Evidence, "\n")
	for _, want := range []string{"with a plain push reached origin", "pushed nothing and named it", "keeping the outside commit"} {
		if !strings.Contains(evidence, want) {
			t.Errorf("C41's evidence\n%s\ndoes not say %q", evidence, want)
		}
	}
	// The scratch branch is deleted from origin afterwards.
	out, err := exec.Command("git", "-C", origin, "branch", "--list", "asmai/*").CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "" {
		t.Errorf("origin still has branches %q (%v)", out, err)
	}
}

func TestC41FailsWhenTheFixtureCannotBeCloned(t *testing.T) {
	usePushFixture(t, filepath.Join(t.TempDir(), "missing.git"))
	r := result(t, harness(t, signedIn, startsDirectly), "C41")
	if r.Outcome != Failed || !strings.Contains(r.Failure, "cloning") {
		t.Errorf("C41 %s: %q, want it to fail cloning the fixture", r.Outcome, r.Failure)
	}
}
