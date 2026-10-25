// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// workerSession plays an Engineering worker for C51: it runs each numbered
// command of the prompt in the workspace, and reports it as a turn. A command
// in refuse is reported denied by the sandbox, as a push that stays in the
// guard is, and is not run; unsandbox reports every command retried outside
// the sandbox.
func workerSession(t *testing.T, refuse string, unsandbox bool) func(AskSpec) (Turn, error) {
	was := checkGuardDependencies
	checkGuardDependencies = func() error { return nil }
	t.Cleanup(func() { checkGuardDependencies = was })
	return func(spec AskSpec) (Turn, error) {
		var settings struct {
			Hooks map[string][]struct {
				Hooks []struct {
					Command string `json:"command"`
				} `json:"hooks"`
			} `json:"hooks"`
		}
		if err := json.Unmarshal([]byte(spec.Args[1]), &settings); err != nil {
			t.Fatal(err)
		}
		hook := exec.Command("sh", "-c", settings.Hooks["Stop"][0].Hooks[0].Command)
		hook.Stdin = strings.NewReader(`{"hook_event_name":"Stop"}`)
		if out, err := hook.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		var turn Turn
		for _, line := range strings.Split(spec.Prompt, "\n") {
			_, rest, ok := strings.Cut(line, ". `")
			if !ok {
				continue
			}
			command := strings.TrimSuffix(rest, "`")
			if command == refuse {
				turn.Commands = append(turn.Commands, Command{Text: command, Denied: true, Output: "deny network-outbound github.com:443 (user denied)"})
				continue
			}
			run := exec.Command("sh", "-c", command)
			run.Dir = spec.Dir
			out, err := run.CombinedOutput()
			turn.Commands = append(turn.Commands, Command{Text: command, Output: string(out), Failed: err != nil, Unsandboxed: unsandbox})
		}
		return turn, nil
	}
}

func TestC51PassesWhenTheWorkerCommitsAndPushesItsBranchWithNoRetry(t *testing.T) {
	origin := localPushFixture(t)
	usePushFixture(t, origin)
	r := result(t, harness(t, fakeClaude{auth: AuthStatus{SignedIn: true, Method: "claude.ai"}, ask: workerSession(t, "", false)}, startsDirectly), "C51")
	if r.Outcome != Passed {
		t.Fatalf("C51 %s: %s", r.Outcome, r.Failure)
	}
	evidence := strings.Join(r.Evidence, "\n")
	if !strings.Contains(evidence, "ran `git add`, `git commit` and `git push origin asmai/qualification-c51-") {
		t.Errorf("C51's evidence does not name the commands:\n%s", evidence)
	}
	out, err := exec.Command("git", "-C", origin, "branch", "--list", "asmai/*").CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "" {
		t.Errorf("origin still has branches %q (%v)", out, err)
	}
}

func TestC51FailsWhenTheWorkersPushIsDeniedOrRetriedOutsideTheSandbox(t *testing.T) {
	for name, c := range map[string]struct {
		refusePush, unsandbox bool
		want                  string
	}{
		"the push is denied":          {refusePush: true, want: "denied or failed"},
		"retried outside the sandbox": {unsandbox: true, want: "retried outside the sandbox"},
	} {
		t.Run(name, func(t *testing.T) {
			origin := localPushFixture(t)
			usePushFixture(t, origin)
			refuse := ""
			if c.refusePush {
				refuse = "push"
			}
			session := workerSession(t, refuse, c.unsandbox)
			wrapped := func(spec AskSpec) (Turn, error) {
				if c.refusePush {
					// The branch name is only known to the prompt.
					for _, line := range strings.Split(spec.Prompt, "\n") {
						if _, rest, ok := strings.Cut(line, ". `git push origin "); ok {
							refuse = "git push origin " + strings.TrimSuffix(rest, "`")
						}
					}
					session = workerSession(t, refuse, false)
				}
				return session(spec)
			}
			r := result(t, harness(t, fakeClaude{auth: AuthStatus{SignedIn: true, Method: "claude.ai"}, ask: wrapped}, startsDirectly), "C51")
			if r.Outcome != Failed || !strings.Contains(r.Failure, c.want) {
				t.Errorf("C51 %s: %q, want it to fail with %q", r.Outcome, r.Failure, c.want)
			}
		})
	}
}
