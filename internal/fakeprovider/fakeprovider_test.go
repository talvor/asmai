// SPDX-License-Identifier: Apache-2.0

package fakeprovider_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
)

// fakeProvider is the fake-provider executable, built once for every test.
var fakeProvider string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "fake-provider")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fakeProvider = filepath.Join(dir, "fake-provider")
	build := exec.Command("go", "build", "-o", fakeProvider, "./cmd/fake-provider")
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "building fake-provider:", err)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// session is the fake provider running in a pseudo-terminal, as the daemon
// will run a provider CLI.
type session struct {
	t      *testing.T
	cmd    *exec.Cmd
	ptmx   *os.File
	stderr bytes.Buffer

	mu     sync.Mutex
	screen bytes.Buffer // everything the fake drew on its terminal
}

func start(t *testing.T, args ...string) *session {
	t.Helper()
	s := &session{t: t, cmd: exec.Command(fakeProvider, args...)}
	s.cmd.Stderr = &s.stderr
	ptmx, err := pty.StartWithSize(s.cmd, &pty.Winsize{Rows: 24, Cols: 80})
	if err != nil {
		t.Fatal(err)
	}
	s.ptmx = ptmx
	t.Cleanup(func() {
		s.cmd.Process.Kill()
		ptmx.Close()
	})
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			s.mu.Lock()
			s.screen.Write(buf[:n])
			s.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	return s
}

func (s *session) drawn() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.screen.String()
}

// waitForScreen waits until the fake has drawn want on its terminal.
func (s *session) waitForScreen(want string) {
	s.t.Helper()
	if !eventually(func() bool { return strings.Contains(s.drawn(), want) }) {
		s.t.Fatalf("fake never drew %q; its terminal shows %q", want, s.drawn())
	}
}

// typeKeys types keys on the fake's terminal.
func (s *session) typeKeys(keys string) {
	s.t.Helper()
	if _, err := s.ptmx.WriteString(keys); err != nil {
		s.t.Fatal(err)
	}
}

// patience is how long a test waits for the fake to draw, deliver or exit.
const patience = 10 * time.Second

func eventually(cond func() bool) bool {
	for deadline := time.Now().Add(patience); !cond(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			return false
		}
	}
	return true
}

// exit waits for the fake to exit and returns its exit code.
func (s *session) exit() int {
	s.t.Helper()
	exited := make(chan error, 1)
	go func() { exited <- s.cmd.Wait() }()
	select {
	case <-exited:
	case <-time.After(patience):
		s.t.Fatalf("fake did not exit; its terminal shows %q", s.drawn())
	}
	return s.cmd.ProcessState.ExitCode()
}

func writeScript(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "script.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTheFakeDrawsItsScreensInOrderAfterNamingItselfAFake(t *testing.T) {
	script := writeScript(t,
		`{"screen": "\u001b[2J\u001b[Hwelcome\r\n"}`,
		`{"screen": "\u001b[1mready\u001b[0m\r\n"}`,
	)
	s := start(t, "--script", script)
	s.waitForScreen("ready")
	if code := s.exit(); code != 0 {
		t.Errorf("fake exited %d, want 0 (stderr %q)", code, s.stderr.String())
	}

	drawn := s.drawn()
	banner := strings.Index(drawn, "fake provider")
	welcome := strings.Index(drawn, "\x1b[2J\x1b[Hwelcome\r\n")
	ready := strings.Index(drawn, "\x1b[1mready\x1b[0m\r\n")
	if banner < 0 || !strings.Contains(drawn, "not a qualified provider") {
		t.Errorf("fake did not name itself a fake: %q", drawn)
	}
	if !(banner < welcome && welcome < ready) {
		t.Errorf("fake drew %q, want its banner, then welcome, then ready", drawn)
	}
}

// hookLog is where the hooks configured by hookArgs record each delivery, one
// line per payload: the event name, a tab, and the payload as delivered.
type hookLog string

// hookArgs configures a hook command for each event that appends the event
// name and its payload to log.
func hookArgs(log hookLog, events ...string) []string {
	var args []string
	for _, event := range events {
		args = append(args, "--hook", fmt.Sprintf(`%[1]s=printf '%%s\t' %[1]s >> '%[2]s'; cat >> '%[2]s'; echo >> '%[2]s'`, event, log))
	}
	return args
}

func (l hookLog) deliveries(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(string(l))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	// A hook writes its line in several appends, so a line not yet ended by a
	// newline is a delivery still being written.
	complete := data[:bytes.LastIndexByte(data, '\n')+1]
	if len(complete) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(complete), "\n"), "\n")
}

// startSample starts the fake on the sample script, with a hook command for
// each of the sample's events.
func startSample(t *testing.T) (*session, hookLog) {
	t.Helper()
	log := hookLog(filepath.Join(t.TempDir(), "hooks.log"))
	args := append([]string{"--script", "testdata/sample.jsonl"}, hookArgs(log, "SessionStart", "UserPromptSubmit", "Stop")...)
	return start(t, args...), log
}

func TestTheFakePlaysTheSampleScriptReactingToInputAndDeliveringHooks(t *testing.T) {
	s, log := startSample(t)

	s.waitForScreen("\x1b[2J\x1b[H> \r\n")
	sessionStart := `SessionStart	{"session_id": "fake-session", "transcript_path": "/tmp/fake-session.jsonl", "cwd": "/tmp", "hook_event_name": "SessionStart", "source": "startup"}`
	if got := log.deliveries(t); !slices.Equal(got, []string{sessionStart}) {
		t.Fatalf("before any input, hooks received %q, want only SessionStart", got)
	}

	s.typeKeys("hello\r")
	s.waitForScreen("Hello! How can I help?\r\n\r\n> \r\n")
	want := []string{
		sessionStart,
		`UserPromptSubmit	{"session_id": "fake-session", "transcript_path": "/tmp/fake-session.jsonl", "cwd": "/tmp", "hook_event_name": "UserPromptSubmit", "prompt": "hello"}`,
		`Stop	{"session_id": "fake-session", "transcript_path": "/tmp/fake-session.jsonl", "cwd": "/tmp", "hook_event_name": "Stop", "stop_hook_active": false}`,
	}
	// The Stop hook comes after the screen just drawn, so it may still be on
	// its way.
	if !eventually(func() bool { return slices.Equal(log.deliveries(t), want) }) {
		got := log.deliveries(t)
		t.Errorf("hooks received\n%q\nwant\n%q", got, want)
	}

	s.typeKeys("/exit\r")
	if code := s.exit(); code != 0 {
		t.Errorf("fake exited %d, want 0 (stderr %q)", code, s.stderr.String())
	}
	drawn := s.drawn()
	screens := []string{
		"\x1b[2J\x1b[H> \r\n",
		"\x1b[2J\x1b[H> hello\r\n",
		"\x1b[32m●\x1b[0m Hello! How can I help?\r\n\r\n> \r\n",
	}
	// The terminal is raw, so the keys typed are not echoed: the fake draws
	// only its banner and the scripted screens.
	banner := "asmai fake provider: scripted for development tests, not a qualified provider\r\n"
	if want := banner + strings.Join(screens, ""); drawn != want {
		t.Errorf("fake drew\n%q\nwant its banner and the sample's screens\n%q", drawn, want)
	}
}

func TestTheFakeStopsAtInputTheScriptDoesNotExpect(t *testing.T) {
	s, log := startSample(t)

	s.waitForScreen("\x1b[2J\x1b[H> \r\n")
	s.typeKeys("help\r")
	if code := s.exit(); code != 1 {
		t.Errorf("fake exited %d, want 1", code)
	}
	if want := `expected input "hello\r", got "help"`; !strings.Contains(s.stderr.String(), want) {
		t.Errorf("stderr %q does not say %q", s.stderr.String(), want)
	}
	if got := log.deliveries(t); len(got) != 1 {
		t.Errorf("hooks received %q, want only SessionStart", got)
	}
}

func TestTheFakeRefusesAScriptItCannotPlay(t *testing.T) {
	for name, tc := range map[string]struct {
		line, want string
	}{
		"two kinds in one step":  {`{"screen": "a", "expect": "b"}`, `exactly one of "screen", "expect" and "hook"`},
		"a hook without payload": {`{"hook": "Stop"}`, `a "hook" step has a "payload"`},
		"a payload on a screen":  {`{"screen": "a", "payload": {}}`, `only a "hook" step has a "payload"`},
		"an empty screen":        {`{"screen": ""}`, `a "screen" step draws something`},
		"an unknown field":       {`{"screen": "a", "delay": 3}`, `unknown field "delay"`},
		"not JSON":               {`screen: a`, `invalid character`},
	} {
		t.Run(name, func(t *testing.T) {
			script := writeScript(t, `{"screen": "fine"}`, tc.line)
			s := start(t, "--script", script)
			if code := s.exit(); code != 2 {
				t.Errorf("fake exited %d, want 2", code)
			}
			for _, want := range []string{"line 2: ", tc.want} {
				if !strings.Contains(s.stderr.String(), want) {
					t.Errorf("stderr %q does not say %q", s.stderr.String(), want)
				}
			}
		})
	}
}
