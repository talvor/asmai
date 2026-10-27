// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/talvor/asmai/internal/fakeprovider"
)

// A leader other than Coordination's that cannot go on, for a reason that is
// not the user's choice, files a blocked report. It reaches Coordination's
// leader as a message of kind blocked, and the daemon nudges that leader.
// Coordination resolves it once its cause is settled, and the note of what
// changed reaches the leader that filed it as a message of kind
// blocked.resolved. The open report is in the job's brief, and nothing waits
// on the user for it.
func TestALeaderThatCannotGoOnFilesABlockedReportThatCoordinationResolves(t *testing.T) {
	gitIdentity(t)
	testDir := t.TempDir()
	t.Setenv("ASMAI_TEST_DIR", testDir)
	t.Setenv(fakeprovider.ScriptEnv+"_ENGINEERING", filepath.Join(testDir, "engineering.jsonl"))

	refused := func(command, name, want string) string {
		return fakeRun(command+` > "$ASMAI_TEST_DIR/`+name+`" 2>&1; status=$?; cat "$ASMAI_TEST_DIR/`+name+`"; test "$status" -eq 1`, 0, want)
	}
	engineering := concat(agentSession,
		turn(1, "handoff",
			fakeRun(`asmai inbox --dispatch 1`, 0, "Add a greeting"),
			fakeRun(`asmai handoff accept --handoff 1`, 0, "reply dispatch 2"),
			awaitFile("accept-read"),
			refused(`asmai blocked --job 1 --reason 'no cache' --needs 'a cache' --blocks 99`, "unknown-assignment-refusal", "assignment 99 does not exist"),
			refused(`asmai blocked --job 1 --reason 'no cache'`, "no-needs-refusal", "needs --needs"),
			refused(`asmai blocked --reason 'no cache' --needs 'a cache'`, "leader-worker-blocked-refusal", "only a worker may run asmai blocked without --job"),
			fakeRun(`asmai blocked --job 1 --reason 'the Go build cache is missing from the sandbox' --needs 'a writable build cache' | tee "$ASMAI_TEST_DIR/blocked-receipt"`, 0, "blocked report 1 recorded for job 1; sent to leader@coordination as dispatch 3"),
			refused(`asmai blocked resolve 1 --note 'fixed it myself'`, "engineering-resolve-refusal", "only leader@coordination may run asmai blocked resolve"),
		),
		turn(4, "resolved",
			fakeRun(`asmai inbox --dispatch 4 | tee "$ASMAI_TEST_DIR/resolved-notice"`, 0, "(?s)blocked.resolved from leader@coordination.*leader@coordination resolved your blocked report 1 of job 1. What changed:\nthe user made the build cache writable.*blocked report 1 \\| job 1 \\| leader@engineering \\| resolved"),
			fakeRun(`asmai brief 1 | tee "$ASMAI_TEST_DIR/resolved-brief"`, 0, "(?s)Open blocked reports [^\n]*:\n- none\n"),
			touchFile("engineering-done"),
		),
		[]string{idleSession},
	)
	writeFiles(t, testDir, map[string][]string{"engineering.jsonl": engineering})
	coordination := []string{
		hookStep("SessionStart", "", ""),
		`{"screen":"> "}`,
		`{"expect":"Please add a greeting\r"}`,
		hookStep("UserPromptSubmit", "", "Please add a greeting"),
		fakeRun(`asmai job open --message latest --repository fixture --reading 'Add a greeting' --criterion 'Greeting is printed'`, 0, "Opened job 1"),
		fakeRun(`asmai handoff send --job 1 --to engineering --outcome 'Add a greeting' --decisions none --evidence none --constraints none --permissions none --criterion 'Greeting is printed'`, 0, "dispatch 1"),
		hookStep("Stop", "", ""),
		`{"expect":"asmai inbox --dispatch 2\r"}`,
		hookStep("UserPromptSubmit", "reply", "asmai inbox --dispatch 2"),
		fakeRun(`asmai inbox --dispatch 2`, 0, "accepted from leader@engineering"),
		touchFile("accept-read"),
		hookStep("Stop", "reply", ""),
		`{"expect":"asmai inbox --dispatch 3\r"}`,
		hookStep("UserPromptSubmit", "blocked", "asmai inbox --dispatch 3"),
		fakeRun(`asmai inbox --dispatch 3 | tee "$ASMAI_TEST_DIR/blocked-notice"`, 0, "(?s)blocked from leader@engineering.*blocked report 1 \\| job 1 \\| leader@engineering \\| open\nReason: the Go build cache is missing from the sandbox\nNeeds: a writable build cache.*asmai blocked resolve 1 --note"),
		fakeRun(`asmai brief 1 | tee "$ASMAI_TEST_DIR/open-brief"`, 0, "(?s)Pending decisions:\n- none\nOpen blocked reports [^\n]*:\n- blocked report 1 from leader@engineering, [^\n]*\n  Reason: the Go build cache is missing from the sandbox\n  Needs: a writable build cache\n"),
		refused(`asmai blocked --job 1 --reason 'stuck' --needs 'help'`, "coordination-blocked-refusal", "leader@coordination files no blocked report"),
		refused(`asmai blocked resolve 1`, "no-note-refusal", "needs --note"),
		fakeRun(`asmai blocked resolve 1 --note 'the user made the build cache writable' | tee "$ASMAI_TEST_DIR/resolve-receipt"`, 0, "blocked report 1 of job 1 resolved; the note sent to leader@engineering as dispatch 4"),
		refused(`asmai blocked resolve 1 --note 'again'`, "resolve-again-refusal", "blocked report 1 was already resolved"),
		hookStep("Stop", "blocked", ""),
		idleSession,
	}
	readyFactory(t, coordination)
	t.Cleanup(func() {
		if t.Failed() {
			log, _, _ := runAsmai(t, "log")
			t.Logf("the daemon's log:\n%s", log)
		}
	})
	if _, stderr, code := runAsmai(t, "start"); code != 0 {
		t.Fatal(stderr)
	}
	registerWritingFixture(t)
	terminal := inTerminal(t, 100, 25)
	waitFor(t, "Coordination prompt", terminal.shows(">"))
	terminal.ptmx.WriteString("Please add a greeting\r")
	waitFor(t, "Engineering reading the resolution", touched(testDir, "engineering-done"))
	for _, name := range []string{"unknown-assignment-refusal", "no-needs-refusal", "leader-worker-blocked-refusal", "blocked-receipt", "engineering-resolve-refusal", "blocked-notice", "open-brief", "coordination-blocked-refusal", "no-note-refusal", "resolve-receipt", "resolve-again-refusal", "resolved-notice", "resolved-brief"} {
		output, err := os.ReadFile(filepath.Join(testDir, name))
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s:\n%s", name, output)
	}

	// The report waits on nobody but Coordination: the user has no decision
	// pending for it.
	if stdout, stderr, code := runAsmai(t, "decisions"); code != 0 || !strings.Contains(stdout, "No decisions are pending.") {
		t.Errorf("asmai decisions exited %d printing %q %q, want nothing pending", code, stdout, stderr)
	}
	if got := len(journaled(t, "blocked_report.filed")); got != 1 {
		t.Errorf("blocked reports filed: %d, want 1", got)
	}
	if got := len(journaled(t, "blocked_report.resolved")); got != 1 {
		t.Errorf("blocked reports resolved: %d, want 1", got)
	}
	states := dispatchStates(t, 0)
	for _, id := range []int64{3, 4} {
		if got := strings.Join(states[id], ","); !strings.Contains(got, "delivered,working") {
			t.Errorf("dispatch %d moved through %s, want it delivered and worked on", id, got)
		}
	}
}
