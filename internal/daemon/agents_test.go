// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/providers"
	"github.com/talvor/asmai/internal/store"
	"github.com/talvor/asmai/internal/vt"
)

// standInClaude stands in for Claude Code: it hands the test its session's
// credential, draws a line and waits to be stopped.
const standInClaude = `#!/bin/sh
printf '%s' "$ASMAI_SESSION" > "$ASMAI_TEST_DIR/credential.part" && mv "$ASMAI_TEST_DIR/credential.part" "$ASMAI_TEST_DIR/credential"
printf 'leader up\r\n'
exec sleep 60
`

func readFrame(t *testing.T, r *bufio.Reader) Frame {
	t.Helper()
	line, err := r.ReadBytes('\n')
	if err != nil {
		t.Fatalf("reading a frame: %v", err)
	}
	var f Frame
	if err := json.Unmarshal(line, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestTheDaemonRunsCoordinationsLeaderOnceItsChecksPass(t *testing.T) {
	testDir := t.TempDir()
	t.Setenv("ASMAI_TEST_DIR", testDir)
	p := stateDir(t)
	configFile := filepath.Join(testDir, "config.toml")
	pins := providers.Pins{ClaudeCode: providers.Pin{Version: "2.1.292"}}
	stopped, cancel := runDaemonWith(t, Config{Paths: p, Version: "v1.2.3", ConfigFile: configFile, Pins: pins})

	// Without staffing or the pinned Claude Code, the checks fail and no
	// leader starts.
	resp, err := Call(p.Socket, Request{Command: CommandStart})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Checks) != 3 || resp.Checks[0].OK || resp.Checks[1].OK || !resp.Checks[2].OK || resp.Leaders[0].State != store.AgentStopped {
		t.Fatalf("starting an unready factory answered %+v, %+v, want the staffing and Claude Code checks failed and the leaders stopped", resp.Checks, resp.Leaders)
	}

	staffing := "[roles.%s]\nleader_provider = \"claude\"\nleader_model = \"opus\"\nworker_provider = \"claude\"\nworker_model = \"opus\"\n"
	var config strings.Builder
	for _, role := range []string{"coordination", "engineering", "quality"} {
		config.WriteString(strings.ReplaceAll(staffing, "%s", role))
	}
	if err := os.WriteFile(configFile, []byte(config.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	claude := filepath.Join(p.Providers, "claude-code", "2.1.292", "claude")
	os.MkdirAll(filepath.Dir(claude), 0o700)
	if err := os.WriteFile(claude, []byte(standInClaude), 0o700); err != nil {
		t.Fatal(err)
	}
	install := store.ProviderInstall{Name: "claude-code", Version: "2.1.292", Path: claude, SHA256: strings.Repeat("a", 64)}
	if _, err := Call(p.Socket, Request{Command: CommandProviderInstalled, Provider: &install}); err != nil {
		t.Fatal(err)
	}
	resp, err = Call(p.Socket, Request{Command: CommandStart})
	if err != nil {
		t.Fatal(err)
	}
	if failed(resp.Checks) || resp.Leaders[0] != (Leader{Agent: "leader@coordination", State: store.AgentRunning, Generation: 1, PID: resp.Leaders[0].PID, Input: InputAutomation}) {
		t.Fatalf("starting a ready factory answered %+v, %+v, want leader@coordination running", resp.Checks, resp.Leaders)
	}

	// A hook journals an observation of the session its credential names.
	var credential []byte
	for deadline := time.Now().Add(10 * time.Second); len(credential) == 0; time.Sleep(10 * time.Millisecond) {
		credential, _ = os.ReadFile(filepath.Join(testDir, "credential"))
		if time.Now().After(deadline) {
			t.Fatal("the leader did not start")
		}
	}
	if _, err := Call(p.Socket, Request{Command: CommandHook, Session: string(credential), Payload: json.RawMessage(`{"hook_event_name": "Stop"}`)}); err != nil {
		t.Errorf("a hook was refused: %v", err)
	}
	for name, req := range map[string]Request{
		"another credential": {Command: CommandHook, Session: "not-a-session", Payload: json.RawMessage(`{"hook_event_name": "Stop"}`)},
		"no event":           {Command: CommandHook, Session: string(credential), Payload: json.RawMessage(`{}`)},
		"not an object":      {Command: CommandHook, Session: string(credential), Payload: json.RawMessage(`[1]`)},
	} {
		if _, err := Call(p.Socket, req); err == nil {
			t.Errorf("a hook with %s was journaled", name)
		}
	}

	// Attaching streams the terminal at the client's size, and follows a
	// resize; nothing the client sends but its size reaches the agent.
	conn, r, resp, err := Open(p.Socket, Request{Command: CommandAttach, Agent: "coordination", Columns: 50, Rows: 10})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if resp.Attached == nil || resp.Attached.Agent != "leader@coordination" || resp.Attached.Generation != 1 {
		t.Errorf("attach answered %+v, want generation 1 of leader@coordination", resp.Attached)
	}
	first := readFrame(t, r)
	if first.State == nil || first.State.Columns != 50 || first.State.Rows != 10 {
		t.Fatalf("the first frame is %+v, want the 50 by 10 terminal's state", first)
	}
	if term, _ := vt.FromState(*first.State); !strings.Contains(term.String(), "leader up") {
		t.Errorf("the attached screen shows %q", term.String())
	}
	conn.Write([]byte("typed keys\n{\"columns\": 40, \"rows\": 8}\n"))
	if resized := readFrame(t, r); resized.State == nil || resized.State.Columns != 40 || resized.State.Rows != 8 {
		t.Errorf("after a resize, the frame is %+v, want the 40 by 8 terminal's state", resized)
	}
	snapshot, err := Call(p.Socket, Request{Command: CommandAttach, Agent: "leader@coordination", Snapshot: true})
	if err != nil || snapshot.Screen == nil || snapshot.Screen.Columns != 40 {
		t.Errorf("a snapshot answered %+v (%v), want the 40-column screen", snapshot.Screen, err)
	}

	agents, err := Call(p.Socket, Request{Command: CommandAgents})
	if err != nil || len(agents.Agents) != 1 || agents.Agents[0].State != store.AgentRunning {
		t.Errorf("the daemon lists agents %+v (%v), want the leader running", agents.Agents, err)
	}

	// Stopping the daemon ends the session first, and the attach stream with
	// it.
	cancel()
	if err := stopped(); err != nil {
		t.Fatal(err)
	}
	for {
		f := readFrame(t, r)
		if f.Ended != "" {
			break
		}
	}
	got := journal(t, p)
	var kinds []string
	for _, e := range got {
		kinds = append(kinds, strings.Fields(e)[0])
	}
	want := "daemon.started provider.installed session.started observation session.ended daemon.stopped"
	if strings.Join(kinds, " ") != want {
		t.Errorf("the journal holds\n%s\nwant %s", strings.Join(got, "\n"), want)
	}
	if !strings.Contains(got[4], `"by":"signal"`) {
		t.Errorf("the session's end is journaled as %s, want it ended by the signal that stopped the daemon", got[4])
	}
	// The session's guard lets it reach this daemon's socket, in this state
	// directory, and no other.
	var started struct {
		Args []string `json:"args"`
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(got[2], store.KindSessionStarted+" ")), &started); err != nil || len(started.Args) < 2 || started.Args[0] != "--settings" {
		t.Fatalf("the session's start is journaled as %s (%v), want its command line, --settings first", got[2], err)
	}
	var settings struct {
		Sandbox struct {
			Network map[string][]string `json:"network"`
		} `json:"sandbox"`
	}
	if err := json.Unmarshal([]byte(started.Args[1]), &settings); err != nil {
		t.Fatal(err)
	}
	if want := map[string][]string{"allowUnixSockets": {p.Socket}}; len(settings.Sandbox.Network) != 1 || !slices.Equal(settings.Sandbox.Network["allowUnixSockets"], want["allowUnixSockets"]) {
		t.Errorf("the session's sandbox network settings are %v, want %v", settings.Sandbox.Network, want)
	}
}

// readyConfig staffs every role in a configuration file in dir, and returns
// its path.
func readyConfig(t *testing.T, dir string) string {
	t.Helper()
	var config strings.Builder
	for _, role := range []string{"coordination", "engineering", "quality"} {
		config.WriteString("[roles." + role + "]\nleader_provider = \"claude\"\nleader_model = \"opus\"\nworker_provider = \"claude\"\nworker_model = \"opus\"\n")
	}
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(config.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// An agent started in a folder Claude Code does not trust would wait at its
// question about trusting the folder, which only the user can answer: the
// checks fail naming `asmai setup`, and no agent starts, until the record
// holds the user's trust of both folders. The daemon only reads the record.
func TestTheDaemonStartsNoAgentUntilTheUserTrustsTheFoldersAgentsStartIn(t *testing.T) {
	testDir := t.TempDir()
	t.Setenv("ASMAI_TEST_DIR", testDir)
	p := stateDir(t)
	record := filepath.Join(testDir, ".claude.json")
	untrusted := []byte(`{"projects": {"` + p.Agents + `": {"hasTrustDialogAccepted": true}, "` + p.Workspaces + `": {"hasTrustDialogAccepted": false}}}`)
	if err := os.WriteFile(record, untrusted, 0o600); err != nil {
		t.Fatal(err)
	}
	pins := providers.Pins{ClaudeCode: providers.Pin{Version: "2.1.292"}}
	runDaemonWith(t, Config{Paths: p, Version: "v1.2.3", ConfigFile: readyConfig(t, testDir), Pins: pins, ClaudeRecord: record})
	claude := filepath.Join(p.Providers, "claude-code", "2.1.292", "claude")
	os.MkdirAll(filepath.Dir(claude), 0o700)
	if err := os.WriteFile(claude, []byte(standInClaude), 0o700); err != nil {
		t.Fatal(err)
	}
	install := store.ProviderInstall{Name: "claude-code", Version: "2.1.292", Path: claude, SHA256: strings.Repeat("a", 64)}
	if _, err := Call(p.Socket, Request{Command: CommandProviderInstalled, Provider: &install}); err != nil {
		t.Fatal(err)
	}

	resp, err := Call(p.Socket, Request{Command: CommandStart})
	if err != nil {
		t.Fatal(err)
	}
	var trust Check
	for _, c := range resp.Checks {
		if c.Name == "directory trust" {
			trust = c
		}
	}
	if trust.OK || !strings.Contains(trust.Problem, p.Workspaces) || strings.Contains(trust.Problem, p.Agents+" ") || !strings.Contains(trust.Fix, "asmai setup") {
		t.Errorf("with the workspaces folder untrusted, the trust check is %+v, want it failed naming the folder and `asmai setup`", trust)
	}
	if resp.Leaders[0].State != store.AgentStopped {
		t.Errorf("with a folder untrusted, Coordination's leader is %s", resp.Leaders[0].State)
	}
	if _, err := os.Stat(filepath.Join(testDir, "credential")); err == nil {
		t.Error("an agent started in an untrusted factory")
	}
	if after, _ := os.ReadFile(record); string(after) != string(untrusted) {
		t.Errorf("the daemon changed Claude Code's record to %s", after)
	}

	trusted := []byte(`{"projects": {"` + p.Dir + `": {"hasTrustDialogAccepted": true}}}`)
	if err := os.WriteFile(record, trusted, 0o600); err != nil {
		t.Fatal(err)
	}
	if resp, err = Call(p.Socket, Request{Command: CommandStart}); err != nil {
		t.Fatal(err)
	}
	if failed(resp.Checks) || resp.Leaders[0].State != store.AgentRunning {
		t.Errorf("with the folders trusted, start answered %+v, %+v, want Coordination's leader running", resp.Checks, resp.Leaders)
	}
	if after, _ := os.ReadFile(record); string(after) != string(trusted) {
		t.Errorf("the daemon changed Claude Code's record to %s", after)
	}
}

// The daemon refuses to start an agent in a folder Claude Code does not
// trust, rather than leave it at the question, even once the checks passed.
func TestTheDaemonRefusesToLaunchAnAgentWhoseFolderIsNotTrusted(t *testing.T) {
	p := stateDir(t)
	record := filepath.Join(t.TempDir(), ".claude.json")
	if err := os.WriteFile(record, []byte(`{"projects": {"`+p.Agents+`": {"hasTrustDialogAccepted": true}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	d := &daemon{cfg: Config{Paths: p, ClaudeRecord: record}}
	err := d.startSession(&leader{}, store.ProviderInstall{Path: "/bin/false"}, launch{dir: filepath.Join(p.Workspaces, "job-1", "assignment-1", "repo")})
	if err == nil || !strings.Contains(err.Error(), "does not trust") || !strings.Contains(err.Error(), "asmai setup") {
		t.Errorf("starting an agent in an untrusted workspace returned %v, want a refusal naming `asmai setup`", err)
	}
	d.cfg.ClaudeRecord = ""
	if err := d.startSession(&leader{}, store.ProviderInstall{Path: "/bin/false"}, launch{dir: filepath.Join(p.Agents, "leader@engineering")}); err == nil {
		t.Error("a daemon that reads no record of Claude Code's started an agent")
	}
}

// The pinned Claude Code at its path, matching the pin, is installed though
// no install records it, as when `asmai setup` fetched it with the daemon
// stopped: the daemon records it as it checks.
func TestTheDaemonRecordsThePinnedCopyItFindsAtItsPath(t *testing.T) {
	platform, ok := providers.Platform(runtime.GOOS, runtime.GOARCH)
	if !ok {
		t.Skipf("Claude Code is not published for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	testDir := t.TempDir()
	t.Setenv("ASMAI_TEST_DIR", testDir)
	p := stateDir(t)
	sum := sha256.Sum256([]byte(standInClaude))
	pins := providers.Pins{ClaudeCode: providers.Pin{Version: "2.1.292", Channel: "https://example.com", Platforms: map[string]providers.Download{
		platform: {SHA256: hex.EncodeToString(sum[:]), Size: int64(len(standInClaude))},
	}}}
	runDaemonWith(t, Config{Paths: p, Version: "v1.2.3", ConfigFile: readyConfig(t, testDir), Pins: pins})
	claude := filepath.Join(p.Providers, "claude-code", "2.1.292", "claude")
	os.MkdirAll(filepath.Dir(claude), 0o700)
	if err := os.WriteFile(claude, []byte(standInClaude+"# not the pinned download\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if resp, err := Call(p.Socket, Request{Command: CommandStart}); err != nil || !failed(resp.Checks) {
		t.Fatalf("with a copy that does not match the pin, start answered %+v (%v), want a failed check", resp.Checks, err)
	}

	if err := os.WriteFile(claude, []byte(standInClaude), 0o700); err != nil {
		t.Fatal(err)
	}
	resp, err := Call(p.Socket, Request{Command: CommandStart})
	if err != nil || failed(resp.Checks) || resp.Leaders[0].State != store.AgentRunning {
		t.Fatalf("with the pinned copy at its path, start answered %+v, %+v (%v), want Coordination's leader running", resp.Checks, resp.Leaders, err)
	}
	listed, err := Call(p.Socket, Request{Command: CommandProviders})
	if err != nil || len(listed.Providers) != 1 || listed.Providers[0].Path != claude || listed.Providers[0].SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("the daemon lists installs %+v (%v), want the pinned copy found at its path", listed.Providers, err)
	}
}
