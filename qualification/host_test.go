// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTheHarnessRunsOnlyAsTheHostsOwnUserInTheirOwnHome(t *testing.T) {
	home := t.TempDir()
	noEnv := func(string) string { return "" }
	uid := os.Getuid()

	if err := checkAccount(uid, home, noEnv); err != nil {
		t.Errorf("the user's own home was refused: %v", err)
	}

	for name, tc := range map[string]struct {
		uid  int
		home string
		env  map[string]string
		want string
	}{
		"hosted CI":           {uid, home, map[string]string{"GITHUB_ACTIONS": "true"}, "never runs in hosted CI"},
		"a CI system":         {uid, home, map[string]string{"CI": "1"}, "never runs in hosted CI"},
		"root":                {0, home, nil, "running as root"},
		"no home":             {uid, "", nil, "needs the qualification user's home directory"},
		"a relative home":     {uid, "home", nil, "needs the qualification user's home directory"},
		"a missing home":      {uid, filepath.Join(home, "missing"), nil, "finding the home directory"},
		"a file for a home":   {uid, writeFile(t, home), nil, "not a directory owned by the user"},
		"another user's home": {uid + 1, home, nil, "never touches another user's factory, state or sign-in"},
	} {
		t.Run(name, func(t *testing.T) {
			err := checkAccount(tc.uid, tc.home, func(name string) string { return tc.env[name] })
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("checkAccount = %v, want an error saying %q", err, tc.want)
			}
		})
	}
}

func writeFile(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "file")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
