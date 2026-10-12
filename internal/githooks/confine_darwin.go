// SPDX-License-Identifier: Apache-2.0

package githooks

import (
	"os/exec"
	"strconv"
	"strings"
)

// sandboxExec is macOS's Seatbelt, the sandbox Claude Code's write guard
// uses there.
const sandboxExec = "/usr/bin/sandbox-exec"

// startConfined starts cmd in Seatbelt, able to write only beneath the paths
// dirs and the devices any program writes to. Everything else it may read
// and run as before.
func startConfined(cmd *exec.Cmd, dirs []string) error {
	profile := []string{"(version 1)", "(allow default)", "(deny file-write*)", "(allow file-write*"}
	args := []string{sandboxExec}
	for i, dir := range dirs {
		param := "WRITABLE_" + strconv.Itoa(i)
		profile = append(profile, `  (subpath (param "`+param+`"))`)
		args = append(args, "-D", param+"="+dir)
	}
	for _, device := range append(devices, "/dev/stdout", "/dev/stderr", "/dev/dtracehelper") {
		profile = append(profile, `  (literal "`+device+`")`)
	}
	profile = append(profile, `  (regex #"^/dev/fd/") (regex #"^/dev/ttys[0-9]+$"))`)
	cmd.Args = append(append(args, "-p", strings.Join(profile, "\n"), cmd.Path), cmd.Args[1:]...)
	cmd.Path = sandboxExec
	return cmd.Start()
}
