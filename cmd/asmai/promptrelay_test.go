// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/talvor/asmai/internal/fakeprovider"
)

// promptRelayFlow runs a job whose worker stalls at a native prompt, under
// the managed policy or without it: Coordination hands the job to
// Engineering, whose leader assigns the work, and the worker's session reports
// a permission request, then waits for the keys relayed (nothing, with no
// policy) to its terminal. Coordination handles the request the daemon raises
// for it, the user answers in the conversation with userWords, and Coordination
// records the choice. The dispatches are numbered 1 the handoff, 2 its answer,
// 3 the assignment, and, under the policy, 4 the prompt's request.
func promptRelayFlow(t *testing.T, managed bool, userWords, choice, relayed string) (testDir string, user *attachedTerminal) {
	t.Helper()
	gitIdentity(t)
	if managed {
		t.Setenv(fakeprovider.ManagedRulesOnlyEnv, "1")
		t.Setenv(fakeprovider.PlatformEnv, "darwin")
	}
	testDir = t.TempDir()
	t.Setenv("ASMAI_TEST_DIR", testDir)
	t.Setenv(fakeprovider.ScriptEnv+"_ENGINEERING", writeScript(t, "engineering", []string{
		hookStep("SessionStart", "", ""),
		`{"screen":"\u001b[?2004h> "}`,
		`{"expect":"asmai inbox --dispatch 1\r"}`,
		hookStep("UserPromptSubmit", "handoff", "asmai inbox --dispatch 1"),
		fakeRun(`asmai inbox --dispatch 1`, 0, "(?s)Add a greeting.*Greeting is printed"),
		fakeRun(`asmai handoff accept --handoff 1`, 0, "reply dispatch 2"),
		fakeRun(`asmai assign --job 1 --outcome 'Add greeting.txt' --criterion 'greeting.txt says hello'`, 0, "assignment 1 of job 1 given to worker1@engineering; dispatch 3"),
		hookStep("Stop", "handoff", ""),
		`{"expect":"only typed by a test that means to"}`,
	}))
	worker := []string{
		hookStep("SessionStart", "", ""),
		`{"screen":"\u001b[?2004h> "}`,
		`{"expect":"asmai inbox --dispatch 3\r"}`,
		hookStep("UserPromptSubmit", "assignment", "asmai inbox --dispatch 3"),
		`{"hook": "PermissionRequest", "payload": {"hook_event_name": "PermissionRequest", "tool_name": "Bash", "tool_input": {"command": "rm -rf build"}}}`,
		fakeRun(`touch "$ASMAI_TEST_DIR/stalled"`, 0, ""),
	}
	if relayed != "" {
		worker = append(worker, `{"expect":`+relayed+`}`, fakeRun(`touch "$ASMAI_TEST_DIR/relayed"`, 0, ""))
	}
	worker = append(worker,
		fakeRun(`touch "$ASMAI_TEST_DIR/worker-done"`, 0, ""),
		`{"expect":"only typed by a test that means to"}`,
	)
	t.Setenv(fakeprovider.ScriptEnv+"_WORKER", writeScript(t, "worker", worker))

	coordination := []string{
		hookStep("SessionStart", "", ""),
		`{"screen":"> "}`,
		`{"expect":"Please add a greeting\r"}`,
		hookStep("UserPromptSubmit", "", "Please add a greeting"),
		fakeRun(`asmai job open --message latest --repository fixture --reading 'Add a greeting' --criterion 'Greeting is printed'`, 0, "Opened job 1"),
		fakeRun(`asmai handoff send --job 1 --to engineering --outcome 'Add a greeting' --decisions none --evidence none --constraints none --permissions none --criterion 'Greeting is printed'`, 0, "dispatch 1"),
		hookStep("Stop", "", ""),
		`{"expect":"asmai inbox --dispatch 2\r"}`,
		hookStep("UserPromptSubmit", "reply", "asmai inbox --dispatch 2"),
		fakeRun(`asmai inbox --dispatch 2`, 0, "accepted from leader@engineering"),
		hookStep("Stop", "reply", ""),
	}
	if managed {
		coordination = append(coordination,
			`{"expect":"asmai inbox --dispatch 4\r"}`,
			hookStep("UserPromptSubmit", "prompt", "asmai inbox --dispatch 4"),
			fakeRun(`asmai inbox --dispatch 4`, 0, "(?s)Decision request 1, version 1.*from worker1@engineering.*worker1@engineering waits at a native permission prompt.*Bash `rm -rf build`.*Allow once.*Deny"),
			// The daemon does not take an agent's word for the user's answer, and
			// no agent but the daemon raises the request.
			fakeRun(`asmai decision request --job 1 --kind prompt --title 'Go on?' --option 'Allow once' --option Deny`, 1, "raised by the daemon"),
			fakeRun(`asmai decision present --request 1 --version 1`, 0, "presented"),
			fakeRun(`touch "$ASMAI_TEST_DIR/shown"`, 0, ""),
			`{"expect":`+quoted(userWords+"\r")+`}`,
			hookStep("UserPromptSubmit", "answer", userWords),
			fakeRun(`asmai decision record --request 1 --version 1 --cites latest --choice '`+choice+`' --reading 'The user answered the prompt.'`, 0, "(?s)decision 1 recorded, human by leader@coordination: "+choice),
			hookStep("Stop", "answer", ""),
			fakeRun(`touch "$ASMAI_TEST_DIR/coordination-done"`, 0, ""),
		)
	}
	coordination = append(coordination, `{"expect":"only typed by a test that means to"}`)
	stateDir := readyFactory(t, coordination)
	t.Cleanup(func() {
		if t.Failed() {
			log, _, _ := runAsmai(t, "log")
			t.Logf("the daemon's log:\n%s", log)
			filepath.WalkDir(stateDir, func(path string, e os.DirEntry, err error) error {
				if err == nil && !e.IsDir() && strings.HasSuffix(path, ".log") {
					text, _ := os.ReadFile(path)
					t.Logf("%s:\n%s", path, text)
				}
				return nil
			})
		}
	})
	if _, stderr, code := runAsmai(t, "start"); code != 0 {
		t.Fatal(stderr)
	}
	registerWritingFixtureWith(t, nil)
	user = inTerminal(t, 100, 25)
	waitFor(t, "Coordination prompt", user.shows(">"))
	user.ptmx.WriteString("Please add a greeting\r")
	waitFor(t, "the worker at its native prompt", touched(testDir, "stalled"))
	return testDir, user
}

// Under the managed policy, a native prompt at a worker, which has no
// terminal of the user's, is raised to the user through Coordination as a
// decision request, and the daemon types the user's witnessed answer, and
// only that, into the worker's terminal.
func TestUnderTheManagedPolicyTheUsersWitnessedAnswerToAWorkersNativePromptIsRelayedToItsTerminal(t *testing.T) {
	for _, tt := range []struct {
		name, words, choice, keys string
	}{
		{"allowed", "yes, go ahead", "Allow once", `"\r"`},
		{"denied", "no, not that", "Deny", `"\u001b"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			testDir, user := promptRelayFlow(t, true, tt.words, tt.choice, tt.keys)
			t.Cleanup(func() {
				if t.Failed() {
					t.Logf("the user's terminal:\n%s", strings.Join(user.lines(), "\n"))
				}
			})
			waitFor(t, "the request to be shown to the user", touched(testDir, "shown"))
			// The worker still waits: nothing was typed before the user answered.
			if touched(testDir, "relayed")() {
				t.Fatal("the daemon typed into the worker's terminal before the user answered")
			}
			if got := journaled(t, "prompt.relayed"); len(got) != 0 {
				t.Fatalf("relays journaled before the user answered: %+v", got)
			}
			stdout, _, code := runAsmai(t, "decisions")
			if code != 0 || !strings.Contains(stdout, "worker1@engineering") || !strings.Contains(stdout, "conversation") {
				t.Fatalf("asmai decisions printed %q, want the worker's prompt waiting for the user in the conversation", stdout)
			}
			if stdout, _, _ := runAsmai(t, "status"); !strings.Contains(stdout, "worker1@engineering waits at a native prompt the user decides") {
				t.Errorf("asmai status printed %q, want the prompt shown as the user's to decide", stdout)
			}

			user.ptmx.WriteString(tt.words + "\r")
			waitFor(t, "the answer to be relayed to the worker", touched(testDir, "relayed"))
			waitFor(t, "Coordination to record the answer", touched(testDir, "coordination-done"))
			waitFor(t, "the worker to finish", touched(testDir, "worker-done"))

			relays := journaled(t, "prompt.relayed")
			if len(relays) != 1 {
				t.Fatalf("relays journaled: %+v", relays)
			}
			relay := decode[struct {
				Agent, Choice string
				Cites         int64
			}](t, relays[0])
			answered := journaled(t, "decision.answered")
			if len(answered) != 1 {
				t.Fatalf("answers journaled: %+v", answered)
			}
			answer := decode[struct{ Answer string }](t, answered[0])
			if relay.Agent != "worker1@engineering" || relay.Choice != tt.choice || answer.Answer != tt.words {
				t.Errorf("the relay %+v of the answer %+v, want the user's words %q and choice %q for the worker", relay, answer, tt.words, tt.choice)
			}
			if witnessed := journaled(t, "message.witnessed"); !slices.ContainsFunc(witnessed, func(e journalEntry) bool { return e.ID == relay.Cites }) {
				t.Errorf("the relay cites entry %d, which is no witnessed message", relay.Cites)
			}
			if stdout, _, _ := runAsmai(t, "status"); strings.Contains(stdout, "native prompt") {
				t.Errorf("asmai status printed %q, want no native prompt once the worker is past it", stdout)
			}
			if stdout, _, _ := runAsmai(t, "decisions"); strings.TrimSpace(stdout) != "No decisions are pending." {
				t.Errorf("asmai decisions printed %q", stdout)
			}
		})
	}
}

// Without the managed policy nothing changes: the prompt is reported as one
// nobody can answer, no request is raised, and nothing is typed into the
// worker's terminal.
func TestWithoutTheManagedPolicyAWorkersNativePromptIsOnlyReported(t *testing.T) {
	testDir, _ := promptRelayFlow(t, false, "", "", "")
	waitFor(t, "the worker to finish", touched(testDir, "worker-done"))
	if stdout, _, _ := runAsmai(t, "status"); !strings.Contains(stdout, "worker1@engineering waits at a native prompt nobody can answer: Bash `rm -rf build`") {
		t.Errorf("asmai status printed %q, want the prompt reported as one nobody can answer", stdout)
	}
	if got := journaled(t, "decision.requested"); len(got) != 0 {
		t.Errorf("requests journaled without the policy: %+v", got)
	}
	if got := journaled(t, "prompt.relayed"); len(got) != 0 {
		t.Errorf("relays journaled without the policy: %+v", got)
	}
}
