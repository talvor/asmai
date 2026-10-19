// SPDX-License-Identifier: Apache-2.0

package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestAWorkersDecisionRequestReachesItsOwningLeaderAndNeverTheUser(t *testing.T) {
	const branch = "asmai/job-1/1"
	worker := []string{
		fakeRun(`asmai inbox --dispatch 3`, 0, "assignment 1"),
		fakeRun(`printf 'hello\n' > greeting.txt && printf '#!/bin/sh\ntest "$(cat greeting.txt)" = hello\n' > check.sh && chmod +x check.sh && ./check.sh`, 0, ""),
		fakeRun(`git add -A && git commit -q -m 'Add greeting.txt and its check' && asmai effect commit "$(git rev-parse HEAD)"`, 0, "effect 1 recorded: commit"),
		fakeRun(`git merge --no-edit asmai/job-1-add-a-greeting`, 0, "up to date"),
		fakeRun(`git push -q origin `+branch+` && asmai effect push "origin/`+branch+`@$(git rev-parse HEAD)"`, 0, "effect 2 recorded: push"),
		fakeRun(`asmai decision request --title 'Which greeting?' --option hello --option hi --recommend hello`, 0, "decision request 1, version 1, recorded for job 1; it goes to leader@engineering and is answered in conversation"),
		// The work that depends on the choice waits.
		fakeRun(resultFlagsForGreeting, 1, "assignment 1 waits for decision request 1 \\(version 1\\), which is pending"),
		fakeRun(`asmai decision answer --request 1 --version 1 --answer hello`, 1, "only a leader may run asmai decision answer"),
		fakeRun(`asmai decisions`, 1, "an agent cannot run asmai decisions"),
		fakeRun(`touch "$ASMAI_TEST_DIR/refused"`, 0, ""),
		hookStep("Stop", "assignment", ""),
		`{"expect":"asmai inbox --dispatch 5\r"}`,
		hookStep("UserPromptSubmit", "answer", "asmai inbox --dispatch 5"),
		fakeRun(`asmai inbox --dispatch 5`, 0, "(?s)Answer to version 1 of decision request 1, from leader@engineering:\nhello"),
	}
	leader := []string{
		fakeRun(`asmai inbox --dispatch 4`, 0, "(?s)Decision request 1, version 1 \\| job 1 \\| from worker1@engineering to leader@engineering.*Question q1: Which greeting\\?.*recommended: hello"),
		fakeRun(`while [ ! -e "$ASMAI_TEST_DIR/refused" ]; do sleep 0.02; done; asmai brief 1`, 0, "(?s)Pending decisions:\n- request 1, version 1, from worker1@engineering to leader@engineering, answered in conversation: Which greeting\\?\n  the work of assignments \\[1\\] waits for the answer"),
		// An answer applies only to the version it names.
		fakeRun(`asmai decision answer --request 1 --version 2 --answer hello`, 1, "decision request 1 has no version 2"),
		fakeRun(`asmai decision answer --request 1 --version 1 --answer hello --relied-on "the job's decisions"`, 0, "version 1 of decision request 1 answered; the answer goes to worker1@engineering, dispatch 5"),
	}
	assignmentFlow(t, worker, leader)

	// The user is not asked what a worker asked its leader, and listing
	// answers nothing.
	stdout, stderr, code := runAsmai(t, "decisions")
	if code != 0 || strings.TrimSpace(stdout) != "No decisions are pending." {
		t.Fatalf("asmai decisions = %q, %q, %d", stdout, stderr, code)
	}
	if got := journaled(t, "decision.requested"); len(got) != 1 {
		t.Fatalf("requests journaled: %+v", got)
	}
	if got := journaled(t, "decision.answered"); len(got) != 1 {
		t.Fatalf("answers journaled: %+v", got)
	}
}

func TestAsmaiDecisionsListsThePendingDecisionsWithTheirAnswerSurfaceAndAnswersNothing(t *testing.T) {
	testDir := t.TempDir()
	t.Setenv("ASMAI_TEST_DIR", testDir)
	script := []string{
		`{"screen": "> "}`,
		`{"expect": "Please fix the fixture\r"}`,
		`{"hook": "UserPromptSubmit", "payload": {"hook_event_name": "UserPromptSubmit", "prompt": "Please fix the fixture"}}`,
		fakeRun(`asmai job open --message latest --repository fixture --reading 'Fix the fixture' --criterion 'Tests pass'`, 0, "Opened job 1"),
		fakeRun(`asmai decision request --job 1 --title 'Which cache?' --option redis --option in-memory`, 0, "decision request 1, version 1, recorded for job 1; it goes to user and is answered in conversation"),
		fakeRun(`asmai decision request --job 1 --question '{"id":"a","title":"Name?"}' --question '{"id":"b","title":"Scope?"}' --content 'a prototype'`, 0, "decision request 2, version 1, recorded for job 1; it goes to user and is answered in lavish"),
		fakeRun(`asmai decision request --job 1 --request 2 --question '{"id":"a","title":"Name?"}' --question '{"id":"b","title":"Scope, now?"}'`, 0, "decision request 2, version 2"),
		fakeRun(`asmai decision request --job 1 --title 'Which cache?' --kind choice`, 1, "single choice"),
		fakeRun(`asmai decision request --job 1 --title 'x' --question '{"id":"a","title":"y"}'`, 1, "not both"),
		fakeRun(`asmai decision answer --request 1 --version 1 --answer redis`, 1, "goes to the user"),
		fakeRun(`touch "$ASMAI_TEST_DIR/done"`, 0, ""),
		`{"expect": "only typed by a test that means to"}`,
	}
	readyFactory(t, script)
	origin := filepath.Join(t.TempDir(), "fixture.git")
	gitIn(t, t.TempDir(), "init", "--bare", "-b", "main", origin)
	work := t.TempDir()
	gitIn(t, work, "clone", "--quiet", origin, "fixture")
	checkout := filepath.Join(work, "fixture")
	gitIn(t, checkout, "commit", "--allow-empty", "-m", "initial")
	gitIn(t, checkout, "push", "--quiet", "origin", "HEAD:main")
	if _, stderr, code := runAsmai(t, "repo", "add", origin); code != 0 {
		t.Fatalf("repo add: %s", stderr)
	}
	a := inTerminal(t, 100, 25)
	waitFor(t, "Coordination's prompt", a.shows(">"))
	a.ptmx.WriteString("Please fix the fixture\r")
	waitFor(t, "the fake leader to ask its decisions", touched(testDir, "done"))

	stdout, stderr, code := runAsmai(t, "decisions")
	if code != 0 {
		t.Fatalf("asmai decisions: %d, %s", code, stderr)
	}
	for _, want := range []string{"ANSWER SURFACE", "Which cache?", "conversation", "Lavish", "Scope, now?", "leader@coordination", "answers none"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("asmai decisions did not show %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "Scope?") {
		t.Errorf("asmai decisions showed the replaced version:\n%s", stdout)
	}
	var listed struct {
		Decisions []struct {
			Request int64  `json:"request"`
			Version int    `json:"version"`
			Surface string `json:"answer_surface"`
			State   string `json:"state"`
		} `json:"decisions"`
	}
	asmaiJSON(t, &listed, "decisions")
	if len(listed.Decisions) != 2 || listed.Decisions[0].Request != 1 || listed.Decisions[0].Surface != "conversation" || listed.Decisions[1].Request != 2 || listed.Decisions[1].Version != 2 || listed.Decisions[1].Surface != "lavish" {
		t.Fatalf("decisions = %+v", listed.Decisions)
	}
	// Listing answered nothing and changed nothing.
	journalBefore := len(journaled(t, "decision.answered")) + len(journaled(t, "decision.withdrawn"))
	runAsmai(t, "decisions")
	asmaiJSON(t, &listed, "decisions")
	if len(listed.Decisions) != 2 || journalBefore != 0 || len(journaled(t, "decision.answered")) != 0 {
		t.Fatalf("listing changed something: %+v", listed.Decisions)
	}
	if len(journaled(t, "decision.requested")) != 3 || len(journaled(t, "decision.replaced")) != 1 {
		t.Fatalf("journal: %d requested, %d replaced", len(journaled(t, "decision.requested")), len(journaled(t, "decision.replaced")))
	}
	// The user does not run the agents' commands.
	if _, stderr, code := runAsmai(t, "decision", "request", "--job", "1", "--title", "x", "--option", "a", "--option", "b"); code != 1 || !strings.Contains(stderr, "agent command") {
		t.Fatalf("user requesting a decision: code %d, %s", code, stderr)
	}
}
