// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/statedir"
	"github.com/talvor/asmai/internal/store"
)

// standInOtherClaude stands in for a Claude Code that is not the pinned one,
// as a copy that updated itself is. It reports version 2.1.300, and if it is
// ever run for anything else, it hands the test its session's credential, as
// a session would.
const standInOtherClaude = `#!/bin/sh
if [ "$1" = --version ]; then
	echo "2.1.300 (Claude Code)"
	exit 0
fi
printf '%s' "$ASMAI_SESSION" > "$ASMAI_TEST_DIR/credential"
exec sleep 60
`

// certificationFactory starts a daemon whose checks other than certification
// pass, with install the stand-in at the pinned copy's path.
func certificationFactory(t *testing.T, cfg Config, stand string) (p statedir.Paths, testDir, claude string) {
	t.Helper()
	testDir = t.TempDir()
	t.Setenv("ASMAI_TEST_DIR", testDir)
	writeRulesAnswer(t, false)
	p = stateDir(t)
	record := filepath.Join(testDir, ".claude.json")
	if err := os.WriteFile(record, []byte(`{"projects": {"`+p.Dir+`": {"hasTrustDialogAccepted": true}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Paths, cfg.Version, cfg.ConfigFile, cfg.ClaudeRecord = p, "v1.2.3", readyConfig(t, testDir), record
	if cfg.Pins.ClaudeCode.Version == "" {
		cfg.Pins = testPins("2.1.292")
	}
	runDaemonWith(t, cfg)
	claude = filepath.Join(p.Providers, "claude-code", "2.1.292", "claude")
	os.MkdirAll(filepath.Dir(claude), 0o700)
	replaceClaude(t, claude, stand)
	install := store.ProviderInstall{Name: "claude-code", Version: "2.1.292", Path: claude, SHA256: strings.Repeat("a", 64)}
	if _, err := Call(p.Socket, Request{Command: CommandProviderInstalled, Provider: &install}); err != nil {
		t.Fatal(err)
	}
	return p, testDir, claude
}

// replaceClaude puts a new file at path, as an update does, in place of the
// executable that may be running.
func replaceClaude(t *testing.T, path, content string) {
	t.Helper()
	next := path + ".next"
	if err := os.WriteFile(next, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, path); err != nil {
		t.Fatal(err)
	}
}

func checkNamed(checks []Check, name string) (Check, bool) {
	for _, c := range checks {
		if c.Name == name {
			return c, true
		}
	}
	return Check{}, false
}

// An uncertified platform starts no agent, and the check says which platform
// it is and what to do. Nothing is run on it.
func TestAnUncertifiedPlatformStartsNoAgentAndSaysWhatToDo(t *testing.T) {
	p, testDir, _ := certificationFactory(t, Config{GOOS: "freebsd", GOARCH: "amd64"}, standInClaude)

	resp, err := Call(p.Socket, Request{Command: CommandStart})
	if err != nil {
		t.Fatal(err)
	}
	platform, ok := checkNamed(resp.Checks, "platform")
	if !ok || platform.OK {
		t.Fatalf("on freebsd/amd64 the platform check is %+v, want it failed", platform)
	}
	for _, want := range []string{"freebsd/amd64", "starts no agents"} {
		if !strings.Contains(platform.Problem, want) {
			t.Errorf("the problem %q does not say %q", platform.Problem, want)
		}
	}
	if !strings.Contains(platform.Fix, "certified platform") {
		t.Errorf("the fix %q does not say what to do", platform.Fix)
	}
	if resp.Leaders[0].State != store.AgentStopped {
		t.Errorf("Coordination's leader is %s on an uncertified platform", resp.Leaders[0].State)
	}
	if _, err := os.Stat(filepath.Join(testDir, "credential")); err == nil {
		t.Error("an agent started on an uncertified platform")
	}
}

// A Claude Code that is not the pinned version is refused before any
// dispatch: no agent starts on it, and it is run for nothing but its version.
// Once the pinned copy is at its path the same factory starts.
func TestAClaudeCodeThatIsNotThePinnedVersionIsRefusedBeforeAnyDispatch(t *testing.T) {
	p, testDir, claude := certificationFactory(t, Config{}, standInOtherClaude)

	resp, err := Call(p.Socket, Request{Command: CommandStart})
	if err != nil {
		t.Fatal(err)
	}
	version, ok := checkNamed(resp.Checks, "Claude Code version")
	if !ok || version.OK {
		t.Fatalf("with Claude Code 2.1.300 at the pinned path the version check is %+v, want it failed", version)
	}
	for _, want := range []string{"2.1.300", "2.1.292", "before any agent runs"} {
		if !strings.Contains(version.Problem, want) {
			t.Errorf("the problem %q does not say %q", version.Problem, want)
		}
	}
	if !strings.Contains(version.Fix, "asmai providers install") {
		t.Errorf("the fix %q does not say what to do", version.Fix)
	}
	if _, probed := checkNamed(resp.Checks, PermissionRulesName); probed {
		t.Error("the refused copy was run to probe its permission rules")
	}
	if resp.Leaders[0].State != store.AgentStopped {
		t.Errorf("Coordination's leader is %s on a Claude Code that is not the pin", resp.Leaders[0].State)
	}
	if _, err := os.Stat(filepath.Join(testDir, "credential")); err == nil {
		t.Error("an agent started on a Claude Code that is not the pinned version")
	}

	replaceClaude(t, claude, standInClaude)
	if resp, err = Call(p.Socket, Request{Command: CommandStart}); err != nil {
		t.Fatal(err)
	}
	if failed(resp.Checks) || resp.Leaders[0].State != store.AgentRunning {
		t.Errorf("with the pinned copy back, start answered %+v, %+v, want Coordination's leader running", resp.Checks, resp.Leaders)
	}
}

// A provider CLI that updated itself under a running agent does not stop it,
// but the agent is held at its next restart: the daemon does not start it on
// a version outside the qualification record, and says why in its status,
// until the pinned copy is back.
func TestAProviderThatUpdatedItselfHoldsItsAgentAtTheNextRestart(t *testing.T) {
	p, testDir, claude := certificationFactory(t, Config{}, standInClaude)

	resp, err := Call(p.Socket, Request{Command: CommandStart})
	if err != nil {
		t.Fatal(err)
	}
	if failed(resp.Checks) || resp.Leaders[0].State != store.AgentRunning {
		t.Fatalf("start answered %+v, %+v, want Coordination's leader running", resp.Checks, resp.Leaders)
	}
	running := resp.Leaders[0]

	// The provider updates itself while the agent runs, then its session
	// ends on its own.
	waitForFile(t, filepath.Join(testDir, "credential"))
	replaceClaude(t, claude, standInOtherClaude)
	if err := os.Remove(filepath.Join(testDir, "credential")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(running.PID, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}

	var held Check
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		status, err := Call(p.Socket, Request{Command: CommandStatus})
		if err != nil {
			t.Fatal(err)
		}
		if c, ok := checkNamed(status.Checks, "Claude Code version"); ok && !c.OK {
			held = c
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if held.OK || !strings.Contains(held.Problem, "2.1.300") {
		t.Fatalf("after the restart the status shows %+v, want the version check failed for 2.1.300", held)
	}
	status, err := Call(p.Socket, Request{Command: CommandStatus})
	if err != nil {
		t.Fatal(err)
	}
	if status.Leaders[0].State != store.AgentStopped {
		t.Errorf("the leader is %s, want it held", status.Leaders[0].State)
	}
	if _, err := os.Stat(filepath.Join(testDir, "credential")); err == nil {
		t.Error("the agent was restarted on a provider that updated itself")
	}

	replaceClaude(t, claude, standInClaude)
	if resp, err = Call(p.Socket, Request{Command: CommandStart}); err != nil {
		t.Fatal(err)
	}
	if failed(resp.Checks) || resp.Leaders[0].State != store.AgentRunning || resp.Leaders[0].Generation != running.Generation+1 {
		t.Errorf("with the pinned copy back, start answered %+v, %+v, want the leader running in a new generation", resp.Checks, resp.Leaders)
	}
}
