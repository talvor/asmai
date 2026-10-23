// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bufio"
	"context"
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
	"github.com/talvor/asmai/internal/repos"
	"github.com/talvor/asmai/internal/store"
	"github.com/talvor/asmai/internal/vt"
)

// standInClaude stands in for Claude Code: it hands the test its session's
// credential, draws a line and waits to be stopped.
const standInClaude = `#!/bin/sh
if [ "$1" = --version ]; then
	echo "2.1.292 (Claude Code)"
	exit 0
fi
if [ "$1" = auth ]; then
	echo '{"loggedIn":true,"authMethod":"claude.ai"}'
	exit 0
fi
if [ "$1" = -p ]; then
	# The permission rules probe: answer with what the test wrote.
	cat > /dev/null
	cat "$ASMAI_TEST_DIR/rules-answer"
	exit 0
fi
printf '%s' "$ASMAI_SESSION" > "$ASMAI_TEST_DIR/credential.part" && mv "$ASMAI_TEST_DIR/credential.part" "$ASMAI_TEST_DIR/credential"
printf 'leader up\r\n'
exec sleep 60
`

// writeRulesAnswer makes the stand-in Claude Code answer the permission
// rules probe as Claude Code does, listing the rules AsmAI gives it as in
// effect, or, under a managed policy that allows managed rules only, as not
// in effect.
func writeRulesAnswer(t *testing.T, managedOnly bool) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(os.Getenv("ASMAI_TEST_DIR"), "rules-answer"), []byte(rulesAnswer(t, managedOnly)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// rulesAnswer returns Claude Code's answer to the permission rules probe,
// as writeRulesAnswer describes it.
func rulesAnswer(t *testing.T, managedOnly bool) string {
	t.Helper()
	type rule struct {
		Behavior    string `json:"behavior"`
		Source      string `json:"source"`
		Rule        string `json:"rule"`
		NotInEffect bool   `json:"notInEffect,omitempty"`
	}
	var rules []rule
	for _, r := range providers.CommandRules() {
		rules = append(rules, rule{Behavior: "allow", Source: "flagSettings", Rule: r, NotInEffect: managedOnly})
	}
	answer, err := json.Marshal(map[string]any{"type": "control_response", "response": map[string]any{
		"subtype": "success", "request_id": "asmai-permission-rules",
		"response": map[string]any{"state": map[string]any{"rules": rules, "managedOnly": managedOnly}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return string(answer)
}

// answersTheRulesProbe is the start of a stand-in Claude Code's script that
// answers the permission rules probe, as Claude Code does with no policy,
// followed by any further answers, one line each.
func answersTheRulesProbe(t *testing.T, further ...string) string {
	t.Helper()
	answers := append([]string{rulesAnswer(t, false)}, further...)
	return "if [ \"$1\" = auth ]; then echo '{\"loggedIn\":true,\"authMethod\":\"claude.ai\"}'; exit 0; fi\n" +
		"if [ \"$1\" = -p ]; then cat > /dev/null; printf '%s\\n' '" + strings.Join(answers, "'\n printf '%s\\n' '") + "'; exit 0; fi\n"
}

// autoModeAnswer returns Claude Code's answer to the probe's request to
// switch a session to auto mode: it does, or refuses with why.
func autoModeAnswer(t *testing.T, refusal string) string {
	t.Helper()
	response := map[string]any{"subtype": "success", "request_id": "asmai-auto-mode", "response": map[string]any{}}
	if refusal != "" {
		response = map[string]any{"subtype": "error", "request_id": "asmai-auto-mode", "error": refusal}
	}
	answer, err := json.Marshal(map[string]any{"type": "control_response", "response": response})
	if err != nil {
		t.Fatal(err)
	}
	return string(answer)
}

// disabledAutoModeAnswer returns Claude Code's answer to the probe's request
// for the effective settings, as an administrator's managed settings that set
// permissions.disableAutoMode to "disable" make them.
func disabledAutoModeAnswer(t *testing.T) string {
	t.Helper()
	answer, err := json.Marshal(map[string]any{"type": "control_response", "response": map[string]any{
		"subtype": "success", "request_id": "asmai-effective-settings",
		"response": map[string]any{"effective": map[string]any{"permissions": map[string]any{"disableAutoMode": "disable"}}, "sources": []any{}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return string(answer)
}

// testPins pins version of Claude Code for this platform, which the daemon's
// checks certify, with no download: the tests install their own stand-ins.
func testPins(version string) providers.Pins {
	platform, _ := providers.Platform(runtime.GOOS, runtime.GOARCH)
	return providers.Pins{ClaudeCode: providers.Pin{Version: version, Platforms: map[string]providers.Download{platform: {}}}}
}

// reportsVersion is a Config.ClaudeVersion for tests whose stand-in Claude
// Code is not a program that answers --version.
func reportsVersion(version string) func(context.Context, string) (string, error) {
	return func(context.Context, string) (string, error) { return version, nil }
}

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
	writeRulesAnswer(t, false)
	p := stateDir(t)
	configFile := filepath.Join(testDir, "config.toml")
	pins := testPins("2.1.292")
	stopped, cancel := runDaemonWith(t, Config{Paths: p, Version: "v1.2.3", ConfigFile: configFile, Pins: pins})

	// Without staffing or the pinned Claude Code, the checks fail and no
	// leader starts.
	resp, err := Call(p.Socket, Request{Command: CommandStart})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Checks) != 4 || !resp.Checks[0].OK || resp.Checks[1].OK || resp.Checks[2].OK || !resp.Checks[3].OK || resp.Leaders[0].State != store.AgentStopped {
		t.Fatalf("starting an unready factory answered %+v, %+v, want the staffing and Claude Code checks failed and the leaders stopped", resp.Checks, resp.Leaders)
	}

	staffing := "[roles.%s]\nleader_provider = \"claude\"\nleader_model = \"opus\"\nworker_provider = \"claude\"\nworker_model = \"opus\"\n"
	var config strings.Builder
	for _, role := range []string{"coordination", "planning", "research", "engineering", "quality"} {
		config.WriteString(strings.ReplaceAll(staffing, "%s", role))
	}
	if err := os.WriteFile(configFile, []byte(config.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if resp, err := Call(p.Socket, Request{Command: CommandStart}); err != nil || !slices.ContainsFunc(resp.Checks, func(c Check) bool { return c.Name == "staffing" && strings.Contains(c.Fix, "asmai config apply") }) {
		t.Fatalf("starting before applying staffing returned %+v, %v", resp.Checks, err)
	}
	if resp, err := applyConfig(t, p.Socket, config.String(), true); err != nil || !resp.ConfigApply.Applied {
		t.Fatalf("applying staffing returned %+v, %v", resp.ConfigApply, err)
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
	// Coordination's native prompts are the user's to answer, in the
	// conversation, so none is reported as one nobody can answer.
	if _, err := Call(p.Socket, Request{Command: CommandHook, Session: string(credential), Payload: json.RawMessage(`{"hook_event_name": "PermissionRequest", "tool_name": "Bash", "tool_input": {"command": "rm -rf build"}}`)}); err != nil {
		t.Errorf("a permission request was refused: %v", err)
	}
	if status, err := Call(p.Socket, Request{Command: CommandStatus}); err != nil || len(status.Prompts) != 0 {
		t.Errorf("the status shows the prompts %+v (%v), want none for Coordination's", status.Prompts, err)
	}
	if snapshot, err := Call(p.Socket, Request{Command: CommandDashboardSnapshot}); err != nil || snapshot.Dashboard == nil || len(snapshot.Dashboard.Waiting) != 1 || snapshot.Dashboard.Waiting[0].Kind != WaitingNativePrompt || snapshot.Dashboard.Waiting[0].Agent != "leader@coordination" {
		t.Errorf("the snapshot missed Coordination's native prompt: %+v (%v)", snapshot.Dashboard, err)
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
	if first.Prompts == nil || len(*first.Prompts) != 0 {
		t.Errorf("the first frame carries the prompts %v, want the status line told there are none", first.Prompts)
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
	want := "daemon.started configuration.applied provider.installed session.started observation observation session.ended daemon.stopped"
	if strings.Join(kinds, " ") != want {
		t.Errorf("the journal holds\n%s\nwant %s", strings.Join(got, "\n"), want)
	}
	if !strings.Contains(got[6], `"by":"signal"`) {
		t.Errorf("the session's end is journaled as %s, want it ended by the signal that stopped the daemon", got[6])
	}
	// The session's guard lets it reach this daemon's socket, in this state
	// directory, and no other.
	var started struct {
		Args []string `json:"args"`
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(got[3], store.KindSessionStarted+" ")), &started); err != nil || len(started.Args) < 2 || started.Args[0] != "--settings" {
		t.Fatalf("the session's start is journaled as %s (%v), want its command line, --settings first", got[3], err)
	}
	var settings struct {
		Sandbox struct {
			Network map[string][]string `json:"network"`
		} `json:"sandbox"`
	}
	if err := json.Unmarshal([]byte(started.Args[1]), &settings); err != nil {
		t.Fatal(err)
	}
	// The guard names the socket by the path it resolves to, as macOS's
	// temporary directory is under a symlink.
	stateDir, err := filepath.EvalSymlinks(filepath.Dir(p.Socket))
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string][]string{"allowUnixSockets": {filepath.Join(stateDir, filepath.Base(p.Socket))}}; len(settings.Sandbox.Network) != 1 || !slices.Equal(settings.Sandbox.Network["allowUnixSockets"], want["allowUnixSockets"]) {
		t.Errorf("the session's sandbox network settings are %v, want %v", settings.Sandbox.Network, want)
	}
}

// readyConfig staffs every role in a configuration file in dir, and returns
// its path.
func readyConfig(t *testing.T, dir string) string {
	t.Helper()
	var config strings.Builder
	for _, role := range []string{"coordination", "planning", "research", "engineering", "quality"} {
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
// checks fail naming `asmai init`, and no agent starts, until the record
// holds the user's trust of both folders. The daemon only reads the record.
func TestTheDaemonStartsNoAgentUntilTheUserTrustsTheFoldersAgentsStartIn(t *testing.T) {
	testDir := t.TempDir()
	t.Setenv("ASMAI_TEST_DIR", testDir)
	writeRulesAnswer(t, false)
	p := stateDir(t)
	record := filepath.Join(testDir, ".claude.json")
	untrusted := []byte(`{"projects": {"` + p.Agents + `": {"hasTrustDialogAccepted": true}, "` + p.Workspaces + `": {"hasTrustDialogAccepted": false}}}`)
	if err := os.WriteFile(record, untrusted, 0o600); err != nil {
		t.Fatal(err)
	}
	pins := testPins("2.1.292")
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
	if trust.OK || !strings.Contains(trust.Problem, p.Workspaces) || strings.Contains(trust.Problem, p.Agents+" ") || !strings.Contains(trust.Fix, "asmai init") {
		t.Errorf("with the workspaces folder untrusted, the trust check is %+v, want it failed naming the folder and `asmai init`", trust)
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
	if err == nil || !strings.Contains(err.Error(), "does not trust") || !strings.Contains(err.Error(), "asmai init") {
		t.Errorf("starting an agent in an untrusted workspace returned %v, want a refusal naming `asmai init`", err)
	}
	d.cfg.ClaudeRecord = ""
	if err := d.startSession(&leader{}, store.ProviderInstall{Path: "/bin/false"}, launch{dir: filepath.Join(p.Agents, "leader@engineering")}); err == nil {
		t.Error("a daemon that reads no record of Claude Code's started an agent")
	}
}

// A worker's workspace is a worktree of AsmAI's clone, which Claude Code
// trusts only through the clone's own trust, not the workspaces folder's:
// with the clone untrusted, the checks fail and the worker is refused, each
// naming the clone and `asmai init`.
func TestTheDaemonRefusesAWorkerWhoseClonesTrustIsMissing(t *testing.T) {
	p := stateDir(t)
	clone := filepath.Join(p.Repositories, "fixture")
	if err := os.MkdirAll(p.Repositories, 0o700); err != nil {
		t.Fatal(err)
	}
	git(t, p.Repositories, "clone", "--quiet", remote(t), clone)
	workspace := filepath.Join(p.Workspaces, "job-1", "assignment-1", "repo")
	if err := repos.AddWorkspace(context.Background(), clone, workspace, "asmai/job-1", "HEAD"); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(p.Store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.RepositoryAdded(store.Repository{Name: "fixture", Origin: "/origin", DefaultBranch: "trunk", Clone: clone, AddedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(t.TempDir(), ".claude.json")
	untrusted := []byte(`{"projects": {"` + p.Agents + `": {"hasTrustDialogAccepted": true}, "` + p.Workspaces + `": {"hasTrustDialogAccepted": true}, "` + p.Repositories + `": {"hasTrustDialogAccepted": true}}}`)
	if err := os.WriteFile(record, untrusted, 0o600); err != nil {
		t.Fatal(err)
	}
	d := &daemon{cfg: Config{Paths: p, ClaudeRecord: record}, store: s}

	if c := d.trustCheck(); c.OK || !strings.Contains(c.Problem, "does not trust "+clone) || !strings.Contains(c.Fix, "asmai init") {
		t.Errorf("with the clone untrusted, the trust check is %+v, want it failed naming %s and `asmai init`", c, clone)
	}
	// git names the clone by its resolved path in the worktree's files.
	resolved, err := filepath.EvalSymlinks(clone)
	if err != nil {
		t.Fatal(err)
	}
	err = d.startSession(&leader{}, store.ProviderInstall{Path: "/bin/false"}, launch{dir: workspace})
	if err == nil || !strings.Contains(err.Error(), "does not trust "+resolved+", the repository "+workspace+" is checked out from") || !strings.Contains(err.Error(), "asmai init") {
		t.Errorf("starting a worker in a worktree of the untrusted clone returned %v, want a refusal naming the clone and `asmai init`", err)
	}

	trustRecord := []byte(`{"projects": {"` + p.Agents + `": {"hasTrustDialogAccepted": true}, "` + p.Workspaces + `": {"hasTrustDialogAccepted": true}, "` + clone + `": {"hasTrustDialogAccepted": true}}}`)
	if err := os.WriteFile(record, trustRecord, 0o600); err != nil {
		t.Fatal(err)
	}
	if c := d.trustCheck(); !c.OK {
		t.Errorf("with the clone trusted, the trust check is %+v", c)
	}
	if err := trusted(d.cfg.ClaudeRecord, workspace); err != nil {
		t.Errorf("with the clone trusted, its worktree is %v", err)
	}
}

// The pinned Claude Code at its path, matching the pin, is installed though
// no install records it, as when `asmai init` fetched it with the daemon
// stopped: the daemon records it as it checks.
func TestTheDaemonRecordsThePinnedCopyItFindsAtItsPath(t *testing.T) {
	platform, ok := providers.Platform(runtime.GOOS, runtime.GOARCH)
	if !ok {
		t.Skipf("Claude Code is not published for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	testDir := t.TempDir()
	t.Setenv("ASMAI_TEST_DIR", testDir)
	writeRulesAnswer(t, false)
	p := stateDir(t)
	sum := sha256.Sum256([]byte(standInClaude))
	pins := providers.Pins{ClaudeCode: providers.Pin{Version: "2.1.292", Channel: "https://example.com", Platforms: map[string]providers.Download{
		platform: {SHA256: hex.EncodeToString(sum[:]), Size: int64(len(standInClaude))},
	}}}
	runDaemonWith(t, Config{Paths: p, Version: "v1.2.3", ConfigFile: readyConfig(t, testDir), Pins: pins})
	claude := filepath.Join(p.Providers, "claude-code", "2.1.292", "claude")
	os.MkdirAll(filepath.Dir(claude), 0o700)
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

// Under a managed policy that allows managed permission rules only, Claude
// Code ignores the rules AsmAI gives its agents, which would stop at their
// first command that needs approval. The daemon asks Claude Code, and starts
// no agent, naming the policy and the fix; once the account allows the rules,
// the same factory starts.
func TestTheDaemonStartsNoAgentUnderAManagedPolicyThatIgnoresAsmaisPermissionRules(t *testing.T) {
	testDir := t.TempDir()
	t.Setenv("ASMAI_TEST_DIR", testDir)
	writeRulesAnswer(t, true)
	p := stateDir(t)
	record := filepath.Join(testDir, ".claude.json")
	if err := os.WriteFile(record, []byte(`{"projects": {"`+p.Dir+`": {"hasTrustDialogAccepted": true}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	pins := testPins("2.1.292")
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
	var rules Check
	for _, c := range resp.Checks {
		if c.Name == PermissionRulesName {
			rules = c
		}
	}
	for _, want := range []string{"allowManagedPermissionRulesOnly", "cannot run unprompted", "Bash(asmai *)", "sandbox.autoAllowBashIfSandboxed: true"} {
		if rules.OK || !strings.Contains(rules.Problem+rules.Fix, want) {
			t.Errorf("under the policy, the permission rules check is %+v, want it failed and naming %s", rules, want)
		}
	}
	if !strings.Contains(rules.Fix, "administrator") || !strings.Contains(rules.Fix, "without that policy") {
		t.Errorf("the fix is %q, want both: another account, or an administrator adding the allow rule", rules.Fix)
	}
	if resp.Leaders[0].State != store.AgentStopped {
		t.Errorf("under the policy, Coordination's leader is %s, want stopped", resp.Leaders[0].State)
	}
	if _, err := os.Stat(filepath.Join(testDir, "credential")); err == nil {
		t.Error("an agent started under a policy that ignores AsmAI's permission rules")
	}

	writeRulesAnswer(t, false)
	if resp, err = Call(p.Socket, Request{Command: CommandStart}); err != nil {
		t.Fatal(err)
	}
	if failed(resp.Checks) || resp.Leaders[0].State != store.AgentRunning {
		t.Errorf("without the policy, start answered %+v, %+v, want Coordination's leader running", resp.Checks, resp.Leaders)
	}
}
