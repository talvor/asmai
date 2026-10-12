// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/talvor/asmai/internal/providers"
)

// recordTrust is the fake's stand-in for Claude Code recording the user's
// trust of the folder it runs in, by its resolved path, in ~/.claude.json:
// it keeps every folder trusted so far.
var recordTrust = func() string {
	command := `pwd -P >> "$ASMAI_TEST_DIR/trusted" && { printf '{"projects": {'; sep=; while read -r d; do printf '%s"%s": {"hasTrustDialogAccepted": true}' "$sep" "$d"; sep=', '; done < "$ASMAI_TEST_DIR/trusted"; printf '}}'; } > "$HOME/.claude.json"`
	step, _ := json.Marshal(map[string]any{"run": command, "status": 0})
	return string(step)
}()

// setupTrusting is the Claude Code `asmai setup` opens in a folder: it asks
// whether to trust it, with "No, exit" selected first, records the trust
// once the user moves to Yes and confirms, comes up, and is left with /exit.
var setupTrusting = []string{
	`{"screen": "\u001b[2J\u001b[HAccessing workspace:\r\n\r\n \u001b[1m❯ No, exit\u001b[22m\r\n   Yes, I trust this folder\r\n"}`,
	`{"expect": "\u001b[B"}`,
	`{"screen": "\u001b[2J\u001b[HAccessing workspace:\r\n\r\n   No, exit\r\n \u001b[1m❯ Yes, I trust this folder\u001b[22m\r\n"}`,
	`{"expect": "\r"}`,
	recordTrust,
	`{"screen": "\u001b[2J\u001b[HClaude Code is ready\r\n❯ "}`,
	`{"expect": "/exit\r"}`,
}

// perFolder has the fake play scripts, by the name of the folder it is
// started in, in place of the leader's script.
func perFolder(t *testing.T, h *Harness, scripts map[string][]string) {
	t.Helper()
	dir := envLookup(h.Env, "ASMAI_TEST_DIR")
	for folder, script := range scripts {
		if err := os.WriteFile(filepath.Join(dir, "script-"+folder+".jsonl"), []byte(strings.Join(script, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// With the agents and workspaces folders trusted through `asmai setup`,
// whose questions the harness answers in the disposable state directory, a
// new leader directory and a new workspace start with no trust question.
func TestC48PassesWhenTheFoldersTrustedAtSetupCoverNewDirectories(t *testing.T) {
	h := harness(t, signedIn, startsDirectly)
	perFolder(t, h, map[string][]string{"agents": setupTrusting, "workspaces": setupTrusting})

	r := result(t, h, "C48")

	if r.Outcome != Passed {
		t.Fatalf("C48 %s: %s", r.Outcome, r.Failure)
	}
	evidence := strings.Join(r.Evidence, "\n")
	for _, want := range []string{"asmai setup", "leader@coordination (generation 1) started", "a new leader directory", "a new assignment workspace", "no trust question"} {
		if !strings.Contains(evidence, want) {
			t.Errorf("C48's evidence\n%s\ndoes not say %q", evidence, want)
		}
	}
	record := filepath.Join(envLookup(h.Env, "HOME"), ".claude.json")
	for _, dir := range []string{h.QualificationPaths.Agents, h.QualificationPaths.Workspaces, h.Paths.Agents} {
		if ok, err := providers.Trusted(record, dir); err != nil || !ok {
			t.Errorf("after C48, Claude Code's record trusts %s: %t, %v", dir, ok, err)
		}
	}
}

// A Claude Code that draws in place below what setup printed, with no clear
// of the screen, leaves setup's explanation, which names the answers too, in
// view above it: the harness answers only Claude Code's question, and leaves
// once it is answered.
func TestTheHarnessAnswersClaudeCodeNotSetupsExplanation(t *testing.T) {
	h := harness(t, signedIn, startsDirectly)
	var inline []string
	for i, step := range setupTrusting {
		// Each screen after the first moves up over the four lines of the one
		// before, and redraws from there.
		from := `\u001b[4A\r\u001b[J`
		if i == 0 {
			from = ""
		}
		inline = append(inline, strings.Replace(step, `\u001b[2J\u001b[H`, from, 1))
	}
	perFolder(t, h, map[string][]string{"agents": inline, "workspaces": inline})

	r := result(t, h, "C48")

	if r.Outcome != Passed {
		t.Fatalf("C48 %s: %s", r.Outcome, r.Failure)
	}
}

func TestC48FailsWhenANewDirectoryBelowATrustedFolderAsksToBeTrusted(t *testing.T) {
	h := harness(t, signedIn, startsDirectly)
	perFolder(t, h, map[string][]string{"agents": setupTrusting, "workspaces": setupTrusting, "repo": startsAfterTrust})

	r := result(t, h, "C48")

	if r.Outcome != Failed || !strings.Contains(r.Failure, "Claude Code asked whether to trust") || !strings.Contains(r.Failure, filepath.Join("assignment-1", "repo")) || !strings.Contains(r.Failure, "does not cover") {
		t.Errorf("C48 %s: %q, want a failure at the new workspace's trust question", r.Outcome, r.Failure)
	}
}

// When the user's answer is not recorded, setup exits 1 naming the folder,
// and the case fails with setup's own error.
func TestTheHarnessReportsASetupThatLeavesAFolderUntrusted(t *testing.T) {
	h := harness(t, signedIn, startsDirectly)
	notRecorded := append(append([]string(nil), setupTrusting[:4]...), setupTrusting[5:]...)
	perFolder(t, h, map[string][]string{"agents": setupTrusting, "workspaces": notRecorded})

	r := result(t, h, "C48")

	if r.Outcome != Failed || !strings.Contains(r.Failure, "asmai setup failed") || !strings.Contains(r.Failure, "does not trust "+h.QualificationPaths.Workspaces) {
		t.Errorf("C48 %s: %q, want setup's failure naming the untrusted workspaces folder", r.Outcome, r.Failure)
	}
}

func TestTheHarnessRunsSetupOnlyForItsDisposableState(t *testing.T) {
	h := harness(t, signedIn, startsDirectly)

	err := h.setup(context.Background(), NewFactory(h.Subject.Asmai, h.Paths, h.Env))

	if err == nil || !strings.Contains(err.Error(), "only for its disposable state directory") {
		t.Errorf("setup for the user's own state directory returned %v, want a refusal", err)
	}
}
