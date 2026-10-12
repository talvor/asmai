// SPDX-License-Identifier: Apache-2.0

package githooks

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// devices are the files any program writes to, which a confined hook may
// write as well as its workspace.
var devices = []string{"/dev/null", "/dev/zero", "/dev/tty"}

// writable returns the directories a repository's hook may write: the
// workspace git runs it in, the workspace's own git directory, which holds
// the message being committed and the index, and the worker's temporary
// directory. The clone's shared git directory, of objects, refs,
// configuration and hooks, is not among them, nor is anywhere else. Each is
// absolute, with its symbolic links resolved.
func writable(ctx context.Context) []string {
	var dirs []string
	for _, flag := range []string{"--show-toplevel", "--absolute-git-dir"} {
		out, err := exec.CommandContext(ctx, "git", "rev-parse", flag).Output()
		if dir := strings.TrimSpace(string(out)); err == nil && dir != "" {
			dirs = append(dirs, dir)
		}
	}
	if tmp := os.Getenv("TMPDIR"); tmp != "" {
		dirs = append(dirs, tmp)
	}
	var resolved []string
	for _, dir := range dirs {
		abs, err := filepath.Abs(dir)
		if err != nil {
			continue
		}
		if real, err := filepath.EvalSymlinks(abs); err == nil {
			abs = real
		}
		resolved = append(resolved, abs)
	}
	return resolved
}
