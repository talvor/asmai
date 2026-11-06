// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/fakeprovider"
)

// A real take client crosses a provider permission boundary and the exported
// journal records the answer even when the user moves to another terminal.
func TestTakeLiveNativePromptAnswer(t *testing.T) {
	gitIdentity(t)
	dir := t.TempDir()
	t.Setenv("ASMAI_TEST_DIR", dir)
	request := `{"hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"cd /workspace\nrm -rf build"}}`
	longAnswer := pasteStart + strings.Repeat("p", 256*1024) + "\r" + pasteEnd + "y\r"
	engineering := []string{
		hookStep("SessionStart", "", ""),
		inputBox(""),
		`{"expect":"asmai inbox --dispatch 1\r"}`,
		hookStep("UserPromptSubmit", "first", "asmai inbox --dispatch 1"),
		fakeRun(`asmai inbox --dispatch 1`, 0, "First request"),
		`{"hook":"PermissionRequest","payload":` + request + `}`,
		`{"screen":"Bash permission requested\r\nAllow once / Deny?\r\n"}`,
		touchFile("prompting"),
		`{"expect":"n"}`,
		touchFile("first-key-received"),
		`{"expect":"\u001b\r\u001b[200~line one\rline two\u001b[201~y\r"}`,
		`{"hook":"PermissionRequest","payload":` + request + `}`,
		touchFile("first-prompt-ended"),
		`{"screen":"Another Bash permission requested\r\nAllow once / Deny?\r\n"}`,
		`{"expect":` + quoted(longAnswer) + `}`,
		hookStep("Stop", "first", ""),
		touchFile("past-second-prompt"),
		`{"expect":"only typed by a test that means to"}`,
	}
	t.Setenv(fakeprovider.ScriptEnv+"_ENGINEERING", writeScript(t, "engineering-native-prompt", engineering))
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
	waitFor(t, "Engineering's native prompt", touched(dir, "prompting"))
	first := inTerminal(t, 80, 25, "take", "leader@engineering")
	waitFor(t, "take granted at the prompt", first.shows("leader@engineering · typing"))
	if got := journaled(t, "prompt.answered"); len(got) != 0 {
		t.Fatalf("answer recorded before the user typed: %+v", got)
	}
	first.ptmx.WriteString("n")
	waitFor(t, "first terminal's key reached the agent", touched(dir, "first-key-received"))
	second := inTerminal(t, 80, 25, "take", "leader@engineering")
	waitFor(t, "take moved to the second terminal", second.shows("leader@engineering · typing"))
	waitFor(t, "first terminal became observing", first.shows("leader@engineering · observing"))
	takeScreenEvidence(t, "take-native-prompt.html", "Take at a native permission prompt", first.lines(), second.lines())
	second.ptmx.WriteString("\x1b\r\x1b[200~line one\rline two\x1b[201~y\r")
	waitFor(t, "first prompt ended", touched(dir, "first-prompt-ended"))
	waitFor(t, "first answer exported", func() bool { return len(journaled(t, "prompt.answered")) == 1 })
	var got struct {
		Agent    string `json:"agent"`
		Terminal string `json:"terminal"`
		Prompt   string `json:"prompt"`
		Dispatch int64  `json:"dispatch"`
		Keys     string `json:"keys"`
	}
	entry := journaled(t, "prompt.answered")[0]
	if err := json.Unmarshal(entry.Data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Agent != "leader@engineering" || got.Terminal != second.name || got.Prompt != request || got.Dispatch != 1 || got.Keys != "n\x1b\r\x1b[200~line one\rline two\x1b[201~y\r" {
		t.Fatalf("exported prompt answer: %+v", got)
	}
	t.Logf("exported prompt.answered: %s", entry.Data)
	for offset := 0; offset < len(longAnswer); offset += 256 {
		end := min(offset+256, len(longAnswer))
		if _, err := second.ptmx.WriteString(longAnswer[offset:end]); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "agent passed the long pasted answer", touched(dir, "past-second-prompt"))
	waitFor(t, "second answer exported", func() bool { return len(journaled(t, "prompt.answered")) == 2 })
	secondEntry := journaled(t, "prompt.answered")[1]
	var secondAnswer struct {
		Keys string `json:"keys"`
	}
	if err := json.Unmarshal(secondEntry.Data, &secondAnswer); err != nil {
		t.Fatal(err)
	}
	if secondAnswer.Keys != longAnswer {
		t.Fatalf("long pasted answer recorded %d keys, want %d", len(secondAnswer.Keys), len(longAnswer))
	}
	t.Logf("second exported prompt.answered retained %d pasted key bytes", len(secondAnswer.Keys))
	if evidence := os.Getenv("ASMAI_TEST_EVIDENCE"); evidence != "" {
		formatted, err := json.MarshalIndent(entry, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(evidence, "take-native-prompt-journal.json"), formatted, 0o600); err != nil {
			t.Fatal(err)
		}
		formatted, err = json.MarshalIndent(secondEntry, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(evidence, "take-native-prompt-long-journal.json"), formatted, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

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
