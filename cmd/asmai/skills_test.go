// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/talvor/asmai/internal/skills"
)

func TestAWorkerIsGivenTheListItsAssignmentNarrowedToAsAPerSessionPluginAndTheDispatchRecordsTheBundleAndFingerprints(t *testing.T) {
	assignmentSkillFlags = " --skill tdd --narrow tdd --narrow diagnosing-bugs"
	t.Cleanup(func() { assignmentSkillFlags = "" })
	const branch = "asmai/job-1/1"
	worker := []string{
		fakeRun(`asmai inbox --dispatch 3`, 0, "(?s)Skill to run: tdd\nYour skill list for this assignment: tdd, diagnosing-bugs\n"),
		fakeRun(`printf 'hello\n' > greeting.txt && git add -A && git commit -q -m 'Add greeting.txt' && asmai effect commit "$(git rev-parse HEAD)"`, 0, "effect 1 recorded: commit"),
		fakeRun(`git push -q origin `+branch+` && asmai effect push "origin/`+branch+`@$(git rev-parse HEAD)"`, 0, "effect 2 recorded: push"),
		// Only a skill of the worker's own list is one it ran.
		fakeRun(`asmai result --evidence x --test none --check none --gap none --skill prototype --pr-section x`, 1, "not on your skill list"),
		fakeRun(`asmai result --evidence x --test none --check none --gap none --skill tdd --pr-section x`, 0, "result recorded for assignment 1"),
	}
	leader := []string{
		fakeRun(`asmai inbox --dispatch 4`, 0, "(?s)Skills run:\n- tdd"),
		// A list is only narrowed, and the skill named stays on it.
		fakeRun(`asmai assign --job 1 --outcome x --criterion y --narrow code-review`, 1, "not on engineering's workers' skill list"),
		fakeRun(`asmai assign --job 1 --outcome x --criterion y --skill implement --narrow tdd`, 1, "not on the worker's skill list"),
	}
	_, _ = assignmentFlowWith(t, gitIdentity, nil, worker, leader)

	// The worker's command line gives it the plugin with exactly the narrowed
	// list, and no setting source.
	args := sessionArgs(t, "worker1@engineering")
	if i := slices.Index(args, "--setting-sources"); i < 0 || args[i+1] != "" {
		t.Errorf("the worker was started with %q, want an empty --setting-sources", args)
	}
	plugin := argValue(args, "--plugin-dir")
	if plugin == "" {
		t.Fatalf("the worker was started with %q, want --plugin-dir", args)
	}
	entries, err := os.ReadDir(filepath.Join(plugin, "skills"))
	if err != nil {
		t.Fatal(err)
	}
	var delivered []string
	for _, e := range entries {
		delivered = append(delivered, e.Name())
	}
	if want := []string{"diagnosing-bugs", "tdd"}; !slices.Equal(delivered, want) {
		t.Errorf("the worker's plugin holds %v, want the narrowed list %v", delivered, want)
	}
	if _, err := os.Stat(filepath.Join(plugin, ".claude-plugin", "plugin.json")); err != nil {
		t.Errorf("the worker's plugin has no manifest: %v", err)
	}
	var settings struct {
		Permissions struct {
			Allow []string `json:"allow"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal([]byte(argValue(args, "--settings")), &settings); err != nil {
		t.Fatal(err)
	}
	if want := "Read(/" + plugin + "/**)"; !slices.Contains(settings.Permissions.Allow, want) {
		t.Errorf("the worker's permission rules are %q, want %q", settings.Permissions.Allow, want)
	}

	// Its dispatch records the bundle's version and each skill's fingerprint.
	var recorded bool
	for _, e := range journaled(t, "dispatch.changed") {
		d := decode[struct {
			ID     int64             `json:"id"`
			Bundle string            `json:"bundle"`
			Skills map[string]string `json:"skills"`
		}](t, e)
		if d.ID != 3 || d.Bundle == "" {
			continue
		}
		recorded = true
		want, _ := skills.DeliveryOf([]string{"tdd", "diagnosing-bugs"})
		if d.Bundle != skills.Version() || len(d.Skills) != 2 || d.Skills["tdd"] != want.Skills["tdd"] || d.Skills["diagnosing-bugs"] != want.Skills["diagnosing-bugs"] {
			t.Errorf("dispatch 3 records bundle %q and %v, want %q and %v", d.Bundle, d.Skills, skills.Version(), want.Skills)
		}
	}
	if !recorded {
		t.Error("no change of dispatch 3 records the bundle version and skill fingerprints")
	}
}
