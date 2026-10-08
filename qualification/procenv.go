// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

// processEnvNames returns the names of the environment variables the process
// pid was started with, as the operating system holds them, sorted. It never
// returns a value: the harness records which variables a session has, never
// what they hold.
func processEnvNames(pid int) ([]string, error) {
	switch runtime.GOOS {
	case "linux":
		// A process that has only just started may not show its environment
		// yet, and one that has none is not an agent session.
		for range 20 {
			environ, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
			if err != nil {
				return nil, fmt.Errorf("reading the environment of process %d: %w", pid, err)
			}
			if len(environ) > 0 {
				return environNames(environ), nil
			}
			time.Sleep(100 * time.Millisecond)
		}
		return nil, nil
	case "darwin":
		// ps prints the environment after the command line, on one line.
		out, err := exec.Command("ps", "eww", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
		if err != nil {
			return nil, fmt.Errorf("reading the environment of process %d with ps: %w", pid, err)
		}
		return psEnvironNames(string(out)), nil
	}
	return nil, fmt.Errorf("reading a process's environment is not supported on %s", runtime.GOOS)
}

// environNames returns the sorted names in environ, a process's environment
// as Linux holds it: NAME=value entries ended by NUL bytes.
func environNames(environ []byte) []string {
	var names []string
	for entry := range bytes.SplitSeq(environ, []byte{0}) {
		if name, _, ok := bytes.Cut(entry, []byte("=")); ok && len(name) > 0 {
			names = append(names, string(name))
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// psAssignment finds an environment assignment in a line ps printed: a
// variable name after a space, then =. Values may hold spaces, so this can
// also find a NAME= that a value happens to hold, never miss a variable that
// is set.
var psAssignment = regexp.MustCompile(`(?:^|\s)([A-Za-z_][A-Za-z0-9_]*)=`)

// psEnvironNames returns the sorted names that out, the line `ps eww` prints
// for a process, shows as set.
func psEnvironNames(out string) []string {
	var names []string
	for _, m := range psAssignment.FindAllStringSubmatch(strings.TrimSpace(out), -1) {
		names = append(names, m[1])
	}
	slices.Sort(names)
	return slices.Compact(names)
}
