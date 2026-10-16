// SPDX-License-Identifier: Apache-2.0

package skills

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// skillDir writes a skill called name, whose SKILL.md has the description
// description, under parent, and returns its directory.
func skillDir(t *testing.T, parent, name, description string) string {
	t.Helper()
	dir := filepath.Join(parent, name)
	if err := os.MkdirAll(filepath.Join(dir, "notes"), 0o700); err != nil {
		t.Fatal(err)
	}
	text := "---\nname: " + name + "\ndescription: " + description + "\n---\n\nThe " + name + " skill.\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes", "more.md"), []byte("More on "+name+".\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Rule 23: an added skill with a shipped skill's name replaces it for the
// role, in its place; any other joins the list after it.
func TestAnAddedSkillReplacesTheShippedSkillOfItsNameAndOthersJoinTheList(t *testing.T) {
	parent := t.TempDir()
	tdd := skillDir(t, parent, "tdd", "Our own test-first method.")
	style := skillDir(t, parent, "house-style", "Write code in the house style. Use when writing code.")
	list, err := List([]string{"implement", "tdd", "pr"}, []string{style, tdd})
	if err != nil {
		t.Fatal(err)
	}
	want := []Given{
		{Name: "implement", Source: Shipped},
		{Name: "tdd", Source: User, Dir: tdd},
		{Name: "pr", Source: Shipped},
		{Name: "house-style", Source: User, Dir: style},
	}
	if !slices.Equal(list, want) {
		t.Errorf("the list is %+v, want %+v", list, want)
	}
	for _, g := range list {
		if g.Qualified() != (g.Source == Shipped) {
			t.Errorf("%s from %s is qualified %v", g.Name, g.Source, g.Qualified())
		}
	}
}

func TestAnAddedSkillMustHoldASkillAndHaveANameOfItsOwn(t *testing.T) {
	parent := t.TempDir()
	if _, err := List(nil, []string{filepath.Join(parent, "missing")}); err == nil || !strings.Contains(err.Error(), "no SKILL.md") {
		t.Errorf("a directory without a SKILL.md was added: %v", err)
	}
	one := skillDir(t, filepath.Join(parent, "one"), "mine", "Mine.")
	two := skillDir(t, filepath.Join(parent, "two"), "mine", "Mine too.")
	if _, err := List([]string{"tdd"}, []string{one, two}); err == nil || !strings.Contains(err.Error(), "same name") {
		t.Errorf("two added skills of one name were added: %v", err)
	}
}

func TestAnAddedSkillInTheHomeDirectoryIsWrittenWithATilde(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := skillDir(t, filepath.Join(home, "skills"), "mine", "Mine.")
	list, err := List(nil, []string{"~/skills/mine"})
	if err != nil || len(list) != 1 || list[0].Dir != dir {
		t.Errorf("~/skills/mine is %+v (%v), want the skill at %s", list, err, dir)
	}
}

// Rule 24: added skills are copied when an agent starts, so editing one
// never changes a running agent, and the next start picks the edit up. Each
// is fingerprinted as copied and marked unqualified, as the user's.
func TestAddedSkillsAreCopiedAtEachStartFingerprintedAndMarkedUnqualified(t *testing.T) {
	parent := t.TempDir()
	tdd := skillDir(t, parent, "tdd", "Our own test-first method.")
	repo := skillDir(t, filepath.Join(parent, "repository"), "house-rules", "The repository's rules.")
	list, err := List([]string{"implement", "tdd"}, []string{tdd})
	if err != nil {
		t.Fatal(err)
	}
	list = append(list, Given{Name: "house-rules", Source: Repository, Dir: repo})
	plugin := filepath.Join(t.TempDir(), "plugin")
	d, err := WriteSessionPlugin(plugin, list)
	if err != nil {
		t.Fatal(err)
	}
	copied := filepath.Join(plugin, Dir, "tdd")
	if data, err := os.ReadFile(filepath.Join(copied, "notes", "more.md")); err != nil || string(data) != "More on tdd.\n" {
		t.Errorf("the plugin's tdd holds %q (%v), want the added skill's files", data, err)
	}
	first, err := FingerprintFS(os.DirFS(tdd))
	if err != nil {
		t.Fatal(err)
	}
	implement, _ := Fingerprint("implement")
	houseRules, _ := FingerprintFS(os.DirFS(repo))
	if d.Bundle != Version() || d.Skills["tdd"] != first || d.Skills["implement"] != implement || d.Skills["house-rules"] != houseRules || len(d.Skills) != 3 {
		t.Errorf("the delivery is %+v, want the bundle %s and the fingerprints of the copies", d, Version())
	}
	if want := map[string]string{"tdd": User, "house-rules": Repository}; len(d.Unqualified) != 2 || d.Unqualified["tdd"] != User || d.Unqualified["house-rules"] != Repository {
		t.Errorf("the delivery marks %v unqualified, want %v", d.Unqualified, want)
	}

	// Editing the added skill leaves the running session's copy as it was.
	if err := os.WriteFile(filepath.Join(tdd, "SKILL.md"), []byte("---\nname: tdd\ndescription: Edited.\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if fp, err := FingerprintFS(os.DirFS(copied)); err != nil || fp != first {
		t.Errorf("editing the added skill changed the running session's copy to %s (%v)", fp, err)
	}
	// The next start copies the edit, and records its new fingerprint.
	d, err = WriteSessionPlugin(plugin, list)
	if err != nil {
		t.Fatal(err)
	}
	edited, _ := FingerprintFS(os.DirFS(tdd))
	if edited == first || d.Skills["tdd"] != edited {
		t.Errorf("the next start records tdd as %s, want the edited skill's %s", d.Skills["tdd"], edited)
	}
	if data, _ := os.ReadFile(filepath.Join(copied, "SKILL.md")); !strings.Contains(string(data), "Edited.") {
		t.Errorf("the next start's tdd is %q, want the edit", data)
	}
}

func TestAShippedOnlyPluginRecordsNothingUnqualified(t *testing.T) {
	d, err := WriteSessionPlugin(filepath.Join(t.TempDir(), "plugin"), Bundled([]string{"tdd"}))
	if err != nil || len(d.Unqualified) != 0 {
		t.Errorf("a plugin of shipped skills records %+v (%v) unqualified", d.Unqualified, err)
	}
}

// Rule 25: a repository's own skills are the directories in its
// .claude/skills that hold a SKILL.md.
func TestARepositorysSkillsAreItsClaudeSkillsDirectories(t *testing.T) {
	root := t.TempDir()
	if list, err := RepositorySkills(root); err != nil || len(list) != 0 {
		t.Errorf("a repository without .claude/skills has %+v (%v)", list, err)
	}
	skills := filepath.Join(root, ".claude", "skills")
	rules := skillDir(t, skills, "house-rules", "The repository's rules.")
	if err := os.MkdirAll(filepath.Join(skills, "empty"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skills, "README.md"), []byte("not a skill\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	list, err := RepositorySkills(root)
	if want := []Given{{Name: "house-rules", Source: Repository, Dir: rules}}; err != nil || !slices.Equal(list, want) {
		t.Errorf("the repository's skills are %+v (%v), want %+v", list, err, want)
	}
}

func TestASkillWhoseSKILLmdNamesItOtherThanItsDirectoryIsRefused(t *testing.T) {
	parent := t.TempDir()
	ours := filepath.Join(parent, "tdd-ours")
	if err := os.MkdirAll(ours, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ours, "SKILL.md"), []byte("---\nname: tdd\ndescription: Ours.\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := List([]string{"tdd"}, []string{ours})
	if err == nil || !strings.Contains(err.Error(), ours) || !strings.Contains(err.Error(), "named tdd") || !strings.Contains(err.Error(), "not tdd-ours") {
		t.Errorf("an added skill named otherwise by its SKILL.md was added: %v", err)
	}

	root := t.TempDir()
	skills := filepath.Join(root, ".claude", "skills")
	skillDir(t, skills, "house-rules", "The repository's rules.")
	odd := filepath.Join(skills, "odd")
	if err := os.MkdirAll(odd, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(odd, "SKILL.md"), []byte("---\nname: pr\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	list, err := RepositorySkills(root)
	if err == nil || !strings.Contains(err.Error(), odd) || !strings.Contains(err.Error(), "named pr") || !strings.Contains(err.Error(), "not odd") {
		t.Errorf("a repository skill named otherwise by its SKILL.md was given: %+v (%v)", list, err)
	}

	plain := filepath.Join(parent, "plain")
	if err := os.MkdirAll(plain, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plain, "SKILL.md"), []byte("---\ndescription: No name.\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := List(nil, []string{plain}); err != nil {
		t.Errorf("an added skill whose SKILL.md gives no name was refused: %v", err)
	}
}

// Rule 12: a leader's index covers its workers' added skills too, from their
// own descriptions, marked as the user's and unqualified.
func TestALeadersIndexMarksItsWorkersAddedSkillsAsTheUsersAndUnqualified(t *testing.T) {
	parent := t.TempDir()
	style := skillDir(t, parent, "house-style", "Write code in the house style. Use when writing or reviewing code.")
	list, err := List([]string{"tdd"}, []string{style})
	if err != nil {
		t.Fatal(err)
	}
	entries := IndexFor(list)
	if len(entries) != 2 {
		t.Fatalf("the index is %+v, want tdd and house-style", entries)
	}
	if e := entries[1]; e.Name != "house-style" || e.Purpose != "Write code in the house style." || e.Applies != "When writing or reviewing code." || e.Source != User {
		t.Errorf("house-style's entry is %+v", e)
	}
	text := IndexText("engineering", entries)
	if !strings.Contains(text, "## house-style\n") || !strings.Contains(text, "- Source: the user's, unqualified\n") {
		t.Errorf("the index does not mark the added skill:\n%s", text)
	}
	if strings.Count(text, "unqualified") != 1 {
		t.Errorf("the index marks a shipped skill unqualified:\n%s", text)
	}
	if IndexText("engineering", IndexOf("engineering")) != Index("engineering") {
		t.Error("the default index changed")
	}
}

func TestNarrowingIsWithinTheRolesListAddedSkillsIncluded(t *testing.T) {
	list := []string{"implement", "tdd", "house-style"}
	got, err := Narrow("engineering", list, []string{"house-style", "tdd"}, "house-style")
	if err != nil || !slices.Equal(got, []string{"house-style", "tdd"}) {
		t.Errorf("narrowing to an added skill gave %v (%v)", got, err)
	}
	if _, err := Narrow("engineering", list, []string{"pr"}, ""); err == nil {
		t.Error("a list was widened")
	}
	if _, err := Narrow("engineering", list, nil, "pr"); err == nil {
		t.Error("a skill off the list was named")
	}
}

func TestASkillHoldingASymbolicLinkIsRefusedAndNothingOutsideItIsCopied(t *testing.T) {
	parent := t.TempDir()
	outside := filepath.Join(parent, "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.md"), []byte("not the skill's\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{"linked-dir": outside, "linked-file": filepath.Join(outside, "secret.md")} {
		dir := skillDir(t, parent, name, "A skill with a link.")
		if err := os.Symlink(target, filepath.Join(dir, "shared")); err != nil {
			t.Fatal(err)
		}
		plugin := filepath.Join(t.TempDir(), "plugin")
		_, err := WriteSessionPlugin(plugin, []Given{{Name: name, Source: Repository, Dir: dir}})
		if err == nil || !strings.Contains(err.Error(), "symbolic link") {
			t.Errorf("%s: writing the plugin returned %v, want a refusal of the symbolic link", name, err)
		}
		if _, err := os.Lstat(filepath.Join(plugin, Dir, name, "shared")); err == nil {
			t.Errorf("%s: the link's target was copied into the plugin", name)
		}
	}
}

func TestARepositoryWhoseSkillsDirectoryIsASymbolicLinkHasNone(t *testing.T) {
	for _, link := range []string{".claude", filepath.Join(".claude", "skills")} {
		outside := t.TempDir()
		skillDir(t, filepath.Join(outside, "skills"), "personal", "Someone's own skill.")
		target := outside
		if link != ".claude" {
			target = filepath.Join(outside, "skills")
		}
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, link)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
		if list, err := RepositorySkills(root); err != nil || len(list) != 0 {
			t.Errorf("a repository whose %s is a symbolic link has %+v (%v), want none", link, list, err)
		}
	}
}
