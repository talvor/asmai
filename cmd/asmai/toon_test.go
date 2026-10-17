// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/toon"
)

// decodeBoth decodes what a command printed with --toon and with --json.
func decodeBoth(t *testing.T, args []string, toonOut, jsonOut string) (fromTOON, fromJSON any) {
	t.Helper()
	fromTOON, err := toon.Decode([]byte(toonOut))
	if err != nil {
		t.Fatalf("asmai %s --toon printed\n%s\nwhich is not TOON: %v", strings.Join(args, " "), toonOut, err)
	}
	if err := json.Unmarshal([]byte(jsonOut), &fromJSON); err != nil {
		t.Fatalf("asmai %s --json printed %q, which is not JSON: %v", strings.Join(args, " "), jsonOut, err)
	}
	return fromTOON, fromJSON
}

func TestJSONAndTOONTogetherIsAUsageError(t *testing.T) {
	t.Setenv(daemon.SessionCredential, "")
	for _, args := range [][]string{{"version", "--json", "--toon"}, {"status", "--toon", "--json"}} {
		var stdout, stderr bytes.Buffer
		code := run(args, strings.NewReader(""), &stdout, &stderr)
		if code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "--json and --toon") || !strings.Contains(stderr.String(), "usage: asmai") {
			t.Errorf("asmai %s exited %d printing %q and %q, want 1 and a usage error on stderr", strings.Join(args, " "), code, stdout.String(), stderr.String())
		}
	}
}

// The commands that need no daemon print the same data with --toon as with
// --json, and a failure under --toon is a TOON error with the same exit code.
func TestTOONPrintsWhatJSONDoesWithoutAFactory(t *testing.T) {
	t.Setenv(daemon.SessionCredential, "")
	for _, args := range [][]string{{"version"}, {"notices"}, {"config", "example"}, {"hook"}} {
		var toonOut, jsonOut, stderr bytes.Buffer
		toonCode := run(append(args, "--toon"), strings.NewReader(""), &toonOut, &stderr)
		jsonCode := run(append(args, "--json"), strings.NewReader(""), &jsonOut, &stderr)
		if toonCode != jsonCode {
			t.Errorf("asmai %s exited %d with --toon and %d with --json", strings.Join(args, " "), toonCode, jsonCode)
		}
		fromTOON, fromJSON := decodeBoth(t, args, toonOut.String(), jsonOut.String())
		if !reflect.DeepEqual(fromTOON, fromJSON) {
			t.Errorf("asmai %s --toon printed\n%s\nwhich reads as %#v; --json printed %#v", strings.Join(args, " "), toonOut.String(), fromTOON, fromJSON)
		}
	}
	// asmai hook is run by agent sessions' hooks; anywhere else it fails.
	var stdout, stderr bytes.Buffer
	code := run([]string{"hook", "--toon"}, strings.NewReader(""), &stdout, &stderr)
	failure, _ := toon.Decode(stdout.Bytes())
	if m, ok := failure.(map[string]any); code != 1 || !ok || !strings.Contains(m["error"].(string), daemon.SessionCredential) {
		t.Errorf("a failing hook --toon exited %d printing %q, want 1 and a TOON error", code, stdout.String())
	}
}

// Every command a running factory answers prints the same data with --toon as
// with --json.
func TestEveryCommandPrintsWithTOONWhatItPrintsWithJSON(t *testing.T) {
	readyFactory(t, idleLeader)
	// Coordination's leader is started first, so that the leaders each start
	// shows are the same.
	var started startJSON
	asmaiJSON(t, &started, "start")
	for _, args := range [][]string{{"version"}, {"notices"}, {"start"}, {"status"}, {"agents"}, {"jobs"}, {"attach", "coordination"}, {"chat"}, {"providers", "list"}, {"repo", "list"}} {
		toonOut, stderr, toonCode := runAsmai(t, append(args, "--toon")...)
		jsonOut, _, jsonCode := runAsmai(t, append(args, "--json")...)
		if toonCode != 0 || jsonCode != 0 {
			t.Fatalf("asmai %s exited %d with --toon (%s) and %d with --json", strings.Join(args, " "), toonCode, stderr, jsonCode)
		}
		fromTOON, fromJSON := decodeBoth(t, args, toonOut, jsonOut)
		if !reflect.DeepEqual(fromTOON, fromJSON) {
			t.Errorf("asmai %s --toon printed\n%s\nwhich reads as %#v; --json printed %#v", strings.Join(args, " "), toonOut, fromTOON, fromJSON)
		}
	}

	// The journal and the log only grow, so what --toon printed first is the
	// start of what --json prints after it.
	toonOut, _, _ := runAsmai(t, "export", "--toon")
	jsonOut, _, _ := runAsmai(t, "export", "--json")
	fromTOON, fromJSON := decodeBoth(t, []string{"export"}, toonOut, jsonOut)
	toonJournal := fromTOON.(map[string]any)["journal"].([]any)
	jsonJournal := fromJSON.(map[string]any)["journal"].([]any)
	if len(toonJournal) == 0 || len(toonJournal) > len(jsonJournal) || !reflect.DeepEqual(toonJournal, jsonJournal[:len(toonJournal)]) {
		t.Errorf("asmai export --toon printed\n%s\nwhich is not the journal --json printed:\n%s", toonOut, jsonOut)
	}
	toonOut, _, _ = runAsmai(t, "log", "--toon")
	jsonOut, _, _ = runAsmai(t, "log", "--json")
	records := strings.Split(strings.TrimSuffix(toonOut, "\n"), "\n\n")
	lines := strings.Split(strings.TrimSuffix(jsonOut, "\n"), "\n")
	if len(records) == 0 || len(records) > len(lines) {
		t.Fatalf("asmai log --toon printed %d records, and --json %d after it", len(records), len(lines))
	}
	for i, record := range records {
		fromTOON, fromJSON := decodeBoth(t, []string{"log"}, record+"\n", lines[i])
		if !reflect.DeepEqual(fromTOON, fromJSON) {
			t.Errorf("asmai log --toon printed the record\n%s\nwhich is not the line --json printed: %s", record, lines[i])
		}
	}

	toonOut, _, toonCode := runAsmai(t, "stop", "--toon")
	fromTOON, _ = toon.Decode([]byte(toonOut))
	if toonCode != 0 || fromTOON.(map[string]any)["stopped"] != true {
		t.Errorf("asmai stop --toon exited %d printing %q, want it stopped", toonCode, toonOut)
	}
	// With --toon, a failure is a TOON error, with the exit code --json gives.
	toonOut, _, toonCode = runAsmai(t, "export", "--toon")
	jsonOut, _, jsonCode := runAsmai(t, "export", "--json")
	fromTOON, fromJSON = decodeBoth(t, []string{"export"}, toonOut, jsonOut)
	if toonCode != 1 || jsonCode != 1 || !reflect.DeepEqual(fromTOON, fromJSON) || fromTOON.(map[string]any)["error"] == "" {
		t.Errorf("a failing export --toon exited %d printing %q, want 1 and the TOON of the JSON error %q", toonCode, toonOut, jsonOut)
	}
}
