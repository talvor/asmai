// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os/exec"
	"runtime"
	"slices"
	"testing"
)

func TestEnvironNamesKeepsNamesAndNeverValues(t *testing.T) {
	environ := []byte("HOME=/home/u\x00ANTHROPIC_API_KEY=secret=with=equals\x00EMPTY=\x00HOME=/again\x00NOEQUALS\x00")

	got := environNames(environ)

	if want := []string{"ANTHROPIC_API_KEY", "EMPTY", "HOME"}; !slices.Equal(got, want) {
		t.Errorf("environNames = %q, want %q", got, want)
	}
}

func TestPsEnvironNamesFindsEachAssignmentAfterTheCommandLine(t *testing.T) {
	line := "/Users/u/.local/state/asmai/providers/claude-code/2.1.292/claude --model opus HOME=/Users/u PATH=/a:/b USER=u TERM=xterm-256color ASMAI_SESSION=abc\n"

	got := psEnvironNames(line)

	if want := []string{"ASMAI_SESSION", "HOME", "PATH", "TERM", "USER"}; !slices.Equal(got, want) {
		t.Errorf("psEnvironNames = %q, want %q", got, want)
	}
}

func TestProcessEnvNamesReadsTheEnvironmentAProcessStartedWith(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reading a process's environment with ps is verified on the Mac qualification host")
	}
	cmd := exec.Command("sleep", "60")
	cmd.Env = []string{"ASMAI_QUALIFY_TEST_VARIABLE=a value that must not be returned", "ANOTHER_ONE=1"}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })

	got, err := processEnvNames(cmd.Process.Pid)

	if want := []string{"ANOTHER_ONE", "ASMAI_QUALIFY_TEST_VARIABLE"}; err != nil || !slices.Equal(got, want) {
		t.Errorf("processEnvNames = %q, %v, want %q", got, err, want)
	}
	if _, err := processEnvNames(1 << 30); err == nil {
		t.Error("processEnvNames of a process that does not exist succeeded")
	}
}
