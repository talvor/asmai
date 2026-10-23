// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/toon-format/toon-go"
)

// decodedJSON and decodedTOON are the data a command printed in each format.
func decodedJSON(t *testing.T, what, text string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(text), &v); err != nil {
		t.Fatalf("%s printed %q, which is not JSON: %v", what, text, err)
	}
	return v
}

func decodedTOON(t *testing.T, what, text string) any {
	t.Helper()
	v, err := toon.DecodeString(text)
	if err != nil {
		t.Fatalf("%s printed %q, which is not TOON: %v", what, text, err)
	}
	return v
}

// sameData runs asmai with args and --json, and again with --toon, and
// requires both to exit alike and to print the same data. It returns the exit
// code and the TOON that was printed.
func sameData(t *testing.T, args ...string) (code int, toonText string) {
	t.Helper()
	command := "asmai " + strings.Join(args, " ")
	jsonOut, jsonErr, jsonCode := runAsmai(t, append(args, "--json")...)
	toonOut, toonErr, toonCode := runAsmai(t, append(args, "--toon")...)
	if jsonCode != toonCode {
		t.Fatalf("%s exited %d with --json and %d with --toon\n%s%s\n%s%s", command, jsonCode, toonCode, jsonOut, jsonErr, toonOut, toonErr)
	}
	if jsonErr != toonErr {
		t.Errorf("%s printed %q to stderr with --json and %q with --toon", command, jsonErr, toonErr)
	}
	want := decodedJSON(t, command+" --json", jsonOut)
	got := decodedTOON(t, command+" --toon", toonOut)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s --toon printed\n%s\nwhich holds %#v, but --json printed\n%s", command, toonOut, got, jsonOut)
	}
	if got, want := jsonCode == 0, !strings.HasPrefix(toonOut, "error:"); got != want {
		t.Errorf("%s exited %d and printed\n%s", command, toonCode, toonOut)
	}
	if len(toonOut) >= len(jsonOut) {
		t.Errorf("%s --toon printed %d bytes, no fewer than the %d of --json", command, len(toonOut), len(jsonOut))
	}
	return toonCode, toonOut
}

// The agents' side of a factory with an open job, run in both formats by the
// fake Coordination leader: what each printed is left in the test directory.
func coordinationRunningInBothFormats() []string {
	both := func(name, command string) string {
		return fakeRun(command+` --json > "$ASMAI_TEST_DIR/`+name+`.json"; code=$?; `+command+` --toon > "$ASMAI_TEST_DIR/`+name+`.toon"; test $code = $?`, 0, "")
	}
	return []string{
		`{"screen": "> "}`,
		`{"expect": "Please fix the fixture\r"}`,
		`{"hook": "UserPromptSubmit", "payload": {"hook_event_name": "UserPromptSubmit", "prompt": "Please fix the fixture"}}`,
		// A command that changes the factory prints its result as TOON too.
		fakeRun(`asmai job open --message latest --repository fixture --reading 'Fix the fixture' --criterion 'Tests pass' --criterion 'Open a "reviewed" PR' --toon > "$ASMAI_TEST_DIR/job-open.toon"`, 0, ""),
		both("status", `asmai status`),
		both("agents", `asmai agents`),
		both("jobs", `asmai jobs`),
		both("brief", `asmai brief 1`),
		both("brief-missing", `asmai brief 99`),
		both("job-open-refused", `asmai job open --message latest --repository absent --reading 'x' --criterion 'y'`),
		fakeRun(`asmai decision request --job 1 --title 'Which cache?' --body 'Redis, or in the process?' --option redis --option in-memory --recommend in-memory --toon > "$ASMAI_TEST_DIR/decision-request.toon"`, 0, ""),
		both("inbox", `asmai inbox`),
		// A failure is a TOON error, with its exit code.
		fakeRun(`asmai brief 99 --toon`, 1, `^error: .*99`),
		fakeRun(`asmai start --toon`, 1, `^error: an agent cannot run`),
		// Both formats at once is a usage error.
		fakeRun(`asmai brief 1 --json --toon`, 1, `asmai brief: --json and --toon cannot be used together`),
		fakeRun(`touch "$ASMAI_TEST_DIR/done"`, 0, ""),
		`{"expect": "only typed by a test that means to"}`,
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestTOONHoldsTheDataJSONHoldsForEveryCommandFamily(t *testing.T) {
	testDir := t.TempDir()
	t.Setenv("ASMAI_TEST_DIR", testDir)
	readyFactory(t, coordinationRunningInBothFormats())
	origin := filepath.Join(t.TempDir(), "fixture.git")
	gitIn(t, t.TempDir(), "init", "--bare", "-b", "main", origin)
	work := t.TempDir()
	gitIn(t, work, "clone", "--quiet", origin, "fixture")
	checkout := filepath.Join(work, "fixture")
	commitSetUp(t, checkout, "initial")
	gitIn(t, checkout, "push", "--quiet", "origin", "HEAD:main")
	if _, stderr, code := runAsmai(t, "repo", "add", origin, "--toon"); code != 0 {
		t.Fatalf("repo add --toon: %s", stderr)
	}
	a := inTerminal(t, 100, 25)
	t.Cleanup(func() {
		if t.Failed() {
			log, _, _ := runAsmai(t, "log")
			t.Logf("Coordination's screen:\n%s\nthe daemon's log:\n%s", strings.Join(a.lines(), "\n"), log)
		}
	})
	waitFor(t, "Coordination's prompt", a.shows(">"))
	a.ptmx.WriteString("Please fix the fixture\r")
	waitFor(t, "the fake leader to run its commands", touched(testDir, "done"))

	// What the agents printed.
	for _, name := range []string{"status", "agents", "jobs", "brief", "brief-missing", "job-open-refused", "inbox"} {
		jsonText, err := os.ReadFile(filepath.Join(testDir, name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		toonText, err := os.ReadFile(filepath.Join(testDir, name+".toon"))
		if err != nil {
			t.Fatal(err)
		}
		want := decodedJSON(t, name+" --json", string(jsonText))
		got := decodedTOON(t, name+" --toon", string(toonText))
		if !reflect.DeepEqual(got, want) {
			t.Errorf("an agent's %s --toon printed\n%s\nwhich holds %#v, but --json printed\n%s", name, toonText, got, jsonText)
		}
	}
	// What the commands that change the factory printed.
	opened, ok := decodedTOON(t, "job open --toon", readFile(t, filepath.Join(testDir, "job-open.toon"))).(map[string]any)["job"].(map[string]any)
	if !ok || opened["number"] != float64(1) || !reflect.DeepEqual(opened["acceptance_criteria"], []any{"Tests pass", `Open a "reviewed" PR`}) {
		t.Errorf("asmai job open --toon printed %v", opened)
	}
	requested := readFile(t, filepath.Join(testDir, "decision-request.toon"))
	if !strings.Contains(requested, "Which cache?") || strings.HasPrefix(requested, "{") {
		t.Errorf("asmai decision request --toon printed %s", requested)
	}
	// The listings are uniform arrays, which TOON prints once with their keys.
	agents, err := os.ReadFile(filepath.Join(testDir, "agents.toon"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(agents), "agents[") || !strings.Contains(string(agents), "]{agent,") {
		t.Errorf("asmai agents --toon did not print a table of agents:\n%s", agents)
	}

	// What the user types.
	for _, args := range [][]string{
		{"version"}, {"notices"}, {"status"}, {"agents"}, {"jobs"}, {"job", "1"}, {"brief", "1"}, {"export"},
		{"decisions"}, {"providers", "list"}, {"lavish"}, {"role", "list"}, {"config", "show"}, {"config", "check"},
		{"repo", "list"}, {"repo", "show", "fixture"}, {"attach", "coordination"}, {"chat"}, {"start"},
		// Failures are TOON errors too, with the same exit code.
		{"job", "99"}, {"repo", "show", "absent"}, {"attach", "absent"}, {"job", "open", "--message", "latest", "--repository", "absent", "--reading", "x", "--criterion", "y"},
	} {
		sameData(t, args...)
	}
	// Stopping prints what it stopped, which holds the daemon's pid, so the
	// two formats stop one daemon each.
	var stopped struct {
		Stopped bool `json:"stopped"`
		Daemon  struct {
			PID     int    `json:"pid"`
			Version string `json:"version"`
		} `json:"daemon"`
	}
	asmaiJSON(t, &stopped, "stop")
	if !stopped.Stopped || stopped.Daemon.PID == 0 {
		t.Fatalf("asmai stop --json printed %+v", stopped)
	}
	if _, stderr, code := runAsmai(t, "start", "--toon"); code != 0 {
		t.Fatalf("asmai start --toon: %s", stderr)
	}
	out, _, code := runAsmai(t, "stop", "--toon")
	if code != 0 || !strings.Contains(out, "stopped: true\n") || !strings.Contains(out, "version: "+e2eVersion+"\n") || !strings.Contains(out, "running: false\n") {
		t.Errorf("asmai stop --toon exited %d printing %q", code, out)
	}
	// Stopped, it has nothing to stop in either format.
	sameData(t, "stop")
	// With the daemon stopped, status says so and its log can still be read.
	sameData(t, "status")
	sameData(t, "export")
	logJSON, _, _ := runAsmai(t, "log", "--json")
	logTOON, _, code := runAsmai(t, "log", "--toon")
	if code != 0 {
		t.Fatalf("asmai log --toon exited %d", code)
	}
	records := strings.Split(strings.TrimSpace(logJSON), "\n")
	documents := strings.Split(strings.TrimSpace(logTOON), "\n\n")
	if len(documents) != len(records) || len(records) < 2 {
		t.Fatalf("asmai log --toon printed %d documents for the %d records of --json", len(documents), len(records))
	}
	for i, record := range records {
		want := decodedJSON(t, "a log record", record)
		if got := decodedTOON(t, "a log document", documents[i]); !reflect.DeepEqual(got, want) {
			t.Errorf("log record %d is %#v in TOON and %#v in JSON", i, got, want)
		}
	}
}

func TestTOONAndJSONTogetherAreAUsageError(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"status"}, {"agents"}, {"repo", "list"}, {"attach", "coordination"}, {"chat"}, {"init"}, {"start"}, {"stop"}, {"log"}, {"job", "open"}, {"decision", "request"}} {
		for _, flags := range [][]string{{"--json", "--toon"}, {"--toon", "--json"}, {"--toon=true", "--json=true"}} {
			stdout, stderr, code := inProcess(t, append(append([]string{}, args...), flags...)...)
			if code != 1 || stdout != "" || !strings.Contains(stderr, "--json and --toon cannot be used together") {
				t.Errorf("asmai %s %s exited %d printing %q and %q, want 1, nothing and a usage error", strings.Join(args, " "), strings.Join(flags, " "), code, stdout, stderr)
			}
		}
	}
	// Either alone is fine; --toon=false asks for nothing.
	if stdout, _, code := inProcess(t, "version", "--toon", "--json=false"); code != 0 || stdout != "version: dev\n" {
		t.Errorf("asmai version --toon --json=false exited %d printing %q", code, stdout)
	}
}

func TestEveryCommandAcceptsTOON(t *testing.T) {
	readyFactory(t, idleLeader)
	for _, args := range [][]string{{"version"}, {"notices"}, {"status"}, {"start"}, {"agents"}, {"attach", "coordination"}, {"export"}, {"providers", "list"}, {"lavish"}, {"stop"}} {
		out, stderr, code := runAsmai(t, append(args, "--toon")...)
		if code != 0 || out == "" || strings.HasPrefix(out, "error:") {
			t.Errorf("asmai %s --toon exited %d printing %q and %q", strings.Join(args, " "), code, out, stderr)
		}
		if strings.HasPrefix(strings.TrimSpace(out), "{") {
			t.Errorf("asmai %s --toon printed JSON: %s", strings.Join(args, " "), out)
		}
	}
	// With --toon, a failure is a TOON error too.
	stdout, stderr, code := runAsmai(t, "export", "--toon")
	if code != 1 || !strings.HasPrefix(stdout, "error: ") || stderr != "" {
		t.Errorf("a failing export --toon exited %d printing %q and %q, want 1 and a TOON error", code, stdout, stderr)
	}
	failure, ok := decodedTOON(t, "export --toon", stdout).(map[string]any)
	if !ok || failure["error"] == "" {
		t.Errorf("export --toon printed %q, want an error", stdout)
	}
	// asmai hook is run by agent sessions' hooks; anywhere else it fails.
	stdout, _, code = runAsmai(t, "hook", "--toon")
	if code != 1 || !strings.HasPrefix(stdout, "error: ") || !strings.Contains(stdout, "ASMAI_SESSION") {
		t.Errorf("a failing hook --toon exited %d printing %q, want 1 and a TOON error", code, stdout)
	}
	// The dashboard draws on a terminal and has no data form.
	stdout, _, code = runAsmai(t, "dashboard", "--toon")
	if code != 1 || !strings.HasPrefix(stdout, "error: ") {
		t.Errorf("asmai dashboard --toon exited %d printing %q, want 1 and a TOON error", code, stdout)
	}
}

func TestDoctorPrintsTheSameDataInTOON(t *testing.T) {
	readyFactory(t, idleLeader)
	fakeUpstream(t, "v99.0.0", nil)
	jsonOut, _, jsonCode := doctorIn(t, "--json")
	toonOut, _, toonCode := doctorIn(t, "--toon")
	if jsonCode != toonCode {
		t.Fatalf("asmai doctor exited %d with --json and %d with --toon", jsonCode, toonCode)
	}
	if got, want := decodedTOON(t, "doctor --toon", toonOut), decodedJSON(t, "doctor --json", jsonOut); !reflect.DeepEqual(got, want) {
		t.Errorf("doctor --toon holds %#v, but --json holds %#v", got, want)
	}
}

// The assignment family: what a leader and its worker run, and see, in both
// formats around an assignment's result.
func TestTOONHoldsTheDataJSONHoldsForTheAssignmentCommands(t *testing.T) {
	const branch = "asmai/job-1/1"
	both := func(name, command string) string {
		return fakeRun(command+` --json > "$ASMAI_TEST_DIR/`+name+`.json"; code=$?; `+command+` --toon > "$ASMAI_TEST_DIR/`+name+`.toon"; test $code = $?`, 0, "")
	}
	worker := []string{
		both("worker-inbox", `asmai inbox --dispatch 3`),
		fakeRun(`printf 'hello\n' > greeting.txt && printf '#!/bin/sh\ntest "$(cat greeting.txt)" = hello\n' > check.sh && chmod +x check.sh && ./check.sh`, 0, ""),
		fakeRun(`asmai commit --message 'Add greeting.txt and its check' --toon > "$ASMAI_TEST_DIR/effect.toon"`, 0, ""),
		fakeRun(`asmai merge asmai/job-1-add-a-greeting`, 0, "nothing to merge"),
		fakeRun(`asmai push --toon`, 0, `^push:`),
		// A result refused is a TOON error, with its exit code.
		fakeRun(`asmai result --evidence x --test none --check none --gap none --toon`, 1, `^error: .*--pr-section`),
		fakeRun(resultFlagsForGreeting+` --toon > "$ASMAI_TEST_DIR/result.toon"`, 0, ""),
	}
	leader := []string{
		both("leader-inbox", `asmai inbox --dispatch 4`),
		both("leader-brief", `asmai brief 1`),
		both("leader-status", `asmai status`),
		fakeRun(`asmai accept --assignment 99 --reason x --toon`, 1, `^error: `),
	}
	assignmentFlow(t, worker, leader)
	testDir := os.Getenv("ASMAI_TEST_DIR")
	for _, name := range []string{"worker-inbox", "leader-inbox", "leader-brief", "leader-status"} {
		want := decodedJSON(t, name+" --json", readFile(t, filepath.Join(testDir, name+".json")))
		got := decodedTOON(t, name+" --toon", readFile(t, filepath.Join(testDir, name+".toon")))
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s --toon holds %#v, but --json holds %#v", name, got, want)
		}
	}
	// What the commands that change the factory printed.
	for _, name := range []string{"effect", "result"} {
		text := readFile(t, filepath.Join(testDir, name+".toon"))
		if got, ok := decodedTOON(t, name+" --toon", text).(map[string]any); !ok || len(got) == 0 || strings.HasPrefix(text, "{") {
			t.Errorf("asmai %s --toon printed %q", name, text)
		}
	}
	for _, args := range [][]string{{"jobs"}, {"job", "1"}, {"brief", "1"}, {"export"}, {"agents"}, {"status"}} {
		sameData(t, args...)
	}
}
