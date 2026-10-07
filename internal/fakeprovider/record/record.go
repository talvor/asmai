// SPDX-License-Identifier: Apache-2.0

// Package record is the recorder that runs a real Claude Code session and
// writes what it drew, the input it was given and the hook payloads it sent
// as a script for the fake provider, with the Claude Code version it came
// from. Personal data is scrubbed and a recording holding a credential is
// refused. Like the fake, the recorder is for development tests only and is
// never built into the asmai executable. ../README.md documents it.
package record

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/creack/pty"
	"golang.org/x/term"
)

const usage = "usage: record-session --out FILE [--redact VALUE]... [-- CLAUDE [ARG]...]"

// columns and rows are the size of the terminal a session is recorded on.
const (
	columns = 80
	rows    = 24
)

// Provider is the provider the recorder records, as a recording names it.
const Provider = "Claude Code"

// hookEvents are the Claude Code events whose payloads are recorded.
var hookEvents = []string{
	"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "Notification",
	"Stop", "SubagentStop", "PreCompact", "SessionEnd",
}

// toolEvents are the events Claude Code matches against a tool name.
var toolEvents = map[string]bool{"PreToolUse": true, "PostToolUse": true}

// settle is how long the recorder keeps reading the terminal after Claude
// Code exits, for what it drew last.
const settle = time.Second

// hookQuiet is how long the terminal is quiet before the recorder takes a
// hook payload as coming after what was drawn.
// It waits at most hookWait, as Claude Code may go on drawing while its hook
// command runs.
const (
	hookQuiet = 20 * time.Millisecond
	hookWait  = 500 * time.Millisecond
)

// Main runs the recorder with args, the command line without the program
// name, and returns its exit code: 0 when the recording is written, 1 when the
// session could not be recorded or the recording was refused, and 2 when the
// recorder is used wrongly.
func Main(args []string, stdin *os.File, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "hook" {
		return hookMain(args[1:], stdin, stderr)
	}
	flags := flag.NewFlagSet("record-session", flag.ContinueOnError)
	flags.SetOutput(stderr)
	out := flags.String("out", "", "write the recording to `FILE`")
	var redact values
	flags.Var(&redact, "redact", "scrub `VALUE` as personal data (repeatable)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	command := flags.Args()
	if len(command) == 0 {
		command = []string{"claude"}
	}
	if *out == "" {
		fmt.Fprintln(stderr, usage)
		return 2
	}

	version, err := claudeVersion(command[0])
	if err != nil {
		fmt.Fprintf(stderr, "record-session: %v\n", err)
		return 1
	}
	dir, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "record-session: %v\n", err)
		return 1
	}
	scrub := newScrubber(dir, redact)

	steps, err := capture(command, stdin, stdout)
	if err != nil {
		fmt.Fprintf(stderr, "record-session: %v\n", err)
		return 1
	}
	header := recorded{Provider: Provider, Version: version, Columns: columns, Rows: rows}
	script, err := write(header, steps, scrub)
	if err != nil {
		fmt.Fprintf(stderr, "record-session: refusing to write %s: %v; nothing was written\n", *out, err)
		return 1
	}
	if err := os.WriteFile(*out, script, 0o644); err != nil {
		fmt.Fprintf(stderr, "record-session: %v\n", err)
		return 1
	}
	fmt.Fprintf(stderr, "record-session: recorded %d steps from %s %s to %s\n", len(steps), Provider, version, *out)
	return 0
}

type values []string

func (v *values) String() string { return strings.Join(*v, ",") }

func (v *values) Set(s string) error {
	if s == "" {
		return errors.New("want a value")
	}
	*v = append(*v, s)
	return nil
}

var versionLine = regexp.MustCompile(`^([0-9]+\.[0-9]+\.[0-9]+\S*) \(Claude Code\)$`)

// claudeVersion asks claude for its version, the way `claude --version`
// prints it: "2.1.292 (Claude Code)".
func claudeVersion(claude string) (string, error) {
	out, err := exec.Command(claude, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("%s --version: %v", claude, err)
	}
	m := versionLine.FindStringSubmatch(strings.TrimSpace(string(out)))
	if m == nil {
		return "", fmt.Errorf("%s --version does not name a Claude Code version", claude)
	}
	return m[1], nil
}

// The kinds of step, named as the script format names them.
const (
	kindScreen = "screen"
	kindExpect = "expect"
	kindHook   = "hook"
)

// A capturedStep is a step as Claude Code produced it, before scrubbing.
type capturedStep struct {
	kind string
	data []byte
	hook string
}

// steps collects a session's steps in the order the recorder receives them.
// Consecutive output makes one screen, and consecutive input one expected
// input.
type steps struct {
	list []capturedStep
	// partial is the start of a character a screen ended in the middle of,
	// carried to the next screen so that each screen is whole text.
	partial []byte
}

func (s *steps) add(kind string, data []byte, hook string) {
	if kind == kindScreen {
		data = append(s.partial, data...)
		s.partial = nil
	}
	if n := len(s.list); n > 0 && kind != kindHook && s.list[n-1].kind == kind {
		s.list[n-1].data = append(s.list[n-1].data, data...)
		return
	}
	if n := len(s.list); n > 0 && s.list[n-1].kind == kindScreen {
		last := &s.list[n-1]
		if cut := incompleteTail(last.data); cut < len(last.data) {
			s.partial = append([]byte(nil), last.data[cut:]...)
			last.data = last.data[:cut]
			if len(last.data) == 0 {
				s.list = s.list[:n-1]
			}
		}
	}
	if len(data) > 0 || kind == kindHook {
		s.list = append(s.list, capturedStep{kind: kind, data: append([]byte(nil), data...), hook: hook})
	}
}

// incompleteTail returns where the unfinished character at the end of b
// starts, or len(b) when b ends with a whole one.
func incompleteTail(b []byte) int {
	for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax; i-- {
		if utf8.RuneStart(b[i]) {
			if !utf8.FullRune(b[i:]) {
				return i
			}
			break
		}
	}
	return len(b)
}

func (s *steps) done() []capturedStep {
	s.add(kindScreen, nil, "")
	return s.list
}

// An event is one thing the session did: draw output, receive input or send
// a hook payload. A hook's sender waits for done before it returns, so Claude
// Code goes on only after its payload is recorded.
type event struct {
	kind string
	data []byte
	hook string
	done chan struct{}
}

// capture runs command in a pseudo-terminal of columns by rows with a hook
// command configured for each of hookEvents, and returns the session's steps
// once it exits. It copies what the session draws to stdout and what is typed
// on stdin to the session, so a person, or tmux, can drive it.
func capture(command []string, stdin *os.File, stdout io.Writer) ([]capturedStep, error) {
	tmp, err := os.MkdirTemp("", "record-session")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	socket := filepath.Join(tmp, "hooks.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return nil, err
	}
	defer listener.Close()
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	settings := filepath.Join(tmp, "settings.json")
	if err := os.WriteFile(settings, hookSettings(exe, socket), 0o600); err != nil {
		return nil, err
	}

	args := append(append([]string(nil), command[1:]...), "--settings", settings)
	cmd := exec.Command(command[0], args...)
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(rows), Cols: uint16(columns)})
	if err != nil {
		return nil, err
	}
	defer ptmx.Close()
	if term.IsTerminal(int(stdin.Fd())) {
		state, err := term.MakeRaw(int(stdin.Fd()))
		if err != nil {
			cmd.Process.Kill()
			cmd.Wait()
			return nil, err
		}
		defer term.Restore(int(stdin.Fd()), state)
	}

	output, input, hooks := make(chan event), make(chan event), make(chan event)
	stop := make(chan struct{})
	defer close(stop)
	sender := func(events chan<- event) func(event) bool {
		return func(e event) bool {
			select {
			case events <- e:
				return true
			case <-stop:
				return false
			}
		}
	}
	outputEnded := make(chan struct{})
	go func() {
		defer close(outputEnded)
		send := sender(output)
		forward(ptmx, func(b []byte) bool { return send(event{kind: kindScreen, data: b}) })
	}()
	go func() {
		send := sender(input)
		forward(stdin, func(b []byte) bool { return send(event{kind: kindExpect, data: b}) })
	}()
	go serveHooks(listener, sender(hooks), stop)
	exited := make(chan struct{})
	go func() {
		cmd.Wait()
		close(exited)
	}()

	var session steps
	var settled <-chan time.Time
	draw := func(e event) {
		session.add(kindScreen, e.data, "")
		stdout.Write(e.data)
		if settled != nil {
			settled = time.After(settle)
		}
	}
	for {
		select {
		case e := <-output:
			draw(e)
		case e := <-input:
			// The input is recorded before Claude Code can see it, so
			// whatever it draws in answer comes after it.
			session.add(kindExpect, e.data, "")
			ptmx.Write(e.data)
		case e := <-hooks:
			// Claude Code ran the hook command after drawing what it drew
			// so far, which may still be on its way: take it in first.
			for quiet, deadline := false, time.After(hookWait); !quiet; {
				select {
				case o := <-output:
					draw(o)
				case <-time.After(hookQuiet):
					quiet = true
				case <-deadline:
					quiet = true
				}
			}
			session.add(kindHook, e.data, e.hook)
			close(e.done)
		case <-exited:
			exited = nil
			settled = time.After(settle)
		case <-outputEnded:
			outputEnded = nil
			if exited == nil {
				return session.done(), nil
			}
		case <-settled:
			return session.done(), nil
		}
	}
}

// forward reads r until it fails, passing each read to send until send
// returns false.
func forward(r io.Reader, send func([]byte) bool) {
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 && !send(append([]byte(nil), buf[:n]...)) {
			return
		}
		if err != nil {
			return
		}
	}
}

// hookSettings is the Claude Code settings that run `exe hook SOCKET EVENT`
// for each of hookEvents, handing every payload to the recorder.
func hookSettings(exe, socket string) []byte {
	type command struct {
		Type    string `json:"type"`
		Command string `json:"command"`
	}
	type matcher struct {
		Matcher string    `json:"matcher,omitempty"`
		Hooks   []command `json:"hooks"`
	}
	hooks := map[string][]matcher{}
	for _, e := range hookEvents {
		m := matcher{Hooks: []command{{Type: "command", Command: strings.Join([]string{shellQuote(exe), "hook", shellQuote(socket), e}, " ")}}}
		if toolEvents[e] {
			m.Matcher = "*"
		}
		hooks[e] = []matcher{m}
	}
	settings, _ := json.Marshal(map[string]any{"hooks": hooks})
	return settings
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// recorded is a recording's first line.
type recorded struct {
	Provider string `json:"provider"`
	Version  string `json:"version"`
	Columns  int    `json:"columns"`
	Rows     int    `json:"rows"`
}

// write scrubs steps and writes them, after header, as a script for the fake
// provider. It refuses, naming the step and what it holds but never the value,
// when a step still holds a credential or personal data, or when a screen as
// Claude Code drew it holds a fragment of one around a cursor movement.
func write(header recorded, captured []capturedStep, scrub *scrubber) ([]byte, error) {
	var script bytes.Buffer
	enc := json.NewEncoder(&script)
	enc.SetEscapeHTML(false)
	enc.Encode(map[string]recorded{"recorded": header})
	var screens, drawn strings.Builder
	for i, c := range captured {
		var line any
		var text string
		switch c.kind {
		case kindHook:
			payload := scrub.scrubPayload(c.data)
			text = string(payload)
			line = struct {
				Hook    string          `json:"hook"`
				Payload json.RawMessage `json:"payload"`
			}{c.hook, payload}
		default:
			text = scrub.scrub(string(c.data))
			line = map[string]string{c.kind: text}
			if c.kind == kindScreen {
				screens.WriteString(text)
				drawn.Write(c.data)
				if what := scrub.fragment(string(c.data)); what != "" {
					return nil, fmt.Errorf("step %d (%s) holds %s", i+1, c.kind, what)
				}
			}
		}
		if what := scrub.problem(text); what != "" {
			return nil, fmt.Errorf("step %d (%s) holds %s", i+1, c.kind, what)
		}
		if err := enc.Encode(line); err != nil {
			return nil, err
		}
	}
	// A value drawn across two screens is in neither on its own.
	if what := scrub.problem(screens.String()); what != "" {
		return nil, fmt.Errorf("the screens together hold %s", what)
	}
	if what := scrub.fragment(drawn.String()); what != "" {
		return nil, fmt.Errorf("the screens together hold %s", what)
	}
	return script.Bytes(), nil
}

// Check reports what a recorded script holds that no recording may: a
// credential or a fragment of one around a cursor movement, an email address
// that is not a placeholder, an identifier that is not a placeholder, or a
// home directory that is not the placeholder's. It names the line, never the
// value.
func Check(script []byte) error {
	var screens strings.Builder
	for n, line := range bytes.Split(script, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var step map[string]json.RawMessage
		if err := json.Unmarshal(line, &step); err != nil {
			return fmt.Errorf("line %d: %v", n+1, err)
		}
		for field, value := range step {
			text := string(value)
			if field == kindScreen || field == kindExpect {
				json.Unmarshal(value, &text)
			}
			if field == kindScreen {
				screens.WriteString(text)
				if credentialFragments.in(text) {
					return fmt.Errorf("line %d holds %s", n+1, credentialFragment)
				}
			}
			if what := genericProblem(text); what != "" {
				return fmt.Errorf("line %d holds %s", n+1, what)
			}
			if what := genericProblem(ansiPattern.ReplaceAllString(text, "")); what != "" {
				return fmt.Errorf("line %d holds %s", n+1, what)
			}
		}
	}
	if what := genericProblem(ansiPattern.ReplaceAllString(screens.String(), "")); what != "" {
		return fmt.Errorf("the screens together hold %s", what)
	}
	if credentialFragments.in(screens.String()) {
		return fmt.Errorf("the screens together hold %s", credentialFragment)
	}
	return nil
}
