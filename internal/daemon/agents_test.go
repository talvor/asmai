// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
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
	if len(resp.Checks) != 2 || resp.Checks[0].OK || resp.Checks[1].OK || resp.Leaders[0].State != store.AgentStopped {
		t.Fatalf("starting an unready factory answered %+v, %+v, want both checks failed and the leaders stopped", resp.Checks, resp.Leaders)
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
}
