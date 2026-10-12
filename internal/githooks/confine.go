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

// shared are the entries of the clone's shared git directory that git writes
// for the objects and refs a hook's own git commands make. The rest of it, its
// configuration, hooks and info among them, a hook may not write.
var shared = []string{"objects", "refs", "logs", "packed-refs", "packed-refs.lock"}

// writable returns the paths a repository's hook may write: the workspace git
// runs it in, the workspace's own git directory, which holds the message being
// committed and the index, the shared entries of the clone's git directory,
// the system's temporary directory and the worker's. Nowhere else is among
// them. Each is absolute, with its symbolic links resolved.
func writable(ctx context.Context) []string {
	var dirs []string
	for _, flag := range []string{"--show-toplevel", "--absolute-git-dir"} {
		out, err := exec.CommandContext(ctx, "git", "rev-parse", flag).Output()
		if dir := strings.TrimSpace(string(out)); err == nil && dir != "" {
			dirs = append(dirs, dir)
		}
	}
	out, err := exec.CommandContext(ctx, "git", "rev-parse", "--path-format=absolute", "--git-common-dir").Output()
	if common := strings.TrimSpace(string(out)); err == nil && common != "" {
		if real, err := filepath.EvalSymlinks(common); err == nil {
			common = real
		}
		for _, entry := range shared {
			dirs = append(dirs, filepath.Join(common, entry))
		}
	}
	dirs = append(dirs, "/tmp")
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
