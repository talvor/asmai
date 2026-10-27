// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/talvor/asmai/internal/statedir"
	"github.com/talvor/asmai/internal/store"
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
		fakeRun(`touch "$ASMAI_TEST_DIR/worker-stopped"`, 0, ""),
		fakeRun(`while [ ! -e "$ASMAI_TEST_DIR/answer-guard-checked" ]; do sleep 0.02; done`, 0, ""),
		`{"expect":"asmai inbox --dispatch 6\r"}`,
		hookStep("UserPromptSubmit", "answer", "asmai inbox --dispatch 6"),
		fakeRun(`asmai inbox --dispatch 6`, 0, "(?s)Answer to version 1 of decision request 1, from leader@engineering:\nhello"),
	}
	leader := []string{
		fakeRun(`asmai inbox --dispatch 4`, 0, "(?s)Decision request 1, version 1 \\| job 1 \\| from worker1@engineering to leader@engineering.*Question q1: Which greeting\\?.*recommended: hello"),
		fakeRun(`while [ ! -e "$ASMAI_TEST_DIR/refused" ]; do sleep 0.02; done; asmai brief 1`, 0, "(?s)Pending decisions:\n- request 1, version 1, from worker1@engineering to leader@engineering, answered in conversation: Which greeting\\?\n  the work of assignments \\[1\\] waits for the answer"),
		// An answer applies only to the version it names.
		fakeRun(`asmai decision answer --request 1 --version 2 --answer hello`, 1, "decision request 1 has no version 2"),
		fakeRun(`while [ ! -e "$ASMAI_TEST_DIR/worker-stopped" ]; do sleep 0.02; done`, 0, ""),
		fakeRun(`asmai decision answer --request 1 --version 1 --answer hello --relied-on "the job's decisions"`, 0, "version 1 of decision request 1 answered; the answer goes to worker1@engineering, dispatch 6"),
		fakeRun(`asmai redispatch --assignment 1 --reason 'try to overtake the answer' > "$ASMAI_TEST_DIR/answer-guard-refusal" 2>&1; command_exit=$?; cat "$ASMAI_TEST_DIR/answer-guard-refusal"; test "$command_exit" -eq 1`, 0, "queued decision.answer dispatch 6"),
		fakeRun(`touch "$ASMAI_TEST_DIR/answer-guard-checked"`, 0, ""),
	}
	assignmentLeaderAfterStop = turn(5, "stalled", fakeRun(`asmai inbox --dispatch 5`, 0, "stalled from daemon"))
	assignmentWorkerStop = "answer"
	t.Cleanup(func() {
		assignmentLeaderAfterStop = nil
		assignmentWorkerStop = "assignment"
	})
	_, testDir := assignmentFlow(t, worker, leader)
	output, err := os.ReadFile(filepath.Join(testDir, "answer-guard-refusal"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("decision answer guard:\n%s", output)
	if states := answerDispatchStates(t, 6); !strings.HasPrefix(states, "created,unknown,nudged,delivered,working") {
		t.Errorf("the guarded answer's dispatch states are %s", states)
	} else {
		t.Logf("decision answer dispatch 6: %s", states)
	}

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
	commitSetUp(t, checkout, "initial")
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
	for _, want := range []string{"ANSWER SURFACE", "STATE", "Which cache?", "conversation", "Lavish", "pending (page opening)", "Scope, now?", "leader@coordination", "answers none"} {
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

func TestAChoiceIsPresentedOnceAndOnlyTheUsersMessageAfterItAnswersWithCoordinationsReading(t *testing.T) {
	testDir := t.TempDir()
	t.Setenv("ASMAI_TEST_DIR", testDir)
	record := `asmai decision record --request 1 --version 1 --cites latest --choice in-memory --reading 'The user picked the in-memory cache.'`
	script := []string{
		`{"screen": "> "}`,
		`{"expect": "Please fix the fixture\r"}`,
		`{"hook": "UserPromptSubmit", "payload": {"hook_event_name": "UserPromptSubmit", "prompt": "Please fix the fixture"}}`,
		fakeRun(`asmai job open --message latest --repository fixture --reading 'Fix the fixture' --criterion 'Tests pass'`, 0, "Opened job 1"),
		fakeRun(`asmai decision request --job 1 --title 'Which cache?' --option redis --option in-memory --recommend in-memory`, 0, "decision request 1, version 1, recorded for job 1; it goes to user and is answered in conversation"),
		// Nothing answers a version before it is shown.
		fakeRun(record, 1, "has not been presented"),
		fakeRun(`asmai decision present --request 1 --version 1`, 0, "(?s)version 1 of decision request 1 presented.*wait for their next message"),
		// It is presented once.
		fakeRun(`asmai decision present --request 1 --version 1`, 1, "presented already"),
		// An entry that is not the user's message, as a nudge is not, is
		// refused, and the current version is shown for presenting again.
		fakeRun(`asmai decision record --request 1 --version 1 --cites 1 --choice in-memory --reading 'The user picked the in-memory cache.'`, 1, "(?s)not a witnessed message.*present the current version to the user again.*Question q1: Which cache\\?"),
		fakeRun(`asmai decision present --request 1 --version 1`, 0, "presented"),
		// The user's earlier message, typed before this version was shown, is
		// no answer to it.
		fakeRun(record, 1, "(?s)was given before version 1.*typed against an older version.*present the current version to the user again"),
		fakeRun(`asmai decision present --request 1 --version 1 && touch "$ASMAI_TEST_DIR/shown"`, 0, "presented"),
		`{"expect": "in-memory please\r"}`,
		`{"hook": "UserPromptSubmit", "payload": {"hook_event_name": "UserPromptSubmit", "prompt": "in-memory please"}}`,
		fakeRun(record, 0, "(?s)decision 1 recorded, human by leader@coordination: in-memory.*version 1 of decision request 1 answered by witnessed message"),
		fakeRun(`asmai brief 1`, 0, "(?s)human, citing witnessed message.*Coordination's reading of the user's words: The user picked the in-memory cache\\."),
		fakeRun(`touch "$ASMAI_TEST_DIR/done"`, 0, ""),
		`{"expect": "only typed by a test that means to"}`,
	}
	stateDir := readyFactory(t, script)
	origin := filepath.Join(t.TempDir(), "fixture.git")
	gitIn(t, t.TempDir(), "init", "--bare", "-b", "main", origin)
	work := t.TempDir()
	gitIn(t, work, "clone", "--quiet", origin, "fixture")
	checkout := filepath.Join(work, "fixture")
	commitSetUp(t, checkout, "initial")
	gitIn(t, checkout, "push", "--quiet", "origin", "HEAD:main")
	if _, stderr, code := runAsmai(t, "repo", "add", origin); code != 0 {
		t.Fatalf("repo add: %s", stderr)
	}
	a := inTerminal(t, 100, 25)
	t.Cleanup(func() {
		if t.Failed() {
			filepath.WalkDir(stateDir, func(path string, e fs.DirEntry, err error) error {
				if err == nil && !e.IsDir() && strings.HasSuffix(path, ".log") {
					text, _ := os.ReadFile(path)
					t.Logf("%s:\n%s", path, text)
				}
				return nil
			})
		}
	})
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("Coordination's terminal:\n%s", strings.Join(a.lines(), "\n"))
		}
	})
	waitFor(t, "Coordination's prompt", a.shows(">"))
	a.ptmx.WriteString("Please fix the fixture\r")
	waitFor(t, "the choice to be shown to the user", touched(testDir, "shown"))
	a.ptmx.WriteString("in-memory please\r")
	waitFor(t, "the answer to be recorded", touched(testDir, "done"))

	// The user has nothing left to answer, and the answer cites their own
	// message beside Coordination's reading.
	stdout, stderr, code := runAsmai(t, "decisions")
	if code != 0 || strings.TrimSpace(stdout) != "No decisions are pending." {
		t.Fatalf("asmai decisions = %q, %q, %d", stdout, stderr, code)
	}
	if got := journaled(t, "decision.presented"); len(got) != 3 {
		t.Fatalf("%d presentations journaled, want 3: the first, and two after refusals", len(got))
	}
	if got := journaled(t, "decision.refused"); len(got) != 2 {
		t.Fatalf("%d refusals journaled, want 2", len(got))
	}
	var answers []struct {
		State   string `json:"state"`
		Answer  string `json:"answer"`
		Cites   int64  `json:"cites"`
		Reading string `json:"reading"`
		By      string `json:"answered_by"`
	}
	for _, e := range journaled(t, "decision.answered") {
		var answer struct {
			State   string `json:"state"`
			Answer  string `json:"answer"`
			Cites   int64  `json:"cites"`
			Reading string `json:"reading"`
			By      string `json:"answered_by"`
		}
		if err := json.Unmarshal(e.Data, &answer); err != nil {
			t.Fatal(err)
		}
		answers = append(answers, answer)
	}
	witnessed := journaled(t, "message.witnessed")
	if len(answers) != 1 || answers[0].State != "answered" || answers[0].Answer != "in-memory please" || answers[0].By != "user" || answers[0].Reading != "The user picked the in-memory cache." ||
		len(witnessed) != 2 || answers[0].Cites != witnessed[1].ID {
		t.Fatalf("answers = %+v, witnessed = %+v", answers, witnessed)
	}
}

func TestAVersionAnsweredInLavishSaysHowFarItIsAnswered(t *testing.T) {
	questions := []store.Question{{ID: "a", Title: "Name?"}, {ID: "b", Title: "Scope?"}}
	page := store.DecisionRequest{Surface: store.SurfaceLavish, Recipient: store.ToUser, State: store.RequestPending, Questions: questions}
	if got := requestState(page); got != "pending (page opening)" {
		t.Errorf("a version whose page is not open is %q", got)
	}
	page.PageFile = "/pages/request-1-version-1.html"
	if got := requestState(page); got != "pending" {
		t.Errorf("a version with its page open and no answers is %q", got)
	}
	page.Answered = []string{"a"}
	if got := requestState(page); got != "partly answered (1 of 2)" {
		t.Errorf("a version with one of two answers is %q", got)
	}
	page.State, page.Answered = store.RequestAnswered, []string{"a", "b"}
	if got := requestState(page); got != "answered" {
		t.Errorf("a version with every answer is %q", got)
	}
	conversation := store.DecisionRequest{Surface: store.SurfaceConversation, Recipient: store.ToUser, State: store.RequestPending, Questions: questions[:1]}
	if got := requestState(conversation); got != "pending" {
		t.Errorf("a version answered in the conversation is %q", got)
	}
}

func TestAStepIsAnsweredFromRecordsNamedByIDAndNeverBesideTheUsersOwnAnswer(t *testing.T) {
	for name, tt := range map[string]struct {
		flags requestFlags
		want  string
	}{
		"a record that is not an ID": {requestFlags{request: 1, version: 1, answer: "x", records: listFlags{"seams"}}, `--record "seams" is not a decision record ID`},
		"a grant that is not an ID":  {requestFlags{request: 1, version: 1, answer: "x", grants: listFlags{"0"}}, `--grant "0" is not a grant ID`},
		"records beside the user's":  {requestFlags{request: 1, version: 1, userAnswer: 2, records: listFlags{"3"}}, "--user-answer or --record and --grant, not both"},
		"a grant beside the user's":  {requestFlags{request: 1, version: 1, userAnswer: 2, grants: listFlags{"3"}}, "--user-answer or --record and --grant, not both"},
	} {
		var stdout, stderr bytes.Buffer
		if code := decisionAnswer(&output{stdout: &stdout, stderr: &stderr}, statedir.Paths{}, tt.flags); code == 0 || !strings.Contains(stderr.String(), tt.want) {
			t.Errorf("%s: decision answer exited %d with %q, want it refused before the daemon is called: %q", name, code, stderr.String(), tt.want)
		}
	}
}

func TestAResultNamesTheRecordsItReliedOnByID(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := result(&output{stdout: &stdout, stderr: &stderr}, statedir.Paths{}, resultFlags{
		evidence: listFlags{"it works"}, tests: listFlags{"none"}, checks: listFlags{"none"}, gaps: listFlags{"none"}, prSection: "changes",
		records: listFlags{"first"},
	})
	if code == 0 || !strings.Contains(stderr.String(), `--record "first" is not a decision record ID`) {
		t.Errorf("a result naming a record that is not an ID exited %d with %q", code, stderr.String())
	}
}

func TestAWorkersStepGoesToItsOwningLeaderWhichAnswersItOnlyFromARecord(t *testing.T) {
	worker := []string{
		fakeRun(`asmai inbox --dispatch 3`, 0, "assignment 1"),
		// Only a step request names a skill and a step.
		fakeRun(`asmai decision request --kind choice --skill tdd --step 'the test seams' --title 'Which seams?' --option handler --option parser`, 1, "only a step request names a skill and a step"),
		fakeRun(`asmai decision request --kind step --title 'Which seams?' --option handler --option parser`, 1, "names the skill and the step"),
		fakeRun(`asmai decision request --kind step --skill tdd --step 'the test seams' --title 'Which seams?' --option handler --option parser`, 0, "decision request 1, version 1, recorded for job 1; it goes to leader@engineering"),
	}
	leader := []string{
		fakeRun(`asmai inbox --dispatch 4`, 0, "(?s)Decision request 1, version 1 \\| job 1 \\| from worker1@engineering to leader@engineering.*Asks for the step of tdd: the test seams"),
		// Words about what the answer rests on are no record, and a record that is not there covers nothing.
		fakeRun(`asmai decision answer --request 1 --version 1 --answer handler --relied-on "the job's decisions"`, 1, "relies on a record"),
		fakeRun(`asmai decision answer --request 1 --version 1 --answer handler --record 99`, 1, "decision record 99 does not exist"),
		fakeRun(`asmai decision answer --request 1 --version 1 --answer handler --grant 99`, 1, "grant 99: the grant does not exist"),
		fakeRun(`asmai decision answer --request 1 --version 1 --user-answer 2 --record 1`, 1, "not both"),
	}
	assignmentFlow(t, worker, leader)
	if got := journaled(t, "decision.requested"); len(got) != 1 {
		t.Fatalf("requests journaled: %+v", got)
	}
	if got := journaled(t, "decision.answered"); len(got) != 0 {
		t.Fatalf("a step no record covers was answered: %+v", got)
	}
}

func TestAQuestionFileGivesAQuestionLikeTheQuestionFlagWithoutABraceInTheCommand(t *testing.T) {
	file := filepath.Join(t.TempDir(), "q.json")
	if err := os.WriteFile(file, []byte(`{"id":"a","title":"Name?"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := requestFlags{questions: listFlags{`{"id":"b","title":"Scope?"}`}, questionFiles: listFlags{file}}.questionsOf()
	if err != nil || len(got) != 2 || got[0].ID != "b" || got[1].ID != "a" || got[1].Title != "Name?" {
		t.Fatalf("questionsOf = %+v, %v", got, err)
	}
	if _, err := (requestFlags{title: "x", questionFiles: listFlags{file}}).questionsOf(); err == nil || !strings.Contains(err.Error(), "not both") {
		t.Errorf("a shorthand question and a question file: %v", err)
	}
	if _, err := (requestFlags{questionFiles: listFlags{file + ".missing"}}).questionsOf(); err == nil {
		t.Error("a missing question file was accepted")
	}
}
