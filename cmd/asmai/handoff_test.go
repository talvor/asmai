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

func TestHandoffSendRequiresExplicitContextFields(t *testing.T) {
	fields := []string{"--decisions", "--evidence", "--constraints", "--permissions"}
	for _, missing := range fields {
		t.Run(missing, func(t *testing.T) {
			args := []string{"handoff", "send", "--job", "1", "--to", "engineering", "--outcome", "implement", "--criterion", "complete"}
			for _, field := range fields {
				if field != missing {
					args = append(args, field, "none")
				}
			}
			_, stderr, code := runAsmai(t, args...)
			if code != 1 || !strings.Contains(stderr, "use 'none' when empty") {
				t.Fatalf("handoff send without %s exited %d with %q", missing, code, stderr)
			}
		})
	}
}

func TestHandoffInboxNudgeAndReplyWithCorrelatedEvidence(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ASMAI_TEST_DIR", dir)
	engineering := []string{
		`{"hook":"SessionStart","payload":{"hook_event_name":"SessionStart","transcript_path":"/fake/engineering.jsonl"}}`,
		fakeRun(`touch "$ASMAI_TEST_DIR/engineering-before-boundary"; while [ ! -f "$ASMAI_TEST_DIR/boundary" ]; do sleep 0.02; done`, 0, ""),
		`{"screen":"\u001b[?2004h> "}`,
		`{"expect":"asmai inbox --dispatch 1\r"}`,
		`{"hook":"UserPromptSubmit","payload":{"hook_event_name":"UserPromptSubmit","prompt_id":"nudge-1","prompt":"asmai inbox --dispatch 1","transcript_path":"/fake/engineering.jsonl"}}`,
		fakeRun(`asmai inbox --dispatch 1`, 0, "(?s)job 1.*handoff.*Implement fixture.*Decision one.*Evidence one.*Constraint one.*Permission one.*Tests pass"),
		fakeRun(`asmai inbox --dispatch 1`, 0, "message 1"),
		fakeRun(`asmai brief 1`, 0, "(?s)Handoffs:.*1 leader@coordination -> leader@engineering: pending"),
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
	registerHandoffFixture(t)
	if _, stderr, code := runAsmai(t, "start"); code != 0 {
		t.Fatal(stderr)
	}
	a := inTerminal(t, 100, 25)
	waitFor(t, "Coordination prompt", a.shows(">"))
	a.ptmx.WriteString("Please implement fixture\r")
	waitFor(t, "Engineering before its input boundary", touched(dir, "engineering-before-boundary"))
	if changes := journaled(t, "dispatch.changed"); len(changes) != 1 || !strings.Contains(string(changes[0].Data), `"state": "created"`) {
		t.Fatalf("dispatch before input boundary: %+v", changes)
	}
	if err := os.WriteFile(filepath.Join(dir, "boundary"), []byte("ready"), 0600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "Engineering answer", touched(dir, "engineering-done"))
	waitFor(t, "Coordination reply fetch", touched(dir, "coordination-done"))
	// The agent command requires its active credential; the journal is the
	// user's read-only proof of the same durable transitions.
	observations := map[int64]bool{}
	for _, e := range journaled(t, "observation") {
		observations[e.ID] = true
	}
	var states map[int64][]string = map[int64][]string{}
	for _, e := range journaled(t, "dispatch.changed") {
		var d struct {
			ID          int64  `json:"id"`
			State       string `json:"state"`
			Observation int64  `json:"observation"`
		}
		json.Unmarshal(e.Data, &d)
		states[d.ID] = append(states[d.ID], d.State)
		if d.State == "nudged" && !observations[d.Observation] {
			t.Errorf("nudge for dispatch %d has no correlated observation: %s", d.ID, e.Data)
		}
	}
	if got := strings.Join(states[1], ","); got != "created,unknown,nudged,delivered,working,stopped" {
		t.Errorf("inbound dispatch states: %s", got)
	}
	if got := strings.Join(states[2], ","); got != "created,unknown,nudged,delivered,working" {
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

func registerHandoffFixture(t *testing.T) {
	t.Helper()
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
}

func TestHandoffClarificationAndDeclineReachCoordination(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ASMAI_TEST_DIR", dir)
	engineering := []string{
		`{"hook":"SessionStart","payload":{"hook_event_name":"SessionStart","transcript_path":"/fake/engineering.jsonl"}}`,
		`{"screen":"\u001b[?2004h> "}`,
		`{"expect":"asmai inbox --dispatch 1\r"}`,
		`{"hook":"UserPromptSubmit","payload":{"hook_event_name":"UserPromptSubmit","prompt":"asmai inbox --dispatch 1","prompt_id":"first","transcript_path":"/fake/engineering.jsonl"}}`,
		fakeRun(`asmai inbox --dispatch 1`, 0, "handoff 1"),
		fakeRun(`asmai handoff clarify --handoff 1`, 1, "need a reason or question"),
		fakeRun(`asmai handoff clarify --handoff 1 --answer 'Which branch?'`, 0, "reply dispatch 2"),
		`{"hook":"Stop","payload":{"hook_event_name":"Stop","prompt_id":"first","transcript_path":"/fake/engineering.jsonl"}}`,
		`{"expect":"asmai inbox --dispatch 3\r"}`,
		`{"hook":"UserPromptSubmit","payload":{"hook_event_name":"UserPromptSubmit","prompt":"asmai inbox --dispatch 3","prompt_id":"second","transcript_path":"/fake/engineering.jsonl"}}`,
		fakeRun(`asmai inbox --dispatch 3`, 0, "handoff 2"),
		fakeRun(`asmai handoff decline --handoff 2`, 1, "need a reason or question"),
		fakeRun(`asmai handoff decline --handoff 2 --answer 'Outside mandate'`, 0, "reply dispatch 4"),
		`{"hook":"Stop","payload":{"hook_event_name":"Stop","prompt_id":"second","transcript_path":"/fake/engineering.jsonl"}}`,
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
		`{"expect":"Please route this work\r"}`,
		`{"hook":"UserPromptSubmit","payload":{"hook_event_name":"UserPromptSubmit","prompt":"Please route this work"}}`,
		fakeRun(`asmai job open --message latest --repository fixture --reading 'Route work' --criterion 'Answered'`, 0, "Opened job 1"),
		fakeRun(`asmai handoff send --job 1 --to engineering --outcome 'First request' --decisions none --evidence none --constraints none --permissions none --criterion Answered`, 0, "dispatch 1"),
		`{"hook":"Stop","payload":{"hook_event_name":"Stop"}}`,
		`{"expect":"asmai inbox --dispatch 2\r"}`,
		`{"hook":"UserPromptSubmit","payload":{"hook_event_name":"UserPromptSubmit","prompt":"asmai inbox --dispatch 2","prompt_id":"reply-one"}}`,
		fakeRun(`asmai inbox --dispatch 2`, 0, "(?s)clarify from leader@engineering.*Which branch?"),
		fakeRun(`asmai handoff send --job 1 --to engineering --outcome 'Second request' --decisions none --evidence none --constraints none --permissions none --criterion Answered`, 0, "dispatch 3"),
		`{"hook":"Stop","payload":{"hook_event_name":"Stop","prompt_id":"reply-one"}}`,
		`{"expect":"asmai inbox --dispatch 4\r"}`,
		`{"hook":"UserPromptSubmit","payload":{"hook_event_name":"UserPromptSubmit","prompt":"asmai inbox --dispatch 4","prompt_id":"reply-two"}}`,
		fakeRun(`asmai inbox --dispatch 4`, 0, "(?s)declined from leader@engineering.*Outside mandate"),
		fakeRun(`touch "$ASMAI_TEST_DIR/done"`, 0, ""),
		`{"hook":"Stop","payload":{"hook_event_name":"Stop","prompt_id":"reply-two"}}`,
		`{"expect":"only typed by a test that means to"}`,
	}
	readyFactory(t, coordination)
	registerHandoffFixture(t)
	if _, stderr, code := runAsmai(t, "start"); code != 0 {
		t.Fatal(stderr)
	}
	a := inTerminal(t, 100, 25)
	waitFor(t, "Coordination prompt", a.shows(">"))
	a.ptmx.WriteString("Please route this work\r")
	waitFor(t, "both answers", touched(dir, "done"))
	answered := journaled(t, "handoff.answered")
	if len(answered) != 2 || !strings.Contains(string(answered[0].Data), `"state": "clarify"`) || !strings.Contains(string(answered[1].Data), `"state": "declined"`) || !strings.Contains(string(answered[1].Data), "Outside mandate") {
		t.Errorf("handoff answers: %+v", answered)
	}
	if fetched := journaled(t, "message.fetched"); len(fetched) != 4 {
		t.Errorf("fetched messages: %+v", fetched)
	}
}

func TestAnUnacknowledgedNudgeIsNeverRetyped(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ASMAI_TEST_DIR", dir)
	engineering := []string{
		`{"hook":"SessionStart","payload":{"hook_event_name":"SessionStart","transcript_path":"/fake/engineering.jsonl"}}`,
		`{"screen":"\u001b[?2004h> "}`,
		`{"expect":"asmai inbox --dispatch 1\r"}`,
		`{"hook":"Stop","payload":{"hook_event_name":"Stop","prompt_id":"unrelated"}}`,
		fakeRun(`touch "$ASMAI_TEST_DIR/no-ack"`, 0, ""),
		`{"expect":"only typed by a test that means to"}`,
	}
	path := filepath.Join(dir, "engineering.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(engineering, "\n")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(fakeprovider.ScriptEnv+"_ENGINEERING", path)
	coordination := []string{
		`{"hook":"SessionStart","payload":{"hook_event_name":"SessionStart"}}`,
		`{"screen":"> "}`,
		`{"expect":"Request a handoff\r"}`,
		`{"hook":"UserPromptSubmit","payload":{"hook_event_name":"UserPromptSubmit","prompt":"Request a handoff"}}`,
		fakeRun(`asmai job open --message latest --repository fixture --reading 'Request handoff' --criterion 'Answer'`, 0, "Opened job 1"),
		fakeRun(`asmai handoff send --job 1 --to engineering --outcome 'Answer' --decisions none --evidence none --constraints none --permissions none --criterion Answer`, 0, "dispatch 1"),
		`{"hook":"Stop","payload":{"hook_event_name":"Stop"}}`,
		`{"expect":"only typed by a test that means to"}`,
	}
	readyFactory(t, coordination)
	registerHandoffFixture(t)
	if _, stderr, code := runAsmai(t, "start"); code != 0 {
		t.Fatal(stderr)
	}
	a := inTerminal(t, 100, 25)
	waitFor(t, "Coordination prompt", a.shows(">"))
	a.ptmx.WriteString("Request a handoff\r")
	waitFor(t, "an unacknowledged nudge", touched(dir, "no-ack"))
	var status struct {
		Leaders []leaderStatusJSON `json:"leaders"`
	}
	asmaiJSON(t, &status, "status")
	found := false
	for _, leader := range status.Leaders {
		if leader.Agent == "leader@engineering" {
			found = true
			if leader.State != "running" || leader.Input != "unknown" {
				t.Errorf("Engineering after unacknowledged nudge: %+v", leader)
			}
		}
	}
	if !found {
		t.Fatal("Engineering leader did not start")
	}
	if changes := journaled(t, "dispatch.changed"); len(changes) != 2 || !strings.Contains(string(changes[1].Data), `"state": "unknown"`) {
		t.Errorf("dispatch transitions after no acknowledgment: %+v", changes)
	}
	if fetched := journaled(t, "message.fetched"); len(fetched) != 0 {
		t.Errorf("unfetched message was delivered: %+v", fetched)
	}
}

func TestAcknowledgedNudgeRepeatsUntilTheInboxIsFetched(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ASMAI_TEST_DIR", dir)
	engineering := []string{
		`{"hook":"SessionStart","payload":{"hook_event_name":"SessionStart","transcript_path":"/fake/engineering.jsonl"}}`,
		`{"screen":"\u001b[?2004h> "}`,
		`{"expect":"asmai inbox --dispatch 1\r"}`,
		`{"hook":"UserPromptSubmit","payload":{"hook_event_name":"UserPromptSubmit","prompt":"asmai inbox --dispatch 1","prompt_id":"first"}}`,
		`{"hook":"Stop","payload":{"hook_event_name":"Stop","prompt_id":"first"}}`,
		`{"expect":"asmai inbox --dispatch 1\r"}`,
		`{"hook":"UserPromptSubmit","payload":{"hook_event_name":"UserPromptSubmit","prompt":"asmai inbox --dispatch 1","prompt_id":"second"}}`,
		fakeRun(`asmai inbox --dispatch 1`, 0, "handoff 1"),
		fakeRun(`asmai inbox --dispatch 1`, 0, "handoff 1"),
		fakeRun(`asmai handoff accept --handoff 1`, 0, "reply dispatch 2"),
		`{"hook":"Stop","payload":{"hook_event_name":"Stop","prompt_id":"second"}}`,
		fakeRun(`touch "$ASMAI_TEST_DIR/done"`, 0, ""),
		`{"expect":"only typed by a test that means to"}`,
	}
	path := filepath.Join(dir, "engineering.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(engineering, "\n")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(fakeprovider.ScriptEnv+"_ENGINEERING", path)
	coordination := []string{
		`{"hook":"SessionStart","payload":{"hook_event_name":"SessionStart"}}`,
		`{"screen":"> "}`,
		`{"expect":"Send a handoff\r"}`,
		`{"hook":"UserPromptSubmit","payload":{"hook_event_name":"UserPromptSubmit","prompt":"Send a handoff"}}`,
		fakeRun(`asmai job open --message latest --repository fixture --reading 'Handoff' --criterion 'Answer'`, 0, "Opened job 1"),
		fakeRun(`asmai handoff send --job 1 --to engineering --outcome Answer --decisions none --evidence none --constraints none --permissions none --criterion Answer`, 0, "dispatch 1"),
		`{"hook":"Stop","payload":{"hook_event_name":"Stop"}}`,
		`{"expect":"asmai inbox --dispatch 2\r"}`,
		`{"hook":"UserPromptSubmit","payload":{"hook_event_name":"UserPromptSubmit","prompt":"asmai inbox --dispatch 2","prompt_id":"reply"}}`,
		fakeRun(`asmai inbox --dispatch 2`, 0, "accepted from leader@engineering"),
		`{"hook":"Stop","payload":{"hook_event_name":"Stop","prompt_id":"reply"}}`,
		`{"expect":"only typed by a test that means to"}`,
	}
	readyFactory(t, coordination)
	registerHandoffFixture(t)
	if _, stderr, code := runAsmai(t, "start"); code != 0 {
		t.Fatal(stderr)
	}
	a := inTerminal(t, 100, 25)
	waitFor(t, "Coordination prompt", a.shows(">"))
	a.ptmx.WriteString("Send a handoff\r")
	waitFor(t, "repeat nudge and answer", touched(dir, "done"))
	var states []string
	for _, e := range journaled(t, "dispatch.changed") {
		var d struct {
			ID    int64  `json:"id"`
			State string `json:"state"`
		}
		json.Unmarshal(e.Data, &d)
		if d.ID == 1 {
			states = append(states, d.State)
		}
	}
	if got := strings.Join(states, ","); got != "created,unknown,nudged,unknown,nudged,delivered,working,stopped" {
		t.Errorf("repeated dispatch states: %s", got)
	}
	if got := journaled(t, "message.fetched"); len(got) != 2 {
		t.Errorf("idempotent inbox reads journaled %d fetches, want one per message", len(got))
	}
	if got := journaled(t, "message.witnessed"); len(got) != 1 {
		t.Errorf("repeated nudge was witnessed: %+v", got)
	}
}
