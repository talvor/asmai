// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
	t.Logf("worker plugin delivered skills: %v", delivered)
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
			State  string            `json:"state"`
			Bundle string            `json:"bundle"`
			Skills map[string]string `json:"skills"`
		}](t, e)
		if d.ID != 3 || d.Bundle == "" {
			continue
		}
		recorded = true
		if d.State == "delivered" {
			t.Logf("asmai export assignment dispatch: %s", e.Data)
		}
		want, _ := skills.DeliveryOf([]string{"tdd", "diagnosing-bugs"})
		if d.Bundle != skills.Version() || len(d.Skills) != 2 || d.Skills["tdd"] != want.Skills["tdd"] || d.Skills["diagnosing-bugs"] != want.Skills["diagnosing-bugs"] {
			t.Errorf("dispatch 3 records bundle %q and %v, want %q and %v", d.Bundle, d.Skills, skills.Version(), want.Skills)
		}
	}
	if !recorded {
		t.Error("no change of dispatch 3 records the bundle version and skill fingerprints")
	}
}

// addedSkill writes the skill called name, whose SKILL.md says text, under
// parent and returns its directory.
func addedSkill(t *testing.T, parent, name, text string) string {
	t.Helper()
	dir := filepath.Join(parent, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(skillFile(name, text)), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func skillFile(name, text string) string {
	return "---\nname: " + name + "\ndescription: " + text + " Use when the work calls for it.\n---\n\n" + text + "\n"
}

// 08 rules 23-26: the configuration adds skills to the Engineering leader's
// and workers' lists, the added tdd replacing the shipped one, and switches
// the fixture's own skills on for Engineering. The worker is given its list
// with both kinds, copied at its start, and its dispatch records each with
// its fingerprint, marked unqualified as the user's or the repository's.
func TestAddedAndSwitchedOnRepositorySkillsReachTheirRolesAgentsMarkedUnqualifiedAndFingerprinted(t *testing.T) {
	added := t.TempDir()
	tdd := addedSkill(t, added, "tdd", "Our own test-first method.")
	style := addedSkill(t, added, "house-style", "Write code in the house style.")
	notes := addedSkill(t, added, "release-notes", "Write the release notes.")
	repository := map[string]string{".claude/skills/house-rules/SKILL.md": skillFile("house-rules", "Follow the fixture's rules.")}
	edit := func(config string) string {
		roles := fmt.Sprintf("[roles.engineering]\nleader_added_skills = [%q]\nworker_added_skills = [%q, %q]\n", notes, tdd, style)
		return strings.Replace(config, "[roles.engineering]\n", roles, 1) + "repository_skills = [\"engineering\"]\n"
	}
	const branch = "asmai/job-1/1"
	list := "implement, tdd, diagnosing-bugs, prototype, codebase-design, improve-codebase-architecture, pr, wizard, writing-for-agents, house-style, house-rules"
	// A skill the worker makes in the workspace's .claude/skills after its
	// start is not one its session was given, so its list leaves it out.
	worker := []string{
		fakeRun(`mkdir -p .claude/skills/late && printf -- '---\nname: late\ndescription: Made late.\n---\n' > .claude/skills/late/SKILL.md && echo made`, 0, "made"),
		fakeRun(`asmai inbox --dispatch 3`, 0, "(?s)Your skill list for this assignment: "+list+"\n"),
		fakeRun(`rm -r .claude/skills/late && echo removed`, 0, "removed"),
		fakeRun(`printf 'hello\n' > greeting.txt && git add greeting.txt && git commit -q -m 'Add greeting.txt' && asmai effect commit "$(git rev-parse HEAD)"`, 0, "effect 1 recorded: commit"),
		fakeRun(`git push -q origin `+branch+` && asmai effect push "origin/`+branch+`@$(git rev-parse HEAD)"`, 0, "effect 2 recorded: push"),
		fakeRun(`asmai result --evidence x --test none --check none --gap none --skill house-style --skill house-rules --pr-section x`, 0, "result recorded for assignment 1"),
	}
	leader := []string{
		fakeRun(`asmai inbox --dispatch 4`, 0, "(?s)Skills run:\n- house-style\n- house-rules"),
	}
	_, _ = assignmentFlowConfigured(t, gitIdentity, repository, edit, worker, leader)

	plugin := argValue(sessionArgs(t, "worker1@engineering"), "--plugin-dir")
	given := func(plugin string) []string {
		t.Helper()
		entries, err := os.ReadDir(filepath.Join(plugin, "skills"))
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		return names
	}
	want := strings.Split(list, ", ")
	slices.Sort(want)
	if got := given(plugin); !slices.Equal(got, want) {
		t.Errorf("the worker's plugin holds %v, want %v", got, want)
	}
	copied := filepath.Join(plugin, "skills", "tdd", "SKILL.md")
	if data, err := os.ReadFile(copied); err != nil || string(data) != skillFile("tdd", "Our own test-first method.") {
		t.Errorf("the worker's tdd is %q (%v), want the added skill's", data, err)
	}
	fingerprint := func(dir string) string {
		t.Helper()
		fp, err := skills.FingerprintFS(os.DirFS(dir))
		if err != nil {
			t.Fatal(err)
		}
		return fp
	}
	wantFingerprints := map[string]string{"tdd": fingerprint(tdd), "house-style": fingerprint(style), "house-rules": fingerprint(filepath.Join(plugin, "skills", "house-rules"))}
	// Editing an added skill does not change the running worker's copy.
	if err := os.WriteFile(filepath.Join(tdd, "SKILL.md"), []byte(skillFile("tdd", "Edited.")), 0o600); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(copied); err != nil || strings.Contains(string(data), "Edited.") {
		t.Errorf("editing the added tdd changed the running worker's copy to %q (%v)", data, err)
	}

	var recorded bool
	for _, e := range journaled(t, "dispatch.changed") {
		d := decode[struct {
			ID          int64             `json:"id"`
			Skills      map[string]string `json:"skills"`
			Unqualified map[string]string `json:"unqualified"`
		}](t, e)
		if d.ID != 3 || len(d.Skills) == 0 {
			continue
		}
		recorded = true
		if want := map[string]string{"tdd": "user", "house-style": "user", "house-rules": "repository"}; !maps.Equal(d.Unqualified, want) {
			t.Errorf("dispatch 3 marks %v unqualified, want %v", d.Unqualified, want)
		}
		implement, _ := skills.Fingerprint("implement")
		for name, fp := range wantFingerprints {
			if d.Skills[name] != fp {
				t.Errorf("dispatch 3 records %s as %s, want %s", name, d.Skills[name], fp)
			}
		}
		if len(d.Skills) != len(want) || d.Skills["implement"] != implement {
			t.Errorf("dispatch 3 records %v, want a fingerprint of each of %v", d.Skills, want)
		}
	}
	if !recorded {
		t.Error("no change of dispatch 3 records its skills")
	}

	// The leader has its added skill, and no repository skill; its index
	// marks its workers' added skills as the user's and unqualified.
	args := sessionArgs(t, "leader@engineering")
	if got := given(argValue(args, "--plugin-dir")); !slices.Equal(got, []string{"pr", "release-notes"}) {
		t.Errorf("Engineering's leader's plugin holds %v, want pr and release-notes", got)
	}
	instructions := argValue(args, "--append-system-prompt")
	if !strings.Contains(instructions, "The skills on your list are pr, release-notes.") || !strings.Contains(instructions, "## house-style\n") || !strings.Contains(instructions, "- Source: the user's, unqualified\n") || strings.Contains(instructions, "house-rules") {
		t.Errorf("Engineering's leader's instructions do not give its list and its workers' added skills:\n%s", instructions)
	}
	for _, e := range journaled(t, "session.started") {
		start := decode[struct {
			Agent       string            `json:"agent"`
			Unqualified map[string]string `json:"unqualified"`
		}](t, e)
		if start.Agent == "leader@engineering" && !maps.Equal(start.Unqualified, map[string]string{"release-notes": "user"}) {
			t.Errorf("Engineering's leader's session records %v unqualified, want release-notes as the user's", start.Unqualified)
		}
	}
}
