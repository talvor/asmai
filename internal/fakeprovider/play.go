// SPDX-License-Identifier: Apache-2.0

package fakeprovider

import (
	"bufio"
	"fmt"
	"io"
)

// banner is the first thing the fake draws, so that nothing can mistake it
// for a qualified provider.
const banner = "asmai fake provider: scripted for development tests, not a qualified provider\r\n"

// play draws banner on out, then plays steps in order: it draws each screen
// on out, reads in until it has received each expected input, and passes
// each hook payload to deliver with its event name. It stops at the first
// input that differs from what the script expects.
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
		default:
			deliver(s.hook, s.payload)
		}
		if err != nil {
			return err
		}
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
