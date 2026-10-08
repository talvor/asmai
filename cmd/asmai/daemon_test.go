// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/fakeprovider"
)

// e2eVersion is the version the end-to-end tests build asmai with.
const e2eVersion = "v0.0.0-e2e"

// asmaiBin is the directory holding the asmai executable the end-to-end tests
// build once, and fakeProvider the fake provider they install as the pinned
// Claude Code.
var asmaiBin, fakeProvider string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "asmai-bin")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	asmaiBin = dir
	build := exec.Command("go", "build", "-ldflags", "-X main.version="+e2eVersion, "-o", filepath.Join(dir, "asmai"), ".")
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "building asmai:", err)
		os.Exit(1)
	}
	fakeProvider = filepath.Join(dir, "fake-provider")
	build = exec.Command("go", "build", "-o", fakeProvider, "../../internal/fakeprovider/cmd/fake-provider")
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "building the fake provider:", err)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// envSentinel is an environment variable's value that must never be recorded.
const envSentinel = "asmai-env-sentinel-value"

// factoryHome gives the test a home directory of its own, so the factory's
// state directory is new, with asmai on the PATH and a sentinel in the
// environment. It stops any daemon left when the test ends.
func factoryHome(t *testing.T) (stateDir string) {
	t.Helper()
	// os.MkdirTemp keeps the socket's path short enough on macOS.
	home, err := os.MkdirTemp("", "home")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", asmaiBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ASMAI_E2E_SENTINEL", envSentinel)
	t.Cleanup(func() {
		exec.Command(filepath.Join(asmaiBin, "asmai"), "stop").Run()
		os.RemoveAll(home)
	})
	return filepath.Join(home, ".local", "state", "asmai")
}

// runAsmai runs the built asmai and returns what it printed and its exit code.
func runAsmai(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(filepath.Join(asmaiBin, "asmai"), args...)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return out.String(), errOut.String(), exit.ExitCode()
	}
	if err != nil {
		t.Fatalf("asmai %s: %v", strings.Join(args, " "), err)
	}
	return out.String(), errOut.String(), 0
}

// asmaiJSON runs the built asmai with --json, requires it to succeed, and
// decodes what it printed into v.
func asmaiJSON(t *testing.T, v any, args ...string) {
	t.Helper()
	stdout, stderr, code := runAsmai(t, append(args, "--json")...)
	if code != 0 {
		t.Fatalf("asmai %s --json exited %d: %s%s", strings.Join(args, " "), code, stdout, stderr)
	}
	if err := json.Unmarshal([]byte(stdout), v); err != nil {
		t.Fatalf("asmai %s --json printed %q, which is not JSON: %v", strings.Join(args, " "), stdout, err)
	}
}

type daemonJSON struct {
	Running   bool      `json:"running"`
	Version   string    `json:"version"`
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
}

type startJSON struct {
	Started  bool       `json:"started"`
	Daemon   daemonJSON `json:"daemon"`
	StateDir string     `json:"state_dir"`
}

type journalJSON struct {
	Journal []struct {
		ID   int64  `json:"id"`
		Kind string `json:"kind"`
		Data struct {
			PID      int    `json:"pid"`
			Previous string `json:"previous"`
			By       string `json:"by"`
		} `json:"data"`
	} `json:"journal"`
}

func alive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

func waitForExit(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for alive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("the daemon, pid %d, is still running", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// agentScript is a scripted agent that looks at the factory with asmai from
// its shell, as Claude Code's Bash tool would.
var agentScript = []string{
	`{"screen": "> \r\n"}`,
	`{"run": "asmai status --json", "status": 0, "output": "\"running\": true"}`,
	`{"run": "asmai status", "status": 0, "output": "(?m)^version +` + strings.ReplaceAll(e2eVersion, ".", `\\.`) + `$"}`,
	`{"run": "asmai start", "status": 0, "output": "already running"}`,
	`{"run": "asmai export", "status": 0, "output": "(?m)^1 +\\S+ +daemon\\.started +pid=\\d+ previous=new version=` + strings.ReplaceAll(e2eVersion, ".", `\\.`) + `$"}`,
	`{"screen": "The factory is running.\r\n"}`,
}

func TestTheFactoryStartsStopsAndKeepsItsStoreWithAnAgentLookingOn(t *testing.T) {
	// The factory is ready to run, and stopped.
	stateDir := readyFactory(t, idleLeader)
	if _, stderr, code := runAsmai(t, "stop"); code != 0 {
		t.Fatalf("asmai stop exited %d: %s", code, stderr)
	}

	var status struct {
		Daemon daemonJSON `json:"daemon"`
	}
	asmaiJSON(t, &status, "status")
	if status.Daemon.Running {
		t.Fatalf("before the first start, status is %+v, want not running", status.Daemon)
	}

	// asmai start returns once the daemon answers, leaving it running.
	var first startJSON
	asmaiJSON(t, &first, "start")
	if !first.Started || !first.Daemon.Running || first.Daemon.Version != e2eVersion || first.StateDir != stateDir {
		t.Fatalf("asmai start printed %+v, want a started daemon of %s in %s", first, e2eVersion, stateDir)
	}
	pid := first.Daemon.PID
	if !alive(pid) {
		t.Fatalf("the daemon, pid %d, is not running after asmai start", pid)
	}

	// A second start reports the running factory and starts no daemon.
	var second startJSON
	asmaiJSON(t, &second, "start")
	if second.Started || second.Daemon.PID != pid {
		t.Errorf("a second start printed %+v, want the running daemon, pid %d, and no new one", second, pid)
	}

	// Commands reach the daemon only over a socket in the state directory
	// that only the user can read, and it listens on no network.
	for path, want := range map[string]fs.FileMode{stateDir: fs.ModeDir | 0o700, filepath.Join(stateDir, "daemon.sock"): fs.ModeSocket | 0o600} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode() & (fs.ModeType | fs.ModePerm); got != want {
			t.Errorf("%s has mode %v, want %v", path, got, want)
		}
	}
	if sockets := networkSockets(t, pid); sockets != "" {
		t.Errorf("the daemon has network sockets:\n%s", sockets)
	}

	// An agent session, played by the fake provider, looks at the factory
	// with asmai from its shell.
	script := filepath.Join(t.TempDir(), "agent.jsonl")
	if err := os.WriteFile(script, []byte(strings.Join(agentScript, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devNull.Close()
	var screen, agentErr bytes.Buffer
	if code := fakeprovider.Main([]string{"--script", script}, devNull, &screen, &agentErr); code != 0 {
		t.Fatalf("the agent exited %d: %s", code, agentErr.String())
	}
	if !strings.Contains(screen.String(), "The factory is running.") {
		t.Errorf("the agent drew %q", screen.String())
	}

	// Following the log shows the stop as it happens.
	follow := exec.Command(filepath.Join(asmaiBin, "asmai"), "log", "--follow")
	followOut, err := follow.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := follow.Start(); err != nil {
		t.Fatal(err)
	}
	defer follow.Process.Kill()
	followed := bufio.NewScanner(followOut)
	waitForLine := func(want string) {
		t.Helper()
		for followed.Scan() {
			if strings.Contains(followed.Text(), want) {
				return
			}
		}
		t.Fatalf("asmai log --follow ended before showing %q", want)
	}
	waitForLine("daemon started")

	// asmai stop persists the state and exits the daemon.
	var stopped struct {
		Stopped bool       `json:"stopped"`
		Daemon  daemonJSON `json:"daemon"`
	}
	asmaiJSON(t, &stopped, "stop")
	if !stopped.Stopped || stopped.Daemon.PID != pid {
		t.Errorf("asmai stop printed %+v, want daemon %d stopped", stopped, pid)
	}
	waitForExit(t, pid)
	waitForLine("daemon stopped")
	follow.Process.Signal(os.Interrupt)
	if err := follow.Wait(); err != nil {
		t.Errorf("asmai log --follow ended with %v after an interrupt, want exit 0", err)
	}

	asmaiJSON(t, &status, "status")
	if status.Daemon.Running {
		t.Errorf("after the stop, status is %+v, want not running", status.Daemon)
	}
	if stdout, _, code := runAsmai(t, "stop"); code != 0 || !strings.Contains(stdout, "not running") {
		t.Errorf("a second stop exited %d printing %q, want it to say the factory is not running", code, stdout)
	}
	if _, stderr, code := runAsmai(t, "export"); code != 1 || !strings.Contains(stderr, "asmai start") {
		t.Errorf("export with no daemon exited %d printing %q, want 1 and to say to start it", code, stderr)
	}

	// The next start finds the store as the stop left it.
	var third startJSON
	asmaiJSON(t, &third, "start")
	if !third.Started || third.Daemon.PID == pid {
		t.Errorf("the start after the stop printed %+v, want a new daemon", third)
	}
	var journal journalJSON
	asmaiJSON(t, &journal, "export")
	var got []string
	for _, e := range journal.Journal {
		if strings.HasPrefix(e.Kind, "daemon.") {
			got = append(got, fmt.Sprintf("%s pid=%d previous=%q by=%q", e.Kind, e.Data.PID, e.Data.Previous, e.Data.By))
		}
	}
	// The first start and stop made the factory ready.
	if len(got) < 2 || !strings.HasPrefix(got[0], "daemon.started") || !strings.HasPrefix(got[1], "daemon.stopped") {
		t.Fatalf("the journal holds %v, want the preparation's start and stop first", got)
	}
	got = got[2:]
	want := []string{
		fmt.Sprintf(`daemon.started pid=%d previous="stopped" by=""`, pid),
		fmt.Sprintf(`daemon.stopped pid=%d previous="" by="asmai stop"`, pid),
		fmt.Sprintf(`daemon.started pid=%d previous="stopped" by=""`, third.Daemon.PID),
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("the journal holds\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	logText, _, code := runAsmai(t, "log")
	if code != 0 || strings.Count(logText, "daemon started") != 3 || strings.Count(logText, "daemon stopped") != 2 {
		t.Errorf("asmai log exited %d and printed\n%s\nwant three starts and two stops", code, logText)
	}
	logJSON, _, _ := runAsmai(t, "log", "--json")
	for line := range strings.Lines(logJSON) {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil || record["msg"] == nil {
			t.Errorf("asmai log --json printed %q, not a log record (%v)", line, err)
		}
	}
	exported, _, _ := runAsmai(t, "export")
	asmaiJSON(t, &stopped, "stop")
	waitForExit(t, third.Daemon.PID)

	// No journal entry or log line records an environment variable.
	recorded := map[string]string{"asmai export": exported, "asmai log": logText, "asmai log --json": logJSON}
	err = filepath.WalkDir(stateDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// The executables AsmAI keeps and the agents' working directories
		// are not records.
		if d.IsDir() && slices.Contains([]string{"bin", "providers", "agents"}, d.Name()) {
			return fs.SkipDir
		}
		if d.IsDir() || d.Type()&fs.ModeSocket != 0 {
			return nil
		}
		data, err := os.ReadFile(path)
		recorded[path] = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for where, text := range recorded {
		for _, env := range []string{envSentinel, "ASMAI_E2E_SENTINEL", "PATH="} {
			if strings.Contains(text, env) {
				t.Errorf("%s records the environment: it holds %q", where, env)
			}
		}
	}
}

func TestStartForegroundKeepsTheDaemonAttachedUntilItIsStopped(t *testing.T) {
	factoryHome(t)
	daemon := exec.Command(filepath.Join(asmaiBin, "asmai"), "start", "--foreground")
	var shown bytes.Buffer
	daemon.Stdout, daemon.Stderr = &shown, &shown
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- daemon.Wait() }()

	var status struct {
		Daemon daemonJSON `json:"daemon"`
	}
	deadline := time.Now().Add(10 * time.Second)
	for !status.Daemon.Running {
		select {
		case err := <-exited:
			t.Fatalf("asmai start --foreground exited (%v) before the daemon answered:\n%s", err, shown.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the foreground daemon did not answer")
		}
		time.Sleep(20 * time.Millisecond)
		asmaiJSON(t, &status, "status")
	}
	if status.Daemon.PID != daemon.Process.Pid {
		t.Errorf("the daemon is pid %d, want the foreground process, pid %d", status.Daemon.PID, daemon.Process.Pid)
	}
	// The factory is not staffed, so its daemon runs without Coordination's
	// leader, and a start says so.
	if _, stderr, code := runAsmai(t, "start", "--foreground"); code != 1 || !strings.Contains(stderr, "The factory's daemon is running, but not Coordination's leader") {
		t.Errorf("a second start --foreground exited %d printing %q, want it to report the running daemon and the failed checks", code, stderr)
	}

	if _, stderr, code := runAsmai(t, "stop"); code != 0 {
		t.Fatalf("asmai stop exited %d: %s", code, stderr)
	}
	select {
	case err := <-exited:
		if err != nil {
			t.Errorf("asmai start --foreground ended with %v, want exit 0", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("asmai start --foreground did not exit after asmai stop")
	}
	for _, want := range []string{"INFO   daemon started  version=" + e2eVersion, "daemon stopped  by=\"asmai stop\""} {
		if !strings.Contains(shown.String(), want) {
			t.Errorf("the foreground daemon showed\n%s\nwant it to show %q", shown.String(), want)
		}
	}
}

func TestEveryCommandAcceptsJSON(t *testing.T) {
	readyFactory(t, idleLeader)
	for _, args := range [][]string{{"version"}, {"notices"}, {"status"}, {"start"}, {"agents"}, {"attach", "coordination"}, {"export"}, {"providers", "list"}, {"stop"}} {
		var v map[string]any
		asmaiJSON(t, &v, args...)
		if len(v) == 0 {
			t.Errorf("asmai %s --json printed an empty object", strings.Join(args, " "))
		}
	}
	// With --json, a failure is a JSON error too.
	stdout, _, code := runAsmai(t, "export", "--json")
	var failure struct {
		Error string `json:"error"`
	}
	if code != 1 || json.Unmarshal([]byte(stdout), &failure) != nil || failure.Error == "" {
		t.Errorf("a failing export --json exited %d printing %q, want 1 and a JSON error", code, stdout)
	}
	// asmai hook is run by agent sessions' hooks; anywhere else it fails.
	stdout, _, code = runAsmai(t, "hook", "--json")
	failure.Error = ""
	if code != 1 || json.Unmarshal([]byte(stdout), &failure) != nil || !strings.Contains(failure.Error, "ASMAI_SESSION") {
		t.Errorf("a failing hook --json exited %d printing %q, want 1 and a JSON error", code, stdout)
	}
}

// networkSockets lists the network sockets the process pid has open, or
// returns "" when it has none.
func networkSockets(t *testing.T, pid int) string {
	t.Helper()
	switch runtime.GOOS {
	case "linux":
		fds, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
		if err != nil {
			t.Fatal(err)
		}
		open := map[string]bool{}
		for _, fd := range fds {
			target, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/%s", pid, fd.Name()))
			if inode, ok := strings.CutPrefix(target, "socket:["); err == nil && ok {
				open[strings.TrimSuffix(inode, "]")] = true
			}
		}
		var found []string
		for _, proto := range []string{"tcp", "tcp6", "udp", "udp6", "raw", "raw6"} {
			table, err := os.ReadFile(fmt.Sprintf("/proc/%d/net/%s", pid, proto))
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			for i, line := range strings.Split(string(table), "\n") {
				if fields := strings.Fields(line); i > 0 && len(fields) > 9 && open[fields[9]] {
					found = append(found, proto+": "+line)
				}
			}
		}
		return strings.Join(found, "\n")
	case "darwin":
		out, err := exec.Command("lsof", "-a", "-n", "-P", "-p", fmt.Sprint(pid), "-i").Output()
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 && len(out) == 0 {
			return ""
		}
		if err != nil {
			t.Fatalf("lsof: %v", err)
		}
		return string(out)
	default:
		t.Skipf("no way to list a process's network sockets on %s", runtime.GOOS)
		return ""
	}
}
