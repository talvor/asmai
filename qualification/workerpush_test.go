// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestC51PassesWhenTheDaemonCommitsMergesAndPushesTheWorkersBranch(t *testing.T) {
	origin := localPushFixture(t)
	usePushFixture(t, origin)
	r := result(t, harness(t, signedIn, startsDirectly), "C51")
	if r.Outcome != Passed {
		t.Fatalf("C51 %s: %s", r.Outcome, r.Failure)
	}
	evidence := strings.Join(r.Evidence, "\n")
	for _, want := range []string{"the daemon's commit, run as the user with the worker's environment and no sandbox, took the worker's new file in", "refused a second commit with nothing changed", "the daemon's merge of origin/main found it already in the worker's branch", "the daemon's plain push of asmai/qualification-c51-"} {
		if !strings.Contains(evidence, want) {
			t.Errorf("C51's evidence\n%s\ndoes not say %q", evidence, want)
		}
	}
	out, err := exec.Command("git", "-C", origin, "branch", "--list", "asmai/*").CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "" {
		t.Errorf("origin still has branches %q (%v)", out, err)
	}
}

func TestC51FailsWhenTheFixtureCannotBeCloned(t *testing.T) {
	usePushFixture(t, filepath.Join(t.TempDir(), "missing.git"))
	r := result(t, harness(t, signedIn, startsDirectly), "C51")
	if r.Outcome != Failed || !strings.Contains(r.Failure, "cloning") {
		t.Errorf("C51 %s: %q, want it to fail cloning the fixture", r.Outcome, r.Failure)
	}
}

// A repository whose hook refuses the commit stops the daemon's commit, which
// never skips a hook (ADR 0018), and nothing reaches origin.
func TestC51FailsWhenARepositoryHookRefusesTheDaemonsCommit(t *testing.T) {
	origin := localPushFixture(t)
	usePushFixture(t, origin)
	// The clone is the case's own, so the hook comes through git's global
	// hooks path.
	dir := t.TempDir()
	hook := filepath.Join(dir, "pre-commit")
	if out, err := exec.Command("sh", "-c", "printf '#!/bin/sh\\necho refused by the repository >&2\\nexit 1\\n' > "+hook+" && chmod +x "+hook).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.hooksPath")
	t.Setenv("GIT_CONFIG_VALUE_0", dir)
	r := result(t, harness(t, signedIn, startsDirectly), "C51")
	if r.Outcome != Failed || !strings.Contains(r.Failure, "the daemon's commit of the worker's new file") || !strings.Contains(r.Failure, "refused by the repository") {
		t.Errorf("C51 %s: %q, want it to fail at the daemon's commit, refused by the hook", r.Outcome, r.Failure)
	}
	out, err := exec.Command("git", "-C", origin, "branch", "--list", "asmai/*").CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "" {
		t.Errorf("origin has branches %q (%v)", out, err)
	}
}
