// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"html"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/fakeprovider"
)

// Drive the installed CLI through two real PTYs against a disposable factory.
// The scripted provider is only the agent terminal, not the daemon or client.
func TestTakeLiveWaitsClearsDraftQueuesAndMoves(t *testing.T) {
	gitIdentity(t)
	dir := t.TempDir()
	t.Setenv("ASMAI_TEST_DIR", dir)
	engineering := []string{
		hookStep("SessionStart", "", ""),
		inputBox(""),
		`{"expect":"asmai inbox --dispatch 1\r"}`,
		hookStep("UserPromptSubmit", "first", "asmai inbox --dispatch 1"),
		fakeRun(`asmai inbox --dispatch 1`, 0, "First request"),
		inputBox("automated draft"),
		touchFile("working"),
		fakeRun(`while [ ! -f "$ASMAI_TEST_DIR/finish" ]; do sleep 0.02; done`, 0, ""),
		hookStep("Stop", "first", ""),
		`{"expect":"\u000c"}`,
		inputBox(""),
		`{"expect":"new terminal\r"}`,
		fakeRun(`asmai take leader@engineering > "$ASMAI_TEST_DIR/credential-refusal" 2>&1; code=$?; cat "$ASMAI_TEST_DIR/credential-refusal"; exit $code`, 1, "an agent cannot run this command"),
		touchFile("new-typed"),
		`{"expect":"only typed by a test that means to"}`,
	}
	t.Setenv(fakeprovider.ScriptEnv+"_ENGINEERING", writeScript(t, "engineering-take", engineering))
	coordination := []string{
		hookStep("SessionStart", "", ""),
		inputBox(""),
		`{"expect":"Please route this work\r"}`,
		hookStep("UserPromptSubmit", "user", "Please route this work"),
		fakeRun(`asmai job open --message latest --repository fixture --reading 'Route work' --criterion Answered`, 0, "Opened job 1"),
		fakeRun(`asmai handoff send --job 1 --to engineering --outcome 'First request' --decisions none --evidence none --constraints none --permissions none --criterion Answered`, 0, "dispatch 1"),
		hookStep("Stop", "user", ""),
		fakeRun(`while [ ! -f "$ASMAI_TEST_DIR/queue" ]; do sleep 0.02; done`, 0, ""),
		fakeRun(`asmai handoff send --job 1 --to engineering --outcome 'Second request' --decisions none --evidence none --constraints none --permissions none --criterion Answered`, 0, "dispatch 2"),
		touchFile("queued"),
		`{"expect":"only typed by a test that means to"}`,
	}
	readyFactory(t, coordination)
	registerHandoffFixture(t)
	if _, stderr, code := runAsmai(t, "start"); code != 0 {
		t.Fatal(stderr)
	}
	chat := inTerminal(t, 80, 25, "chat")
	waitFor(t, "Coordination prompt", chat.shows("❯ Try"))
	chat.ptmx.WriteString("Please route this work\r")
	waitFor(t, "Engineering's running turn", touched(dir, "working"))
	first := inTerminal(t, 80, 25, "take", "leader@engineering")
	time.Sleep(200 * time.Millisecond)
	if len(journaled(t, "input.taken")) != 0 {
		t.Fatal("take was granted during the running turn")
	}
	if err := os.WriteFile(filepath.Join(dir, "finish"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "take after the boundary", first.shows("leader@engineering · typing"))
	waitFor(t, "recorded draft", func() bool { return len(journaled(t, "draft.cleared")) == 1 })
	if got := string(journaled(t, "draft.cleared")[0].Data); !strings.Contains(got, "automated draft") {
		t.Fatalf("draft journal entry: %s", got)
	}
	t.Logf("persisted draft.cleared: %s", journaled(t, "draft.cleared")[0].Data)
	t.Logf("first terminal status: %s", strings.TrimSpace(first.lines()[24]))
	if err := os.WriteFile(filepath.Join(dir, "queue"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "second handoff", touched(dir, "queued"))
	time.Sleep(250 * time.Millisecond)
	if got := len(journaled(t, "message.fetched")); got != 1 {
		t.Fatalf("held input fetched %d messages, want only the first", got)
	}
	t.Logf("queued handoff: %s; fetched messages: %d", journaled(t, "handoff.sent")[1].Data, len(journaled(t, "message.fetched")))
	second := inTerminal(t, 80, 25, "take", "leader@engineering")
	waitFor(t, "new terminal typing", second.shows("leader@engineering · typing"))
	waitFor(t, "older terminal observing", first.shows("leader@engineering · observing"))
	t.Logf("moved terminal status: %s", strings.TrimSpace(first.lines()[24]))
	first.ptmx.WriteString("old terminal\r")
	second.ptmx.WriteString("new terminal\r")
	waitFor(t, "new terminal's input reached Engineering", touched(dir, "new-typed"))
	refusal, err := os.ReadFile(filepath.Join(dir, "credential-refusal"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("agent credential refusal: %s", strings.TrimSpace(string(refusal)))
	if len(entries(t, "session.ended")) != 0 {
		t.Fatal("the old terminal's keys reached the agent")
	}
	t.Logf("new terminal status: %s", strings.TrimSpace(second.lines()[24]))
	takeScreenEvidence(t, "take-wait-move.html", "Take waits, then moves", first.lines(), second.lines())
}

func TestTakeLiveInterruptsRunningTurn(t *testing.T) {
	gitIdentity(t)
	dir := t.TempDir()
	t.Setenv("ASMAI_TEST_DIR", dir)
	engineering := []string{
		hookStep("SessionStart", "", ""),
		inputBox(""),
		`{"expect":"asmai inbox --dispatch 1\r"}`,
		hookStep("UserPromptSubmit", "first", "asmai inbox --dispatch 1"),
		fakeRun(`asmai inbox --dispatch 1`, 0, "First request"),
		`{"screen":"Working (esc to interrupt)\r\n"}`,
		touchFile("working"),
		`{"expect":"\u001b"}`,
		touchFile("interrupted"),
		inputBox(""),
		`{"expect":"only typed by a test that means to"}`,
	}
	t.Setenv(fakeprovider.ScriptEnv+"_ENGINEERING", writeScript(t, "engineering-interrupt", engineering))
	coordination := []string{
		hookStep("SessionStart", "", ""),
		inputBox(""),
		`{"expect":"Please route this work\r"}`,
		hookStep("UserPromptSubmit", "user", "Please route this work"),
		fakeRun(`asmai job open --message latest --repository fixture --reading 'Route work' --criterion Answered`, 0, "Opened job 1"),
		fakeRun(`asmai handoff send --job 1 --to engineering --outcome 'First request' --decisions none --evidence none --constraints none --permissions none --criterion Answered`, 0, "dispatch 1"),
		hookStep("Stop", "user", ""),
		`{"expect":"only typed by a test that means to"}`,
	}
	readyFactory(t, coordination)
	registerHandoffFixture(t)
	if _, stderr, code := runAsmai(t, "start"); code != 0 {
		t.Fatal(stderr)
	}
	chat := inTerminal(t, 80, 25, "chat")
	waitFor(t, "Coordination prompt", chat.shows("❯ Try"))
	chat.ptmx.WriteString("Please route this work\r")
	waitFor(t, "Engineering's running turn", touched(dir, "working"))
	taken := inTerminal(t, 80, 25, "take", "leader@engineering", "--interrupt")
	waitFor(t, "interrupt delivered", touched(dir, "interrupted"))
	waitFor(t, "input owner", taken.shows("leader@engineering · typing"))
	if got := len(journaled(t, "input.taken")); got != 1 {
		t.Fatalf("interrupt journaled %d takes, want one", got)
	}
	if got := string(journaled(t, "input.taken")[0].Data); !strings.Contains(got, `"interrupted": true`) {
		t.Fatalf("take was not journaled as interrupted: %s", got)
	}
	t.Logf("interrupt status: %s", strings.TrimSpace(taken.lines()[24]))
	takeScreenEvidence(t, "take-interrupt.html", "Take interrupts a running turn", taken.lines())
}

// Export the actual terminal frames for a reviewer when a local test run asks
// for evidence. CI runs the same behavioral test without writing an artifact.
func takeScreenEvidence(t *testing.T, name, title string, screens ...[]string) {
	t.Helper()
	dir := os.Getenv("ASMAI_TEST_EVIDENCE")
	if dir == "" {
		return
	}
	page := "<!doctype html><meta charset=\"utf-8\"><title>" + html.EscapeString(title) + "</title>"
	page += "<style>body{background:#111827;color:#e5e7eb;font:16px system-ui;padding:2rem}pre{background:#030712;border:1px solid #374151;padding:1rem;width:max-content;font:13px/1.25 monospace}</style>"
	page += "<h1>" + html.EscapeString(title) + "</h1>"
	for i, screen := range screens {
		page += fmt.Sprintf("<h2>Terminal %d</h2><pre>%s</pre>", i+1, html.EscapeString(strings.Join(screen, "\n")))
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(page), 0o600); err != nil {
		t.Fatal(err)
	}
}
