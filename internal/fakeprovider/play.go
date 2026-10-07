// SPDX-License-Identifier: Apache-2.0

package fakeprovider

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os/exec"
)

// banner is the first thing the fake draws, so that nothing can mistake it
// for a qualified provider.
const banner = "asmai fake provider: scripted for development tests, not a qualified provider\r\n"

// play draws banner on out, then plays steps in order: it draws each screen
// on out, reads in until it has received each expected input, and passes
// each hook payload to deliver with its event name, and runs each command.
// It stops at the first input that differs from what the script expects, and
// at the first command whose exit status or output differs, naming the
// step's line.
func play(steps []step, in io.Reader, out io.Writer, deliver func(event string, payload []byte)) error {
	if _, err := io.WriteString(out, banner); err != nil {
		return err
	}
	input := bufio.NewReader(in)
	for _, s := range steps {
		var err error
		switch {
		case s.screen != "":
			_, err = io.WriteString(out, s.screen)
		case s.expect != "":
			err = expect(input, s.expect)
		case s.run != "":
			err = run(s)
		default:
			deliver(s.hook, s.payload)
		}
		if err != nil {
			return fmt.Errorf("line %d: %w", s.line, err)
		}
	}
	return nil
}

// run runs the step's command through /bin/sh in the fake's own environment
// and working directory, as an agent's tool call runs in the session's, and
// checks its exit status and output. The command reads nothing: its stdin is
// empty, so it cannot take the input meant for the session.
func run(s step) error {
	cmd := exec.Command("/bin/sh", "-c", s.run)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		return fmt.Errorf("running %q: %w", s.run, err)
	}
	if code := cmd.ProcessState.ExitCode(); s.status != nil && code != *s.status {
		return fmt.Errorf("%q exited %d, want %d; it printed %q", s.run, code, *s.status, out)
	}
	if s.output != nil && !s.output.Match(out) {
		return fmt.Errorf("%q printed %q, which does not match %q", s.run, out, s.output)
	}
	return nil
}

// expect reads input byte by byte until it has received want, and fails as
// soon as a byte differs.
func expect(input *bufio.Reader, want string) error {
	for i := 0; i < len(want); i++ {
		b, err := input.ReadByte()
		if err == io.EOF {
			return fmt.Errorf("input ended while waiting for %q", want)
		}
		if err != nil {
			return err
		}
		if b != want[i] {
			return fmt.Errorf("expected input %q, got %q", want, want[:i]+string([]byte{b}))
		}
	}
	return nil
}
