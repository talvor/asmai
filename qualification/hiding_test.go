// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// hidingSession plays Claude Code listing a session's skills as the pinned
// version does: the personal and repository skills of the setting sources it
// loads, its own skills unless its settings hide them, the plugin's under its
// namespace, and its built-in slash commands.
type hidingSession struct {
	// sources is what the session loads instead of the empty
	// --setting-sources it is given, keepsCodeReview has it list its
	// built-in code-review whatever its settings say, and slashSkill has it
	// list /init as a skill.
	sources         bool
	keepsCodeReview bool
	slashSkill      bool
	// seen are the personal skills the session found in the user's home.
	seen []string
}

func (s *hidingSession) ask(spec AskSpec) (Turn, error) {
	plugin := argAfter(spec.Args, "--plugin-dir")
	var settings struct {
		DisableBundledSkills bool              `json:"disableBundledSkills"`
		SkillOverrides       map[string]string `json:"skillOverrides"`
	}
	if err := json.Unmarshal([]byte(argAfter(spec.Args, "--settings")), &settings); err != nil {
		return Turn{}, err
	}
	turn := Turn{Plugins: []string{"asmai"}, Reply: "done"}
	if entries, err := os.ReadDir(filepath.Join(plugin, "skills")); err == nil {
		for _, e := range entries {
			turn.Skills = append(turn.Skills, "asmai:"+e.Name())
		}
	}
	personal, _ := os.ReadDir(filepath.Join(os.Getenv("HOME"), ".claude", "skills"))
	for _, e := range personal {
		s.seen = append(s.seen, e.Name())
	}
	if s.sources || !hasFlagValue(spec.Args, "--setting-sources", "") {
		for _, dir := range []string{filepath.Join(os.Getenv("HOME"), ".claude", "skills"), filepath.Join(spec.Dir, ".claude", "skills")} {
			entries, _ := os.ReadDir(dir)
			for _, e := range entries {
				turn.Skills = append(turn.Skills, e.Name())
			}
		}
	}
	if !settings.DisableBundledSkills || s.keepsCodeReview {
		turn.Skills = append(turn.Skills, "code-review")
	}
	if !settings.DisableBundledSkills {
		turn.Skills = append(turn.Skills, "simplify", "loop")
	}
	for _, own := range []string{"design", "doctor", "plugin-authoring"} {
		if settings.SkillOverrides[own] != "off" {
			turn.Skills = append(turn.Skills, own)
		}
	}
	if s.slashSkill {
		turn.Skills = append(turn.Skills, "init")
	}
	turn.SlashCommands = append(slices.Clone(turn.Skills), "security-review", "init", "clear")
	return turn, nil
}

func hidingHarness(t *testing.T, s *hidingSession) Result {
	t.Helper()
	checkGuardDependencies = func() error { return nil }
	t.Cleanup(func() { checkGuardDependencies = guardDependencies })
	return result(t, harness(t, fakeClaude{auth: AuthStatus{SignedIn: true, Method: "claude.ai"}, ask: s.ask}, startsDirectly), "C33")
}

func TestC33PassesWhenTheSessionListsOnlyItsOwnSkills(t *testing.T) {
	s := &hidingSession{}
	r := hidingHarness(t, s)
	if r.Outcome != Passed {
		t.Fatalf("C33 %s: %s", r.Outcome, r.Failure)
	}
	evidence := strings.Join(r.Evidence, "\n")
	for _, want := range []string{"exactly its list as the plugin asmai: asmai:code-review, asmai:retro, asmai:tdd", "built-in skills, its code-review next to the shipped asmai:code-review", "security-review, init"} {
		if !strings.Contains(evidence, want) {
			t.Errorf("C33's evidence\n%s\ndoes not say %q", evidence, want)
		}
	}
	if len(s.seen) != 1 || !strings.HasPrefix(s.seen[0], "asmai-c33-personal-") {
		t.Errorf("the session found the personal skills %v, want the one the case planted", s.seen)
	}
	if left, _ := os.ReadDir(filepath.Join(os.Getenv("HOME"), ".claude", "skills")); len(left) != 0 {
		t.Errorf("the case left %v in the user's skills", left)
	}
	if exists(filepath.Join(os.Getenv("HOME"), ".claude", "skills")) {
		t.Error("the case left the user's skills directory it made")
	}
}

func TestC33KeepsTheUsersOwnSkills(t *testing.T) {
	s := &hidingSession{}
	checkGuardDependencies = func() error { return nil }
	t.Cleanup(func() { checkGuardDependencies = guardDependencies })
	h := harness(t, fakeClaude{auth: AuthStatus{SignedIn: true, Method: "claude.ai"}, ask: s.ask}, startsDirectly)
	own := filepath.Join(os.Getenv("HOME"), ".claude", "skills", "mine")
	if err := writeSkill(own, "mine", "The user's own."); err != nil {
		t.Fatal(err)
	}
	if r := result(t, h, "C33"); r.Outcome != Passed {
		t.Fatalf("C33 %s: %s", r.Outcome, r.Failure)
	}
	if !exists(filepath.Join(own, "SKILL.md")) {
		t.Error("the case removed the user's own skill")
	}
}

func TestC33FailsWhenASkillIsNotHidden(t *testing.T) {
	for name, c := range map[string]struct {
		session hidingSession
		want    string
	}{
		"personal and repository": {hidingSession{sources: true}, "personal skills are not hidden"},
		"built-in code-review":    {hidingSession{keepsCodeReview: true}, "built-in code-review next to the shipped asmai:code-review"},
	} {
		t.Run(name, func(t *testing.T) {
			r := hidingHarness(t, &c.session)
			if r.Outcome != Failed || !strings.Contains(r.Failure, c.want) {
				t.Errorf("C33 %s: %q, want it to fail saying %q", r.Outcome, r.Failure, c.want)
			}
		})
	}
}

func TestC33FailsWhenTheRecordOfWhatIsNotHiddenIsOutOfDate(t *testing.T) {
	r := hidingHarness(t, &hidingSession{slashSkill: true})
	if r.Outcome != Failed || !strings.Contains(r.Failure, "init") {
		t.Errorf("C33 %s: %q, want it to fail on /init listed as a skill", r.Outcome, r.Failure)
	}
}
