// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/talvor/asmai/internal/providers"
)

func TestC1PassesOnACertifiedPlatformAndShowsTheRefusalOfOthers(t *testing.T) {
	h := harness(t, signedIn, startsDirectly)

	r := result(t, h, "C1")

	if r.Outcome != Passed {
		t.Fatalf("C1 %s: %s", r.Outcome, r.Failure)
	}
	evidence := strings.Join(r.Evidence, "\n")
	for _, want := range []string{runtime.GOOS + "/" + runtime.GOARCH + " passed asmai start's platform check", "Linux on x86_64 and macOS on arm64", "refuses 4 platforms"} {
		if !strings.Contains(evidence, want) {
			t.Errorf("C1's evidence\n%s\ndoes not say %q", evidence, want)
		}
	}
}

// With a Claude Code that is not the pinned version at the pinned copy's
// path, asmai start is refused before any dispatch; a provider that updated
// itself under a running leader holds it at its next restart; and the hold
// lifts when the pinned copy is back, which the case leaves where it was.
func TestC2PassesWhenAnUnpinnedVersionIsRefusedAndASelfUpdatedProviderIsHeld(t *testing.T) {
	h := harness(t, signedIn, startsDirectly)
	perFolder(t, h, map[string][]string{"agents": setupTrusting, "workspaces": setupTrusting})

	r := result(t, h, "C2")

	if r.Outcome != Passed {
		t.Fatalf("C2 %s: %s", r.Outcome, r.Failure)
	}
	evidence := strings.Join(r.Evidence, "\n")
	for _, want := range []string{"refused before any dispatch", otherVersion, "the leader was not restarted", "with the pinned copy back"} {
		if !strings.Contains(evidence, want) {
			t.Errorf("C2's evidence\n%s\ndoes not say %q", evidence, want)
		}
	}
	platform, _ := providers.Platform(runtime.GOOS, runtime.GOARCH)
	plan, err := h.Subject.Pins.ClaudeCodePlan(platform, h.QualificationPaths.Providers)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(plan.Path)
	if err != nil || strings.Contains(string(data), otherVersion) {
		t.Errorf("after C2 the pinned copy at %s is %q, %v, want it put back", plan.Path, data, err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(plan.Path), "claude.qualification-real")); err == nil {
		t.Error("C2 left the pinned copy's stand-in aside")
	}
}
