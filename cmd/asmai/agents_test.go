// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/talvor/asmai/internal/fakeprovider"
	"github.com/talvor/asmai/internal/vt"
)

// staffing staffs every role M1 runs on Claude.
const staffing = `[roles.coordination]
leader_provider = "claude"
leader_model = "opus"
worker_provider = "claude"
worker_model = "sonnet"

[roles.engineering]
leader_provider = "claude"
leader_model = "opus"
worker_provider = "claude"
worker_model = "opus"

[roles.quality]
leader_provider = "claude"
leader_model = "opus"
worker_provider = "claude"
worker_model = "opus"
`

// idleLeader is a leader that draws a prompt and waits for input no test
// types.
var idleLeader = []string{`{"screen": "> "}`, `{"expect": "only typed by a test that means to"}`}

// writeConfig writes the configuration file in the test's home.
func writeConfig(t *testing.T, text string) {
	t.Helper()
	path := filepath.Join(os.Getenv("HOME"), ".config", "asmai", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// startUnready runs `asmai start` in a factory not ready to run agents: it
// starts the daemon, names the checks that failed and exits 1, leaving the
// daemon running so that what failed can be fixed. It returns what start
// printed.
func startUnready(t *testing.T) (stderr string) {
	t.Helper()
	_, stderr, code := runAsmai(t, "start")
	if code != 1 || !strings.Contains(stderr, "Started the factory's daemon, but not Coordination's leader") {
		t.Fatalf("asmai start exited %d printing %q, want 1 and the checks that failed", code, stderr)
	}
	var status struct {
		Daemon daemonJSON `json:"daemon"`
	}
	asmaiJSON(t, &status, "status")
	if !status.Daemon.Running {
		t.Fatal("after asmai start, the daemon is not running")
	}
	return stderr
}

// readyFactory gives the test a factory ready to run agents, whose pinned
// Claude Code is the fake provider playing script: it staffs the roles and
// installs the fake with `asmai providers install`. That needs the daemon,
// which it leaves running with no agent. The environment the test set
// before is the daemon's.
func readyFactory(t *testing.T, script []string) (stateDir string) {
	t.Helper()
	stateDir = factoryHome(t)
	writeConfig(t, staffing)
	path := filepath.Join(t.TempDir(), "leader.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(script, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(fakeprovider.ScriptEnv, path)
	if stderr := startUnready(t); !strings.Contains(stderr, "fix: run `asmai providers install`") || strings.Contains(stderr, "failed  staffing") {
		t.Fatalf("with the roles staffed, asmai start printed %q, want only the pinned Claude Code to fail", stderr)
	}
	fake, err := os.ReadFile(fakeProvider)
	if err != nil {
		t.Fatal(err)
	}
	standInChannelPinning(t, fake, fake)
	if stdout, stderr, code := install(t, "y\n"); code != 0 {
		t.Fatalf("installing the fake provider exited %d: %s%s", code, stdout, stderr)
	}
	return stateDir
}

type leaderJSON struct {
	Agent      string `json:"agent"`
	State      string `json:"state"`
	Generation int    `json:"generation"`
	PID        int    `json:"pid"`
}

type agentJSON struct {
	Agent      string `json:"agent"`
	Role       string `json:"role"`
	State      string `json:"state"`
	Generation int    `json:"generation"`
	Provider   string `json:"provider"`
	Version    string `json:"version"`
	Model      string `json:"model"`
	PID        int    `json:"pid"`
	Exit       string `json:"exit"`
}

func agentsListed(t *testing.T) []agentJSON {
	t.Helper()
	var list struct {
		Agents []agentJSON `json:"agents"`
	}
	asmaiJSON(t, &list, "agents")
	return list.Agents
}

type entryJSON struct {
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
}

func entries(t *testing.T, kind string) []json.RawMessage {
	t.Helper()
	var j struct {
		Journal []entryJSON `json:"journal"`
	}
	asmaiJSON(t, &j, "export")
	var data []json.RawMessage
	for _, e := range j.Journal {
		if e.Kind == kind {
			data = append(data, e.Data)
		}
	}
	return data
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); !cond(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// attachedTerminal is asmai running in a terminal of its own, such as
// `asmai attach` or the conversation, with what that terminal shows and its
// name.
type attachedTerminal struct {
	cmd  *exec.Cmd
	ptmx *os.File
	name string
	mu   sync.Mutex
	term *vt.Terminal
	done chan error
	read chan struct{} // closed once everything asmai wrote has reached term
}

func attachInTerminal(t *testing.T, agent string, columns, rows int) *attachedTerminal {
	t.Helper()
	return inTerminal(t, columns, rows, "attach", agent)
}

// inTerminal runs asmai with args in a new terminal of columns by rows, as
// the controlling terminal of a session of its own.
func inTerminal(t *testing.T, columns, rows int, args ...string) *attachedTerminal {
	t.Helper()
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := pty.Setsize(ptmx, &pty.Winsize{Cols: uint16(columns), Rows: uint16(rows)}); err != nil {
		t.Fatal(err)
	}
	a := &attachedTerminal{cmd: exec.Command(filepath.Join(asmaiBin, "asmai"), args...), ptmx: ptmx, name: tty.Name(), term: vt.New(columns, rows), done: make(chan error, 1), read: make(chan struct{})}
	a.cmd.Stdin, a.cmd.Stdout, a.cmd.Stderr = tty, tty, tty
	a.cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	err = a.cmd.Start()
	tty.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		a.cmd.Process.Kill()
		ptmx.Close()
	})
	go func() {
		defer close(a.read)
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			a.mu.Lock()
			a.term.Write(buf[:n])
			a.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	go func() { a.done <- a.cmd.Wait() }()
	return a
}

func (a *attachedTerminal) lines() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.term.Lines()
}

func (a *attachedTerminal) cell(x, y int) vt.Cell {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.term.Cell(x, y)
}

// leaderScript is Coordination's leader, played by the fake: it reports
// each lifecycle event M1 journals, records what its session was given,
// looks at the factory with asmai, draws its screen and waits.
var leaderScript = []string{
	`{"hook": "SessionStart", "payload": {"session_id": "fake-session", "hook_event_name": "SessionStart", "source": "startup", "transcript_path": "/tmp/fake-session.jsonl"}}`,
	`{"run": "env > \"$ASMAI_TEST_DIR/session.env\" && command -v asmai > \"$ASMAI_TEST_DIR/asmai.path\"", "status": 0}`,
	`{"run": "asmai status", "status": 0, "output": "(?m)^leader@coordination +running +input: automation$"}`,
	`{"screen": "\u001b[2J\u001b[H\u001b[1mCoordination\u001b[22m is ready\r\n\u001b[5;10Hplaced"}`,
	`{"hook": "UserPromptSubmit", "payload": {"session_id": "fake-session", "hook_event_name": "UserPromptSubmit", "prompt": "hello"}}`,
	`{"hook": "PermissionRequest", "payload": {"session_id": "fake-session", "hook_event_name": "PermissionRequest", "tool_name": "Bash", "tool_input": {"command": "rm -rf build"}}}`,
	`{"hook": "Stop", "payload": {"session_id": "fake-session", "hook_event_name": "Stop", "stop_hook_active": false}}`,
	`{"run": "touch \"$ASMAI_TEST_DIR/played\""}`,
	`{"expect": "only typed by a test that means to"}`,
}

func TestCoordinationsLeaderRunsInADaemonOwnedTerminalWithItsSettingsOnItsCommandLine(t *testing.T) {
	testDir := t.TempDir()
	t.Setenv("ASMAI_TEST_DIR", testDir)
	t.Setenv(fakeprovider.ArgsEnv, filepath.Join(testDir, "args.json"))
	// Whatever provider API keys the daemon's environment holds, no session
	// starts with them.
	apiKeys := []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "OPENAI_API_KEY", "CODEX_API_KEY"}
	for _, name := range apiKeys {
		t.Setenv(name, "not-a-real-key")
	}
	stateDir := readyFactory(t, leaderScript)
	fixedPath := filepath.Join(stateDir, "bin", "asmai")

	// asmai start runs the checks, which now pass, and starts Coordination's
	// leader.
	var started struct {
		Started bool         `json:"started"`
		Checks  []checkJSON  `json:"checks"`
		Leaders []leaderJSON `json:"leaders"`
	}
	asmaiJSON(t, &started, "start")
	if started.Started || len(started.Checks) != 2 || !started.Checks[0].OK || !started.Checks[1].OK {
		t.Errorf("asmai start printed %+v, want the running daemon with both checks passed", started)
	}
	if len(started.Leaders) != 3 || started.Leaders[0].Agent != "leader@coordination" || started.Leaders[0].State != "running" || started.Leaders[0].Generation != 1 {
		t.Fatalf("asmai start shows the leaders %+v, want leader@coordination running as generation 1", started.Leaders)
	}
	waitFor(t, "the leader to play its script", func() bool {
		_, err := os.Stat(filepath.Join(testDir, "played"))
		return err == nil
	})

	// The session's environment holds no provider API key, and runs the
	// daemon's copy of asmai at its fixed path.
	env, err := os.ReadFile(filepath.Join(testDir, "session.env"))
	if err != nil {
		t.Fatal(err)
	}
	var credential string
	for line := range strings.Lines(string(env)) {
		name, value, _ := strings.Cut(strings.TrimSuffix(line, "\n"), "=")
		if slices.Contains(apiKeys, name) {
			t.Errorf("the session started with %s in its environment", name)
		}
		if name == "ASMAI_SESSION" {
			credential = value
		}
	}
	if credential == "" {
		t.Error("the session has no credential in its environment")
	}
	if path, _ := os.ReadFile(filepath.Join(testDir, "asmai.path")); strings.TrimSpace(string(path)) != fixedPath {
		t.Errorf("the session runs asmai at %q, want the daemon's copy at %s", path, fixedPath)
	}
	if out, err := exec.Command(fixedPath, "version").Output(); err != nil || strings.TrimSpace(string(out)) != "asmai "+e2eVersion {
		t.Errorf("the daemon's copy of asmai prints %q (%v), want this asmai's version", out, err)
	}

	// Everything the session was given is on its command line: hooks naming
	// the fixed path, the asmai allow-list entry and Coordination's
	// instructions, with the native prompts kept.
	argsJSON, err := os.ReadFile(filepath.Join(testDir, "args.json"))
	if err != nil {
		t.Fatal(err)
	}
	var args []string
	if err := json.Unmarshal(argsJSON, &args); err != nil {
		t.Fatal(err)
	}
	flags := map[string]string{}
	for i := 0; i < len(args); i++ {
		if args[i] == "--strict-mcp-config" {
			flags[args[i]] = "true"
			continue
		}
		flags[args[i]] = args[i+1]
		i++
	}
	if len(args) != 9 || flags["--setting-sources"] != "user" || flags["--strict-mcp-config"] != "true" || flags["--model"] != "opus" || !strings.Contains(flags["--append-system-prompt"], "You are Coordination's leader") {
		t.Errorf("the session was started with %q, want --settings, --setting-sources user, --strict-mcp-config, --model opus and Coordination's instructions", args)
	}
	for _, arg := range args {
		if strings.Contains(arg, "dangerously") || strings.Contains(arg, "--permission-mode") || strings.Contains(arg, "bypassPermissions\"") {
			t.Errorf("the session was started with %q, which bypasses the native permission prompts", arg)
		}
	}
	var settings struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
		Permissions struct {
			Allow       []string `json:"allow"`
			DefaultMode string   `json:"defaultMode"`
		} `json:"permissions"`
		Sandbox struct {
			Enabled           bool `json:"enabled"`
			FailIfUnavailable bool `json:"failIfUnavailable"`
		} `json:"sandbox"`
	}
	if err := json.Unmarshal([]byte(flags["--settings"]), &settings); err != nil {
		t.Fatalf("--settings is %q: %v", flags["--settings"], err)
	}
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "PermissionRequest", "Stop"} {
		h := settings.Hooks[event]
		if len(h) != 1 || len(h[0].Hooks) != 1 || h[0].Hooks[0].Command != "'"+fixedPath+"' hook" {
			t.Errorf("the %s hook is %+v, want the daemon's copy of asmai at %s", event, h, fixedPath)
		}
	}
	if !slices.Equal(settings.Permissions.Allow, []string{
		"Bash(asmai:*)", "Bash(gh:*)", "Bash(git add:*)", "Bash(git commit:*)", "Bash(git push:*)", "Bash(git fetch:*)",
		"Bash(git ls-remote:*)", "Bash(git status:*)", "Bash(git diff:*)", "Bash(git log:*)", "Bash(git rev-parse:*)",
		"Bash(git merge:*)", "Bash(git var:*)", "Bash(git show:*)",
	}) || settings.Permissions.DefaultMode != "default" {
		t.Errorf("the session's permissions are %+v, want asmai, git and gh allowed and the default mode", settings.Permissions)
	}
	if !settings.Sandbox.Enabled || !settings.Sandbox.FailIfUnavailable {
		t.Errorf("the session's sandbox is %+v, want the write guard on", settings.Sandbox)
	}

	// AsmAI wrote nothing of Claude Code's in the home.
	home, err := os.ReadDir(os.Getenv("HOME"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range home {
		if e.Name() != ".local" && e.Name() != ".config" {
			t.Errorf("the home holds %s, want only AsmAI's configuration and state", e.Name())
		}
	}

	// The session's start is journaled with the provider version, the
	// settings passed and the generation; each hook event is journaled as an
	// observation of that generation.
	var start struct {
		Agent      string   `json:"agent"`
		Role       string   `json:"role"`
		Provider   string   `json:"provider"`
		Version    string   `json:"version"`
		Executable string   `json:"executable"`
		Args       []string `json:"args"`
		Generation int      `json:"generation"`
	}
	sessionStarts := entries(t, "session.started")
	if len(sessionStarts) != 1 {
		t.Fatalf("the journal holds %d session starts, want 1", len(sessionStarts))
	}
	json.Unmarshal(sessionStarts[0], &start)
	if start.Agent != "leader@coordination" || start.Role != "coordination" || start.Provider != "claude-code" || start.Version != "2.1.292" ||
		start.Generation != 1 || !slices.Equal(start.Args, args) || start.Executable != filepath.Join(stateDir, "providers", "claude-code", "2.1.292", "claude") {
		t.Errorf("the session's start is journaled as %s, want generation 1 of the pinned Claude Code with the arguments the session was given", sessionStarts[0])
	}
	var events []string
	for _, data := range entries(t, "observation") {
		var o struct {
			Agent      string         `json:"agent"`
			Generation int            `json:"generation"`
			Event      string         `json:"event"`
			Payload    map[string]any `json:"payload"`
		}
		json.Unmarshal(data, &o)
		if o.Agent != "leader@coordination" || o.Generation != 1 || o.Payload["hook_event_name"] != o.Event {
			t.Errorf("an observation is journaled as %s, want one of generation 1 of leader@coordination with its payload", data)
		}
		events = append(events, o.Event)
	}
	if want := []string{"SessionStart", "UserPromptSubmit", "PermissionRequest", "Stop"}; !slices.Equal(events, want) {
		t.Errorf("the journal observes %v, want %v", events, want)
	}
	// No one typed the prompt the leader reported submitted, so it is no
	// witnessed message.
	if witnessed := entries(t, "message.witnessed"); len(witnessed) != 0 {
		t.Errorf("the journal holds the witnessed messages %s, want none: the user typed nothing", witnessed)
	}

	// asmai agents lists the leader, and asmai status shows each leader.
	agents := agentsListed(t)
	if len(agents) != 1 || agents[0].Agent != "leader@coordination" || agents[0].State != "running" || agents[0].Generation != 1 ||
		agents[0].Provider != "claude-code" || agents[0].Version != "2.1.292" || agents[0].Model != "opus" {
		t.Errorf("asmai agents lists %+v, want leader@coordination running", agents)
	}
	if stdout, _, _ := runAsmai(t, "agents"); !strings.Contains(stdout, "leader@coordination  running  1") {
		t.Errorf("asmai agents printed\n%s", stdout)
	}
	stdout, _, _ := runAsmai(t, "status")
	for _, want := range []string{"leader@coordination  running", "leader@engineering   stopped", "leader@quality       stopped"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("asmai status printed\n%s\nwant it to show %q", stdout, want)
		}
	}

	// asmai attach draws the leader's screen with AsmAI's emulation, one row
	// shorter than the terminal, over a status line, and observes.
	a := attachInTerminal(t, "coordination", 80, 25)
	waitFor(t, "the attached screen", func() bool {
		lines := a.lines()
		return lines[0] == "Coordination is ready" && lines[4] == "         placed" && strings.Contains(lines[24], "leader@coordination · observing · Ctrl-] detaches")
	})
	if c := a.cell(0, 0); c.Flags&vt.Bold == 0 || a.cell(13, 0).Flags&vt.Bold != 0 {
		t.Errorf("the attached screen lost the leader's renditions: %+v", c)
	}
	var snapshot struct {
		Agent  leaderJSON `json:"agent"`
		Screen struct {
			Columns int      `json:"columns"`
			Rows    int      `json:"rows"`
			Lines   []string `json:"lines"`
		} `json:"screen"`
	}
	asmaiJSON(t, &snapshot, "attach", "leader@coordination")
	if snapshot.Agent.Agent != "leader@coordination" || snapshot.Screen.Columns != 80 || snapshot.Screen.Rows != 24 || snapshot.Screen.Lines[0] != "Coordination is ready" {
		t.Errorf("asmai attach --json printed %+v, want the leader's 80 by 24 screen", snapshot)
	}
	// Keys typed in the attached terminal never reach the leader: had they,
	// the fake would have exited on input it did not expect.
	if _, err := a.ptmx.WriteString("hello\r"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if agents := agentsListed(t); agents[0].State != "running" || agents[0].Generation != 1 {
		t.Errorf("after keys were typed in the attached terminal, the leader is %+v, want generation 1 still running", agents[0])
	}
	// Ctrl-] detaches.
	a.ptmx.WriteString("\x1d")
	select {
	case err := <-a.done:
		if err != nil {
			t.Errorf("asmai attach ended with %v after Ctrl-], want exit 0", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("asmai attach did not detach on Ctrl-]")
	}
	if len(entries(t, "session.ended")) != 0 {
		t.Error("attaching and detaching ended the leader's session")
	}

	// Only running agents can be attached, and an address names an agent.
	for agent, want := range map[string]string{"engineering": "leader@engineering is not running", "nobody": `"nobody" names no agent`} {
		if _, stderr, code := runAsmai(t, "attach", agent); code != 1 || !strings.Contains(stderr, want) {
			t.Errorf("asmai attach %s exited %d printing %q, want 1 and %q", agent, code, stderr, want)
		}
	}

	// The leader runs as long as the factory does: stopping the factory ends
	// its session, and the next start starts the next generation.
	var stopped struct{}
	asmaiJSON(t, &stopped, "stop")
	var ended struct {
		Agent      string `json:"agent"`
		Generation int    `json:"generation"`
		By         string `json:"by"`
	}
	if _, stderr, code := runAsmai(t, "start"); code != 0 {
		t.Fatalf("asmai start exited %d: %s", code, stderr)
	}
	endings := entries(t, "session.ended")
	if len(endings) != 1 {
		t.Fatalf("the journal holds %d session ends, want 1", len(endings))
	}
	json.Unmarshal(endings[0], &ended)
	if ended.Agent != "leader@coordination" || ended.Generation != 1 || ended.By != "asmai stop" {
		t.Errorf("the session's end is journaled as %s, want generation 1 ended by asmai stop", endings[0])
	}
	if agents := agentsListed(t); agents[0].State != "running" || agents[0].Generation != 2 {
		t.Errorf("after the factory started again, the leader is %+v, want generation 2 running", agents[0])
	}

	// Neither the session's environment nor its credential is recorded.
	recorded := map[string]string{}
	for _, args := range [][]string{{"export"}, {"log"}, {"export", "--json"}, {"log", "--json"}} {
		out, _, _ := runAsmai(t, args...)
		recorded["asmai "+strings.Join(args, " ")] = out
	}
	for _, name := range []string{"store.db", "store.db-wal", "daemon.log"} {
		data, err := os.ReadFile(filepath.Join(stateDir, name))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
		recorded[name] = string(data)
	}
	never := append([]string{envSentinel, "not-a-real-key", "PATH="}, apiKeys...)
	if credential != "" {
		never = append(never, credential)
	}
	for where, text := range recorded {
		for _, never := range never {
			if strings.Contains(text, never) {
				t.Errorf("%s records the session's environment: it holds %q", where, never)
			}
		}
	}
}

type checkJSON struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Problem string `json:"problem"`
	Fix     string `json:"fix"`
}

func TestStartNamesTheLineAndTheFixOfAConfigurationItCannotHonour(t *testing.T) {
	factoryHome(t)
	writeConfig(t, "[roles.coordination]\nleader_model = \"opus\"\nleader_provider = \"codex\"\n")
	stderr := startUnready(t)
	path := filepath.Join(os.Getenv("HOME"), ".config", "asmai", "config.toml")
	for _, want := range []string{
		`failed  staffing: ` + path + `, line 3: leader_provider in [roles.coordination] is "codex", but this asmai runs only "claude"`,
		`fix: write leader_provider = "claude"`,
		`failed  Claude Code 2.1.292: the pinned Claude Code 2.1.292 is not installed`,
		"fix: run `asmai providers install`",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("asmai start printed\n%s\nwant it to say %q", stderr, want)
		}
	}

	// Once the file is valid, start names each role and field still missing.
	writeConfig(t, "[roles.coordination]\nleader_provider = \"claude\"\nleader_model = \"opus\"\nworker_provider = \"claude\"\nworker_model = \"opus\"\n\n[roles.quality]\nleader_model = \"opus\"\n")
	stdout, _, code := runAsmai(t, "start", "--json")
	var failed struct {
		Error   string       `json:"error"`
		Started bool         `json:"started"`
		Checks  []checkJSON  `json:"checks"`
		Leaders []leaderJSON `json:"leaders"`
	}
	if err := json.Unmarshal([]byte(stdout), &failed); err != nil || code != 1 || failed.Error == "" || failed.Started {
		t.Fatalf("asmai start --json exited %d printing %s, want 1 and the failed checks", code, stdout)
	}
	var problems []string
	for _, c := range failed.Checks {
		if !c.OK && c.Name == "staffing" {
			problems = append(problems, c.Problem)
		}
	}
	want := []string{
		path + ": there is no [roles.engineering], so Engineering is not staffed",
		path + ": [roles.quality] has no leader_provider",
		path + ": [roles.quality] has no worker_provider",
		path + ": [roles.quality] has no worker_model",
	}
	if !slices.Equal(problems, want) {
		t.Errorf("asmai start --json names the staffing problems\n%s\nwant\n%s", strings.Join(problems, "\n"), strings.Join(want, "\n"))
	}
	for _, l := range failed.Leaders {
		if l.State != "stopped" {
			t.Errorf("with checks failed, %s is %s, want stopped", l.Agent, l.State)
		}
	}
	stdout, _, _ = runAsmai(t, "status")
	for _, want := range []string{"leader@coordination  stopped", "check failed", "`asmai start` shows how to fix what failed."} {
		if !strings.Contains(stdout, want) {
			t.Errorf("asmai status printed\n%s\nwant it to show %q", stdout, want)
		}
	}
	if agents := agentsListed(t); len(agents) != 0 {
		t.Errorf("with checks failed, agents ran: %+v", agents)
	}
}

func TestCoordinationsLeaderIsRestartedWhenItsSessionEnds(t *testing.T) {
	// This leader exits as soon as it has drawn its screen.
	readyFactory(t, []string{`{"screen": "bye\r\n"}`})
	if _, stderr, code := runAsmai(t, "start"); code != 0 {
		t.Fatalf("asmai start exited %d: %s", code, stderr)
	}
	waitFor(t, "the leader's second session", func() bool {
		agents := agentsListed(t)
		return len(agents) == 1 && agents[0].Generation >= 2
	})
	var ended struct {
		Generation int    `json:"generation"`
		Exit       string `json:"exit"`
		By         string `json:"by"`
	}
	json.Unmarshal(entries(t, "session.ended")[0], &ended)
	if ended.Generation != 1 || ended.Exit != "exit status 0" || ended.By != "its own exit" {
		t.Errorf("the first session's end is journaled as %+v, want generation 1 exiting on its own", ended)
	}
}
