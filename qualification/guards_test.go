// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func argAfter(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

// guardSession plays Claude Code in the fixture's workspace: what the
// repository's configuration does when its settings load, and what the guard
// does when it is on, each as its field says.
type guardSession struct {
	t *testing.T
	// loadsRepositoryConfiguration makes a session that has AsmAI's command
	// line run the repository's hook, as one that loaded its settings would.
	loadsRepositoryConfiguration bool
	// ignoresInstructions makes the session miss the instruction files.
	ignoresInstructions bool
	// controlBlind makes the control run leave no mark.
	controlBlind bool
	// guardOff lets a write outside the workspace and an unlisted host through.
	guardOff bool
	// promptsForGit makes git raise a native prompt, which the guard should not.
	promptsForGit      bool
	ghUnavailable      bool
	noPromptForOutside bool
	noPromptForCurl    bool
	commandInstead     map[int]string
	environmentCommand *Command
}

var codeWord = regexp.MustCompile(`Code word: (\S+)`)

func (g guardSession) ask(spec AskSpec) (Turn, error) {
	marks := filepath.Join(filepath.Dir(spec.Dir), "marks")
	system := argAfter(spec.Args, "--append-system-prompt")
	switch {
	case strings.Contains(strings.Join(spec.Args, " "), "user,project"):
		if !g.controlBlind {
			touch(g.t, filepath.Join(marks, "hook-ran"))
		}
		return Turn{Reply: "ok", MCPServers: []string{fixtureMCPServer}}, nil
	case spec.Prompt == "" || strings.HasPrefix(spec.Prompt, "You are being qualified"):
		return g.guarded(spec)
	}
	probe := Command{Text: "printenv " + fixtureEnvVariable + " || echo unset", Output: "unset\n"}
	if g.environmentCommand != nil {
		probe = *g.environmentCommand
	}
	turn := Turn{Commands: []Command{probe}}
	if !g.ignoresInstructions {
		for _, m := range codeWord.FindAllStringSubmatch(system, -1) {
			turn.Said += m[1] + " "
		}
	}
	if g.loadsRepositoryConfiguration {
		touch(g.t, filepath.Join(marks, "hook-ran"))
	}
	return turn, nil
}

var listed = regexp.MustCompile("(?m)^\\d+\\. `(.*)`$")

func (g guardSession) guarded(spec AskSpec) (Turn, error) {
	var turn Turn
	for i, m := range listed.FindAllStringSubmatch(spec.Prompt, -1) {
		text := m[1]
		if instead, ok := g.commandInstead[i]; ok {
			text = instead
		}
		c := Command{Text: text}
		switch {
		case strings.HasPrefix(text, "git ls-remote"):
			c.Output = "0123456789abcdef0123456789abcdef01234567\trefs/heads/master\n"
		case strings.HasPrefix(text, "gh pr list"):
			if g.ghUnavailable {
				c.Failed, c.Output = true, "gh: command not found"
			} else {
				c.Output = "[{\"number\":1}]\n"
			}
		case strings.HasPrefix(text, "echo outside"):
			if g.guardOff {
				cmd := exec.Command("sh", "-c", text)
				cmd.Dir = spec.Dir
				cmd.Run()
			} else if g.noPromptForOutside {
				c.Failed, c.Output = true, "Operation not permitted"
			} else {
				c.Denied, c.Failed, c.Output = true, true, "requires approval"
			}
		case strings.HasPrefix(text, "curl"):
			if g.guardOff {
				if err := os.WriteFile(filepath.Join(spec.Dir, "curl.out"), []byte("<html>"), 0o644); err != nil {
					return Turn{}, err
				}
			} else {
				c.Failed, c.Output = true, "network is unavailable"
				if !g.noPromptForCurl {
					c.Denied = true
				}
			}
		default:
			if g.promptsForGit && strings.HasPrefix(text, "git push") {
				c.Denied, c.Failed = true, true
				break
			}
			cmd := exec.Command("sh", "-c", text)
			cmd.Dir = spec.Dir
			out, err := cmd.CombinedOutput()
			c.Output, c.Failed = string(out), err != nil
		}
		turn.Commands = append(turn.Commands, c)
	}
	turn.Reply = "done"
	return turn, nil
}

func guardHarness(t *testing.T, s guardSession) *Harness {
	t.Helper()
	s.t = t
	checkGuardDependencies = func() error { return nil }
	t.Cleanup(func() { checkGuardDependencies = guardDependencies })
	return harness(t, fakeClaude{auth: AuthStatus{SignedIn: true, Method: "claude.ai"}, ask: s.ask}, startsDirectly)
}

func TestC36PassesWhenTheInstructionFilesLoadAndNoneOfTheRepositorysConfigurationDoes(t *testing.T) {
	r := result(t, guardHarness(t, guardSession{}), "C36")
	if r.Outcome != Passed {
		t.Fatalf("C36 %s: %s", r.Outcome, r.Failure)
	}
	evidence := strings.Join(r.Evidence, "\n")
	for _, want := range []string{"control: with the project setting source loaded", "all three instruction files", "loaded none of the repository's .claude configuration"} {
		if !strings.Contains(evidence, want) {
			t.Errorf("C36's evidence\n%s\ndoes not say %q", evidence, want)
		}
	}
}

func TestC36FailsWhenTheRepositorysHookRuns(t *testing.T) {
	r := result(t, guardHarness(t, guardSession{loadsRepositoryConfiguration: true}), "C36")
	if r.Outcome != Failed || !strings.Contains(r.Failure, "hook ran in the session") {
		t.Errorf("C36 %s: %q, want it to fail because the repository's hook ran", r.Outcome, r.Failure)
	}
}

func TestC36FailsWhenTheSessionIsNotToldTheInstructionFiles(t *testing.T) {
	r := result(t, guardHarness(t, guardSession{ignoresInstructions: true}), "C36")
	if r.Outcome != Failed || !strings.Contains(r.Failure, "did not follow") {
		t.Errorf("C36 %s: %q, want it to fail because the instruction files were missed", r.Outcome, r.Failure)
	}
}

func TestC36FailsWhenItCannotSeeTheRepositorysConfigurationLoad(t *testing.T) {
	r := result(t, guardHarness(t, guardSession{controlBlind: true}), "C36")
	if r.Outcome != Failed || !strings.Contains(r.Failure, "could not see the repository's configuration load") {
		t.Errorf("C36 %s: %q, want it to fail because the control showed nothing", r.Outcome, r.Failure)
	}
}

func TestC36RequiresTheExactSuccessfulEnvironmentProbe(t *testing.T) {
	command := "printenv " + fixtureEnvVariable + " || echo unset"
	for name, probe := range map[string]Command{
		"wrong variable": {Text: "printenv OTHER || echo unset", Output: "unset\n"},
		"failed":         {Text: command, Output: "unset\n", Failed: true},
		"denied":         {Text: command, Output: "unset\n", Denied: true},
		"quoted output":  {Text: command, Output: "echo unset\n"},
	} {
		t.Run(name, func(t *testing.T) {
			r := result(t, guardHarness(t, guardSession{environmentCommand: &probe}), "C36")
			if r.Outcome != Failed {
				t.Errorf("C36 %s: %q, want the invalid probe rejected", r.Outcome, r.Failure)
			}
		})
	}
}

func TestC37PassesWhenTheGuardKeepsWritesInTheWorkspaceAndLetsGitAndGhThrough(t *testing.T) {
	r := result(t, guardHarness(t, guardSession{}), "C37")
	if r.Outcome != Passed {
		t.Fatalf("C37 %s: %s", r.Outcome, r.Failure)
	}
	evidence := strings.Join(r.Evidence, "\n")
	for _, want := range []string{"ran without a native prompt", "a write outside the workspace did not happen", "did not go through: it raised a native permission prompt", "was not loaded"} {
		if !strings.Contains(evidence, want) {
			t.Errorf("C37's evidence\n%s\ndoes not say %q", evidence, want)
		}
	}
}

func TestC37FailsWhenAWriteOutsideTheWorkspaceGoesThrough(t *testing.T) {
	r := result(t, guardHarness(t, guardSession{guardOff: true}), "C37")
	if r.Outcome != Failed || !strings.Contains(r.Failure, "outside the workspace went through") {
		t.Errorf("C37 %s: %q, want it to fail because the write got out", r.Outcome, r.Failure)
	}
}

func TestC37FailsWhenAnOutsideWriteFailsWithoutRaisingAPrompt(t *testing.T) {
	r := result(t, guardHarness(t, guardSession{noPromptForOutside: true}), "C37")
	if r.Outcome != Failed || !strings.Contains(r.Failure, "did not raise a native permission prompt") {
		t.Errorf("C37 %s: %q, want it to fail because the outside write did not raise a prompt", r.Outcome, r.Failure)
	}
}

func TestC37FailsWhenCurlFailsWithoutRaisingAPrompt(t *testing.T) {
	r := result(t, guardHarness(t, guardSession{noPromptForCurl: true}), "C37")
	if r.Outcome != Failed || !strings.Contains(r.Failure, "did not raise a native permission prompt") {
		t.Errorf("C37 %s: %q, want it to fail because curl did not raise a prompt", r.Outcome, r.Failure)
	}
}

func TestC37FailsWhenGitRaisesANativePrompt(t *testing.T) {
	r := result(t, guardHarness(t, guardSession{promptsForGit: true}), "C37")
	if r.Outcome != Failed || !strings.Contains(r.Failure, "raised a native permission prompt, but the guard allows it") {
		t.Errorf("C37 %s: %q, want it to fail because git was stopped", r.Outcome, r.Failure)
	}
}

func TestC37FailsWhenGhCannotRun(t *testing.T) {
	r := result(t, guardHarness(t, guardSession{ghUnavailable: true}), "C37")
	if r.Outcome != Failed || !strings.Contains(r.Failure, "gh pr list` was refused or failed") {
		t.Errorf("C37 %s: %q, want it to fail because gh could not run", r.Outcome, r.Failure)
	}
}

func TestC37RequiresTheRequestedCommands(t *testing.T) {
	for name, change := range map[string]struct {
		index   int
		instead string
	}{
		"allowed write": {0, "echo inside > inside.txt # changed"},
		"remote host":   {4, "git ls-remote --heads origin main"},
		"gh repository": {5, "gh pr list --repo other/repo --state all --limit 1 --json number"},
		"outside write": {6, "echo outside > somewhere-else.txt"},
		"curl host":     {7, "curl -sS --max-time 20 -o curl.out https://other.example"},
	} {
		t.Run(name, func(t *testing.T) {
			s := guardSession{commandInstead: map[int]string{change.index: change.instead}}
			r := result(t, guardHarness(t, s), "C37")
			if r.Outcome != Failed || !strings.Contains(r.Failure, "never tried") {
				t.Errorf("C37 %s: %q, want the substituted command rejected", r.Outcome, r.Failure)
			}
		})
	}
}

func TestC37FailsClearlyWhenTheHostLacksWhatTheGuardNeeds(t *testing.T) {
	h := guardHarness(t, guardSession{})
	checkGuardDependencies = func() error { return errors.New("install bubblewrap and socat") }
	r := result(t, h, "C37")
	if r.Outcome != Failed || !strings.Contains(r.Failure, "install bubblewrap and socat") {
		t.Errorf("C37 %s: %q", r.Outcome, r.Failure)
	}
}

func TestATurnReadsTheEventsClaudeCodePrints(t *testing.T) {
	stream := `{"type":"system","subtype":"hook_started"}
{"type":"system","subtype":"permission_denied","tool_name":"Bash","message":"needs approval"}
{"type":"system","subtype":"init","mcp_servers":[{"name":"one","status":"connected"}],"permissionMode":"default"}
{"type":"assistant","message":{"content":[{"type":"thinking","thinking":""},{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"echo hi > a"}},{"type":"tool_use","id":"t2","name":"Bash","input":{"command":"curl x"}},{"type":"tool_use","id":"t3","name":"Read","input":{"file_path":"/x"}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"fine","is_error":false},{"type":"tool_result","tool_use_id":"t2","content":[{"type":"text","text":"needs approval"}],"is_error":true}]}}
{"type":"result","subtype":"success","result":"done","permission_denials":[{"tool_name":"Bash","tool_use_id":"t2","tool_input":{}}]}
`
	turn, err := parseTurn(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if len(turn.MCPServers) != 1 || turn.MCPServers[0] != "one" || turn.Reply != "done" || len(turn.Commands) != 2 {
		t.Fatalf("the turn is %+v", turn)
	}
	if c := turn.Commands[0]; c.Text != "echo hi > a" || c.Output != "fine" || c.Failed || c.Denied {
		t.Errorf("the first command is %+v", c)
	}
	if c := turn.Commands[1]; c.Text != "curl x" || c.Output != "needs approval" || !c.Failed || !c.Denied {
		t.Errorf("the second command is %+v, want it failed and denied", c)
	}
	if got := turn.Tried("curl x"); len(got) != 1 {
		t.Errorf("Tried(curl x) = %+v", got)
	}
	if got := turn.Tried("curl"); len(got) != 0 {
		t.Errorf("Tried(curl) = %+v, want no exact match", got)
	}
	if _, err := parseTurn(strings.NewReader("not json\n")); err == nil {
		t.Error("a line that is not JSON was accepted")
	}
}
