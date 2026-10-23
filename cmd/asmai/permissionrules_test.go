// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"runtime"
	"strings"
	"testing"

	"github.com/talvor/asmai/internal/fakeprovider"
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
// Code ignores the rules AsmAI gives its agents. With no administrator allow
// row for `asmai` and the sandbox's auto-allow or socket control not in
// effect, an agent would stop at its first `asmai` command. `asmai start`
// refuses before any agent starts, and `asmai init` reports the same, each
// saying `asmai` cannot run unprompted and giving the three options.
func TestStartAndSetupRefuseUnderAManagedPolicyWhereAsmaiCannotRunUnprompted(t *testing.T) {
	for name, env := range map[string]string{
		"sandbox auto-allow off":    fakeprovider.ManagedSandboxOffEnv,
		"daemon socket not allowed": fakeprovider.ManagedSocketsOffEnv,
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(fakeprovider.ManagedRulesOnlyEnv, "1")
			t.Setenv(env, "1")
			t.Setenv(fakeprovider.PlatformEnv, "darwin")
			readyFactory(t, idleLeader)

			_, stderr, code := runAsmai(t, "start")
			if code != 1 {
				t.Errorf("asmai start exited %d under the policy, want 1; it printed %q", code, stderr)
			}
			for _, want := range []string{"failed  permission rules", "`asmai` cannot run unprompted", "allowManagedPermissionRulesOnly", "Bash(asmai *)", "sandbox.autoAllowBashIfSandboxed: true", "account without that policy", "administrator"} {
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
			for _, want := range []string{"permission rules: failed.", "cannot run unprompted", "administrator"} {
				if !strings.Contains(stdout+stderr, want) {
					t.Errorf("asmai init printed\n%s\nwant it to say %q", stdout+stderr, want)
				}
			}
		})
	}
}

// An allow row an administrator added to the managed settings for `asmai` is
// in effect, so the policy needs no other account, and neither does the
// sandbox's route when auto-allow and the daemon's socket are in effect.
func TestStartPassesUnderAManagedPolicyWhereAsmaiRunsUnprompted(t *testing.T) {
	for name, set := range map[string]func(t *testing.T){
		"an admin allow row covers asmai": func(t *testing.T) {
			t.Setenv(fakeprovider.ManagedSandboxOffEnv, "1")
			t.Setenv(fakeprovider.ManagedRulesAllowEnv, "Bash(asmai *)")
		},
		"the sandbox auto-allows with the socket": func(t *testing.T) {
			if runtime.GOOS != "darwin" {
				t.Skip("the sandbox route's socket control is confirmed on macOS only")
			}
		},
		"the sandbox auto-allows with the socket, on a platform the fake names": func(t *testing.T) {
			t.Setenv(fakeprovider.PlatformEnv, "darwin")
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(fakeprovider.ManagedRulesOnlyEnv, "1")
			set(t)
			readyFactory(t, idleLeader)

			stdout, stderr, code := runAsmai(t, "start")
			if code != 0 {
				t.Errorf("asmai start exited %d under the policy, printing %q", code, stdout+stderr)
			}
			if strings.Contains(stdout+stderr, "warning") {
				t.Errorf("asmai start printed a warning where asmai runs unprompted: %q", stdout+stderr)
			}
		})
	}
}

// Without a policy nothing changes.
func TestStartPassesWithoutAManagedPolicy(t *testing.T) {
	readyFactory(t, idleLeader)
	if stdout, stderr, code := runAsmai(t, "start"); code != 0 || strings.Contains(stdout+stderr, "warning") {
		t.Errorf("asmai start exited %d without the policy, printing %q", code, stdout+stderr)
	}
}

// Where the sandbox route cannot be confirmed, as on Linux, `asmai start`
// starts with a plain warning that `asmai` may be prompted, at every start.
func TestStartWarnsWhereTheSandboxRouteCannotBeConfirmed(t *testing.T) {
	t.Setenv(fakeprovider.ManagedRulesOnlyEnv, "1")
	t.Setenv(fakeprovider.PlatformEnv, "linux")
	readyFactory(t, idleLeader)

	for range 2 {
		stdout, stderr, code := runAsmai(t, "start")
		if code != 0 {
			t.Fatalf("asmai start exited %d, printing %q", code, stdout+stderr)
		}
		for _, want := range []string{"warning", "`asmai` may be prompted", "Bash(asmai *)"} {
			if !strings.Contains(stderr, want) {
				t.Errorf("asmai start printed\n%s\nwant it to say %q", stderr, want)
			}
		}
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

// Coordination's leader, whose terminal is the user's conversation, keeps
// its native prompts under the managed policy as without it; the other
// agents' modes are pinned by the providers' tests.
func TestCoordinationsLeaderKeepsItsDefaultPermissionModeUnderTheManagedPolicy(t *testing.T) {
	for name, managed := range map[string]bool{"under the managed policy": true, "without it": false} {
		t.Run(name, func(t *testing.T) {
			if managed {
				t.Setenv(fakeprovider.ManagedRulesOnlyEnv, "1")
				t.Setenv(fakeprovider.PlatformEnv, "darwin")
			}
			readyFactory(t, idleLeader)
			if stdout, stderr, code := runAsmai(t, "start"); code != 0 {
				t.Fatalf("asmai start exited %d, printing %q", code, stdout+stderr)
			}
			var settings struct {
				Permissions struct {
					DefaultMode string `json:"defaultMode"`
				} `json:"permissions"`
			}
			if err := json.Unmarshal([]byte(argValue(sessionArgs(t, "leader@coordination"), "--settings")), &settings); err != nil {
				t.Fatal(err)
			}
			if got := settings.Permissions.DefaultMode; got != "default" {
				t.Errorf("Coordination's leader runs in the %q permission mode, want %q", got, "default")
			}
		})
	}
}
