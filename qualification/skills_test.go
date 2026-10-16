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
// skills it lists, and what each fresh session does with the skill implement
// names.
type skillsSession struct {
	// listsExtra makes the session list a skill the assignment narrowed out,
	// noPlugin has it not report the plugin, noList has it list no skills and
	// bareList lists them without the plugin's namespace.
	listsExtra, noPlugin, noList, bareList bool
	// attempts is what each fresh session does about tdd, in order: "bare"
	// runs it by its bare name, "namespaced" as asmai:tdd, "quiet" makes no
	// call, "missing" makes a call that is not found and "unresolved" one
	// that returns no result. The last repeats.
	attempts []string
	asked    int
}

func (s *skillsSession) ask(spec AskSpec) (Turn, error) {
	if strings.Contains(spec.Prompt, "tdd") || !strings.HasPrefix(spec.Prompt, "/asmai:implement ") {
		return Turn{}, fmt.Errorf("the prompt is %q, want the slash invocation of implement and no name of the skill it calls", spec.Prompt)
	}
	plugin := argAfter(spec.Args, "--plugin-dir")
	if plugin == "" || !hasFlagValue(spec.Args, "--setting-sources", "") {
		return Turn{}, fmt.Errorf("the session was started with %q, want a plugin and an empty --setting-sources", spec.Args)
	}
	entries, err := os.ReadDir(filepath.Join(plugin, "skills"))
	if err != nil {
		return Turn{}, err
	}
	turn := Turn{Plugins: []string{"asmai"}, Reply: "done"}
	if s.noPlugin {
		turn.Plugins = nil
	}
	for _, e := range entries {
		name := "asmai:" + e.Name()
		if s.bareList {
			name = e.Name()
		}
		turn.Skills = append(turn.Skills, name)
	}
	turn.Skills = append(turn.Skills, "code-review")
	if s.listsExtra {
		turn.Skills = append(turn.Skills, "asmai:prototype")
	}
	if s.noList {
		turn.Skills = nil
	}
	behaviours := s.attempts
	if len(behaviours) == 0 {
		behaviours = []string{"bare"}
	}
	behaviour := behaviours[min(s.asked, len(behaviours)-1)]
	s.asked++
	switch behaviour {
	case "bare":
		turn.SkillCalls = []Command{{Text: "tdd", CallEvent: 1, ResultEvent: 2}}
	case "namespaced":
		turn.SkillCalls = []Command{{Text: "asmai:tdd", CallEvent: 1, ResultEvent: 2}}
	case "missing":
		turn.SkillCalls = []Command{{Text: "tdd", CallEvent: 1, ResultEvent: 2, Failed: true, Output: "Unknown skill: tdd"}}
	case "unresolved":
		turn.SkillCalls = []Command{{Text: "asmai:tdd", CallEvent: 1}}
	case "failed then successful":
		turn.SkillCalls = []Command{
			{Text: "tdd", CallEvent: 1, ResultEvent: 2, Failed: true, Output: "Unknown skill: tdd"},
			{Text: "asmai:tdd", CallEvent: 3, ResultEvent: 4},
		}
	case "successful then unresolved":
		turn.SkillCalls = []Command{
			{Text: "tdd", CallEvent: 1, ResultEvent: 2},
			{Text: "asmai:tdd", CallEvent: 3},
		}
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

func skillsHarness(t *testing.T, s *skillsSession) *Harness {
	t.Helper()
	checkGuardDependencies = func() error { return nil }
	t.Cleanup(func() { checkGuardDependencies = guardDependencies })
	return harness(t, fakeClaude{auth: AuthStatus{SignedIn: true, Method: "claude.ai"}, ask: s.ask}, startsDirectly)
}

func TestC34PassesWhenTheCalledSkillRunsUnderItsBareOrItsNamespacedName(t *testing.T) {
	for _, behaviour := range []string{"bare", "namespaced"} {
		r := result(t, skillsHarness(t, &skillsSession{attempts: []string{behaviour}}), "C34")
		if r.Outcome != Passed {
			t.Fatalf("C34 with a %s call %s: %s", behaviour, r.Outcome, r.Failure)
		}
		evidence := strings.Join(r.Evidence, "\n")
		for _, want := range []string{"narrowed list as the plugin asmai: implement, tdd", "started by /asmai:implement"} {
			if !strings.Contains(evidence, want) {
				t.Errorf("C34's evidence\n%s\ndoes not say %q", evidence, want)
			}
		}
	}
}

func TestC34AsksAgainInAFreshSessionWhenTheModelMakesNoCall(t *testing.T) {
	s := &skillsSession{attempts: []string{"quiet", "quiet", "namespaced"}}
	r := result(t, skillsHarness(t, s), "C34")
	if r.Outcome != Passed || s.asked != 3 {
		t.Errorf("C34 %s after %d sessions: %q, want it to pass on the third", r.Outcome, s.asked, r.Failure)
	}
}

func TestC34IsInconclusiveWhenNoSessionMakesTheCall(t *testing.T) {
	s := &skillsSession{attempts: []string{"quiet"}}
	r := result(t, skillsHarness(t, s), "C34")
	if r.Outcome != Failed || !strings.Contains(r.Failure, "inconclusive") || s.asked != 3 {
		t.Errorf("C34 %s after %d sessions: %q, want it to fail as inconclusive after three", r.Outcome, s.asked, r.Failure)
	}
}

func TestC34FailsAtOnceWhenTheCallDoesNotResolve(t *testing.T) {
	for _, behaviour := range []string{"missing", "unresolved", "failed then successful", "successful then unresolved"} {
		s := &skillsSession{attempts: []string{behaviour, "bare"}}
		r := result(t, skillsHarness(t, s), "C34")
		if r.Outcome != Failed || !strings.Contains(r.Failure, "did not resolve") || s.asked != 1 {
			t.Errorf("C34 with a %s call %s after %d sessions: %q, want it to fail at once", behaviour, r.Outcome, s.asked, r.Failure)
		}
	}
}

func TestC34FailsWhenTheSessionHoldsASkillTheAssignmentNarrowedOut(t *testing.T) {
	r := result(t, skillsHarness(t, &skillsSession{listsExtra: true}), "C34")
	if r.Outcome != Failed || !strings.Contains(r.Failure, "narrowed") {
		t.Errorf("C34 %s: %q, want it to fail because the list was not the narrowed one", r.Outcome, r.Failure)
	}
}

func TestC34RequiresPluginAndNamespacedSkillInventory(t *testing.T) {
	for name, session := range map[string]skillsSession{
		"no plugin":   {noPlugin: true},
		"no list":     {noList: true},
		"bare skills": {bareList: true},
	} {
		t.Run(name, func(t *testing.T) {
			r := result(t, skillsHarness(t, &session), "C34")
			if r.Outcome != Failed || !(strings.Contains(r.Failure, "plugin") || strings.Contains(r.Failure, "listed")) {
				t.Errorf("C34 %s: %q, want missing plugin skill evidence to fail", r.Outcome, r.Failure)
			}
		})
	}
}

func TestATurnReadsTheSkillsASessionListedAndRan(t *testing.T) {
	events := `{"type":"system","subtype":"init","skills":["asmai:tdd","code-review"],"plugins":[{"name":"asmai","path":"/p"}],"slash_commands":["asmai:tdd","code-review","init"]}
{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Skill","input":{"skill":"tdd"}},{"type":"tool_use","id":"t2","name":"Skill","input":{"skill":"asmai:implement"}}]}}
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
	if strings.Join(turn.SlashCommands, ",") != "asmai:tdd,code-review,init" {
		t.Errorf("the turn lists slash commands %v", turn.SlashCommands)
	}
	if len(turn.SkillCalls) != 2 || turn.SkillCalls[0].Text != "tdd" || turn.SkillCalls[0].CallEvent != 2 || turn.SkillCalls[0].ResultEvent != 3 || !turn.SkillCalls[0].Failed || turn.SkillCalls[0].Output != "Unknown skill: tdd" || turn.SkillCalls[1].ResultEvent != 0 {
		t.Errorf("the turn's skill calls are %+v", turn.SkillCalls)
	}
}
