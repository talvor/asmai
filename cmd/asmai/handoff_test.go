// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/talvor/asmai/internal/fakeprovider"
)

func TestHandoffInboxNudgeAndReplyWithCorrelatedEvidence(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ASMAI_TEST_DIR", dir)
	engineering := []string{
		`{"hook":"SessionStart","payload":{"hook_event_name":"SessionStart","transcript_path":"/fake/engineering.jsonl"}}`,
		`{"screen":"> "}`,
		`{"expect":"asmai inbox --dispatch 1\r"}`,
		`{"hook":"UserPromptSubmit","payload":{"hook_event_name":"UserPromptSubmit","prompt_id":"nudge-1","prompt":"asmai inbox --dispatch 1","transcript_path":"/fake/engineering.jsonl"}}`,
		fakeRun(`asmai inbox --dispatch 1`, 0, "(?s)job 1.*handoff.*Implement fixture.*Decision one.*Evidence one.*Constraint one.*Permission one.*Tests pass"),
		fakeRun(`asmai inbox --dispatch 1`, 0, "message 1"),
		fakeRun(`asmai handoff accept --handoff 1`, 0, "reply dispatch 2"),
		`{"hook":"Stop","payload":{"hook_event_name":"Stop","prompt_id":"nudge-1","transcript_path":"/fake/engineering.jsonl"}}`,
		fakeRun(`touch "$ASMAI_TEST_DIR/engineering-done"`, 0, ""),
		`{"expect":"only typed by a test that means to"}`,
	}
	path := filepath.Join(dir, "engineering.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(engineering, "\n")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(fakeprovider.ScriptEnv+"_ENGINEERING", path)
	coordination := []string{
		`{"hook":"SessionStart","payload":{"hook_event_name":"SessionStart","transcript_path":"/fake/coordination.jsonl"}}`,
		`{"screen":"> "}`,
		`{"expect":"Please implement fixture\r"}`,
		`{"hook":"UserPromptSubmit","payload":{"hook_event_name":"UserPromptSubmit","prompt":"Please implement fixture","transcript_path":"/fake/coordination.jsonl"}}`,
		fakeRun(`asmai job open --message latest --repository fixture --reading 'Implement fixture' --criterion 'Tests pass'`, 0, "Opened job 1"),
		fakeRun(`asmai handoff send --job 1 --to engineering --outcome 'Implement fixture' --decisions 'Decision one' --evidence 'Evidence one' --constraints 'Constraint one' --permissions 'Permission one' --criterion 'Tests pass'`, 0, "dispatch 1"),
		`{"hook":"Stop","payload":{"hook_event_name":"Stop","transcript_path":"/fake/coordination.jsonl"}}`,
		`{"expect":"asmai inbox --dispatch 2\r"}`,
		`{"hook":"UserPromptSubmit","payload":{"hook_event_name":"UserPromptSubmit","prompt":"asmai inbox --dispatch 2","transcript_path":"/fake/coordination.jsonl"}}`,
		fakeRun(`asmai inbox --dispatch 2`, 0, "(?s)accepted from leader@engineering.*Answer:"),
		fakeRun(`touch "$ASMAI_TEST_DIR/coordination-done"`, 0, ""),
		`{"hook":"Stop","payload":{"hook_event_name":"Stop","transcript_path":"/fake/coordination.jsonl"}}`,
		`{"expect":"only typed by a test that means to"}`,
	}
	readyFactory(t, coordination)
	origin := filepath.Join(t.TempDir(), "fixture.git")
	gitIn(t, t.TempDir(), "init", "--bare", "-b", "main", origin)
	work := t.TempDir()
	gitIn(t, work, "clone", "--quiet", origin, "fixture")
	checkout := filepath.Join(work, "fixture")
	gitIn(t, checkout, "commit", "--allow-empty", "-m", "initial")
	gitIn(t, checkout, "push", "--quiet", "origin", "HEAD:main")
	if _, stderr, code := runAsmai(t, "repo", "add", origin); code != 0 {
		t.Fatal(stderr)
	}
	if _, stderr, code := runAsmai(t, "start"); code != 0 {
		t.Fatal(stderr)
	}
	a := inTerminal(t, 100, 25)
	waitFor(t, "Coordination prompt", a.shows(">"))
	a.ptmx.WriteString("Please implement fixture\r")
	waitFor(t, "Engineering answer", touched(dir, "engineering-done"))
	waitFor(t, "Coordination reply fetch", touched(dir, "coordination-done"))
	var inbox []struct {
		ID         int64  `json:"id"`
		State      string `json:"dispatch_state"`
		Transcript string `json:"transcript"`
	}
	// The agent command requires its active credential; the journal is the
	// user's read-only proof of the same durable transitions.
	_ = inbox
	var states map[int64][]string = map[int64][]string{}
	for _, e := range journaled(t, "dispatch.changed") {
		var d struct {
			ID    int64  `json:"id"`
			State string `json:"state"`
		}
		json.Unmarshal(e.Data, &d)
		states[d.ID] = append(states[d.ID], d.State)
	}
	if got := strings.Join(states[1], ","); got != "created,unknown,nudged,delivered,working,stopped" {
		t.Errorf("inbound dispatch states: %s", got)
	}
	if got := strings.Join(states[2], ","); got != "created,unknown,nudged,delivered" {
		t.Errorf("reply dispatch states: %s", got)
	}
	if got := journaled(t, "message.witnessed"); len(got) != 1 || !strings.Contains(string(got[0].Data), "Please implement fixture") {
		t.Errorf("witnessed messages: %+v", got)
	}
	if got := journaled(t, "message.fetched"); len(got) != 2 {
		t.Errorf("fetches: %+v", got)
	}
	found := false
	for _, a := range agentsListed(t) {
		if a.Agent == "leader@engineering" && a.State == "running" && a.Model == "opus" {
			found = true
		}
	}
	if !found {
		t.Error("Engineering leader did not start on its configured model")
	}
}
