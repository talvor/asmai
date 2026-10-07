// SPDX-License-Identifier: Apache-2.0

package fakeprovider

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"golang.org/x/term"
)

const usage = "usage: fake-provider --script FILE [--hook EVENT=COMMAND]..."

// Main runs the fake provider CLI with args, the command line without the
// program name, and returns its exit code.
func Main(args []string, stdin *os.File, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("fake-provider", flag.ContinueOnError)
	flags.SetOutput(stderr)
	scriptPath := flags.String("script", "", "the script to play")
	hooks := hookCommands{}
	flags.Var(hooks, "hook", "run `EVENT=COMMAND` for each payload of EVENT (repeatable)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *scriptPath == "" || flags.NArg() != 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	f, err := os.Open(*scriptPath)
	if err != nil {
		fmt.Fprintf(stderr, "fake-provider: %v\n", err)
		return 2
	}
	steps, err := readScript(f)
	f.Close()
	if err != nil {
		fmt.Fprintf(stderr, "fake-provider: %s: %v\n", *scriptPath, err)
		return 2
	}

	// Like the real providers, the fake reads its terminal in raw mode: every
	// key arrives as typed, Enter as "\r", and nothing is echoed unless a
	// screen draws it.
	if term.IsTerminal(int(stdin.Fd())) {
		state, err := term.MakeRaw(int(stdin.Fd()))
		if err != nil {
			fmt.Fprintf(stderr, "fake-provider: %v\n", err)
			return 1
		}
		defer term.Restore(int(stdin.Fd()), state)
	}

	if err := play(steps, stdin, stdout, hooks.deliver(stderr)); err != nil {
		fmt.Fprintf(stderr, "fake-provider: %v\n", err)
		return 1
	}
	return 0
}

// hookCommands maps each event name to the shell command its payloads are
// delivered to.
type hookCommands map[string]string

func (h hookCommands) String() string { return "" }

func (h hookCommands) Set(v string) error {
	event, command, ok := strings.Cut(v, "=")
	if !ok || event == "" || command == "" {
		return errors.New("want EVENT=COMMAND")
	}
	h[event] = command
	return nil
}

// deliver runs the event's hook command with the payload on its stdin and
// waits for it. A failing hook command does not stop the session, as with the
// real providers; the failure is reported on stderr.
func (h hookCommands) deliver(stderr io.Writer) func(string, []byte) {
	return func(event string, payload []byte) {
		command, ok := h[event]
		if !ok {
			return
		}
		cmd := exec.Command("/bin/sh", "-c", command)
		cmd.Stdin = bytes.NewReader(payload)
		var out bytes.Buffer
		cmd.Stderr = &out
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(stderr, "fake-provider: %s hook failed: %v: %s\n", event, err, bytes.TrimSpace(out.Bytes()))
		}
	}
}
