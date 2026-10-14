// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/talvor/asmai/internal/store"
)

// afterTheCompoundPrompt is the journal from the stop on, in which the user
// asks generation 2 of Coordination's leader to run the compound command, and
// the leader stops its turn, naming its transcript.
func afterTheCompoundPrompt(t *testing.T, prompt string, extra ...store.Entry) []store.Entry {
	t.Helper()
	entries := []store.Entry{
		entry(t, store.KindDaemonStopped, map[string]any{"by": "asmai stop"}),
		entry(t, store.KindDaemonStarted, map[string]any{"previous": "stopped"}),
		entry(t, store.KindObservation, map[string]any{"agent": "leader@coordination", "generation": 2, "event": "UserPromptSubmit", "payload": map[string]any{"prompt": prompt}}),
		entry(t, store.KindMessageWitnessed, map[string]any{"agent": "leader@coordination", "generation": 2, "text": prompt, "observation": 3}),
	}
	entries = append(entries, extra...)
	entries = append(entries, entry(t, store.KindObservation, map[string]any{"agent": "leader@coordination", "generation": 2, "event": "Stop", "payload": map[string]any{"transcript_path": "/transcript.jsonl"}}))
	return runJournal(t, entries...)
}

func TestTheCompoundCommandsTurnIsAnsweredWithItsTranscript(t *testing.T) {
	prompt := compoundPrompt(1, compoundAsmai(1))
	e := compoundEvidenceOf(afterTheCompoundPrompt(t, prompt), 1, prompt, 2)

	if e.asked != 3 || !e.answered || e.transcript != "/transcript.jsonl" || len(e.prompts) != 0 {
		t.Errorf("the evidence is %+v, want the prompt asked at observation 3 and answered with its transcript, and no native prompt", e)
	}
	// A Stop of another generation, or before the prompt, is not its answer.
	if e := compoundEvidenceOf(afterTheCompoundPrompt(t, prompt), 1, prompt, 3); e.answered {
		t.Errorf("generation 3's evidence %+v counts generation 2's turn", e)
	}
}

func TestANativePromptAfterTheStartIsNamedWithItsAgentAndCommand(t *testing.T) {
	prompt := compoundPrompt(1, compoundAsmai(1))
	request := entry(t, store.KindObservation, map[string]any{"agent": "leader@engineering", "generation": 2, "event": "PermissionRequest", "payload": map[string]any{
		"tool_name": "Bash", "tool_input": map[string]any{"command": "asmai brief 1; echo ----; asmai inbox", "dangerouslyDisableSandbox": true},
	}})
	e := compoundEvidenceOf(afterTheCompoundPrompt(t, prompt, request), 1, prompt, 2)

	if want := "leader@engineering for `asmai brief 1; echo ----; asmai inbox`"; len(e.prompts) != 1 || e.prompts[0] != want {
		t.Errorf("the native prompts are %q, want %q", e.prompts, want)
	}
	// One raised before the stop belongs to the factory before the restart.
	if e := compoundEvidenceOf(afterTheCompoundPrompt(t, prompt, request), 6, prompt, 2); len(e.prompts) != 0 {
		t.Errorf("the native prompts after entry 6 are %q, want none", e.prompts)
	}
}

func TestTheCompoundCommandMustRunInTheSandboxAndReachTheDaemon(t *testing.T) {
	command := compoundAsmai(1)
	reached := "Job 1: Handoff test\n----\nNothing is waiting for you."
	for name, c := range map[string]struct {
		command     string
		unsandboxed bool
		failed      bool
		output      string
		want        string
	}{
		"run in the sandbox":     {command: command, output: reached},
		"never run":              {command: "asmai brief 1", output: "Job 1", want: "inconclusive"},
		"run outside it":         {command: command, unsandboxed: true, output: reached, want: "outside the sandbox"},
		"stopped by the sandbox": {command: command, failed: true, output: "connecting to the daemon: operation not permitted\n----\n", want: "failed in the sandbox"},
		"printing half":          {command: command, output: "----\nNothing is waiting for you.", want: "not the brief, then the inbox"},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "transcript.jsonl")
			writeTranscript(t, path, c.command, c.unsandboxed, c.failed, c.output)
			turn, err := readTranscript(path)
			if err != nil {
				t.Fatal(err)
			}
			err = ranInTheSandbox(turn, command)
			if c.want == "" && err != nil {
				t.Errorf("it fails: %v", err)
			}
			if c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
				t.Errorf("it gives %v, want it to say %q", err, c.want)
			}
		})
	}
}

func TestWhereTheSandboxCannotAllowOneSocketTheCompoundCommandIsAKnownLimitation(t *testing.T) {
	allows := sandboxAllowsOneSocket
	sandboxAllowsOneSocket = false
	t.Cleanup(func() { sandboxAllowsOneSocket = allows })
	var r Result

	if err := (&Harness{}).compoundAsmaiReachesTheDaemon(context.Background(), nil, &r, newRestartEvidence(0)); err != nil {
		t.Fatal(err)
	}
	if len(r.Evidence) != 1 || !strings.HasPrefix(r.Evidence[0], "known limitation:") {
		t.Errorf("the evidence is %q, want the known limitation", r.Evidence)
	}
}
