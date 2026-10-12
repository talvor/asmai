// SPDX-License-Identifier: Apache-2.0

//go:build !darwin && !linux

package githooks

import (
	"errors"
	"os/exec"
)

// startConfined refuses to start cmd, as this system has no sandbox to
// confine its writes.
func startConfined(cmd *exec.Cmd, dirs []string) error {
	return errors.New("this system cannot confine a hook's writes")
}
