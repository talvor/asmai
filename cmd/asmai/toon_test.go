// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/store"
	"github.com/talvor/asmai/internal/toon"
)

// decodeTOON reads what asmai printed with --toon.
func decodeTOON(t *testing.T, args []string, stdout string) any {
	t.Helper()
	v, err := toon.Unmarshal([]byte(stdout))
	if err != nil {
		t.Fatalf("asmai %s --toon printed\n%s\nwhich is not TOON: %v", strings.Join(args, " "), stdout, err)
	}
	return v
}

// decodeJSON reads what asmai printed with --json.
func decodeJSON(t *testing.T, args []string, stdout string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(stdout), &v); err != nil {
		t.Fatalf("asmai %s --json printed\n%s\nwhich is not JSON: %v", strings.Join(args, " "), stdout, err)
	}
	return v
}

// sameData runs asmai with --json and with --toon, and requires that both
// exit alike and print the same data.
func sameData(t *testing.T, args ...string) {
	t.Helper()
	jsonOut, jsonErr, jsonCode := runAsmai(t, append(slices.Clone(args), "--json")...)
	toonOut, toonErr, toonCode := runAsmai(t, append(slices.Clone(args), "--toon")...)
	if jsonCode != toonCode {
		t.Errorf("asmai %s exited %d with --json and %d with --toon: %s%s", strings.Join(args, " "), jsonCode, toonCode, jsonErr, toonErr)
	}
	if got, want := decodeTOON(t, args, toonOut), decodeJSON(t, args, jsonOut); !reflect.DeepEqual(got, want) {
		t.Errorf("asmai %s --toon printed\n%s\nwhich reads as\n%#v\nwant what --json printed\n%#v", strings.Join(args, " "), toonOut, got, want)
	}
}

// Every command family that prints JSON with --json prints the same data
// as TOON with --toon, and a failure is a TOON error with the same exit
// code as the JSON one.
func TestEveryCommandAcceptsTOON(t *testing.T) {
	readyFactory(t, idleLeader)
	for _, args := range [][]string{
		{"version"},
		{"notices"},
		{"start"},
		{"status"},
		{"agents"},
		{"jobs"},
		{"attach", "coordination"},
		{"export"},
		{"providers", "list"},
		{"repo", "list"},
		{"role", "list"},
		{"config", "check"},
		{"config", "show"},
		{"config", "example"},
		// Failures.
		{"brief", "99"},
		{"job", "99"},
		{"attach", "nobody@nowhere"},
		{"hook"},
	} {
		sameData(t, args...)
	}
	stdout, _, code := runAsmai(t, "hook", "--toon")
	failure, ok := decodeTOON(t, []string{"hook"}, stdout).(map[string]any)
	if code != 1 || !ok || !strings.Contains(failure["error"].(string), "ASMAI_SESSION") {
		t.Errorf("a failing hook --toon exited %d printing %q, want 1 and a TOON error", code, stdout)
	}
}

// busyFactory is what a busy factory's commands print with --json, by
// command family: the very values they give printJSON.
func busyFactory() map[string]any {
	at := time.Date(2026, 10, 18, 9, 30, 0, 0, time.UTC)
	status := daemon.Status{Version: "v1.4.0", PID: 4242, StartedAt: at}
	var leaders []daemon.Leader
	var agentList []store.Agent
	var journal []store.Entry
	var messages []store.Message
	var jobList []store.Job
	for i, role := range roles.All {
		leaders = append(leaders, daemon.Leader{Agent: "leader@" + role, State: store.AgentRunning, Generation: 1, PID: 5000 + i})
		agentList = append(agentList, store.Agent{Agent: "leader@" + role, Role: role, State: store.AgentRunning, Generation: 1, Provider: "claude", Version: "2.1.292", Model: "opus", PID: 5000 + i, StartedAt: at})
		agentList = append(agentList, store.Agent{Agent: fmt.Sprintf("worker%d@%s", i+1, role), Role: role, State: "stopped", Generation: 2, Provider: "claude", Version: "2.1.292", Model: "sonnet", StartedAt: at, EndedAt: at.Add(time.Hour), Exit: "exit status 0"})
	}
	for i := range 40 {
		data := fmt.Sprintf(`{"agent":"worker1@engineering","dispatch":%d,"job":1,"summary":"fetched the inbox, ran go test ./... and pushed"}`, i)
		journal = append(journal, store.Entry{ID: int64(i + 1), At: at.Add(time.Duration(i) * time.Second), Kind: "message.fetched", Data: json.RawMessage(data)})
	}
	for i := range 6 {
		messages = append(messages, store.Message{ID: int64(i + 1), Job: 1, Sender: "leader@engineering", Recipient: "worker1@engineering", Kind: "assignment", Body: "Outcome: implement the change.\nAcceptance criteria:\n- tests pass, go vet is clean", Dispatch: int64(i + 3), State: "delivered"})
		jobList = append(jobList, store.Job{Number: int64(i + 1), Role: "coordination", Repository: "asmai", State: "open", Witness: int64(9 + i), Words: "Implement https://github.com/talvor/asmai/issues/150", Reading: "Add --toon beside --json", Mandate: store.MandateTestedPR, Criteria: []string{"every command accepts --toon", "--json is unchanged"}, OpenedAt: at})
	}
	return map[string]any{
		"status": map[string]any{"daemon": runningJSON(status), "leaders": leaders, "prompts": []daemon.WaitingPrompt{}, "checks": []daemon.Check{{Name: "staffing", OK: true}, {Name: "Claude Code 2.1.292", OK: false, Problem: "not installed", Fix: "run `asmai providers install`"}}, "state_dir": "/home/u/.local/state/asmai"},
		"agents": map[string]any{"agents": agentList},
		"export": map[string]any{"journal": journal},
		"jobs":   map[string]any{"jobs": jobList},
		"brief":  map[string]any{"job": jobList[0], "brief": "Job 1 | role: coordination | state: open\nRepository: asmai"},
		"inbox":  daemon.Response{Messages: messages},
		"screen": map[string]any{"agent": leaders[0], "screen": map[string]any{"columns": 100, "rows": 3, "lines": []string{"> hello", "", "  ✻ Thinking…"}, "cursor": map[string]any{"x": 2, "y": 0, "visible": true}, "title": "claude"}},
		"error":  map[string]string{"error": "the factory is not running: run `asmai start`"},
	}
}

// For every command family, --toon prints the very data --json prints.
func TestTOONPrintsWhatJSONPrintsForEveryFamily(t *testing.T) {
	for family, v := range busyFactory() {
		var jsonOut, toonOut bytes.Buffer
		(&output{stdout: &jsonOut, json: true}).printJSON(v)
		(&output{stdout: &toonOut, json: true, toon: true}).printJSON(v)
		args := []string{family}
		if got, want := decodeTOON(t, args, toonOut.String()), decodeJSON(t, args, jsonOut.String()); !reflect.DeepEqual(got, want) {
			t.Errorf("%s as TOON is\n%s\nwhich reads as\n%#v\nwant %#v", family, toonOut.String(), got, want)
		}
	}
}

// TOON is smaller than the indented JSON --json prints. The test logs the
// sizes, in bytes and in an estimate of tokens: the runs of letters and
// digits, and each other character that is not a space.
func TestTOONIsSmallerThanJSON(t *testing.T) {
	estimate := func(s string) int {
		return len(regexp.MustCompile(`[\p{L}\p{N}_]+|[^\s\p{L}\p{N}_]`).FindAllString(s, -1))
	}
	values := busyFactory()
	for _, family := range slices.Sorted(maps.Keys(values)) {
		var jsonOut, toonOut bytes.Buffer
		(&output{stdout: &jsonOut, json: true}).printJSON(values[family])
		(&output{stdout: &toonOut, json: true, toon: true}).printJSON(values[family])
		j, o := jsonOut.String(), toonOut.String()
		if len(o) >= len(j) {
			t.Errorf("%s is %d bytes as TOON and %d as JSON, want TOON smaller", family, len(o), len(j))
		}
		t.Logf("%-7s JSON %6d bytes %5d tokens, TOON %6d bytes %5d tokens: %3.0f%% fewer bytes, %3.0f%% fewer tokens",
			family, len(j), estimate(j), len(o), estimate(o), 100-100*float64(len(o))/float64(len(j)), 100-100*float64(estimate(o))/float64(estimate(j)))
	}
}

// asmai attach --toon and asmai chat --toon print the agent's screen as it is
// now, as TOON.
func TestAttachAndChatPrintTheScreenAsTOON(t *testing.T) {
	readyFactory(t, idleLeader)
	if _, stderr, code := runAsmai(t, "start"); code != 0 {
		t.Fatalf("asmai start exited %d: %s", code, stderr)
	}
	for _, args := range [][]string{{"attach", "coordination"}, {"chat"}} {
		stdout, stderr, code := runAsmai(t, append(args, "--toon")...)
		if code != 0 {
			t.Fatalf("asmai %s --toon exited %d: %s%s", strings.Join(args, " "), code, stdout, stderr)
		}
		v, _ := decodeTOON(t, args, stdout).(map[string]any)
		agent, _ := v["agent"].(map[string]any)
		screen, _ := v["screen"].(map[string]any)
		lines, _ := screen["lines"].([]any)
		if agent["agent"] != "leader@coordination" || len(lines) == 0 || screen["columns"] == nil {
			t.Errorf("asmai %s --toon printed\n%s\nwant Coordination's screen", strings.Join(args, " "), stdout)
		}
		if !strings.Contains(stdout, "\nscreen:\n") || !strings.Contains(stdout, "  lines[") {
			t.Errorf("asmai %s --toon printed\n%s\nwant the screen as TOON", strings.Join(args, " "), stdout)
		}
		if args[0] == "chat" && v["started"] != false {
			t.Errorf("asmai chat --toon printed\n%s\nwant started: false", stdout)
		}
	}
}

// asmai log --toon prints each of the log's records as a TOON document of
// its own, in the order --json prints them as lines, with an empty line
// between them.
func TestLogPrintsEachRecordAsATOONDocument(t *testing.T) {
	readyFactory(t, idleLeader)
	toonOut, _, code := runAsmai(t, "log", "--toon")
	if code != 0 {
		t.Fatalf("asmai log --toon exited %d", code)
	}
	jsonOut, _, _ := runAsmai(t, "log", "--json")
	documents := strings.Split(strings.TrimSuffix(toonOut, "\n"), "\n\n")
	lines := strings.Split(strings.TrimSuffix(jsonOut, "\n"), "\n")
	if len(documents) == 0 || len(lines) < len(documents) {
		t.Fatalf("asmai log --toon printed %d records and --json %d", len(documents), len(lines))
	}
	for i, d := range documents {
		if got, want := decodeTOON(t, []string{"log"}, d+"\n"), decodeJSON(t, []string{"log"}, lines[i]); !reflect.DeepEqual(got, want) {
			t.Errorf("record %d of asmai log --toon is\n%s\nwant %s", i, d, lines[i])
		}
	}
}

// --json and --toon together are a usage error.
func TestJSONAndTOONTogetherAreAUsageError(t *testing.T) {
	for _, args := range [][]string{{"version", "--json", "--toon"}, {"status", "--toon", "--json"}} {
		var stdout, stderr bytes.Buffer
		code := run(args, strings.NewReader(""), &stdout, &stderr)
		if code != 1 {
			t.Errorf("asmai %s exited %d, want 1", strings.Join(args, " "), code)
		}
		if stdout.Len() != 0 {
			t.Errorf("asmai %s printed %q on stdout, want nothing", strings.Join(args, " "), stdout.String())
		}
		if !strings.Contains(stderr.String(), "--json and --toon") || !strings.Contains(stderr.String(), "usage: asmai") {
			t.Errorf("asmai %s printed %q on stderr, want the usage error and the usage", strings.Join(args, " "), stderr.String())
		}
	}
}

// A failure under --toon is a TOON error object on stdout, with the exit
// code the same failure has under --json.
func TestAFailureUnderTOONIsATOONError(t *testing.T) {
	factoryHome(t)
	jsonOut, _, jsonCode := runAsmai(t, "agents", "--json")
	toonOut, stderr, toonCode := runAsmai(t, "agents", "--toon")
	if toonCode != jsonCode || toonCode == 0 {
		t.Errorf("asmai agents with the factory stopped exited %d with --toon and %d with --json, want the same failure", toonCode, jsonCode)
	}
	if stderr != "" {
		t.Errorf("asmai agents --toon printed %q on stderr, want the error on stdout", stderr)
	}
	if !strings.HasPrefix(toonOut, "error: ") {
		t.Errorf("asmai agents --toon printed %q, want a TOON error object", toonOut)
	}
	got, want := decodeTOON(t, []string{"agents"}, toonOut), decodeJSON(t, []string{"agents"}, jsonOut)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("asmai agents --toon printed %#v, want the error --json prints, %#v", got, want)
	}
}

// The help documents --toon.
func TestHelpDocumentsTOON(t *testing.T) {
	var stdout, stderr bytes.Buffer
	run([]string{"help"}, strings.NewReader(""), &stdout, &stderr)
	for _, want := range []string{"[--json|--toon]", "--toon to print TOON"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("asmai help does not say %q", want)
		}
	}
}
