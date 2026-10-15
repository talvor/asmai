// SPDX-License-Identifier: Apache-2.0

package skills

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestAPluginHoldsExactlyTheShippedSkillsUnderAManifestNamedAsmai(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "plugin")
	// What an earlier start left is not carried into the next.
	if err := os.MkdirAll(filepath.Join(dir, Dir, "stale"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := WritePlugin(dir, []string{"tdd", "implement"}); err != nil {
		t.Fatal(err)
	}
	var manifest struct{ Name, Version string }
	data, err := os.ReadFile(filepath.Join(dir, ".claude-plugin", "plugin.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &manifest); err != nil || manifest.Name != PluginName || manifest.Version != Version() {
		t.Errorf("the plugin manifest is %s (%v), want the name %q and the bundle version %q", data, err, PluginName, Version())
	}
	entries, err := os.ReadDir(filepath.Join(dir, Dir))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if want := []string{"implement", "tdd"}; !slices.Equal(got, want) {
		t.Fatalf("the plugin holds %v, want %v", got, want)
	}
	for _, name := range []string{"tdd", "implement"} {
		fp, err := FingerprintFS(os.DirFS(filepath.Join(dir, Dir, name)))
		want, _ := Fingerprint(name)
		if err != nil || fp != want {
			t.Errorf("the plugin's %s has the fingerprint %s (%v), want the bundle's %s", name, fp, err, want)
		}
	}
	if err := WritePlugin(dir, []string{"no-such-skill"}); err == nil {
		t.Error("a plugin was written for a skill the bundle does not ship")
	}
}

func TestDeliveredRequestOnlySkillsCanBeInvokedAndKeepTheirOriginalStatus(t *testing.T) {
	names := []string{
		"ask-matt", "grill-me", "grill-with-docs", "handoff", "implement", "implement-spec",
		"improve-codebase-architecture", "retro", "setup-matt-pocock-skills", "teach",
		"to-questionnaire", "to-spec", "to-tickets", "triage", "wait-what", "wayfinder",
	}
	dir := filepath.Join(t.TempDir(), "plugin")
	if err := WritePlugin(dir, names); err != nil {
		t.Fatal(err)
	}
	delivery, err := DeliveryOf(names)
	if err != nil {
		t.Fatal(err)
	}
	upstream := Upstream()
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(dir, Dir, name, "SKILL.md"))
		if err != nil {
			t.Fatal(err)
		}
		if got := frontMatter(string(data))["disable-model-invocation"]; got != "" {
			t.Errorf("delivered %s disables model invocation: %q", name, got)
		}
		skill, ok := Get(name)
		if !ok || !skill.UserInvoked {
			t.Errorf("%s lost its upstream request-only status", name)
		}
		fingerprint, err := FingerprintFS(os.DirFS(filepath.Join(dir, Dir, name)))
		if err != nil {
			t.Fatal(err)
		}
		if fingerprint != delivery.Skills[name] || fingerprint == upstream[name] {
			t.Errorf("%s has delivery fingerprint %s, plugin fingerprint %s, upstream fingerprint %s", name, delivery.Skills[name], fingerprint, upstream[name])
		}
	}
}

func TestAWorkersListIsItsRolesDefaultOrANarrowingOfItAndTheSkillNamedStaysOnIt(t *testing.T) {
	list, err := WorkerSkills("engineering", nil, "")
	if err != nil || !slices.Equal(list, WorkerList("engineering")) {
		t.Errorf("the default list is %v (%v), want the role's", list, err)
	}
	list, err = WorkerSkills("engineering", []string{"tdd", "diagnosing-bugs", "tdd"}, "tdd")
	if err != nil || !slices.Equal(list, []string{"tdd", "diagnosing-bugs"}) {
		t.Errorf("the narrowed list is %v (%v), want tdd and diagnosing-bugs", list, err)
	}
	if _, err := WorkerSkills("engineering", []string{"code-review"}, ""); err == nil {
		t.Error("a list was widened with a skill of another role's list")
	}
	if _, err := WorkerSkills("engineering", []string{"tdd"}, "implement"); err == nil {
		t.Error("a skill off the narrowed list was named")
	}
	if _, err := WorkerSkills("engineering", nil, "code-review"); err == nil {
		t.Error("a skill off the role's list was named")
	}
}

func TestADeliveryRecordsTheBundleVersionAndEachSkillsFingerprint(t *testing.T) {
	d, err := DeliveryOf([]string{"tdd", "pr"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Bundle != Version() || len(d.Skills) != 2 {
		t.Fatalf("the delivery is %+v", d)
	}
	for name, fp := range d.Skills {
		if want, _ := Fingerprint(name); fp != want || fp == "" {
			t.Errorf("%s is recorded with %q, want %q", name, fp, want)
		}
	}
	if got := Namespaced("tdd"); got != "asmai:tdd" {
		t.Errorf("Claude Code names the skill %q, want asmai:tdd", got)
	}
}
