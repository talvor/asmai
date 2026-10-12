// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/creack/pty"

	"github.com/talvor/asmai/internal/config"
	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/providers"
	"github.com/talvor/asmai/internal/vt"
)

// The scripted fake's screens for Claude Code's question about trusting a
// folder, in the shape Claude Code 2.1.292 draws it, with "No, exit"
// selected first.
const (
	trustAsked    = `{"screen": "\u001b[2J\u001b[HAccessing workspace:\r\n\r\n \u001b[1m❯ No, exit\u001b[22m\r\n   Yes, I trust this folder\r\n"}`
	trustYesShown = `{"screen": "\u001b[2J\u001b[HAccessing workspace:\r\n\r\n   No, exit\r\n \u001b[1m❯ Yes, I trust this folder\u001b[22m\r\n"}`
)

// recordTrust is the fake's stand-in for Claude Code recording the user's
// trust of the folder it runs in, by its resolved path, in ~/.claude.json:
// it keeps every folder trusted so far.
var recordTrust = func() string {
	command := `pwd -P >> "$ASMAI_TEST_DIR/trusted" && { printf '{"projects": {'; sep=; while read -r d; do printf '%s"%s": {"hasTrustDialogAccepted": true}' "$sep" "$d"; sep=', '; done < "$ASMAI_TEST_DIR/trusted"; printf '}}'; } > "$HOME/.claude.json"`
	step, _ := json.Marshal(map[string]any{"run": command, "status": 0})
	return string(step)
}()

// trusting is Claude Code asking whether to trust its folder, the user moving
// to "Yes, I trust this folder" and confirming, Claude Code recording it and
// coming up, and the user leaving it with /exit.
var trusting = []string{
	trustAsked,
	`{"expect": "\u001b[B"}`,
	trustYesShown,
	`{"expect": "\r"}`,
	recordTrust,
	`{"screen": "\u001b[2J\u001b[HClaude Code is ready\r\n❯ "}`,
	`{"expect": "/exit\r"}`,
}

// refusing is the user answering "No, exit", on which Claude Code exits
// without recording anything.
var refusing = []string{trustAsked, `{"expect": "\r"}`}

// setupFactory gives the test a fresh home with its pinned Claude Code a
// stand-in that plays, through the fake, the script the test gives for the
// folder it runs in, and otherwise the fake's own script. Each folder it is
// started in is appended to the test directory's opened file, and with its
// DISABLE_AUTOUPDATER to its environments file. It returns the
// state directory and the test directory.
func setupFactory(t *testing.T, scripts map[string][]string) (stateDir, testDir string) {
	t.Helper()
	stateDir = factoryHome(t)
	testDir = t.TempDir()
	t.Setenv("ASMAI_TEST_DIR", testDir)
	setScripts(t, testDir, scripts)
	leader := filepath.Join(testDir, "leader.jsonl")
	if err := os.WriteFile(leader, []byte(strings.Join(idleLeader, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ASMAI_FAKE_PROVIDER_SCRIPT", leader)
	claude := []byte(`#!/bin/sh
folder=$(basename "$PWD")
echo "$folder" >> "$ASMAI_TEST_DIR/opened"
echo "$folder DISABLE_AUTOUPDATER=${DISABLE_AUTOUPDATER-}" >> "$ASMAI_TEST_DIR/environments"
if [ -e "$ASMAI_TEST_DIR/script-$folder.jsonl" ]; then
	ASMAI_FAKE_PROVIDER_SCRIPT="$ASMAI_TEST_DIR/script-$folder.jsonl"
	export ASMAI_FAKE_PROVIDER_SCRIPT
fi
exec ` + providers.ShellQuote(fakeProvider) + ` "$@"
`)
	standInChannelPinning(t, claude, claude)
	return stateDir, testDir
}

// setScripts makes scripts, by folder name, the ones the stand-in plays.
func setScripts(t *testing.T, testDir string, scripts map[string][]string) {
	t.Helper()
	for folder, script := range scripts {
		if err := os.WriteFile(filepath.Join(testDir, "script-"+folder+".jsonl"), []byte(strings.Join(script, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// opened returns the folders the stand-in Claude Code was started in.
func opened(testDir string) []string {
	data, _ := os.ReadFile(filepath.Join(testDir, "opened"))
	return strings.Fields(string(data))
}

// setupTerminal is `asmai setup`, run in this process, with a terminal of its
// own as its stdin, stdout and stderr, as a user runs it.
type setupTerminal struct {
	ptmx *os.File
	mu   sync.Mutex
	term *vt.Terminal
	out  bytes.Buffer
	code chan int
	read chan struct{}
}

// setupReturned is what the test writes on setup's terminal once setup
// returns.
const setupReturned = "(asmai setup returned)"

func setupInTerminal(t *testing.T, args ...string) *setupTerminal {
	t.Helper()
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := pty.Setsize(ptmx, &pty.Winsize{Cols: 200, Rows: 40}); err != nil {
		t.Fatal(err)
	}
	s := &setupTerminal{ptmx: ptmx, term: vt.New(200, 40), code: make(chan int, 1), read: make(chan struct{})}
	t.Cleanup(func() {
		tty.Close()
		ptmx.Close()
	})
	go func() {
		defer close(s.read)
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			s.mu.Lock()
			s.term.Write(buf[:n])
			s.out.Write(buf[:n])
			done := strings.Contains(s.out.String(), setupReturned)
			s.mu.Unlock()
			if done || err != nil {
				return
			}
		}
	}()
	// The terminal stays open once setup returns: macOS drops what is still
	// unread on a terminal whose last other end is closed. The mark, written
	// after everything setup wrote, tells the reader it has it all.
	go func() {
		code := run(append([]string{"setup"}, args...), tty, tty, tty)
		tty.WriteString(setupReturned)
		s.code <- code
	}()
	return s
}

func (s *setupTerminal) shows(text string) func() bool {
	return func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return slices.ContainsFunc(s.term.Lines(), func(line string) bool { return strings.Contains(line, text) })
	}
}

func (s *setupTerminal) printed() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.out.String()
}

func (s *setupTerminal) typeKeys(t *testing.T, keys string) {
	t.Helper()
	if _, err := s.ptmx.Write([]byte(keys)); err != nil {
		t.Fatal(err)
	}
}

// exited waits for setup to exit, and for what it printed, and returns its
// exit code.
func (s *setupTerminal) exited(t *testing.T) int {
	t.Helper()
	var code int
	waitFor(t, "asmai setup to exit", func() bool {
		select {
		case code = <-s.code:
			return true
		default:
			return false
		}
	})
	<-s.read
	return code
}

// confirmInstall answers yes when setup asks to install the pinned Claude
// Code.
func (s *setupTerminal) confirmInstall(t *testing.T) {
	t.Helper()
	waitFor(t, "setup to ask to install Claude Code", s.shows("Install it? [y/N]"))
	s.typeKeys(t, "y\r")
}

// trust answers Claude Code's question about trusting its folder as a user
// does, "Yes, I trust this folder", then leaves Claude Code.
func (s *setupTerminal) trust(t *testing.T) {
	t.Helper()
	waitFor(t, "Claude Code to ask about trusting its folder", s.shows("❯ No, exit"))
	s.typeKeys(t, "\x1b[B")
	waitFor(t, "Yes to be selected", s.shows("❯ Yes, I trust this folder"))
	s.typeKeys(t, "\r")
	waitFor(t, "Claude Code to come up", s.shows("Claude Code is ready"))
	s.typeKeys(t, "/exit\r")
}

// refuse answers "No, exit".
func (s *setupTerminal) refuse(t *testing.T) {
	t.Helper()
	waitFor(t, "Claude Code to ask about trusting its folder", s.shows("❯ No, exit"))
	s.typeKeys(t, "\r")
}

// lineSaying returns the line of out that holds prefix, from prefix on: a
// line setup prints after Claude Code exits may follow what Claude Code left
// on the terminal's last line.
func lineSaying(out, prefix string) string {
	for line := range strings.Lines(out) {
		if _, after, found := strings.Cut(line, prefix); found {
			return prefix + " " + strings.TrimSpace(after)
		}
	}
	return ""
}

// On a fresh account, setup writes the configuration file with every role
// staffed, makes the folders, installs the pinned Claude Code once the user
// confirms, and opens it in agents/ and then workspaces/ for the user to
// trust each. Then `asmai start` starts the factory with no further edits.
// Run again, every step is already done and no Claude Code is opened.
func TestSetupOnAFreshAccountMakesTheFactoryReadyToStart(t *testing.T) {
	stateDir, testDir := setupFactory(t, map[string][]string{"agents": trusting, "workspaces": trusting})
	// The daemon, which here is running, records the install as soon as
	// setup fetches it; a stopped one finds it at its next start.
	startUnready(t)
	configFile := filepath.Join(os.Getenv("HOME"), ".config", "asmai", "config.toml")

	s := setupInTerminal(t)
	s.confirmInstall(t)
	s.trust(t)
	s.trust(t)
	if code := s.exited(t); code != 0 {
		t.Fatalf("asmai setup exited %d, printing\n%s", code, s.printed())
	}

	out := s.printed()
	agents, workspaces := filepath.Join(stateDir, "agents"), filepath.Join(stateDir, "workspaces")
	for prefix, want := range map[string]string{
		"config:":            "config: done. Wrote " + configFile + ", staffing Coordination, Engineering and Quality",
		"folders:":           "folders: done.",
		"Claude Code:":       "Claude Code: done. Installed the pinned Claude Code 2.1.292",
		"trust agents/:":     "trust agents/: done. Claude Code trusts " + agents,
		"trust workspaces/:": "trust workspaces/: done. Claude Code trusts " + workspaces,
	} {
		if got := lineSaying(out, prefix); !strings.HasPrefix(got, want) {
			t.Errorf("setup reported %q, want %q", got, want)
		}
	}
	if !strings.Contains(out, "Trusting "+agents) || strings.Index(out, "Trusting "+agents) > strings.Index(out, "Trusting "+workspaces) {
		t.Errorf("setup did not say it was about to trust agents/, then workspaces/:\n%s", out)
	}
	if got := opened(testDir); !slices.Equal(got, []string{"agents", "workspaces"}) {
		t.Errorf("setup opened Claude Code in %q, want agents, then workspaces", got)
	}
	// Claude Code's auto-updater, which would install a Claude Code of the
	// user's own, is off in the sessions setup opens.
	if got, _ := os.ReadFile(filepath.Join(testDir, "environments")); string(got) != "agents DISABLE_AUTOUPDATER=1\nworkspaces DISABLE_AUTOUPDATER=1\n" {
		t.Errorf("setup opened Claude Code with\n%swant DISABLE_AUTOUPDATER=1 in agents and workspaces", got)
	}
	if got, _ := os.ReadFile(configFile); string(got) != string(config.Default()) {
		t.Errorf("the configuration file holds\n%s\nwant the default", got)
	}
	for _, dir := range []string{"providers", "bin", "githooks", "agents", "repositories", "workspaces", "views"} {
		if info, err := os.Stat(filepath.Join(stateDir, dir)); err != nil || !info.IsDir() {
			t.Errorf("setup made no %s folder: %v", dir, err)
		}
	}
	// AsmAI wrote nothing of Claude Code's: its record is exactly what Claude
	// Code wrote, and there is no ~/.claude.
	record, _ := os.ReadFile(filepath.Join(os.Getenv("HOME"), ".claude.json"))
	resolvedAgents, _ := filepath.EvalSymlinks(agents)
	resolvedWorkspaces, _ := filepath.EvalSymlinks(workspaces)
	if want := `{"projects": {"` + resolvedAgents + `": {"hasTrustDialogAccepted": true}, "` + resolvedWorkspaces + `": {"hasTrustDialogAccepted": true}}}`; string(record) != want {
		t.Errorf("Claude Code's record is %s, want only what Claude Code recorded, %s", record, want)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".claude")); err == nil {
		t.Error("the home holds ~/.claude")
	}

	var started struct {
		Leaders []leaderJSON `json:"leaders"`
	}
	asmaiJSON(t, &started, "start")
	if len(started.Leaders) == 0 || started.Leaders[0].Agent != "leader@coordination" || started.Leaders[0].State != "running" {
		t.Errorf("after setup, asmai start shows the leaders %+v, want Coordination's running", started.Leaders)
	}

	again := setupInTerminal(t)
	if code := again.exited(t); code != 0 {
		t.Fatalf("asmai setup again exited %d, printing\n%s", code, again.printed())
	}
	for _, prefix := range []string{"config:", "folders:", "Claude Code:", "trust agents/:", "trust workspaces/:"} {
		if got := lineSaying(again.printed(), prefix); !strings.HasPrefix(got, prefix+" already done.") {
			t.Errorf("setup again reported %q, want it already done", got)
		}
	}
	waitFor(t, "Coordination's leader to start", func() bool { return len(opened(testDir)) == 3 })
	if got := opened(testDir); !slices.Equal(got, []string{"agents", "workspaces", "leader@coordination"}) {
		t.Errorf("Claude Code was started in %q, want no Claude Code opened by setup again", got)
	}
	if after, _ := os.ReadFile(filepath.Join(os.Getenv("HOME"), ".claude.json")); !bytes.Equal(after, record) {
		t.Errorf("Claude Code's record became %s", after)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"setup", "--json"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("asmai setup --json exited %d: %s%s", code, stdout.String(), stderr.String())
	}
	var result struct {
		Steps []setupStep `json:"steps"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("asmai setup --json printed %q: %v", stdout.String(), err)
	}
	if len(result.Steps) != 5 || slices.ContainsFunc(result.Steps, func(s setupStep) bool { return s.State != stepAlreadyDone }) {
		t.Errorf("asmai setup --json reported %+v, want five steps already done", result.Steps)
	}
}

// Answering "No, exit" makes setup exit 1 naming the folder still untrusted.
// The next run asks only about that folder.
func TestAnsweringNoMakesSetupNameTheUntrustedFolderAndTheNextRunAsksOnlyAboutIt(t *testing.T) {
	stateDir, testDir := setupFactory(t, map[string][]string{"agents": trusting, "workspaces": refusing})
	startUnready(t)
	workspaces := filepath.Join(stateDir, "workspaces")

	s := setupInTerminal(t)
	s.confirmInstall(t)
	s.trust(t)
	s.refuse(t)
	if code := s.exited(t); code != 1 {
		t.Fatalf("asmai setup exited %d after No, want 1; it printed\n%s", code, s.printed())
	}
	if out := s.printed(); !strings.Contains(out, "Claude Code does not trust "+workspaces) || !strings.Contains(out, "asmai setup") {
		t.Errorf("after No, setup printed\n%s\nwant it to name %s, still untrusted", out, workspaces)
	}
	var status struct {
		Checks []checkJSON `json:"checks"`
	}
	runAsmai(t, "start")
	asmaiJSON(t, &status, "status")
	if !slices.ContainsFunc(status.Checks, func(c checkJSON) bool { return c.Name == "directory trust" && !c.OK }) {
		t.Errorf("with workspaces/ untrusted, the checks are %+v, want directory trust failed", status.Checks)
	}

	setScripts(t, testDir, map[string][]string{"workspaces": trusting})
	again := setupInTerminal(t)
	again.trust(t)
	if code := again.exited(t); code != 0 {
		t.Fatalf("asmai setup again exited %d, printing\n%s", code, again.printed())
	}
	if got := lineSaying(again.printed(), "trust agents/:"); !strings.HasPrefix(got, "trust agents/: already done.") {
		t.Errorf("setup again reported %q, want agents/ already done", got)
	}
	if got := opened(testDir); !slices.Equal(got, []string{"agents", "workspaces", "workspaces"}) {
		t.Errorf("Claude Code was opened in %q, want agents and workspaces, then only workspaces", got)
	}
}

// An existing configuration file is left byte for byte as it is, and its
// problems are reported.
func TestSetupLeavesAnExistingConfigurationFileAsItIsAndReportsItsProblems(t *testing.T) {
	readyFactory(t, idleLeader)
	mine := "# my own\n[roles.coordination]\nleader_provider = \"codex\"\n"
	writeConfig(t, mine)
	configFile := filepath.Join(os.Getenv("HOME"), ".config", "asmai", "config.toml")
	if err := os.Chmod(configFile, 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"setup"}, strings.NewReader(""), &stdout, &stderr)

	if code != 1 {
		t.Errorf("asmai setup exited %d, want 1 for a file with problems", code)
	}
	out := stdout.String()
	for _, want := range []string{
		"config: already done. " + configFile + " exists, and setup leaves it as it is",
		configFile + ", line 3: leader_provider in [roles.coordination] is \"codex\"",
		"trust workspaces/: already done.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("setup printed\n%s\nwant it to say %q", out, want)
		}
	}
	if !strings.Contains(stderr.String(), "has a problem; fix it") {
		t.Errorf("setup's error is %q, want the file's problems named", stderr.String())
	}
	if got, _ := os.ReadFile(configFile); string(got) != mine {
		t.Errorf("the configuration file became\n%s\nwant it unchanged", got)
	}
	if info, _ := os.Stat(configFile); info.Mode().Perm() != 0o644 {
		t.Errorf("the configuration file's mode became %v", info.Mode())
	}
}

// With either folder untrusted, `asmai start`'s checks fail naming `asmai
// setup`, and start asks nothing.
func TestStartFailsNamingSetupWhileAFolderAgentsStartInIsUntrusted(t *testing.T) {
	stateDir := readyFactory(t, idleLeader)
	record := filepath.Join(os.Getenv("HOME"), ".claude.json")
	only := `{"projects": {"` + filepath.Join(stateDir, "agents") + `": {"hasTrustDialogAccepted": true}}}`
	if err := os.WriteFile(record, []byte(only), 0o600); err != nil {
		t.Fatal(err)
	}

	_, stderr, code := runAsmai(t, "start")

	if code != 1 || !strings.Contains(stderr, "failed  directory trust: Claude Code does not trust "+filepath.Join(stateDir, "workspaces")) || !strings.Contains(stderr, "fix: run `asmai setup`") {
		t.Errorf("asmai start exited %d printing\n%s\nwant 1 and the directory trust check failed, naming `asmai setup`", code, stderr)
	}
	if got, _ := os.ReadFile(record); string(got) != only {
		t.Errorf("asmai start changed Claude Code's record to %s", got)
	}
	if list := agentsListed(t); len(list) != 0 {
		t.Errorf("with workspaces/ untrusted, agents ran: %+v", list)
	}
}

func TestAnAgentCannotRunSetup(t *testing.T) {
	t.Setenv(daemon.SessionCredential, "a-session")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"setup"}, strings.NewReader(""), &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "ask the user to run `asmai setup`") {
		t.Errorf("an agent's asmai setup exited %d printing %q, want it refused", code, stderr.String())
	}
}
