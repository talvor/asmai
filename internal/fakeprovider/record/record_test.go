// SPDX-License-Identifier: Apache-2.0

package record_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
	xterm "golang.org/x/term"

	"github.com/talvor/asmai/internal/fakeprovider/record"
)

// recordSession and fakeProvider are the recorder and the fake provider,
// built once for every test.
var recordSession, fakeProvider string

// stubEnv makes this test executable act as Claude Code for the recorder to
// record, so the tests need no real provider.
const stubEnv = "RECORD_SESSION_STUB"

func TestMain(m *testing.M) {
	if os.Getenv(stubEnv) != "" {
		os.Exit(stub(os.Args[1:]))
	}
	dir, err := os.MkdirTemp("", "record-session")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	recordSession = filepath.Join(dir, "record-session")
	fakeProvider = filepath.Join(dir, "fake-provider")
	for out, pkg := range map[string]string{recordSession: "../cmd/record-session", fakeProvider: "../cmd/fake-provider"} {
		build := exec.Command("go", "build", "-o", out, pkg)
		build.Stdout, build.Stderr = os.Stderr, os.Stderr
		if err := build.Run(); err != nil {
			fmt.Fprintln(os.Stderr, "building", pkg+":", err)
			os.Exit(1)
		}
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// stubSessionID is the session identifier the stub's hook payloads carry.
const stubSessionID = "3f2a9c1e-5b7d-4e8a-9c0b-1d2e3f4a5b6c"

// stub is a stand-in for Claude Code, run as `stub --version` or
// `stub --settings FILE`. It greets its user by name and email and shows its
// working directory, waits for a prompt, answers it and waits for another to
// end the session, running the hooks in FILE as Claude Code would. With
// STUB_LEAK set, its answer also draws something more: mostly what the
// recorder must refuse, whole or in pieces around cursor movements.
func stub(args []string) int {
	if slices.Equal(args, []string{"--version"}) {
		fmt.Println("9.9.9 (Claude Code)")
		return 0
	}
	if len(args) != 2 || args[0] != "--settings" {
		fmt.Fprintln(os.Stderr, "stub: want --settings FILE")
		return 2
	}
	data, err := os.ReadFile(args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "stub:", err)
		return 2
	}
	var settings struct {
		Hooks map[string][]struct {
			Hooks []struct{ Command string }
		}
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		fmt.Fprintln(os.Stderr, "stub:", err)
		return 2
	}
	cwd, _ := os.Getwd()
	home, _ := os.UserHomeDir()
	project := strings.NewReplacer("/", "-", ".", "-").Replace(cwd)
	hook := func(event string, fields map[string]any) {
		payload := map[string]any{
			"session_id":      stubSessionID,
			"transcript_path": filepath.Join(home, ".claude", "projects", project, stubSessionID+".jsonl"),
			"cwd":             cwd,
			"hook_event_name": event,
		}
		for k, v := range fields {
			payload[k] = v
		}
		body, _ := json.Marshal(payload)
		for _, m := range settings.Hooks[event] {
			for _, h := range m.Hooks {
				cmd := exec.Command("/bin/sh", "-c", h.Command)
				cmd.Stdin = bytes.NewReader(body)
				cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
				if err := cmd.Run(); err != nil {
					fmt.Fprintf(os.Stderr, "stub: %s hook: %v\n", event, err)
				}
			}
		}
	}
	// With STUB_ECHO=masked, the stub echoes each key typed as "*", as a
	// password prompt does, and Ctrl-U erases what was typed.
	masked := os.Getenv("STUB_ECHO") == "masked"
	readLine := func() string {
		var line []byte
		b := make([]byte, 1)
		for {
			if _, err := os.Stdin.Read(b); err != nil || b[0] == '\r' {
				return string(line)
			}
			switch {
			case masked && b[0] == 0x15:
				line = nil
				os.Stdout.WriteString("\r\x1b[K> ")
			case masked:
				line = append(line, b[0])
				os.Stdout.WriteString("*")
			default:
				line = append(line, b[0])
				os.Stdout.Write(b)
			}
		}
	}
	// Like Claude Code, the stub reads its terminal raw and echoes what is
	// typed itself.
	if state, err := xterm.MakeRaw(int(os.Stdin.Fd())); err == nil {
		defer xterm.Restore(int(os.Stdin.Fd()), state)
	}

	hook("SessionStart", map[string]any{"source": "startup"})
	fmt.Printf("\x1b[2J\x1b[H\x1b[1mWelcome back Alice!\x1b[0m alice@corp.example\r\n%s\r\n> ", cwd)
	prompt := readLine()
	hook("UserPromptSubmit", map[string]any{"prompt": prompt})
	answer := "\r\n\x1b[32m●\x1b[0m ok"
	switch os.Getenv("STUB_LEAK") {
	case "token":
		answer += " sk-ant-oat01-" + strings.Repeat("Q", 24)
	case "environment":
		answer += " " + os.Getenv("STUB_SECRET_TOKEN")
	case "styled email":
		answer += " bob\x1b[1m@corp.example\x1b[0m"
	case "partial token":
		answer += " sk-ant\x1b[10G-oat01"
	case "partial name":
		answer += "\x1b[3;1HCaroli\x1b[3;10G!"
	case "wrapped reply":
		// Pieces of the names the recorder is given, but ordinary words.
		answer += " the first\r\n\x1b[1Gline of the file\x1b[K\r\n\x1b[1Glocal changes"
	case "partial redraw":
		// Redraw the working directory's line, skipping the cells that
		// did not change, as Claude Code's renderer does.
		answer += fmt.Sprintf("\x1b[2;1H%s\x1b[%dG%s", cwd[:len(cwd)-3], len(cwd)-1, cwd[len(cwd)-2:])
	}
	fmt.Print(answer)
	hook("Stop", map[string]any{"stop_hook_active": false})
	fmt.Print("\r\n> ")
	readLine()
	hook("SessionEnd", map[string]any{"reason": "prompt_input_exit"})
	fmt.Print("\r\nbye\r\n")
	return 0
}

// terminal is a program running in a pseudo-terminal, as a person's terminal
// or tmux would run it.
type terminal struct {
	t      *testing.T
	cmd    *exec.Cmd
	ptmx   *os.File
	stderr bytes.Buffer

	mu     sync.Mutex
	screen bytes.Buffer
}

func startTerminal(t *testing.T, cmd *exec.Cmd, columns, rows int) *terminal {
	t.Helper()
	term := &terminal{t: t, cmd: cmd}
	cmd.Stderr = &term.stderr
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(rows), Cols: uint16(columns)})
	if err != nil {
		t.Fatal(err)
	}
	term.ptmx = ptmx
	t.Cleanup(func() {
		cmd.Process.Kill()
		ptmx.Close()
	})
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			term.mu.Lock()
			term.screen.Write(buf[:n])
			term.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	return term
}

func (term *terminal) drawn() string {
	term.mu.Lock()
	defer term.mu.Unlock()
	return term.screen.String()
}

func (term *terminal) waitFor(what string, cond func() bool) {
	term.t.Helper()
	if !eventually(cond) {
		term.t.Fatalf("never %s; the terminal shows %q", what, term.drawn())
	}
}

func (term *terminal) waitForScreen(want string) {
	term.t.Helper()
	term.waitFor(fmt.Sprintf("drew %q", want), func() bool { return strings.Contains(term.drawn(), want) })
}

func (term *terminal) typeKeys(keys string) {
	term.t.Helper()
	if _, err := term.ptmx.WriteString(keys); err != nil {
		term.t.Fatal(err)
	}
}

func (term *terminal) exit() int {
	term.t.Helper()
	exited := make(chan error, 1)
	go func() { exited <- term.cmd.Wait() }()
	select {
	case <-exited:
	case <-time.After(patience):
		term.t.Fatalf("did not exit; the terminal shows %q", term.drawn())
	}
	return term.cmd.ProcessState.ExitCode()
}

const patience = 20 * time.Second

func eventually(cond func() bool) bool {
	for deadline := time.Now().Add(patience); !cond(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			return false
		}
	}
	return true
}

// recordStub records a session with the stub as Claude Code, in a working
// directory inside a home directory of its own, typing hello and then /exit.
// It has the recorder redact Alice, Caroline and a macOS host name.
// It returns the recorder's terminal and where the recording goes.
func recordStub(t *testing.T, env ...string) (*terminal, string) {
	t.Helper()
	return recordStubTyping(t, "", env...)
}

// recordStubTyping is recordStub typing keys, one at a time and each after
// the stub has drawn the last, before hello.
func recordStubTyping(t *testing.T, keys string, env ...string) (*terminal, string) {
	t.Helper()
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(tmp, "alice")
	dir := filepath.Join(home, "scratch")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "recording.jsonl")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(recordSession, "--out", out, "--redact", "Alice", "--redact", "Caroline", "--redact", "Someones-MacBook-Pro.local", "--", self)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HOME="+home, stubEnv+"=1")
	cmd.Env = append(cmd.Env, env...)
	term := startTerminal(t, cmd, 80, 24)
	term.waitForScreen("\r\n> ")
	for _, key := range keys {
		drawn := len(term.drawn())
		term.typeKeys(string(key))
		term.waitFor(fmt.Sprintf("drew key %q", key), func() bool { return len(term.drawn()) > drawn })
	}
	term.typeKeys("hello\r")
	// The second prompt comes after the Stop hook.
	term.waitFor("drew a second prompt", func() bool { return strings.Count(term.drawn(), "\r\n> ") == 2 })
	term.typeKeys("/exit\r")
	return term, out
}

// A step is one line of a recording, decoded.
type step struct {
	Recorded *struct {
		Provider, Version string
		Columns, Rows     int
	}
	Screen, Expect, Hook *string
	Payload              json.RawMessage
}

func readRecording(t *testing.T, path string) []step {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var steps []step
	for line := range strings.Lines(string(data)) {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var s step
		dec := json.NewDecoder(strings.NewReader(line))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&s); err != nil {
			t.Fatalf("%s: %v in %q", path, err, line)
		}
		steps = append(steps, s)
	}
	if len(steps) == 0 || steps[0].Recorded == nil {
		t.Fatalf("%s does not start with a recorded line", path)
	}
	return steps
}

// placeholderSession is what the recorder writes for the stub's session
// identifier: the first identifier it scrubbed.
const placeholderSession = "00000000-0000-4000-8000-000000000001"

func TestTheRecorderRecordsTheSessionsScreensInputAndHooksInOrderWithTheVersion(t *testing.T) {
	term, out := recordStub(t)
	if code := term.exit(); code != 0 {
		t.Fatalf("recorder exited %d, want 0; stderr %q", code, term.stderr.String())
	}
	steps := readRecording(t, out)

	if r := steps[0].Recorded; r.Provider != "Claude Code" || r.Version != "9.9.9" || r.Columns != 80 || r.Rows != 24 {
		t.Errorf("recorded %+v, want Claude Code 9.9.9 on 80x24", *r)
	}

	// The order the session did things in, each screen and input reduced to
	// what it shows.
	var got []string
	var screens, input strings.Builder
	payloads := map[string]map[string]any{}
	for _, s := range steps[1:] {
		switch {
		case s.Screen != nil:
			screens.WriteString(*s.Screen)
			if strings.Contains(*s.Screen, "Welcome") {
				got = append(got, "screen: welcome")
			}
			if strings.Contains(*s.Screen, "●\x1b[0m ok") {
				got = append(got, "screen: ok")
			}
			if strings.Contains(*s.Screen, "bye") {
				got = append(got, "screen: bye")
			}
		case s.Expect != nil:
			input.WriteString(*s.Expect)
			if strings.HasSuffix(*s.Expect, "\r") {
				got = append(got, "input: "+strings.TrimSpace(input.String()))
				input.Reset()
			}
		case s.Hook != nil:
			got = append(got, "hook: "+*s.Hook)
			var p map[string]any
			if err := json.Unmarshal(s.Payload, &p); err != nil {
				t.Fatalf("%s payload %s: %v", *s.Hook, s.Payload, err)
			}
			payloads[*s.Hook] = p
		}
	}
	want := []string{
		"hook: SessionStart",
		"screen: welcome",
		"input: hello",
		"hook: UserPromptSubmit",
		"screen: ok",
		"hook: Stop",
		"input: /exit",
		"hook: SessionEnd",
		"screen: bye",
	}
	if !slices.Equal(got, want) {
		t.Errorf("recorded\n%q\nwant\n%q", got, want)
	}

	// Personal data is scrubbed, consistently across the session.
	for event, p := range payloads {
		for field, want := range map[string]any{
			"session_id":      placeholderSession,
			"cwd":             "/home/user/project",
			"transcript_path": "/home/user/.claude/projects/-home-user-project/" + placeholderSession + ".jsonl",
			"hook_event_name": event,
		} {
			if p[field] != want {
				t.Errorf("%s payload %s is %q, want %q", event, field, p[field], want)
			}
		}
	}
	if p := payloads["UserPromptSubmit"]["prompt"]; p != "hello" {
		t.Errorf("UserPromptSubmit prompt is %q, want hello", p)
	}
	for _, want := range []string{"Welcome back redacted!", "user@example.com", "/home/user/project\r\n"} {
		if !strings.Contains(screens.String(), want) {
			t.Errorf("screens %q do not show %q", screens.String(), want)
		}
	}
	data, _ := os.ReadFile(out)
	for _, personal := range []string{"alice", "Alice", "corp.example", stubSessionID, term.cmd.Dir} {
		if strings.Contains(string(data), personal) {
			t.Errorf("the recording holds %q", personal)
		}
	}
	if err := record.Check(data); err != nil {
		t.Errorf("Check: %v", err)
	}

	// What the recorder records, the fake plays.
	replay(t, out)
}

func TestTheRecorderRefusesToWriteARecordingHoldingACredentialOrPersonalData(t *testing.T) {
	const secret = "s3cr3t-from-the-environment"
	for leak, want := range map[string]string{
		"token":          "holds an Anthropic API key or OAuth token",
		"environment":    "holds the value of a credential in the environment",
		"styled email":   "holds an email address",
		"partial token":  "holds a fragment of a credential",
		"partial redraw": "holds a fragment of personal data from this host",
		"partial name":   "holds a fragment of personal data from this host",
	} {
		t.Run(leak, func(t *testing.T) {
			term, out := recordStub(t, "STUB_LEAK="+leak, "STUB_SECRET_TOKEN="+secret)
			if code := term.exit(); code != 1 {
				t.Errorf("recorder exited %d, want 1", code)
			}
			stderr := term.stderr.String()
			for _, want := range []string{"refusing to write", want, "nothing was written"} {
				if !strings.Contains(stderr, want) {
					t.Errorf("stderr %q does not say %q", stderr, want)
				}
			}
			for _, leaked := range []string{"sk-an", secret, "bob", "corp.example", "alice", "scra", "Caroli"} {
				if strings.Contains(stderr, leaked) {
					t.Errorf("stderr %q repeats %q", stderr, leaked)
				}
			}
			if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the recording was written (%v)", err)
			}
		})
	}
}

func TestTheRecorderRefusesToWriteACredentialOrPersonalDataTypedKeyByKey(t *testing.T) {
	const token = "sk-ant-oat01-QQQQQQQQQQQQQQQQQQQQQQQQ"
	for name, test := range map[string]struct{ keys, want string }{
		// Typed and erased, the credential never reaches the prompt.
		"an erased credential": {token + "\x15", "holds an Anthropic API key or OAuth token"},
		"a redacted name":      {"say Caroline\x15", "holds personal data from this host"},
	} {
		t.Run(name, func(t *testing.T) {
			term, out := recordStubTyping(t, test.keys, "STUB_ECHO=masked")
			if code := term.exit(); code != 1 {
				t.Errorf("recorder exited %d, want 1", code)
			}
			stderr := term.stderr.String()
			for _, want := range []string{"refusing to write", "the input typed up to step", test.want, "nothing was written"} {
				if !strings.Contains(stderr, want) {
					t.Errorf("stderr %q does not say %q", stderr, want)
				}
			}
			for _, leaked := range []string{"sk-an", "Caroli"} {
				if strings.Contains(stderr, leaked) {
					t.Errorf("stderr %q repeats %q", stderr, leaked)
				}
			}
			if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the recording was written (%v)", err)
			}
		})
	}
}

func TestTheRecorderAcceptsOrdinaryWordsThatArePiecesOfPersonalData(t *testing.T) {
	term, out := recordStub(t, "STUB_LEAK=wrapped reply")
	if code := term.exit(); code != 0 {
		t.Fatalf("recorder exited %d, want 0; stderr %q", code, term.stderr.String())
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := record.Check(data); err != nil {
		t.Errorf("Check: %v", err)
	}
}

func TestTheRecorderRefusesWhatIsNotClaudeCode(t *testing.T) {
	cmd := exec.Command(recordSession, "--out", filepath.Join(t.TempDir(), "x.jsonl"), "--", "/bin/echo")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if code := cmd.ProcessState.ExitCode(); code != 1 {
		t.Errorf("recorder exited %d (%v), want 1", code, err)
	}
	if want := "does not name a Claude Code version"; !strings.Contains(stderr.String(), want) {
		t.Errorf("stderr %q does not say %q", stderr.String(), want)
	}
}

// recordings are the sessions recorded from the real Claude Code.
var recordings, _ = filepath.Glob("../testdata/recorded/*.jsonl")

func TestARealClaudeCodeSessionIsRecorded(t *testing.T) {
	if len(recordings) == 0 {
		t.Fatal("no recording in ../testdata/recorded")
	}
	name := regexp.MustCompile(`^claude-code-([0-9]+\.[0-9]+\.[0-9]+)-[a-z0-9-]+\.jsonl$`)
	for _, path := range recordings {
		m := name.FindStringSubmatch(filepath.Base(path))
		if m == nil {
			t.Errorf("%s is not named claude-code-VERSION-WHAT.jsonl", path)
			continue
		}
		r := readRecording(t, path)[0].Recorded
		if r.Provider != "Claude Code" || r.Version != m[1] {
			t.Errorf("%s is recorded from %s %s, not Claude Code %s as its name says", path, r.Provider, r.Version, m[1])
		}
	}
}

func TestTheRecordedSessionsHoldNoCredentialsOrPersonalData(t *testing.T) {
	for _, path := range recordings {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := record.Check(data); err != nil {
			t.Errorf("%s: %v", path, err)
		}
	}
}

func TestCheckRefusesHomeDirectoriesAndCredentialFragments(t *testing.T) {
	script := func(steps ...map[string]any) []byte {
		var b bytes.Buffer
		for _, s := range steps {
			line, err := json.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			b.Write(append(line, '\n'))
		}
		return b.Bytes()
	}
	for name, test := range map[string]struct {
		script []byte
		want   string
	}{
		"a Linux home":           {script(map[string]any{"screen": "cd /home/phillip/scratch\r\n"}), "line 1 holds a home directory that is not the placeholder's"},
		"a macOS home":           {script(map[string]any{"screen": "ok"}, map[string]any{"hook": "Stop", "payload": map[string]any{"cwd": "/Users/phillip"}}), "line 2 holds a home directory that is not the placeholder's"},
		"a project directory":    {script(map[string]any{"hook": "Stop", "payload": map[string]any{"transcript_path": "/home/user/.claude/projects/-Users-phillip-scratch/x.jsonl"}}), "line 1 holds a home directory that is not the placeholder's"},
		"a credential redrawn":   {script(map[string]any{"screen": "key sk-ant\x1b[11G-oat01"}), "line 1 holds a fragment of a credential around a cursor movement"},
		"a credential split":     {script(map[string]any{"screen": "key sk-ant"}, map[string]any{"screen": "\x1b[11G-oat01"}), "the screens together hold a fragment of a credential around a cursor movement"},
		"a credential typed":     {script(map[string]any{"expect": "sk-ant-"}, map[string]any{"screen": "*"}, map[string]any{"expect": "oat01-QQQQQQQQ"}), "the input typed up to line 3 holds an Anthropic API key or OAuth token"},
		"the placeholder's home": {script(map[string]any{"screen": "~/project /home/user/project\r\n"}, map[string]any{"hook": "Stop", "payload": map[string]any{"transcript_path": "/home/user/.claude/projects/-home-user-project/x.jsonl"}}), ""},
	} {
		t.Run(name, func(t *testing.T) {
			err := record.Check(test.script)
			if got := fmt.Sprint(err); test.want == "" && err != nil || test.want != "" && got != test.want {
				t.Errorf("Check: %v, want %q", err, test.want)
			}
		})
	}
}

func TestTheFakeReplaysTheRecordedSessions(t *testing.T) {
	for _, path := range recordings {
		t.Run(filepath.Base(path), func(t *testing.T) { replay(t, path) })
	}
}

// banner is what the fake draws before its script.
const banner = "asmai fake provider: scripted for development tests, not a qualified provider\r\n"

// replay plays the recording at path through the fake provider, on a terminal
// of the size it was recorded on. Before typing each expected input, it waits
// until the fake has drawn exactly the screens before it and delivered exactly
// the hook payloads before it, so the fake plays the session in its order. At
// the end it checks that the fake drew every screen and delivered every
// payload, byte for byte, and exited 0.
func replay(t *testing.T, path string) {
	t.Helper()
	steps := readRecording(t, path)
	r := steps[0].Recorded

	log := filepath.Join(t.TempDir(), "hooks.log")
	args := []string{"--script", path}
	events := map[string]bool{}
	for _, s := range steps[1:] {
		if s.Hook != nil && !events[*s.Hook] {
			events[*s.Hook] = true
			args = append(args, "--hook", fmt.Sprintf(`%[1]s=printf '%%s\t' %[1]s >> '%[2]s'; cat >> '%[2]s'; echo >> '%[2]s'`, *s.Hook, log))
		}
	}
	deliveries := func() []string {
		data, err := os.ReadFile(log)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			t.Fatal(err)
		}
		// A delivery not yet ended by a newline is still being written.
		complete := data[:bytes.LastIndexByte(data, '\n')+1]
		if len(complete) == 0 {
			return nil
		}
		return strings.Split(strings.TrimSuffix(string(complete), "\n"), "\n")
	}

	fake := startTerminal(t, exec.Command(fakeProvider, args...), r.Columns, r.Rows)
	screens := banner
	var hooks []string
	for i, s := range steps[1:] {
		switch {
		case s.Screen != nil:
			screens += *s.Screen
		case s.Hook != nil:
			hooks = append(hooks, *s.Hook+"\t"+string(s.Payload))
		case s.Expect != nil:
			fake.waitFor(fmt.Sprintf("drew the screens before step %d", i+1), func() bool { return fake.drawn() == screens })
			fake.waitFor(fmt.Sprintf("delivered the payloads before step %d", i+1), func() bool { return slices.Equal(deliveries(), hooks) })
			fake.typeKeys(*s.Expect)
		}
	}
	if code := fake.exit(); code != 0 {
		t.Fatalf("fake exited %d, want 0; stderr %q", code, fake.stderr.String())
	}
	if !eventually(func() bool { return fake.drawn() == screens }) {
		t.Errorf("fake drew\n%q\nwant\n%q", fake.drawn(), screens)
	}
	if got := deliveries(); !slices.Equal(got, hooks) {
		t.Errorf("hooks received\n%q\nwant\n%q", got, hooks)
	}
}
