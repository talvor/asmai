// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/talvor/asmai/internal/daemon"
)

// The command line is refused before any daemon or state directory is
// touched, with exit code 2 and the command's name and usage.
func TestCommandLinesAreRefusedWithUsage(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"repo"}, "asmai repo: want add, list, show or remove"},
		{[]string{"repo", "frobnicate"}, "asmai repo: want add, list, show or remove"},
		{[]string{"repo", "--json"}, "asmai repo: want add, list, show or remove"},
		{[]string{"providers"}, "asmai providers: want install or list"},
		{[]string{"handoff", "send2"}, "asmai handoff: want send, accept, clarify or decline"},
		{[]string{"decision"}, "asmai decision: want record"},
		{[]string{"attach"}, "asmai attach: name the agent, such as leader@coordination or coordination"},
		{[]string{"repo", "add"}, "asmai repo add: name a local checkout or a URL"},
		{[]string{"repo", "show"}, "asmai repo show: name the repository"},
		{[]string{"job"}, "asmai job show: name the job number"},
		{[]string{"brief"}, "asmai brief: name the job number"},
		{[]string{"push"}, "asmai push: name the job number"},
		{[]string{"effect"}, "asmai effect: name the kind of effect"},
		{[]string{"effect", "commit"}, "asmai effect: name what the effect is of"},
		{[]string{"effect", "commit", "abc", "more"}, `asmai effect: unexpected argument "more"`},
		{[]string{"status", "extra"}, `asmai status: unexpected argument "extra"`},
		{[]string{"chat", "extra"}, `asmai chat: unexpected argument "extra"`},
		{[]string{"status", "--frobnicate"}, "asmai status: unknown flag: --frobnicate"},
		{[]string{"start", "--foreground=maybe"}, "asmai start: invalid argument"},
		{[]string{"inbox", "--dispatch", "x"}, "asmai inbox: invalid argument"},
		{[]string{"job", "open", "--bogus"}, "asmai job open: unknown flag: --bogus"},
		{[]string{"unknown"}, `asmai: unknown command "unknown"`},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			var stdout, stderr bytes.Buffer
			code := run(tc.args, strings.NewReader(""), &stdout, &stderr)
			if code != 2 {
				t.Errorf("exited %d, want 2", code)
			}
			if stdout.Len() != 0 {
				t.Errorf("printed %q to stdout, want nothing", stdout.String())
			}
			if !strings.Contains(stderr.String(), tc.want) || !strings.Contains(stderr.String(), "commands:") {
				t.Errorf("stderr %q does not say %q followed by the usage", stderr.String(), tc.want)
			}
		})
	}
}

// Flags may come before or after a command's operands.
func TestFlagsMayFollowOperands(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	if code := run([]string{"version", "--json"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("asmai version --json exited %d: %s", code, stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, `"version": "dev"`) {
		t.Errorf("asmai version --json printed %q, want JSON", got)
	}
	// An operand then a flag: the missing second operand is what is refused,
	// not the flag.
	stderr.Reset()
	code := run([]string{"effect", "commit", "--json"}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "asmai effect: name what the effect is of") {
		t.Errorf("asmai effect commit --json exited %d printing %q, want 2 and the missing operand", code, stderr.String())
	}
}

// A command's -h lists its flags on stderr and exits 2, as it always has.
func TestACommandsHelpFlagListsItsFlags(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"start", "-h"}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 {
		t.Errorf("asmai start -h exited %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "--foreground") || !strings.Contains(stderr.String(), "--json") {
		t.Errorf("asmai start -h printed %q, want its flags", stderr.String())
	}
}

// An agent session may run only its coordination commands, whatever else is
// on the command line, and is told whom to ask for the others.
func TestAnAgentSessionIsRefusedTheUsersCommands(t *testing.T) {
	t.Setenv(daemon.SessionCredential, "credential")
	t.Setenv("HOME", t.TempDir())
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"repo", "list"}, "an agent cannot run this command; ask the user to run `asmai repo list`"},
		{[]string{"start"}, "an agent cannot run this command; ask the user to run `asmai start`"},
		{[]string{"export"}, "an agent cannot run asmai export; use an agent coordination command"},
		{[]string{"log", "--follow"}, "an agent cannot run asmai log; use an agent coordination command"},
		{[]string{"version"}, "an agent cannot run asmai version; use an agent coordination command"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(tc.args, strings.NewReader(""), &stdout, &stderr); code != 1 {
			t.Errorf("asmai %s exited %d, want 1", strings.Join(tc.args, " "), code)
		}
		if !strings.Contains(stderr.String(), tc.want) {
			t.Errorf("asmai %s printed %q, want %q", strings.Join(tc.args, " "), stderr.String(), tc.want)
		}
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"help"}, strings.NewReader(""), &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "an agent may run only its coordination commands") {
		t.Errorf("asmai help in an agent session exited %d printing %q, want 1 and the refusal", code, stderr.String())
	}
}
