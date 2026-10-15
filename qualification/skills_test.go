// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// skillsSession plays Claude Code starting a worker with a plugin: which
// skills it lists, and how it resolves the skill implement names.
type skillsSession struct {
	// listsExtra makes the session list a skill the assignment narrowed out.
	listsExtra bool
	// bareFails makes the bare name not resolve, and namespacedFails the
	// namespaced one too.
	bareFails, namespacedFails bool
	// asksNothing makes the session run no skill.
	asksNothing bool
	// noList makes the start list no skills.
	noList bool
}

func (s skillsSession) ask(spec AskSpec) (Turn, error) {
	plugin := argAfter(spec.Args, "--plugin-dir")
	if plugin == "" || !hasFlagValue(spec.Args, "--setting-sources", "") {
		return Turn{}, fmt.Errorf("the session was started with %q, want a plugin and an empty --setting-sources", spec.Args)
	}
	entries, err := os.ReadDir(filepath.Join(plugin, "skills"))
	if err != nil {
		return Turn{}, err
	}
	turn := Turn{Plugins: []string{"asmai"}, Reply: "done"}
	for _, e := range entries {
		turn.Skills = append(turn.Skills, "asmai:"+e.Name())
	}
	turn.Skills = append(turn.Skills, "code-review")
	if s.listsExtra {
		turn.Skills = append(turn.Skills, "asmai:prototype")
	}
	if s.noList {
		turn.Skills = nil
	}
	if s.asksNothing {
		return turn, nil
	}
	turn.SkillCalls = []Command{{Text: "asmai:implement"}}
	if s.bareFails {
		turn.SkillCalls = append(turn.SkillCalls, Command{Text: "tdd", Failed: true, Output: "Unknown skill: tdd"})
	}
	if s.namespacedFails {
		turn.SkillCalls = append(turn.SkillCalls, Command{Text: "asmai:tdd", Failed: true, Output: "Unknown skill: asmai:tdd"})
	} else {
		name := "asmai:tdd"
		if !s.bareFails {
			name = "tdd"
		}
		turn.SkillCalls = append(turn.SkillCalls, Command{Text: name})
	}
	return turn, nil
}

// hasFlagValue reports whether args has flag followed by value.
func hasFlagValue(args []string, flag, value string) bool {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1] == value
		}
	}
	return false
}

func skillsHarness(t *testing.T, s skillsSession) *Harness {
	t.Helper()
	checkGuardDependencies = func() error { return nil }
	t.Cleanup(func() { checkGuardDependencies = guardDependencies })
	return harness(t, fakeClaude{auth: AuthStatus{SignedIn: true, Method: "claude.ai"}, ask: s.ask}, startsDirectly)
}

func TestC34PassesWhenTheNarrowedListIsDeliveredAndASkillsBareNameCallResolves(t *testing.T) {
	r := result(t, skillsHarness(t, skillsSession{}), "C34")
	if r.Outcome != Passed {
		t.Fatalf("C34 %s: %s", r.Outcome, r.Failure)
	}
	evidence := strings.Join(r.Evidence, "\n")
	for _, want := range []string{"narrowed list as the plugin asmai: implement, tdd", "ran from the session's list as \"asmai:implement\"", "under that bare name"} {
		if !strings.Contains(evidence, want) {
			t.Errorf("C34's evidence\n%s\ndoes not say %q", evidence, want)
		}
	}
}

func TestC34PassesWhenTheBareNameFailsAndTheGuidesNamespacedNameResolves(t *testing.T) {
	r := result(t, skillsHarness(t, skillsSession{bareFails: true}), "C34")
	if r.Outcome != Passed {
		t.Fatalf("C34 %s: %s", r.Outcome, r.Failure)
	}
	if evidence := strings.Join(r.Evidence, "\n"); !strings.Contains(evidence, `"tdd" was not found`) || !strings.Contains(evidence, `"asmai:tdd" ran`) {
		t.Errorf("C34's evidence\n%s\ndoes not say both attempts", evidence)
	}
}

func TestC34FailsWhenTheCalledSkillResolvesUnderNoName(t *testing.T) {
	r := result(t, skillsHarness(t, skillsSession{bareFails: true, namespacedFails: true}), "C34")
	if r.Outcome != Failed || !strings.Contains(r.Failure, "did not resolve") {
		t.Errorf("C34 %s: %q, want it to fail because the call did not resolve", r.Outcome, r.Failure)
	}
}

func TestC34FailsWhenTheSessionHoldsASkillTheAssignmentNarrowedOut(t *testing.T) {
	r := result(t, skillsHarness(t, skillsSession{listsExtra: true}), "C34")
	if r.Outcome != Failed || !strings.Contains(r.Failure, "narrowed") {
		t.Errorf("C34 %s: %q, want it to fail because the list was not the narrowed one", r.Outcome, r.Failure)
	}
}

func TestC34IsInconclusiveWhenTheSessionRunsNoSkill(t *testing.T) {
	r := result(t, skillsHarness(t, skillsSession{asksNothing: true}), "C34")
	if r.Outcome != Failed || !strings.Contains(r.Failure, "inconclusive") {
		t.Errorf("C34 %s: %q, want it to fail as inconclusive", r.Outcome, r.Failure)
	}
}

func TestC34SaysSoWhenTheSessionsStartListsNoSkills(t *testing.T) {
	r := result(t, skillsHarness(t, skillsSession{noList: true}), "C34")
	if r.Outcome != Passed {
		t.Fatalf("C34 %s: %s", r.Outcome, r.Failure)
	}
	if evidence := strings.Join(r.Evidence, "\n"); !strings.Contains(evidence, "known limitation") {
		t.Errorf("C34's evidence\n%s\ndoes not say the list was not shown", evidence)
	}
}

func TestATurnReadsTheSkillsASessionListedAndRan(t *testing.T) {
	events := `{"type":"system","subtype":"init","skills":["asmai:tdd","code-review"],"plugins":[{"name":"asmai","path":"/p"}]}
{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Skill","input":{"skill":"tdd"}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"Unknown skill: tdd","is_error":true}]}}
{"type":"result","subtype":"success","result":"done"}
`
	turn, err := parseTurn(strings.NewReader(events))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(turn.Skills, ",") != "asmai:tdd,code-review" || strings.Join(turn.Plugins, ",") != "asmai" {
		t.Errorf("the turn lists skills %v and plugins %v", turn.Skills, turn.Plugins)
	}
	if len(turn.SkillCalls) != 1 || turn.SkillCalls[0].Text != "tdd" || !turn.SkillCalls[0].Failed || turn.SkillCalls[0].Output != "Unknown skill: tdd" {
		t.Errorf("the turn's skill calls are %+v", turn.SkillCalls)
	}
}
