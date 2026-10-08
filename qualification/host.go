// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// checkAccount refuses to run unless the harness is the host's own
// qualification user working in that user's home: with the user's own
// factory, state directory and provider sign-in, and never another user's
// (11 rules 6 and 16). getenv reads the harness's environment.
//
// It refuses to run:
//   - in hosted CI, because provider sign-in never leaves the user's host;
//   - as root, which is nobody's own account;
//   - when HOME is not a directory owned by the user the harness runs as,
//     such as one inherited through sudo or su, whose state directory and
//     sign-in would be another user's.
func checkAccount(uid int, home string, getenv func(string) string) error {
	for _, name := range []string{"CI", "GITHUB_ACTIONS"} {
		if getenv(name) != "" {
			return fmt.Errorf("%s is set: the harness never runs in hosted CI, because provider sign-in never leaves the user's host", name)
		}
	}
	if uid == 0 {
		return errors.New("running as root: run the harness as the qualification host's own OS user, which has its own factory and sign-in")
	}
	if home == "" || !filepath.IsAbs(home) {
		return fmt.Errorf("HOME is %q: the harness needs the qualification user's home directory", home)
	}
	info, err := os.Stat(home)
	if err != nil {
		return fmt.Errorf("finding the home directory: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || !ok || int(stat.Uid) != uid {
		return fmt.Errorf("HOME is %s, which is not a directory owned by the user the harness runs as: the harness never touches another user's factory, state or sign-in", home)
	}
	return nil
}
