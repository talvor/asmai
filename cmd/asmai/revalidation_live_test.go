// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/fakeprovider"
)

// This test sends real CLI commands to a running daemon and uses a local Git
// origin. The provider only keeps agent sessions open so the test can issue
// their commands; it supplies no validation verdicts or repository state.
func TestEarlyAssignmentReviewAndCleanMovedBaseMergeThroughTheRunningFactory(t *testing.T) {
	gitIdentity(t)
	testDir := t.TempDir()
	t.Setenv("ASMAI_TEST_DIR", testDir)
	t.Cleanup(func() {
		if t.Failed() {
			log, _, _ := runAsmai(t, "log")
			t.Logf("daemon log:\n%s", log)
			for _, name := range []string{"coordination-2.inbox", "coordination-13.inbox", "coordination-16.inbox"} {
				data, _ := os.ReadFile(filepath.Join(testDir, name))
				t.Logf("%s:\n%s", name, data)
			}
		}
	})
	for role, file := range map[string]string{
		"ENGINEERING": "engineering.jsonl", "QUALITY": "quality.jsonl",
		"WORKER": "worker.jsonl", "QUALITY_WORKER": "quality-worker.jsonl",
	} {
		t.Setenv(fakeprovider.ScriptEnv+"_"+role, filepath.Join(testDir, file))
	}
	observe := func(name string, dispatch int, handoff int) []string {
		steps := []string{fakeRun(fmt.Sprintf(`asmai inbox --dispatch %d > "$ASMAI_TEST_DIR/%s-%d.inbox"`, dispatch, name, dispatch), 0, "")}
		if handoff != 0 {
			steps = append(steps, fakeRun(fmt.Sprintf(`asmai handoff accept --handoff %d`, handoff), 0, ""))
		}
		steps = append(steps, touchFile(fmt.Sprintf("%s-%d", name, dispatch)))
		return turn(dispatch, name, steps...)
	}
	activeWorker := func(name string, dispatch int) []string {
		return concat(agentSession, turn(dispatch, name,
			fakeRun(`printf '%s' "$ASMAI_SESSION" > "$ASMAI_TEST_DIR/`+name+`.cred"`, 0, ""),
			fakeRun(fmt.Sprintf(`asmai inbox --dispatch %d > "$ASMAI_TEST_DIR/%s-%d.inbox"`, dispatch, name, dispatch), 0, ""),
			touchFile(fmt.Sprintf("%s-%d", name, dispatch)),
			awaitFile(fmt.Sprintf("%s-%d-release", name, dispatch)),
		), []string{idleSession})
	}
	writeFiles(t, testDir, map[string][]string{
		"engineering.jsonl": concat(agentSession,
			[]string{fakeRun(`printf '%s' "$ASMAI_SESSION" > "$ASMAI_TEST_DIR/engineering.cred"`, 0, "")},
			observe("engineering", 1, 1), observe("engineering", 4, 0), observe("engineering", 6, 0),
			observe("engineering", 9, 0), observe("engineering", 11, 0), observe("engineering", 18, 0),
			observe("engineering", 21, 0), []string{idleSession}),
		"quality.jsonl": concat(agentSession,
			[]string{fakeRun(`printf '%s' "$ASMAI_SESSION" > "$ASMAI_TEST_DIR/quality.cred"`, 0, "")},
			observe("quality", 5, 2), observe("quality", 8, 0), observe("quality", 12, 3),
			observe("quality", 15, 0), observe("quality", 17, 4), observe("quality", 20, 0), []string{idleSession}),
		"worker.jsonl":           activeWorker("worker", 3),
		"quality-worker.jsonl":   activeWorker("quality-worker", 7),
		"worker-2.jsonl":         activeWorker("worker", 10),
		"quality-worker-2.jsonl": activeWorker("quality-worker", 14),
		"quality-worker-3.jsonl": activeWorker("quality-worker", 19),
	})
	coordination := concat([]string{
		hookStep("SessionStart", "", ""), `{"screen":"> "}`,
		fakeRun(`printf '%s' "$ASMAI_SESSION" > "$ASMAI_TEST_DIR/coordination.cred"`, 0, ""),
		`{"expect":"Please add a greeting\r"}`,
		hookStep("UserPromptSubmit", "", "Please add a greeting"),
		fakeRun(`asmai job open --message latest --repository fixture --reading 'Add a greeting' --criterion 'Greeting is printed'`, 0, "Opened job 1"),
		fakeRun(`asmai handoff send --job 1 --to engineering --outcome 'Add a greeting' --decisions none --evidence none --constraints none --permissions none --criterion 'Greeting is printed'`, 0, "dispatch 1"),
		hookStep("Stop", "Please add a greeting", ""),
	}, observe("coordination", 2, 0), observe("coordination", 13, 0), observe("coordination", 16, 0), []string{idleSession})
	readyFactory(t, coordination)
	if _, stderr, code := runAsmai(t, "start"); code != 0 {
		t.Fatal(stderr)
	}
	fixture := registerWritingFixture(t)
	terminal := inTerminal(t, 100, 25)
	waitFor(t, "Coordination prompt", terminal.shows(">"))
	terminal.ptmx.WriteString("Please add a greeting\r")
	waitFor(t, "Engineering session", touched(testDir, "engineering.cred"))
	waitFor(t, "Coordination session", touched(testDir, "coordination.cred"))
	credential := func(name string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(testDir, name+".cred"))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	call := func(session string, args ...string) string {
		t.Helper()
		t.Setenv("ASMAI_SESSION", credential(session))
		stdout, stderr, code := runAsmai(t, args...)
		if code != 0 {
			t.Fatalf("asmai %s exited %d: %s%s", strings.Join(args, " "), code, stdout, stderr)
		}
		return stdout
	}
	jsonCall := func(session string, args ...string) daemon.Response {
		t.Helper()
		var resp daemon.Response
		if err := json.Unmarshal([]byte(call(session, append(args, "--json")...)), &resp); err != nil {
			t.Fatal(err)
		}
		return resp
	}
	assign := func(owner string, args ...string) daemon.Response {
		t.Helper()
		resp := jsonCall(owner, append([]string{"assign", "--job", "1"}, args...)...)
		if resp.Assignment == nil || resp.Dispatch == nil {
			t.Fatalf("assignment response: %+v", resp)
		}
		return resp
	}
	clearCredential := func(name string) {
		t.Helper()
		if err := os.Remove(filepath.Join(testDir, name+".cred")); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	workerResult := func(a daemon.Response, change func(string), evidence string) string {
		t.Helper()
		waitFor(t, "worker assignment", touched(testDir, fmt.Sprintf("worker-%d", a.Dispatch.ID)))
		worker := "worker"
		change(a.Assignment.Workspace.Path)
		sha := gitIn(t, a.Assignment.Workspace.Path, "rev-parse", "HEAD")
		gitIn(t, a.Assignment.Workspace.Path, "push", "origin", a.Assignment.Workspace.Branch)
		call(worker, "effect", "push", "origin/"+a.Assignment.Workspace.Branch+"@"+sha)
		result := jsonCall(worker, "result", "--evidence", evidence, "--test", "none", "--check", "./check.sh -> passed", "--gap", "none", "--pr-section", evidence)
		if result.Dispatch == nil {
			t.Fatalf("result response: %+v", result)
		}
		if err := os.WriteFile(filepath.Join(testDir, fmt.Sprintf("worker-%d-release", a.Dispatch.ID)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "Engineering result", touched(testDir, fmt.Sprintf("engineering-%d", result.Dispatch.ID)))
		return sha
	}
	validate := func(handoff daemon.Response, wantReview string, of int64) string {
		t.Helper()
		if handoff.Handoff == nil || handoff.Dispatch == nil {
			t.Fatalf("handoff response: %+v", handoff)
		}
		waitFor(t, "Quality handoff", touched(testDir, fmt.Sprintf("quality-%d", handoff.Dispatch.ID)))
		inboxBytes, err := os.ReadFile(filepath.Join(testDir, fmt.Sprintf("quality-%d.inbox", handoff.Dispatch.ID)))
		if err != nil {
			t.Fatal(err)
		}
		inbox := string(inboxBytes)
		if !strings.Contains(inbox, wantReview) {
			t.Fatalf("Quality inbox omitted %q:\n%s", wantReview, inbox)
		}
		args := []string{"--read-only", "--commit", handoff.Handoff.Head, "--outcome", "Check the result", "--criterion", "./check.sh passes"}
		if of != 0 {
			args = append(args, "--of", fmt.Sprint(of))
		}
		clearCredential("quality-worker")
		qualityDispatch := 7
		if of != 0 {
			qualityDispatch = 14
		} else if wantReview == "Review: checks" {
			qualityDispatch = 19
		}
		if qualityDispatch != 7 {
			data, err := os.ReadFile(filepath.Join(testDir, fmt.Sprintf("quality-worker-%d.jsonl", map[int]int{14: 2, 19: 3}[qualityDispatch])))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(testDir, "quality-worker.jsonl"), data, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		a := assign("quality", args...)
		if a.Assignment.Commit != handoff.Handoff.Head {
			t.Fatalf("validation commit %s, want %s", a.Assignment.Commit, handoff.Handoff.Head)
		}
		waitFor(t, "Quality worker assignment", touched(testDir, fmt.Sprintf("quality-worker-%d", a.Dispatch.ID)))
		gitIn(t, a.Assignment.Workspace.Path, "rev-parse", "HEAD")
		check := filepath.Join(a.Assignment.Workspace.Path, "check.sh")
		if out, err := os.ReadFile(check); err != nil || len(out) == 0 {
			t.Fatalf("Quality workspace check: %v", err)
		}
		checkCommand := exec.Command("./check.sh")
		checkCommand.Dir = a.Assignment.Workspace.Path
		if output, err := checkCommand.CombinedOutput(); err != nil {
			t.Fatalf("Quality check failed: %v: %s", err, output)
		}
		if output := gitIn(t, a.Assignment.Workspace.Path, "status", "--porcelain"); output != "" {
			t.Fatalf("Quality workspace is dirty: %s", output)
		}
		report := jsonCall("quality-worker", "result", "--evidence", "./check.sh passed at the reviewed commit", "--check", "./check.sh -> passed", "--gap", "none", "--finding", "none")
		if report.Dispatch == nil {
			t.Fatalf("validation report response: %+v", report)
		}
		if err := os.WriteFile(filepath.Join(testDir, fmt.Sprintf("quality-worker-%d-release", a.Dispatch.ID)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "Quality report", touched(testDir, fmt.Sprintf("quality-%d", report.Dispatch.ID)))
		accepted := jsonCall("quality", "accept", "--assignment", fmt.Sprint(a.Assignment.ID), "--reason", "the check passed at the exact commit")
		if accepted.Dispatch == nil {
			t.Fatalf("validation acceptance response: %+v", accepted)
		}
		t.Logf("Quality inbox:\n%s", inbox)
		return fmt.Sprint(accepted.Dispatch.ID)
	}
	waitFor(t, "Engineering's opening handoff", touched(testDir, "engineering-1"))
	clearCredential("worker")
	first := assign("engineering", "--outcome", "Add greeting and its check", "--criterion", "check passes")
	firstSHA := workerResult(first, func(dir string) {
		os.WriteFile(filepath.Join(dir, "greeting.txt"), []byte("hello\n"), 0o644)
		os.WriteFile(filepath.Join(dir, "check.sh"), []byte("#!/bin/sh\ntest \"$(cat greeting.txt)\" = hello\n"), 0o755)
		gitIn(t, dir, "add", "-A")
		gitIn(t, dir, "commit", "-m", "Add greeting")
	}, "first check passes")
	call("engineering", "accept", "--assignment", fmt.Sprint(first.Assignment.ID), "--reason", "the check passes")
	full := jsonCall("engineering", "handoff", "send", "--job", "1", "--to", "quality", "--outcome", "Validate", "--decisions", "none", "--evidence", "first check passes", "--constraints", "none", "--permissions", "none", "--criterion", "check passes")
	fullDelivery := validate(full, "Review: full", 0)
	waitFor(t, "Engineering's first verdict", touched(testDir, "engineering-"+fullDelivery))

	// Move main on origin, then merge it through a worker assignment. Ask for
	// an early review before accepting that assignment into the job branch.
	other := filepath.Join(testDir, "other")
	gitIn(t, testDir, "clone", fixture.origin, other)
	if err := os.WriteFile(filepath.Join(other, "base-moved.txt"), []byte("moved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, other, "add", "-A")
	gitIn(t, other, "commit", "-m", "Move main")
	gitIn(t, other, "push", "origin", "main")
	secondWorkerScript, err := os.ReadFile(filepath.Join(testDir, "worker-2.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(testDir, "worker.jsonl"), secondWorkerScript, 0o600); err != nil {
		t.Fatal(err)
	}
	clearCredential("worker")
	merge := assign("engineering", "--outcome", "Merge moved main", "--criterion", "the check passes after the merge")
	mergeSHA := workerResult(merge, func(dir string) {
		gitIn(t, dir, "fetch", "origin")
		gitIn(t, dir, "merge", "--no-edit", "origin/main")
	}, "clean merge check passes")
	if tip := gitIn(t, filepath.Join(os.Getenv("HOME"), ".local", "state", "asmai", "repositories", "fixture"), "rev-parse", "asmai/job-1-add-a-greeting"); tip != firstSHA {
		t.Fatalf("early review setup moved job head to %s, want %s", tip, firstSHA)
	}
	early := jsonCall("coordination", "handoff", "send", "--job", "1", "--to", "quality", "--assignment", fmt.Sprint(merge.Assignment.ID), "--outcome", "Review the merge assignment", "--decisions", "none", "--evidence", "merge result", "--constraints", "none", "--permissions", "none", "--criterion", "reviewed")
	if early.Handoff.Head != mergeSHA {
		t.Fatalf("early review head %s, want assignment result %s", early.Handoff.Head, mergeSHA)
	}
	earlyDelivery := validate(early, "Review: early", merge.Assignment.ID)
	waitFor(t, "Coordination's early verdict", touched(testDir, "coordination-"+earlyDelivery))
	coordInboxBytes, err := os.ReadFile(filepath.Join(testDir, "coordination-"+earlyDelivery+".inbox"))
	if err != nil {
		t.Fatal(err)
	}
	coordInbox := string(coordInboxBytes)
	if !strings.Contains(coordInbox, "validation from leader@quality") {
		t.Fatalf("early verdict not returned to Coordination:\n%s", coordInbox)
	}
	call("engineering", "accept", "--assignment", fmt.Sprint(merge.Assignment.ID), "--reason", "the base merged cleanly and check passed")
	checks := jsonCall("engineering", "handoff", "send", "--job", "1", "--to", "quality", "--outcome", "Recheck moved base", "--decisions", "none", "--evidence", "merge result", "--constraints", "none", "--permissions", "none", "--criterion", "check passes")
	if checks.Handoff.Head != mergeSHA {
		t.Fatalf("recheck head %s, want %s", checks.Handoff.Head, mergeSHA)
	}
	checksDelivery := validate(checks, "Review: checks", 0)
	waitFor(t, "Engineering's recheck verdict", touched(testDir, "engineering-"+checksDelivery))
	engineeringInboxBytes, err := os.ReadFile(filepath.Join(testDir, "engineering-"+checksDelivery+".inbox"))
	if err != nil {
		t.Fatal(err)
	}
	engineeringInbox := string(engineeringInboxBytes)
	if !strings.Contains(engineeringInbox, "0 blocking") {
		t.Fatalf("checks verdict missing:\n%s", engineeringInbox)
	}
	t.Logf("Coordination's early verdict:\n%s", coordInbox)
	t.Logf("Engineering's checks-only verdict:\n%s", engineeringInbox)
}
