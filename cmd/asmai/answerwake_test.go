// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/talvor/asmai/internal/fakeprovider"
)

func answerDispatchStates(t *testing.T, id int64) string {
	t.Helper()
	var states []string
	for _, e := range journaled(t, "dispatch.changed") {
		var d struct {
			ID    int64  `json:"id"`
			State string `json:"state"`
		}
		json.Unmarshal(e.Data, &d)
		if d.ID == id {
			states = append(states, d.State)
		}
	}
	return strings.Join(states, ",")
}

// Claude Code 2.1.292 reports a prompt typed as a finished turn's Stop hook
// runs under that turn's prompt ID, and the turn that takes it stops under
// another. The scripts below acknowledge the answer's nudge under the
// finished turn's ID. The answer must still start the receiver's next turn
// and end at that turn's own Stop.

func TestAnAnswerFromTheOwningLeaderWakesTheWorkerThatAskedAndItsTurnEndsAtItsOwnStop(t *testing.T) {
	const branch = "asmai/test/job-1/1"
	worker := []string{
		fakeRun(`asmai inbox --dispatch 3`, 0, "assignment 1"),
		fakeRun(`printf 'hello\n' > greeting.txt && printf '#!/bin/sh\ntest "$(cat greeting.txt)" = hello\n' > check.sh && chmod +x check.sh && ./check.sh`, 0, ""),
		fakeRun(`git add -A && git commit -q -m 'Add greeting.txt and its check' && asmai effect commit "$(git rev-parse HEAD)"`, 0, "effect 1 recorded: commit"),
		fakeRun(`git merge --no-edit asmai/test/job-1-add-a-greeting`, 0, "up to date"),
		fakeRun(`git push -q origin `+branch+` && asmai effect push "origin/`+branch+`@$(git rev-parse HEAD)"`, 0, "effect 2 recorded: push"),
		fakeRun(`asmai decision request --title 'Which greeting?' --option hello --option hi --recommend hello`, 0, "decision request 1, version 1, recorded for job 1"),
		hookStep("Stop", "assignment", ""),
		`{"expect":"asmai inbox --dispatch 5\r"}`,
		// Typed as the Stop hook ran: reported under the finished turn.
		hookStep("UserPromptSubmit", "assignment", "asmai inbox --dispatch 5"),
		fakeRun(`asmai inbox --dispatch 5`, 0, "(?s)Answer to version 1 of decision request 1, from leader@engineering:\nhello"),
		hookStep("Stop", "answered", ""),
	}
	leader := []string{
		fakeRun(`asmai inbox --dispatch 4`, 0, "Decision request 1, version 1"),
		fakeRun(`asmai decision answer --request 1 --version 1 --answer hello --relied-on "the job's decisions"`, 0, "the answer goes to worker1@engineering, dispatch 5"),
	}
	assignmentWorkerStop = "closing"
	t.Cleanup(func() { assignmentWorkerStop = "assignment" })
	assignmentFlow(t, worker, leader)
	if got := answerDispatchStates(t, 5); got != "created,unknown,nudged,delivered,working,stopped" {
		t.Errorf("the answer's dispatch states: %s", got)
	} else {
		t.Logf("leader answer reached worker1@engineering without a restart: dispatch 5 %s", got)
	}
}

func TestTheUsersAnswerWakesTheLeaderThatAskedAndItsTurnEndsAtItsOwnStop(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ASMAI_TEST_DIR", dir)
	engineering := []string{
		hookStep("SessionStart", "", ""),
		`{"screen":"\u001b[?2004h> "}`,
		`{"expect":"asmai inbox --dispatch 1\r"}`,
		hookStep("UserPromptSubmit", "handoff", "asmai inbox --dispatch 1"),
		fakeRun(`asmai inbox --dispatch 1`, 0, "handoff 1"),
		fakeRun(`asmai handoff accept --handoff 1`, 0, "reply dispatch 2"),
		fakeRun(`asmai decision request --job 1 --title 'Which greeting?' --option hello --option hi --recommend hello`, 0, "decision request 1, version 1, recorded for job 1; it goes to user"),
		hookStep("Stop", "handoff", ""),
		`{"expect":"asmai inbox --dispatch 4\r"}`,
		// Typed as the Stop hook ran: reported under the finished turn.
		hookStep("UserPromptSubmit", "handoff", "asmai inbox --dispatch 4"),
		fakeRun(`asmai inbox --dispatch 4`, 0, "(?s)The user answered version 1 of decision request 1.*Their words: hi please"),
		hookStep("Stop", "answered", ""),
		fakeRun(`touch "$ASMAI_TEST_DIR/engineering-done"`, 0, ""),
		`{"expect":"only typed by a test that means to"}`,
	}
	t.Setenv(fakeprovider.ScriptEnv+"_ENGINEERING", writeScript(t, "engineering", engineering))
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
		`{"expect":"asmai inbox --dispatch 3\r"}`,
		hookStep("UserPromptSubmit", "request", "asmai inbox --dispatch 3"),
		fakeRun(`asmai inbox --dispatch 3`, 0, "Decision request 1, version 1"),
		fakeRun(`asmai decision present --request 1 --version 1 && touch "$ASMAI_TEST_DIR/shown"`, 0, "presented"),
		`{"expect":"hi please\r"}`,
		hookStep("UserPromptSubmit", "", "hi please"),
		fakeRun(`asmai decision record --request 1 --version 1 --cites latest --choice hi --reading 'The user picked hi.'`, 0, "version 1 of decision request 1 answered by witnessed message"),
		hookStep("Stop", "request", ""),
		`{"expect":"only typed by a test that means to"}`,
	}
	readyFactory(t, coordination)
	t.Cleanup(func() {
		if t.Failed() {
			log, _, _ := runAsmai(t, "log")
			t.Logf("the daemon's log:\n%s", log)
		}
	})
	registerHandoffFixture(t)
	if _, stderr, code := runAsmai(t, "start"); code != 0 {
		t.Fatal(stderr)
	}
	a := inTerminal(t, 100, 25)
	waitFor(t, "Coordination prompt", a.shows(">"))
	a.ptmx.WriteString("Please add a greeting\r")
	waitFor(t, "the decision to be shown to the user", touched(dir, "shown"))
	a.ptmx.WriteString("hi please\r")
	waitFor(t, "the leader that asked to read the answer", touched(dir, "engineering-done"))
	if got := answerDispatchStates(t, 4); got != "created,unknown,nudged,delivered,working,stopped" {
		t.Errorf("the answer's dispatch states: %s", got)
	} else {
		t.Logf("user answer reached leader@engineering without a restart: dispatch 4 %s", got)
	}
}
