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
	promptsForGit bool
	// retriesGit makes the session run git push again outside the sandbox.
	retriesGit    bool
	ghUnavailable bool
	// skipsRetry makes the session not retry what the sandbox stopped.
	skipsRetry         bool
	noPromptForOutside bool
	noPromptForCurl    bool
	commandInstead     map[int]string
	environmentCommand *Command
	// C49's: writeDenied denies the Write tool's write, heredocRuns lets the
	// heredoc write its file, heredocAsks denies it as a non-interactive run
	// denies a native prompt it cannot raise, notBlocked has the session not
	// report itself blocked, and requestsPermission has its hooks report a
	// native prompt.
	writeDenied, heredocRuns, heredocAsks, notBlocked, requestsPermission bool
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

// deniedOutput is what the session is told of its retry outside the
// sandbox: that dontAsk mode denied it, unless it failed otherwise.
func deniedOutput(otherwise bool) string {
	if otherwise {
		return "requires approval"
	}
	return "Permission to use Bash has been denied because Claude Code is running in don't ask mode."
}

var listed = regexp.MustCompile("(?m)^\\d+\\. `(.*)`$")

// unattended plays C49's turns: a write with the Write tool, and the same
// content through a heredoc, with the session's hooks logged beside the
// workspace.
func (g guardSession) unattended(spec AskSpec) (Turn, error) {
	log := []string{`{"hook_event_name":"SessionStart"}`}
	if g.requestsPermission {
		log = append(log, `{"hook_event_name":"PermissionRequest","tool_name":"Write"}`)
	}
	hooks, err := os.OpenFile(filepath.Join(filepath.Dir(spec.Dir), "marks", "hooks.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return Turn{}, err
	}
	defer hooks.Close()
	hooks.WriteString(strings.Join(log, "\n") + "\n")
	if strings.Contains(spec.Prompt, "With the Write tool") {
		file := Command{Text: filepath.Join(spec.Dir, "test", "index.test.js")}
		if g.writeDenied {
			file.Denied, file.Failed, file.Output = true, true, "Permission to use Write has been denied because Claude Code is running in don't ask mode."
		} else {
			os.MkdirAll(filepath.Dir(file.Text), 0o755)
			if err := os.WriteFile(file.Text, []byte(testFile), 0o644); err != nil {
				return Turn{}, err
			}
		}
		return Turn{Files: []Command{file}, Reply: "done"}, nil
	}
	c := Command{Text: "cat > test/heredoc.test.js <<'EOF'\n" + testFile + "EOF", Denied: true, Failed: true, Output: deniedOutput(g.heredocAsks)}
	reply := "blocked"
	if g.heredocRuns {
		os.MkdirAll(filepath.Join(spec.Dir, "test"), 0o755)
		if err := os.WriteFile(filepath.Join(spec.Dir, "test", "heredoc.test.js"), []byte(testFile), 0o644); err != nil {
			return Turn{}, err
		}
		c, reply = Command{Text: c.Text}, "done"
	}
	if g.notBlocked {
		reply = "done"
	}
	return Turn{Commands: []Command{c}, Reply: reply}, nil
}

func (g guardSession) guarded(spec AskSpec) (Turn, error) {
	if strings.Contains(spec.Prompt, "With the Write tool") || strings.Contains(spec.Prompt, "heredoc.test.js") {
		return g.unattended(spec)
	}
	var turn Turn
	// commandInstead counts the commands of both of C37's turns together.
	offset := 0
	if strings.Contains(spec.Prompt, "expected to be stopped by the sandbox") {
		offset = 6
	}
	for i, m := range listed.FindAllStringSubmatch(spec.Prompt, -1) {
		text := m[1]
		if instead, ok := g.commandInstead[offset+i]; ok {
			text = instead
		}
		c := Command{Text: text}
		// retry is the session's retry of a command the sandbox stopped,
		// outside the sandbox, as the prompt tells it.
		var retry *Command
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
			} else {
				c.Failed, c.Output = true, "Read-only file system"
				retry = &Command{Text: text, Unsandboxed: true, Failed: true, Output: deniedOutput(g.noPromptForOutside), Denied: !g.noPromptForOutside}
			}
		case strings.HasPrefix(text, "curl"):
			if g.guardOff {
				if err := os.WriteFile(filepath.Join(spec.Dir, "curl.out"), []byte("<html>"), 0o644); err != nil {
					return Turn{}, err
				}
			} else {
				c.Failed, c.Output = true, "network is unavailable"
				retry = &Command{Text: text, Unsandboxed: true, Failed: true, Output: deniedOutput(g.noPromptForCurl), Denied: !g.noPromptForCurl}
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
			if g.retriesGit && strings.HasPrefix(text, "git push") {
				retry = &Command{Text: text, Unsandboxed: true}
			}
		}
		turn.Commands = append(turn.Commands, c)
		if retry != nil && !(g.skipsRetry && retry.Unsandboxed && c.Failed) {
			turn.Commands = append(turn.Commands, *retry)
		}
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
	for _, want := range []string{"ran without a native prompt", "a write outside the workspace did not happen", "did not go through: the sandbox stopped it, and its retry outside the sandbox was denied in dontAsk mode, with no native prompt", "was not loaded"} {
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
	if r.Outcome != Failed || !strings.Contains(r.Failure, "outside the workspace was not denied in dontAsk mode") {
		t.Errorf("C37 %s: %q, want it to fail because the outside write was not denied", r.Outcome, r.Failure)
	}
}

func TestC37FailsWhenCurlFailsWithoutRaisingAPrompt(t *testing.T) {
	r := result(t, guardHarness(t, guardSession{noPromptForCurl: true}), "C37")
	if r.Outcome != Failed || !strings.Contains(r.Failure, "does not allow was not denied in dontAsk mode") {
		t.Errorf("C37 %s: %q, want it to fail because curl was not denied", r.Outcome, r.Failure)
	}
}

func TestC37FailsWhenGitRaisesANativePrompt(t *testing.T) {
	r := result(t, guardHarness(t, guardSession{promptsForGit: true}), "C37")
	if r.Outcome != Failed || !strings.Contains(r.Failure, "was denied, but the guard allows it") {
		t.Errorf("C37 %s: %q, want it to fail because git was stopped", r.Outcome, r.Failure)
	}
}

func TestC37FailsWhenAnAllowedCommandIsRetriedOutsideTheSandbox(t *testing.T) {
	r := result(t, guardHarness(t, guardSession{retriesGit: true}), "C37")
	if r.Outcome != Failed || !strings.Contains(r.Failure, "allows it to run once inside it") {
		t.Errorf("C37 %s: %q, want it to fail because git push was retried outside the sandbox", r.Outcome, r.Failure)
	}
}

func TestC37IsInconclusiveWhenTheSessionSkipsTheRetryOutsideTheSandbox(t *testing.T) {
	r := result(t, guardHarness(t, guardSession{skipsRetry: true}), "C37")
	if r.Outcome != Failed || !strings.Contains(r.Failure, "inconclusive: the session never retried") {
		t.Errorf("C37 %s: %q, want it to fail as inconclusive because the session did not retry", r.Outcome, r.Failure)
	}
}

func TestC37FailsWhenGhCannotRun(t *testing.T) {
	r := result(t, guardHarness(t, guardSession{ghUnavailable: true}), "C37")
	if r.Outcome != Failed || !strings.Contains(r.Failure, "gh pr list` was refused, failed or retried") {
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
{"type":"assistant","message":{"content":[{"type":"thinking","thinking":""},{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"echo hi > a"}},{"type":"tool_use","id":"t2","name":"Bash","input":{"command":"curl x","dangerouslyDisableSandbox":true}},{"type":"tool_use","id":"t3","name":"Read","input":{"file_path":"/x"}}]}}
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
	if c := turn.Commands[1]; c.Text != "curl x" || c.Output != "needs approval" || !c.Failed || !c.Denied || !c.Unsandboxed {
		t.Errorf("the second command is %+v, want it failed and denied outside the sandbox", c)
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

// The Write and Edit tools' calls are read as the files the session tried
// to write, with what came of each, beside its shell commands.
func TestATurnReadsTheFilesTheSessionWrote(t *testing.T) {
	stream := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"w1","name":"Write","input":{"file_path":"/ws/a.js","content":"x"}},{"type":"tool_use","id":"b1","name":"Bash","input":{"command":"ls"}},{"type":"tool_use","id":"e1","name":"Edit","input":{"file_path":"/elsewhere/b.js"}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"w1","content":"written","is_error":false},{"type":"tool_result","tool_use_id":"b1","content":"a.js"},{"type":"tool_result","tool_use_id":"e1","content":"Permission to use Edit has been denied because Claude Code is running in don't ask mode.","is_error":true}]}}
{"type":"result","subtype":"success","result":"blocked","permission_denials":[{"tool_name":"Edit","tool_use_id":"e1","tool_input":{}}]}
`
	turn, err := parseTurn(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if len(turn.Commands) != 1 || turn.Commands[0].Text != "ls" || turn.Commands[0].Output != "a.js" {
		t.Errorf("the commands are %+v, want ls alone", turn.Commands)
	}
	if len(turn.Files) != 2 {
		t.Fatalf("the files are %+v, want the write and the edit", turn.Files)
	}
	if f := turn.Files[0]; f.Text != "/ws/a.js" || f.Output != "written" || f.Failed || f.Denied || deniedWithoutPrompt(f) {
		t.Errorf("the write is %+v", f)
	}
	if f := turn.Files[1]; f.Text != "/elsewhere/b.js" || !f.Failed || !deniedWithoutPrompt(f) {
		t.Errorf("the edit is %+v, want it denied in dontAsk mode", f)
	}
}

func TestC49PassesWhenAWorkerWritesWithTheWriteToolAndItsHeredocIsDeniedWithoutAPrompt(t *testing.T) {
	r := result(t, guardHarness(t, guardSession{}), "C49")
	if r.Outcome != Passed {
		t.Fatalf("C49 %s: %s", r.Outcome, r.Failure)
	}
	evidence := strings.Join(r.Evidence, "\n")
	for _, want := range []string{"with the Write tool, with no native prompt", "told it was denied in dontAsk mode", "reported itself blocked", "no PermissionRequest"} {
		if !strings.Contains(evidence, want) {
			t.Errorf("C49's evidence\n%s\ndoes not say %q", evidence, want)
		}
	}
}

func TestC49FailsWhenAWorkerCannotPromptlesslyLeaveItsWorkspaceSafe(t *testing.T) {
	for name, c := range map[string]struct {
		session guardSession
		failure string
	}{
		"the Write tool denied":            {guardSession{writeDenied: true}, "was denied or failed"},
		"the heredoc allowed":              {guardSession{heredocRuns: true}, "the heredoc wrote its file"},
		"the heredoc at a native prompt":   {guardSession{heredocAsks: true}, "would wait at a native prompt nobody can answer"},
		"the session not reported blocked": {guardSession{notBlocked: true}, "not that it was blocked"},
		"a native prompt raised":           {guardSession{requestsPermission: true}, "raised a native permission prompt"},
	} {
		t.Run(name, func(t *testing.T) {
			r := result(t, guardHarness(t, c.session), "C49")
			if r.Outcome != Failed || !strings.Contains(r.Failure, c.failure) {
				t.Errorf("C49 %s: %q, want it to fail with %q", r.Outcome, r.Failure, c.failure)
			}
		})
	}
}
