// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/talvor/asmai/internal/config"
	"github.com/talvor/asmai/internal/skills"
)

func writeSkill(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	text := "---\nname: " + filepath.Base(dir) + "\ndescription: A skill.\n---\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Rule 25: a repository's own skills reach only the roles its configuration
// switches them on for, and only workers in that repository's jobs.
func TestRepositorySkillsReachOnlyTheRolesSwitchedOnInThatRepositorysJobs(t *testing.T) {
	workspace := t.TempDir()
	writeSkill(t, filepath.Join(workspace, ".claude", "skills", "house-rules"))
	writeSkill(t, filepath.Join(workspace, ".claude", "skills", "tdd"))
	added := writeSkill(t, filepath.Join(t.TempDir(), "house-style"))
	cfg := config.Config{Skills: map[string]config.Skills{"engineering": {WorkerAdded: []string{added}}}}
	on := config.RepositoryOptions{RepositorySkills: []string{"engineering"}}

	names := func(role string, narrowed []string, options config.RepositoryOptions) ([]string, []string) {
		t.Helper()
		list, left, err := sessionSkills(cfg, role, narrowed, options, workspace)
		if err != nil {
			t.Fatal(err)
		}
		return skills.NamesOf(list), skills.NamesOf(left)
	}
	engineering := append(skills.WorkerList("engineering"), "house-style")
	got, left := names("engineering", nil, on)
	if want := append(slices.Clone(engineering), "house-rules"); !slices.Equal(got, want) {
		t.Errorf("an Engineering worker in the repository's job is given %v, want %v", got, want)
	}
	// The list already has a tdd, which the repository's does not replace.
	if !slices.Equal(left, []string{"tdd"}) {
		t.Errorf("the repository skills left out are %v, want its tdd", left)
	}
	if got, _ := names("engineering", []string{"house-style"}, on); !slices.Equal(got, []string{"house-style", "house-rules"}) {
		t.Errorf("a narrowed Engineering worker is given %v, want its narrowed list and the repository's skills", got)
	}
	if got, _ := names("quality", nil, on); !slices.Equal(got, skills.WorkerList("quality")) {
		t.Errorf("a Quality worker, not switched on, is given %v", got)
	}
	// Another repository's job switches nothing on.
	if got, _ := names("engineering", nil, config.DefaultRepositoryOptions()); !slices.Equal(got, engineering) {
		t.Errorf("an Engineering worker in another repository's job is given %v, want %v", got, engineering)
	}
	if _, _, err := sessionSkills(cfg, "engineering", []string{"no-such-skill"}, on, workspace); err == nil {
		t.Error("a list narrowed to a skill no longer on the role's list was given")
	}
}

func TestALeaderIsGivenItsConfiguredListWithItsAddedSkills(t *testing.T) {
	notes := writeSkill(t, filepath.Join(t.TempDir(), "release-notes"))
	pr := writeSkill(t, filepath.Join(t.TempDir(), "pr"))
	cfg := config.Config{Skills: map[string]config.Skills{
		"engineering": {LeaderAdded: []string{notes, pr}},
		"planning":    {LeaderSkills: []string{"grilling"}},
	}}
	list, err := leaderSkills(cfg, "engineering")
	want := []skills.Given{{Name: "pr", Source: skills.User, Dir: pr}, {Name: "release-notes", Source: skills.User, Dir: notes}}
	if err != nil || !slices.Equal(list, want) {
		t.Errorf("Engineering's leader is given %+v (%v), want %+v", list, err, want)
	}
	if list, err := leaderSkills(cfg, "planning"); err != nil || !slices.Equal(skills.NamesOf(list), []string{"grilling"}) {
		t.Errorf("Planning's leader is given %+v (%v), want its configured list", list, err)
	}
	if list, err := leaderSkills(config.Config{}, "coordination"); err != nil || !slices.Equal(skills.NamesOf(list), skills.LeaderList("coordination")) {
		t.Errorf("Coordination's leader is given %+v (%v), want its default list", list, err)
	}
}
