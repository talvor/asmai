// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"

	"github.com/talvor/asmai/internal/fakeprovider"
	"github.com/talvor/asmai/internal/providers"
)

func TestStartDoctorAndInitRefuseNonSubscriptionSignIn(t *testing.T) {
	for _, method := range []string{"apiKey", "unknown"} {
		t.Run(method, func(t *testing.T) {
			t.Setenv(fakeprovider.AuthMethodEnv, method)
			readyFactory(t, idleLeader)
			fakeUpstream(t, "v1.0.0", nil)
			for command, runCommand := range map[string]func() (string, string, int){
				"start":  func() (string, string, int) { return runAsmai(t, "start") },
				"doctor": func() (string, string, int) { return doctorIn(t) },
				"init":   func() (string, string, int) { return runIn(t, "", "init") },
			} {
				stdout, stderr, code := runCommand()
				if code != 1 || !strings.Contains(stdout+stderr, "sign-in") || !strings.Contains(stdout+stderr, "auth login") {
					t.Errorf("%s with %s sign-in exited %d printing %q", command, method, code, stdout+stderr)
				}
			}
			if running := runningLeaders(t); len(running) != 0 {
				t.Errorf("start with %s sign-in started %v", method, running)
			}
		})
	}
}

// Under a managed policy that allows managed permission rules only, Claude
// Code ignores the rules AsmAI gives its agents, so an agent would stop at its
// first command that needs approval. `asmai start` refuses before any agent
// starts, and `asmai init` reports the same, each naming the policy and what
// to do.
func TestStartAndSetupRefuseUnderAManagedPolicyThatIgnoresAsmaisPermissionRules(t *testing.T) {
	t.Setenv(fakeprovider.ManagedRulesOnlyEnv, "1")
	readyFactory(t, idleLeader)

	_, stderr, code := runAsmai(t, "start")
	if code != 1 {
		t.Errorf("asmai start exited %d under the policy, want 1; it printed %q", code, stderr)
	}
	for _, want := range []string{"failed  permission rules", "allowManagedPermissionRulesOnly", "Bash(asmai:*)", "account without that policy", "administrator"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("asmai start printed\n%s\nwant it to say %q", stderr, want)
		}
	}
	if running := runningLeaders(t); len(running) != 0 {
		t.Errorf("asmai start started %v under the policy, want no agent", running)
	}

	stdout, stderr, code := runAsmai(t, "init")
	if code != 1 {
		t.Errorf("asmai init exited %d under the policy, want 1; it printed %q", code, stdout+stderr)
	}
	for _, want := range []string{"permission rules: failed.", "allowManagedPermissionRulesOnly", "administrator"} {
		if !strings.Contains(stdout+stderr, want) {
			t.Errorf("asmai init printed\n%s\nwant it to say %q", stdout+stderr, want)
		}
	}
}

// A rule an administrator added to the managed settings is in effect, so the
// policy needs no other account.
func TestStartPassesUnderAManagedPolicyThatAllowsAsmaisRules(t *testing.T) {
	t.Setenv(fakeprovider.ManagedRulesOnlyEnv, "1")
	t.Setenv(fakeprovider.ManagedRulesAllowEnv, strings.Join(providers.CommandRules(), ","))
	readyFactory(t, idleLeader)

	if stdout, stderr, code := runAsmai(t, "start"); code != 0 {
		t.Errorf("asmai start exited %d with the rules in the managed settings, printing %q", code, stdout+stderr)
	}
}

// runningLeaders returns the leaders `asmai status` shows running.
func runningLeaders(t *testing.T) []string {
	t.Helper()
	var status struct {
		Leaders []leaderJSON `json:"leaders"`
	}
	asmaiJSON(t, &status, "status")
	var running []string
	for _, l := range status.Leaders {
		if l.State == "running" {
			running = append(running, l.Agent)
		}
	}
	return running
}
