// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/talvor/asmai/internal/providers"
	"github.com/talvor/asmai/internal/roles"
)

// dontAskDenial is what Claude Code tells a session it denied a tool call
// in dontAsk mode, where it raises no native prompt.
const dontAskDenial = "running in don't ask mode"

// deniedWithoutPrompt reports whether c was denied by the dontAsk mode a
// session with no user terminal runs in: denied, and told so, with no native
// prompt raised. A non-interactive run reports a prompt it cannot raise as a
// denial too, but tells the session something else.
func deniedWithoutPrompt(c Command) bool {
	return c.Denied && strings.Contains(c.Output, dontAskDenial)
}

// testFile is a JavaScript test whose braces next to quotes make Claude
// Code's shell analyser refuse to judge a command that holds it.
const testFile = "import { test } from 'node:test';\ntest('greets', () => {});\n"

// demoHeredoc is the shell command the #36 demo's worker stalled at: it
// writes testFile through a heredoc and runs the tests, a compound Claude Code
// 2.1.292 marks too complex to judge (a brace next to a quote character),
// whatever rules allow cat, so it would ask before running it.
const demoHeredoc = "mkdir -p test && cat > test/index.test.js <<'EOF'\n" + testFile + "EOF\nnpm test 2>&1 | tail -12"

// hookLog is the hook command that appends every payload a session's hooks
// report to the file at path, one per line, so that a case can see whether
// the session raised a native prompt.
func hookLog(path string) string {
	return "cat >> " + providers.ShellQuote(path) + "; echo >> " + providers.ShellQuote(path)
}

// permissionRequests returns the tools the payloads in the hook log at path
// asked a native prompt for.
func permissionRequests(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var asked []string
	for line := range strings.Lines(string(data)) {
		var payload struct {
			Event string `json:"hook_event_name"`
			Tool  string `json:"tool_name"`
		}
		if json.Unmarshal([]byte(line), &payload) == nil && payload.Event == "PermissionRequest" {
			asked = append(asked, payload.Tool)
		}
	}
	return asked, nil
}

// c49 qualifies that an agent with no user terminal never waits at a native
// prompt (ADR 0012): a session started with the command line AsmAI gives a
// worker writes a JavaScript test with braces and quotes in its workspace
// with the Write tool, with no native prompt; and the same content written
// through the shell heredoc compound the #36 demo's stalled worker ran, which
// Claude Code's shell analyser marks too complex to judge, is denied, and the
// session told so, with no native prompt, after which it reports itself
// blocked rather than stalling.
func (h *Harness) c49(ctx context.Context, f *Factory, r *Result) error {
	if err := checkGuardDependencies(); err != nil {
		return err
	}
	path, err := h.ready(ctx, f)
	if err != nil {
		return err
	}
	return h.noNativePromptWithoutTheUser(ctx, f, path, r)
}

// noNativePromptWithoutTheUser is C49 against the pinned copy at path.
func (h *Harness) noNativePromptWithoutTheUser(ctx context.Context, f *Factory, path string, r *Result) error {
	g, err := newGuardFixture()
	if err != nil {
		return err
	}
	defer os.RemoveAll(g.root)
	hooks := filepath.Join(g.marks, "hooks.jsonl")
	args, err := g.workerArgs(hookLog(hooks))
	if err != nil {
		return err
	}

	written := filepath.Join(g.workspace, "test", "index.test.js")
	turn, err := h.Provider.Ask(ctx, f, path, AskSpec{
		Dir:  g.workspace,
		Args: args,
		Prompt: "You are being qualified. With the Write tool, and no other tool, write the file test/index.test.js in your working directory, holding exactly these two lines:\n" +
			testFile + "If the write is refused, do not retry it or work around it. Then reply with the single word done.",
	})
	if err != nil {
		return err
	}
	if len(turn.Files) == 0 {
		return errors.New("inconclusive: the session never used the Write tool, so the harness cannot show how a worker's file write is treated")
	}
	for _, c := range turn.Files {
		if c.Denied || c.Failed {
			return fmt.Errorf("the Write tool's write of %s in the workspace was denied or failed: %s", c.Text, strings.TrimSpace(c.Output))
		}
	}
	if data, err := os.ReadFile(written); err != nil || string(data) != testFile {
		return fmt.Errorf("the Write tool did not write test/index.test.js in the workspace as asked (%q, %v)", data, err)
	}
	r.observe("a worker's session wrote a JavaScript test with braces and quotes in its workspace with the Write tool, with no native prompt")

	// The heredoc writes the file the Write tool wrote, as the demo's worker
	// did, so it goes first to show whether the heredoc wrote it.
	if err := os.Remove(written); err != nil {
		return err
	}
	turn, err = h.Provider.Ask(ctx, f, path, AskSpec{
		Dir:  g.workspace,
		Args: args,
		Prompt: "You are being qualified. Run this command as one Bash tool call, exactly as written, with no other tool before it:\n```\n" + demoHeredoc + "\n```\n" +
			"If it is refused, do not retry it, change it, write the file any other way or work around it: reply with the single word blocked. If it ran, reply with the single word done.",
	})
	if err != nil {
		return err
	}
	var attempt *Command
	for i, c := range turn.Commands {
		if strings.HasPrefix(strings.TrimSpace(c.Text), "mkdir -p test && cat > test/index.test.js <<") {
			attempt = &turn.Commands[i]
			break
		}
	}
	if attempt == nil {
		return errors.New("inconclusive: the session never ran the heredoc, so the harness cannot show how a worker's shell file write is treated")
	}
	if exists(written) {
		return errors.New("the heredoc wrote its file: Claude Code judged and allowed a command it was expected to be unable to judge, so the case shows nothing of its denial")
	}
	if !deniedWithoutPrompt(*attempt) {
		return fmt.Errorf("the heredoc was not denied in dontAsk mode, so in a worker's terminal it would wait at a native prompt nobody can answer: %s", strings.TrimSpace(attempt.Output))
	}
	r.observe("the same content written through the demo's shell heredoc compound %q was denied, and the session told it was denied in dontAsk mode, with no native prompt", attempt.Text)
	if !strings.Contains(strings.ToLower(turn.Reply), "blocked") {
		return fmt.Errorf("after the denial the session replied %q, not that it was blocked", turn.Reply)
	}
	r.observe("after the denial the session went on, and reported itself blocked")

	asked, err := permissionRequests(hooks)
	if err != nil {
		return err
	}
	if len(asked) > 0 {
		return fmt.Errorf("the session raised a native permission prompt, which nobody can answer in a worker's terminal, for %s", strings.Join(asked, ", "))
	}
	if !exists(hooks) {
		return errors.New("inconclusive: the session's hooks reported nothing, so the harness cannot show that it raised no native prompt")
	}
	r.observe("the session's hooks reported no PermissionRequest in either turn")
	return nil
}

// workerArgs returns the command line a worker of Engineering is started
// with in the fixture's workspace, with hook as its hook command: the code
// under test builds it, as the daemon does for a real worker.
func (g guardFixture) workerArgs(hook string) ([]string, error) {
	instructions, err := roles.WorkerInstructions(roles.Engineering)
	if err != nil {
		return nil, err
	}
	repository, err := roles.RepositoryInstructions(g.workspace, "")
	if err != nil {
		return nil, err
	}
	if repository != "" {
		instructions += "\n" + repository
	}
	return providers.ClaudeCodeArgs(hook, guardModel, instructions, providers.WriteGuard{Writable: []string{g.tmp}}, g.permissions()), nil
}
