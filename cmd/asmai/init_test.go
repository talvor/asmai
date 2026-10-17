// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/creack/pty"

	"github.com/talvor/asmai/internal/config"
	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/fakeprovider"
	"github.com/talvor/asmai/internal/providers"
	"github.com/talvor/asmai/internal/skills"
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
// it keeps every folder trusted so far, and writes the record beside it and
// moves it into place, as Claude Code does, so it is never read half written.
var recordTrust = func() string {
	command := `pwd -P >> "$ASMAI_TEST_DIR/trusted" && { printf '{"projects": {'; sep=; while read -r d; do printf '%s"%s": {"hasTrustDialogAccepted": true}' "$sep" "$d"; sep=', '; done < "$ASMAI_TEST_DIR/trusted"; printf '}}'; } > "$HOME/.claude.json.$$" && mv "$HOME/.claude.json.$$" "$HOME/.claude.json"`
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
# The permission rules probe, sign-in check and version check are not a folder the user opens Claude Code in.
if [ "$1" = -p ] || [ "$1" = --version ] || [ "$1" = auth ]; then exec ` + providers.ShellQuote(fakeProvider) + ` "$@"; fi
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

// setupTerminal is `asmai init`, or another asmai command, run in this
// process, with a terminal of its own as its stdin, stdout and stderr, as a
// user runs it.
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
const setupReturned = "(asmai init returned)"

func setupInTerminal(t *testing.T, args ...string) *setupTerminal {
	t.Helper()
	return inProcessTerminal(t, append([]string{"init"}, args...)...)
}

// inProcessTerminal runs asmai with args as setupInTerminal runs `asmai
// setup`.
func inProcessTerminal(t *testing.T, args ...string) *setupTerminal {
	t.Helper()
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	// Wide enough that no line setup prints wraps.
	if err := pty.Setsize(ptmx, &pty.Winsize{Cols: 400, Rows: 40}); err != nil {
		t.Fatal(err)
	}
	s := &setupTerminal{ptmx: ptmx, term: vt.New(400, 40), code: make(chan int, 1), read: make(chan struct{})}
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
		code := run(args, tty, tty, tty)
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

// exited waits for the command to exit, and for what it printed, and returns its
// exit code.
func (s *setupTerminal) exited(t *testing.T) int {
	t.Helper()
	var code int
	waitFor(t, "asmai to exit", func() bool {
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

func (s *setupTerminal) chooseModel(t *testing.T, model string) {
	t.Helper()
	waitFor(t, "init to ask for Claude's default model", s.shows("Claude default model (opus or sonnet) [opus]:"))
	s.typeKeys(t, model+"\r")
}

// agree waits for setup to explain trusting dir and ask to open Claude Code
// there, and agrees with Enter.
func (s *setupTerminal) agree(t *testing.T, dir string) {
	t.Helper()
	waitFor(t, "setup to ask to open Claude Code in "+dir, s.shows(trustPrompt+dir))
	s.typeKeys(t, "\r")
}

// trust agrees to open Claude Code in dir, answers its question about
// trusting its folder as a user does, "Yes, I trust this folder", then
// leaves Claude Code.
func (s *setupTerminal) trust(t *testing.T, dir string) {
	t.Helper()
	s.agree(t, dir)
	waitFor(t, "Claude Code to ask about trusting its folder", s.shows("❯ No, exit"))
	s.typeKeys(t, "\x1b[B")
	waitFor(t, "Yes to be selected", s.shows("❯ Yes, I trust this folder"))
	s.typeKeys(t, "\r")
	waitFor(t, "Claude Code to come up", s.shows("Claude Code is ready"))
	s.typeKeys(t, "/exit\r")
}

// refuse agrees to open Claude Code in dir, and answers "No, exit".
func (s *setupTerminal) refuse(t *testing.T, dir string) {
	t.Helper()
	s.agree(t, dir)
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

	agents, workspaces := filepath.Join(stateDir, "agents"), filepath.Join(stateDir, "workspaces")
	s := setupInTerminal(t)
	s.chooseModel(t, "sonnet")
	s.confirmInstall(t)
	s.trust(t, agents)
	s.trust(t, workspaces)
	if code := s.exited(t); code != 0 {
		t.Fatalf("asmai init exited %d, printing\n%s", code, s.printed())
	}

	out := s.printed()
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
	// Each stage is headed, in order.
	at := 0
	for _, heading := range []string{"== 1/5 Platform ", "== 2/5 Config ", "== 3/5 Folders ", "== 4/5 Claude Code ", "== 5/5 Trust "} {
		i := strings.Index(out[at:], heading)
		if i < 0 {
			t.Errorf("setup printed no heading %q after the one before it:\n%s", heading, out)
			break
		}
		at += i
	}
	// Before opening Claude Code in each folder, setup says which folder,
	// which answer to choose, and what the other one does.
	for _, want := range []string{
		"You are needed: trust " + agents,
		"Agents start in this folder: every leader's directory is made in it.",
		"You are needed: trust " + workspaces,
		"Agents start in this folder: every assignment's workspace is made in it.",
		`"Yes, I trust this folder"   <- choose this one`,
		`"No, exit"`,
		"type /exit and press Enter",
		`If you choose "No, exit", init stops with exit status 1, naming this folder`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("setup printed\n%s\nwant it to say %q", out, want)
		}
	}
	if strings.Index(out, "You are needed: trust "+agents) > strings.Index(out, "You are needed: trust "+workspaces) {
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
	if got, _ := os.ReadFile(configFile); string(got) != string(config.Default("sonnet")) {
		t.Errorf("the configuration file holds\n%s\nwant the default", got)
	}
	if cfg, problems, err := config.Load(configFile); err != nil || len(problems) != 0 || cfg.Providers["claude"].DefaultModel != "sonnet" {
		t.Errorf("init's Claude default model is %q with problems %v and error %v, want sonnet", cfg.Providers["claude"].DefaultModel, problems, err)
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
	if _, stderr, code := runIn(t, "y\n", "config", "apply"); code != 0 {
		t.Fatalf("applying setup's configuration exited %d: %s", code, stderr)
	}
	asmaiJSON(t, &started, "start")
	if len(started.Leaders) == 0 || started.Leaders[0].Agent != "leader@coordination" || started.Leaders[0].State != "running" {
		t.Errorf("after setup, asmai start shows the leaders %+v, want Coordination's running", started.Leaders)
	}

	again := setupInTerminal(t)
	if code := again.exited(t); code != 0 {
		t.Fatalf("asmai init again exited %d, printing\n%s", code, again.printed())
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
	if code := run([]string{"init", "--json"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("asmai init --json exited %d: %s%s", code, stdout.String(), stderr.String())
	}
	var result struct {
		Steps []setupStep `json:"steps"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("asmai init --json printed %q: %v", stdout.String(), err)
	}
	if len(result.Steps) != 9 || slices.ContainsFunc(result.Steps, func(s setupStep) bool { return s.State != stepAlreadyDone }) {
		t.Errorf("asmai init --json reported %+v, want six steps already done", result.Steps)
	}
}

// fixtureOrigin returns a repository with one commit on main, a bare one
// standing in for an origin.
func fixtureOrigin(t *testing.T) string {
	t.Helper()
	origin := filepath.Join(t.TempDir(), "fixture.git")
	gitIn(t, t.TempDir(), "init", "--bare", "-b", "main", origin)
	work := t.TempDir()
	gitIn(t, work, "clone", "--quiet", origin, "fixture")
	gitIn(t, filepath.Join(work, "fixture"), "commit", "--allow-empty", "-m", "initial")
	gitIn(t, filepath.Join(work, "fixture"), "push", "--quiet", "origin", "HEAD:main")
	return origin
}

// A registered repository's clone is trusted at setup too: a worker starts
// in a git worktree of it, which on the pinned Claude Code only the clone's
// own trust covers. `asmai repo add` with no terminal registers the
// repository, exiting 0, and says to trust the clone with `asmai init` in a
// terminal; with --json, stdout holds only the result. Setup then asks only
// about the new clone, explaining why, and run again finds every folder
// trusted.
func TestSetupHasTheUserTrustEachRegisteredClone(t *testing.T) {
	stateDir, testDir := setupFactory(t, map[string][]string{"agents": trusting, "workspaces": trusting, "fixture": trusting})
	startUnready(t)
	s := setupInTerminal(t)
	s.chooseModel(t, "")
	s.confirmInstall(t)
	s.trust(t, filepath.Join(stateDir, "agents"))
	s.trust(t, filepath.Join(stateDir, "workspaces"))
	if code := s.exited(t); code != 0 {
		t.Fatalf("asmai init exited %d, printing\n%s", code, s.printed())
	}

	clone := filepath.Join(stateDir, "repositories", "fixture")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"repo", "add", fixtureOrigin(t), "--json"}, strings.NewReader("\n"), &stdout, &stderr); code != 0 {
		t.Fatalf("asmai repo add with no terminal exited %d: %s%s", code, stdout.String(), stderr.String())
	}
	var added struct {
		Registered bool           `json:"registered"`
		Repository repositoryJSON `json:"repository"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &added); err != nil || !added.Registered || added.Repository.Clone != clone {
		t.Errorf("asmai repo add --json printed %q (%v), want only fixture registered", stdout.String(), err)
	}
	if want := "Claude Code does not trust the clone yet, so no worker can start in a workspace of fixture, and trusting it needs you at a terminal: run `asmai init` in a terminal, which opens Claude Code in " + clone; !strings.Contains(stderr.String(), want) {
		t.Errorf("asmai repo add with no terminal printed %q, want it to say %q", stderr.String(), want)
	}
	if got := opened(testDir); !slices.Equal(got, []string{"agents", "workspaces"}) {
		t.Errorf("with no terminal, repo add opened Claude Code: it was opened in %q", got)
	}

	again := setupInTerminal(t)
	again.trust(t, clone)
	if code := again.exited(t); code != 0 {
		t.Fatalf("asmai init after repo add exited %d, printing\n%s", code, again.printed())
	}
	out := again.printed()
	for prefix, want := range map[string]string{
		"trust agents/:":               "trust agents/: already done.",
		"trust workspaces/:":           "trust workspaces/: already done.",
		"trust repositories/fixture/:": "trust repositories/fixture/: done. Claude Code trusts " + clone,
	} {
		if got := lineSaying(out, prefix); !strings.HasPrefix(got, want) {
			t.Errorf("setup after repo add reported %q, want %q", got, want)
		}
	}
	for _, want := range []string{
		"You are needed: trust " + clone,
		"Workers start in git worktrees of this clone, one per assignment, and",
		"Claude Code trusts a worktree only when it trusts the clone itself.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("setup after repo add printed\n%s\nwant it to say %q", out, want)
		}
	}
	if got := opened(testDir); !slices.Equal(got, []string{"agents", "workspaces", "fixture"}) {
		t.Errorf("Claude Code was opened in %q, want agents and workspaces, then only the new clone", got)
	}

	stdout.Reset()
	if code := run([]string{"init", "--json"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("asmai init --json exited %d: %s%s", code, stdout.String(), stderr.String())
	}
	var result struct {
		Steps []setupStep `json:"steps"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("asmai init --json printed %q: %v", stdout.String(), err)
	}
	if len(result.Steps) != 10 || result.Steps[9].Step != "trust repositories/fixture/" || slices.ContainsFunc(result.Steps, func(s setupStep) bool { return s.State != stepAlreadyDone }) {
		t.Errorf("asmai init --json reported %+v, want ten steps already done, the clone's last", result.Steps)
	}
	configFile := filepath.Join(os.Getenv("HOME"), ".config", "asmai", "config.toml")
	data, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	origin := added.Repository.Origin
	changed := strings.Replace(string(data), origin, origin+"-conflict", 1)
	if changed == string(data) {
		t.Fatalf("the registered origin %q was absent from the configuration", origin)
	}
	if err := os.WriteFile(configFile, []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"init"}, strings.NewReader(""), &stdout, &stderr); code != 1 || !strings.Contains(stdout.String(), "problem") || !strings.Contains(stdout.String(), "origin") {
		t.Errorf("init with a conflicting registered origin exited %d printing %q", code, stdout.String()+stderr.String())
	}
}

func TestInitSkipsAnUnregisteredClone(t *testing.T) {
	stateDir, _ := setupFactory(t, nil)
	trustFactory(t, stateDir)
	if err := os.MkdirAll(filepath.Join(stateDir, "repositories", "orphan"), 0o700); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := runIn(t, "", "init", "--yes", "--json")
	if code != 0 {
		t.Fatalf("init with an unregistered clone exited %d printing %q", code, stdout+stderr)
	}
	var result struct {
		Steps []setupStep `json:"steps"`
	}
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("init --json printed %q: %v", stdout, err)
	}
	if slices.ContainsFunc(result.Steps, func(step setupStep) bool { return strings.Contains(step.Step, "orphan") }) {
		t.Errorf("init attempted to trust an unregistered clone: %+v", result.Steps)
	}
}

func TestInitDoctorAndStartAgreeOnInstalledProvider(t *testing.T) {
	stateDir := readyFactory(t, idleLeader)
	fakeUpstream(t, skills.Version(), nil)
	path := filepath.Join(stateDir, "providers", "claude-code", "2.1.292", "claude")
	program := "#!/bin/sh\nexec " + providers.ShellQuote(fakeProvider) + " \"$@\"\n"
	if err := os.WriteFile(path, []byte(program), 0o755); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := runIn(t, "", "init", "--yes", "--json")
	if code != 0 {
		t.Fatalf("init rejected the executable at the recorded path: %d, %s%s", code, stdout, stderr)
	}
	var setup struct {
		Steps []setupStep `json:"steps"`
	}
	if err := json.Unmarshal([]byte(stdout), &setup); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(setup.Steps, func(s setupStep) bool { return s.Step == "Claude Code" && s.State == stepAlreadyDone }) {
		t.Errorf("init did not accept the installed provider: %+v", setup.Steps)
	}

	stdout, stderr, code = doctorIn(t, "--json")
	if code != 0 {
		t.Fatalf("doctor rejected the executable at the recorded path: %d, %s%s", code, stdout, stderr)
	}
	var diagnosis struct {
		Checks []daemon.Check `json:"checks"`
	}
	if err := json.Unmarshal([]byte(stdout), &diagnosis); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code = runAsmai(t, "start", "--json")
	if code != 0 {
		t.Fatalf("start rejected the executable at the recorded path: %d, %s%s", code, stdout, stderr)
	}
	var started struct {
		Checks []daemon.Check `json:"checks"`
	}
	if err := json.Unmarshal([]byte(stdout), &started); err != nil {
		t.Fatal(err)
	}
	for _, checks := range [][]daemon.Check{diagnosis.Checks, started.Checks} {
		for _, name := range []string{"Claude Code 2.1.292", "Claude Code version", "Claude Code sign-in", "permission rules"} {
			if !slices.ContainsFunc(checks, func(c daemon.Check) bool { return c.Name == name && c.OK }) {
				t.Errorf("%s did not pass in %+v", name, checks)
			}
		}
	}
}

func TestInitRestoresNonExecutablePinnedProvider(t *testing.T) {
	stateDir := readyFactory(t, idleLeader)
	path := filepath.Join(stateDir, "providers", "claude-code", "2.1.292", "claude")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := runIn(t, "", "init", "--yes")
	if code != 0 {
		t.Fatalf("init with a nonexecutable pinned copy exited %d printing %q", code, stdout+stderr)
	}
	if !providers.Executable(path) {
		t.Errorf("init left the pinned copy at %s nonexecutable", path)
	}
}

func TestInitStopsBeforeTrustWhenPinnedClaudeReportsAnotherVersion(t *testing.T) {
	t.Setenv(fakeprovider.VersionEnv, "2.1.293")
	_, testDir := setupFactory(t, map[string][]string{"agents": trusting})
	stdout, stderr, code := runIn(t, "", "init", "--yes")
	if code != 1 || !strings.Contains(stdout+stderr, "Claude Code version: failed.") {
		t.Errorf("init with the wrong CLI version exited %d printing %q", code, stdout+stderr)
	}
	if got := opened(testDir); len(got) != 0 {
		t.Errorf("init opened Claude Code in %v after its version failed", got)
	}
}

// After setup, `asmai repo add` in a terminal has the user trust the new
// clone as setup does: it says why, opens Claude Code there once the user
// agrees, and reports the clone trusted. A clone trusted already, as one
// registered again is, opens no Claude Code. Then `asmai start` starts the
// factory.
func TestRepoAddHasTheUserTrustTheNewClone(t *testing.T) {
	stateDir, testDir := setupFactory(t, map[string][]string{"agents": trusting, "workspaces": trusting, "fixture": trusting})
	startUnready(t)
	s := setupInTerminal(t)
	s.chooseModel(t, "")
	s.confirmInstall(t)
	s.trust(t, filepath.Join(stateDir, "agents"))
	s.trust(t, filepath.Join(stateDir, "workspaces"))
	if code := s.exited(t); code != 0 {
		t.Fatalf("asmai init exited %d, printing\n%s", code, s.printed())
	}
	if _, stderr, code := runIn(t, "y\n", "config", "apply"); code != 0 {
		t.Fatalf("applying setup's configuration exited %d: %s", code, stderr)
	}

	origin := fixtureOrigin(t)
	clone := filepath.Join(stateDir, "repositories", "fixture")
	a := inProcessTerminal(t, "repo", "add", origin)
	a.trust(t, clone)
	if code := a.exited(t); code != 0 {
		t.Fatalf("asmai repo add exited %d, printing\n%s", code, a.printed())
	}
	out := a.printed()
	if got := lineSaying(out, "trust repositories/fixture/:"); !strings.HasPrefix(got, "trust repositories/fixture/: done. Claude Code trusts "+clone) {
		t.Errorf("repo add reported %q, want the clone trusted", got)
	}
	for _, want := range []string{
		"Registered fixture",
		"You are needed: trust " + clone,
		"Workers start in git worktrees of this clone, one per assignment, and",
		"Claude Code trusts a worktree only when it trusts the clone itself.",
		`"Yes, I trust this folder"   <- choose this one`,
		`If you choose "No, exit", repo add stops with exit status 1, naming this folder`,
		"Running `asmai init` again asks only about the folders still",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("repo add printed\n%s\nwant it to say %q", out, want)
		}
	}
	if got := opened(testDir); !slices.Equal(got, []string{"agents", "workspaces", "fixture"}) {
		t.Errorf("Claude Code was opened in %q, want agents and workspaces, then the new clone", got)
	}

	if _, stderr, code := runAsmai(t, "repo", "remove", "fixture"); code != 0 {
		t.Fatalf("asmai repo remove exited %d: %s", code, stderr)
	}
	again := inProcessTerminal(t, "repo", "add", origin)
	if code := again.exited(t); code != 0 {
		t.Fatalf("asmai repo add again exited %d, printing\n%s", code, again.printed())
	}
	if got := lineSaying(again.printed(), "trust repositories/fixture/:"); !strings.HasPrefix(got, "trust repositories/fixture/: already done. Claude Code trusts "+clone) {
		t.Errorf("repo add of a trusted clone reported %q, want it already done", got)
	}
	if got := opened(testDir); !slices.Equal(got, []string{"agents", "workspaces", "fixture"}) {
		t.Errorf("Claude Code was opened in %q, want none opened for a trusted clone", got)
	}

	if _, stderr, code := runAsmai(t, "start"); code != 0 {
		t.Errorf("after repo add, asmai start exited %d: %s", code, stderr)
	}
}

// Declining at repo add's question, or answering "No, exit" in Claude Code,
// leaves the repository registered and its clone untrusted: repo add exits
// 1 naming the clone and `asmai init`, and `asmai start` refuses.
func TestRepoAddStopsNamingTheCloneTheUserDidNotTrust(t *testing.T) {
	stateDir, testDir := setupFactory(t, map[string][]string{"agents": trusting, "workspaces": trusting, "fixture": refusing})
	startUnready(t)
	s := setupInTerminal(t)
	s.chooseModel(t, "")
	s.confirmInstall(t)
	s.trust(t, filepath.Join(stateDir, "agents"))
	s.trust(t, filepath.Join(stateDir, "workspaces"))
	if code := s.exited(t); code != 0 {
		t.Fatalf("asmai init exited %d, printing\n%s", code, s.printed())
	}

	origin := fixtureOrigin(t)
	clone := filepath.Join(stateDir, "repositories", "fixture")
	a := inProcessTerminal(t, "repo", "add", origin)
	a.refuse(t, clone)
	if code := a.exited(t); code != 1 {
		t.Fatalf("asmai repo add exited %d after No, want 1; it printed\n%s", code, a.printed())
	}
	if out := a.printed(); !strings.Contains(out, "Claude Code does not trust "+clone) || !strings.Contains(out, "run `asmai init` again") {
		t.Errorf("after No, repo add printed\n%s\nwant it to name %s and `asmai init`", out, clone)
	}

	other := filepath.Join(stateDir, "repositories", "other")
	n := inProcessTerminal(t, "repo", "add", origin, "--name", "other")
	waitFor(t, "repo add to ask to open Claude Code in "+other, n.shows(trustPrompt+other))
	n.typeKeys(t, "n\r")
	if code := n.exited(t); code != 1 {
		t.Fatalf("asmai repo add exited %d after the user declined, want 1; it printed\n%s", code, n.printed())
	}
	if got := lineSaying(n.printed(), "trust repositories/other/:"); !strings.HasPrefix(got, "trust repositories/other/: stopped. You stopped repo add before Claude Code opened, so "+other+" is still untrusted") {
		t.Errorf("after the user declined, repo add reported %q", got)
	}
	if got := opened(testDir); !slices.Equal(got, []string{"agents", "workspaces", "fixture"}) {
		t.Errorf("Claude Code was opened in %q, want none opened once the user declined", got)
	}

	if got := repositoriesListed(t); len(got) != 2 {
		t.Errorf("after the clones went untrusted, asmai repo list shows %+v, want both registered", got)
	}
	if _, stderr, code := runAsmai(t, "start"); code != 1 || !strings.Contains(stderr, "Claude Code does not trust "+clone) || !strings.Contains(stderr, "asmai init") {
		t.Errorf("asmai start with the clone untrusted exited %d printing %q, want it refused naming the clone and `asmai init`", code, stderr)
	}
}

// Answering "No, exit" makes setup exit 1 naming the folder still untrusted.
// The next run asks only about that folder.
func TestAnsweringNoMakesSetupNameTheUntrustedFolderAndTheNextRunAsksOnlyAboutIt(t *testing.T) {
	stateDir, testDir := setupFactory(t, map[string][]string{"agents": trusting, "workspaces": refusing})
	startUnready(t)
	workspaces := filepath.Join(stateDir, "workspaces")

	s := setupInTerminal(t)
	s.chooseModel(t, "")
	s.confirmInstall(t)
	s.trust(t, filepath.Join(stateDir, "agents"))
	s.refuse(t, workspaces)
	if code := s.exited(t); code != 1 {
		t.Fatalf("asmai init exited %d after No, want 1; it printed\n%s", code, s.printed())
	}
	if out := s.printed(); !strings.Contains(out, "Claude Code does not trust "+workspaces) || !strings.Contains(out, "asmai init") {
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
	again.trust(t, workspaces)
	if code := again.exited(t); code != 0 {
		t.Fatalf("asmai init again exited %d, printing\n%s", code, again.printed())
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
	code := run([]string{"init"}, strings.NewReader(""), &stdout, &stderr)

	if code != 1 {
		t.Errorf("asmai init exited %d, want 1 for a file with problems", code)
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

// Setup opens no Claude Code until the user agrees to. Declining stops setup
// with exit 1, naming the folder still untrusted and how to go on.
func TestSetupOpensClaudeCodeOnlyOnceTheUserAgrees(t *testing.T) {
	stateDir, testDir := setupFactory(t, map[string][]string{"agents": trusting, "workspaces": trusting})
	agents := filepath.Join(stateDir, "agents")

	s := setupInTerminal(t)
	s.chooseModel(t, "")
	s.confirmInstall(t)
	waitFor(t, "setup to ask to open Claude Code in agents/", s.shows(trustPrompt+agents))
	if got := opened(testDir); len(got) != 0 {
		t.Errorf("before the user agreed, setup opened Claude Code in %q", got)
	}
	s.typeKeys(t, "n\r")
	if code := s.exited(t); code != 1 {
		t.Fatalf("asmai init exited %d after the user declined, want 1; it printed\n%s", code, s.printed())
	}
	out := s.printed()
	if got := lineSaying(out, "trust agents/:"); !strings.HasPrefix(got, "trust agents/: stopped. You stopped init before Claude Code opened, so "+agents+" is still untrusted") {
		t.Errorf("after the user declined, setup reported %q", got)
	}
	if !strings.Contains(out, "Claude Code does not trust "+agents) || !strings.Contains(out, "run `asmai init` again") {
		t.Errorf("after the user declined, setup printed\n%s\nwant it to name %s and `asmai init`", out, agents)
	}
	if got := opened(testDir); len(got) != 0 {
		t.Errorf("after the user declined, setup had opened Claude Code in %q", got)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".claude.json")); err == nil {
		t.Error("after the user declined, the home holds ~/.claude.json")
	}

	again := setupInTerminal(t)
	again.trust(t, agents)
	again.trust(t, filepath.Join(stateDir, "workspaces"))
	if code := again.exited(t); code != 0 {
		t.Fatalf("asmai init again exited %d, printing\n%s", code, again.printed())
	}
}

// With a folder untrusted and stdin not a terminal, setup opens no Claude
// Code and exits 1, saying trusting it needs a terminal.
func TestSetupNeedsATerminalToTrustAFolder(t *testing.T) {
	stateDir, testDir := setupFactory(t, map[string][]string{"agents": trusting, "workspaces": trusting})

	var stdout, stderr bytes.Buffer
	code := run([]string{"init"}, strings.NewReader("y\n\n\n"), &stdout, &stderr)

	if code != 1 {
		t.Errorf("asmai init with no terminal exited %d, want 1", code)
	}
	agents := filepath.Join(stateDir, "agents")
	if got := lineSaying(stdout.String(), "trust agents/:"); !strings.HasPrefix(got, "trust agents/: failed.") {
		t.Errorf("setup with no terminal reported %q, want trust agents/ failed", got)
	}
	if want := "trusting " + agents + " needs you at a terminal"; !strings.Contains(stderr.String(), want) || !strings.Contains(stderr.String(), "Run `asmai init` in a terminal") {
		t.Errorf("setup's error is %q, want it to say %q", stderr.String(), want)
	}
	if strings.Contains(stdout.String(), "\x1b") {
		t.Errorf("setup styled its output for a terminal it does not have:\n%q", stdout.String())
	}
	if got := opened(testDir); len(got) != 0 {
		t.Errorf("with no terminal, setup opened Claude Code in %q", got)
	}

	var result struct {
		Error string      `json:"error"`
		Steps []setupStep `json:"steps"`
	}
	stdout.Reset()
	run([]string{"init", "--json"}, strings.NewReader(""), &stdout, &stderr)
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("asmai init --json printed %q: %v", stdout.String(), err)
	}
	if last := result.Steps[len(result.Steps)-1]; last.Step != "trust agents/" || last.State != stepFailed || !strings.Contains(last.Detail, "needs you at a terminal") {
		t.Errorf("asmai init --json reported its last step %+v, want trust agents/ failed for want of a terminal", last)
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

	if code != 1 || !strings.Contains(stderr, "failed  directory trust: Claude Code does not trust "+filepath.Join(stateDir, "workspaces")) || !strings.Contains(stderr, "fix: run `asmai init`") {
		t.Errorf("asmai start exited %d printing\n%s\nwant 1 and the directory trust check failed, naming `asmai init`", code, stderr)
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
	if code := run([]string{"init"}, strings.NewReader(""), &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "ask the user to run `asmai init`") {
		t.Errorf("an agent's asmai init exited %d printing %q, want it refused", code, stderr.String())
	}
}

// Setup's question before opening Claude Code takes only the answers it
// offers: Enter agrees, n declines, as does the end of stdin, and anything
// else asks the same question again.
func TestSetupAsksAgainUntilTheUserPressesEnterOrTypesN(t *testing.T) {
	const dir = "/state/agents"
	prompt := trustPrompt + dir + ", or type n and press Enter to stop setup: "
	for _, c := range []struct {
		input  string
		agreed bool
		asked  int
	}{
		{"\n", true, 1},
		{"n\n", false, 1},
		{"N\n", false, 1},
		{"", false, 1},
		{"y\nyes\nY\nno\nok\n\n", true, 6},
		{"y\nyes\nno\nN\n", false, 4},
		{"y\n", false, 2},
	} {
		var out bytes.Buffer
		if got := agreeToTrust(&out, strings.NewReader(c.input), dir, "setup"); got != c.agreed {
			t.Errorf("answering %q, the user agreed: %v, want %v", c.input, got, c.agreed)
		}
		if got := strings.Count(out.String(), prompt); got != c.asked {
			t.Errorf("answering %q, setup asked %d times, want %d; it printed %q", c.input, got, c.asked, out.String())
		}
	}
}

// With --yes, init installs the pinned Claude Code and opens Claude Code in
// each folder without asking either question, so that the only thing left
// for the user is Claude Code's own question about trusting the folder.
func TestInitYesAsksNeitherToInstallNorToOpenClaudeCode(t *testing.T) {
	stateDir, _ := setupFactory(t, map[string][]string{"agents": trusting, "workspaces": trusting})
	startUnready(t)

	s := setupInTerminal(t, "--yes")
	for _, dir := range []string{"agents", "workspaces"} {
		waitFor(t, "Claude Code to ask about trusting "+dir, s.shows("❯ No, exit"))
		s.typeKeys(t, "\x1b[B")
		waitFor(t, "Yes to be selected", s.shows("❯ Yes, I trust this folder"))
		s.typeKeys(t, "\r")
		waitFor(t, "Claude Code to come up", s.shows("Claude Code is ready"))
		s.typeKeys(t, "/exit\r")
	}
	if code := s.exited(t); code != 0 {
		t.Fatalf("asmai init --yes exited %d, printing\n%s", code, s.printed())
	}
	out := s.printed()
	if strings.Contains(out, "Claude default model (opus or sonnet)") || strings.Contains(out, "Install it? [y/N]") || strings.Contains(out, trustPrompt) {
		t.Errorf("asmai init --yes asked a question:\n%s", out)
	}
	for _, want := range []string{"Installing it: --yes confirms.", "Claude Code version: already done.", "permission rules: already done.", "trust workspaces/: done. Claude Code trusts " + filepath.Join(stateDir, "workspaces")} {
		if !strings.Contains(out, want) {
			t.Errorf("asmai init --yes printed\n%s\nwant it to say %q", out, want)
		}
	}
}

func TestInitYesAcceptsClaudeDefaultModelFlagWithoutPrompts(t *testing.T) {
	stateDir, _ := setupFactory(t, nil)
	stdout, stderr, code := runIn(t, "", "init", "--yes", "--claude-default-model", "sonnet")
	if code != 1 || !strings.Contains(stdout+stderr, "needs you at a terminal") {
		t.Fatalf("init --yes with an untrusted folder exited %d printing %q", code, stdout+stderr)
	}
	if strings.Contains(stdout+stderr, "Claude default model (opus or sonnet)") {
		t.Errorf("init --yes asked for a model: %q", stdout+stderr)
	}
	cfg, problems, err := config.Load(configPath())
	if err != nil || len(problems) != 0 || cfg.Providers["claude"].DefaultModel != "sonnet" {
		t.Fatalf("init saved model %q with problems %v and error %v, want sonnet", cfg.Providers["claude"].DefaultModel, problems, err)
	}
	trustFactory(t, stateDir)
	stdout, stderr, code = runIn(t, "", "init", "--yes", "--claude-default-model", "sonnet")
	if code != 0 || !strings.Contains(stdout, "Init is done") {
		t.Errorf("init retry with the saved Sonnet model exited %d printing %q", code, stdout+stderr)
	}
	stdout, stderr, code = runIn(t, "", "init", "--yes", "--claude-default-model", "opus")
	if code != 1 || !strings.Contains(stdout+stderr, `has Claude default model "sonnet"`) || !strings.Contains(stdout+stderr, `requests "opus"`) {
		t.Errorf("init with a conflicting model flag exited %d printing %q", code, stdout+stderr)
	}
	cfg, problems, err = config.Load(configPath())
	if err != nil || len(problems) != 0 || cfg.Providers["claude"].DefaultModel != "sonnet" {
		t.Errorf("the existing model became %q with problems %v and error %v", cfg.Providers["claude"].DefaultModel, problems, err)
	}
}

func TestInitRejectsUnknownClaudeDefaultModelBeforeWritingConfig(t *testing.T) {
	setupFactory(t, nil)
	stdout, stderr, code := runIn(t, "", "init", "--yes", "--claude-default-model", "haiku")
	if code != 1 || !strings.Contains(stdout+stderr, "not opus or sonnet") {
		t.Errorf("init with an unknown model exited %d printing %q", code, stdout+stderr)
	}
	if _, err := os.Stat(configPath()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("init wrote a configuration despite the unknown model: %v", err)
	}
}
