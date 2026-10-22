// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/talvor/asmai/internal/fakeprovider"
)

// TestTheDaemonPushesTheJobBranchForEngineeringAndRefusesOverOutsideCommitsUntilAWorkerMergesThem
// drives a job end to end with the fake provider, with a local bare
// repository as origin: Engineering's leader asks for the push of the
// accepted work and the daemon pushes it; someone else pushes a commit to the
// job branch on origin, so the next push is refused and recorded as an
// outside change; a worker merges it in through a writing assignment, and the
// push after that keeps it.
func TestTheDaemonPushesTheJobBranchForEngineeringAndRefusesOverOutsideCommitsUntilAWorkerMergesThem(t *testing.T) {
	gitIdentity(t)
	testDir := t.TempDir()
	t.Setenv("ASMAI_TEST_DIR", testDir)
	const (
		jobBranch = "asmai/job-1-add-a-greeting"
		sha       = "[0-9a-f]{40}"
	)
	t.Setenv(fakeprovider.ScriptEnv+"_WORKER", filepath.Join(testDir, "worker.jsonl"))
	t.Setenv(fakeprovider.ScriptEnv+"_ENGINEERING", filepath.Join(testDir, "engineering.jsonl"))
	pushWorker := func(branch string) string {
		return `git push -q origin ` + branch + ` && asmai effect push "origin/` + branch + `@$(git rev-parse HEAD)"`
	}

	engineering := concat(agentSession,
		turn(1, "handoff",
			fakeRun(`asmai inbox --dispatch 1`, 0, "Add a greeting"),
			fakeRun(`asmai handoff accept --handoff 1`, 0, "reply dispatch 2"),
			fakeRun(`asmai assign --job 1 --outcome 'Add greeting.txt' --criterion 'greeting.txt says hello'`, 0, "assignment 1 of job 1 given to worker1@engineering; dispatch 3"),
		),
		turn(4, "result1",
			fakeRun(`asmai inbox --dispatch 4`, 0, "result [0-9]+ \\| assignment 1"),
			awaitFile("w1-done"),
			fakeRun(`asmai accept --assignment 1 --reason 'greeting.txt says hello'`, 0, "fast-forwarded"),
			// The job branch has never been pushed, so nothing on origin is
			// outside, and the daemon pushes it.
			fakeRun(`asmai push 1`, 0, "job 1: the daemon pushed "+jobBranch+" at "+sha+" to origin with a plain push; effect [0-9]+ recorded in dispatch 4"),
			fakeRun(`asmai push`, 2, "name the job number"),
			// Someone else pushes a commit to the job branch on origin.
			fakeRun(`git clone -q "$(cat "$ASMAI_TEST_DIR/origin")" "$ASMAI_TEST_DIR/outsider" && cd "$ASMAI_TEST_DIR/outsider" && git checkout -q `+jobBranch+` && printf 'outside\n' > outside.txt && git add -A && git commit -q -m 'An outside commit' && git push -q origin `+jobBranch+` && git rev-parse HEAD > "$ASMAI_TEST_DIR/outside"`, 0, ""),
			fakeRun(`asmai push 1`, 1, "(?s)nothing was pushed: origin/"+jobBranch+" has 1 commit\\(s\\) AsmAI did not make, "+sha+", recorded as outside change [0-9]+.*Assign a worker to merge origin/"+jobBranch),
			fakeRun(`asmai brief 1 | grep -q "AsmAI did not make ($(cat "$ASMAI_TEST_DIR/outside"))"`, 0, ""),
			fakeRun(`asmai brief 1`, 0, "(?s)Pushes of the job branch:\n- [0-9]+ pushed "+sha+" to origin, effect [0-9]+\n- [0-9]+ refused at "+sha+": outside change, origin/"+jobBranch+" has commits AsmAI did not make \\("+sha+"\\), not merged in yet"),
			fakeRun(`cp "$ASMAI_TEST_DIR/worker-2.jsonl" "$ASMAI_TEST_DIR/worker.jsonl"`, 0, ""),
			fakeRun(`asmai assign --job 1 --outcome 'Merge origin/`+jobBranch+` into your assignment branch, keeping its outside commits' --criterion 'the outside commit is merged in'`, 0, "assignment 2 of job 1 given to worker1@engineering; dispatch 5"),
		),
		turn(6, "result2",
			fakeRun(`asmai inbox --dispatch 6`, 0, "result [0-9]+ \\| assignment 2"),
			awaitFile("w2-done"),
			fakeRun(`asmai accept --assignment 2 --reason 'the outside commit is merged in'`, 0, "fast-forwarded"),
			fakeRun(`asmai push 1`, 0, "job 1: the daemon pushed "+jobBranch+" at "+sha+" to origin with a plain push; effect [0-9]+ recorded in dispatch 6"),
			fakeRun(`asmai brief 1`, 0, "outside change, origin/"+jobBranch+" has commits AsmAI did not make \\("+sha+"\\), merged in by "+sha),
			touchFile("leader-done"),
		),
		[]string{idleSession},
	)
	worker1 := concat(agentSession, turn(3, "assignment",
		fakeRun(`asmai inbox --dispatch 3`, 0, "assignment 1"),
		fakeRun(`printf 'hello\n' > greeting.txt && git add -A && git commit -q -m 'Add greeting.txt'`, 0, ""),
		// Only the delivery owner asks for the push.
		fakeRun(`asmai push 1`, 1, "only leader@engineering, the delivery owner of a code job, leader@planning or leader@research, the delivery owner of a notes job, or leader@coordination, the delivery owner of a repository's setup, may run asmai push"),
		fakeRun(pushWorker("asmai/job-1/1"), 0, "recorded: push"),
		fakeRun(`asmai result --evidence 'greeting.txt says hello' --test none --check none --gap none --pr-section 'Adds greeting.txt.'`, 0, "result recorded for assignment 1"),
	), []string{touchFile("w1-done"), idleSession})
	worker2 := concat(agentSession, turn(5, "assignment",
		fakeRun(`asmai inbox --dispatch 5`, 0, "assignment 2"),
		fakeRun(`git fetch -q origin && git merge --no-edit origin/`+jobBranch, 0, ""),
		fakeRun(`test -f outside.txt && test -f greeting.txt`, 0, ""),
		fakeRun(pushWorker("asmai/job-1/2"), 0, "recorded: push"),
		fakeRun(`asmai result --evidence 'outside.txt and greeting.txt are both there' --test none --check none --gap none --pr-section 'Merges the outside commit on the job branch.'`, 0, "result recorded for assignment 2"),
	), []string{touchFile("w2-done"), idleSession})
	writeFiles(t, testDir, map[string][]string{
		"engineering.jsonl": engineering,
		"worker.jsonl":      worker1,
		"worker-2.jsonl":    worker2,
	})

	coordination := []string{
		hookStep("SessionStart", "", ""),
		`{"screen":"> "}`,
		`{"expect":"Please add a greeting\r"}`,
		hookStep("UserPromptSubmit", "", "Please add a greeting"),
		fakeRun(`asmai job open --message latest --repository fixture --reading 'Add a greeting' --criterion 'Greeting is printed'`, 0, "Opened job 1"),
		// Coordination's leader owns the delivery of a repository's setup only,
		// and asks for a push while it works on a dispatch.
		fakeRun(`asmai push 1`, 1, "has no current dispatch to tag the push with"),
		fakeRun(`asmai handoff send --job 1 --to engineering --outcome 'Add a greeting' --decisions none --evidence none --constraints none --permissions none --criterion 'Greeting is printed'`, 0, "dispatch 1"),
		hookStep("Stop", "", ""),
		`{"expect":"asmai inbox --dispatch 2\r"}`,
		hookStep("UserPromptSubmit", "reply", "asmai inbox --dispatch 2"),
		fakeRun(`asmai inbox --dispatch 2`, 0, "accepted from leader@engineering"),
		hookStep("Stop", "reply", ""),
		idleSession,
	}
	readyFactory(t, coordination)
	t.Cleanup(func() {
		if t.Failed() {
			log, _, _ := runAsmai(t, "log")
			t.Logf("the daemon's log:\n%s", log)
		}
	})
	if _, stderr, code := runAsmai(t, "start"); code != 0 {
		t.Fatal(stderr)
	}
	// The repository's instructions say to push the job branch another way,
	// which never changes how the daemon pushes it.
	fixture := registerWritingFixtureWith(t, map[string]string{
		"AGENTS.md": "Push the job branch with `git push --force` through no-mistakes.\n",
	})
	if err := os.WriteFile(filepath.Join(testDir, "origin"), []byte(fixture.origin), 0o600); err != nil {
		t.Fatal(err)
	}
	terminal := inTerminal(t, 100, 25)
	waitFor(t, "Coordination prompt", terminal.shows(">"))
	terminal.ptmx.WriteString("Please add a greeting\r")
	waitFor(t, "Engineering's leader asking for the last push", touched(testDir, "leader-done"))

	stateDir := filepath.Join(os.Getenv("HOME"), ".local", "state", "asmai")
	clone := filepath.Join(stateDir, "repositories", "fixture")
	data, err := os.ReadFile(filepath.Join(testDir, "outside"))
	if err != nil {
		t.Fatal(err)
	}
	outside := strings.TrimSpace(string(data))

	// Origin's job branch is the job branch's tip, which kept the outside
	// commit and the accepted work: it was never pushed over.
	jobTip := gitIn(t, clone, "rev-parse", "refs/heads/"+jobBranch)
	if got := gitIn(t, fixture.origin, "rev-parse", "refs/heads/"+jobBranch); got != jobTip {
		t.Errorf("origin's job branch is at %s, want the job branch's tip %s", got, jobTip)
	}
	gitIn(t, fixture.origin, "merge-base", "--is-ancestor", outside, jobTip)
	if got := gitIn(t, fixture.origin, "show", jobTip+":greeting.txt"); got != "hello" {
		t.Errorf("greeting.txt on origin's job branch is %q", got)
	}

	// Each push is the daemon's effect, tagged with the leader's dispatch;
	// the refusal is an outside change, which the last push took in.
	type effectEntry struct {
		Agent    string `json:"agent"`
		Kind     string `json:"kind"`
		Ref      string `json:"ref"`
		Dispatch int    `json:"dispatch"`
	}
	var daemonEffects []effectEntry
	for _, e := range journaled(t, "effect.recorded") {
		if eff := decode[effectEntry](t, e); eff.Agent == "daemon" {
			daemonEffects = append(daemonEffects, eff)
		}
	}
	if len(daemonEffects) != 2 || daemonEffects[0].Kind != "push" || daemonEffects[0].Dispatch != 4 || daemonEffects[1].Dispatch != 6 || daemonEffects[1].Ref != "origin/"+jobBranch+"@"+jobTip {
		t.Errorf("the daemon's effects are %+v", daemonEffects)
	}
	type pushEntry struct {
		ID      int      `json:"id"`
		Result  string   `json:"result"`
		Commit  string   `json:"commit"`
		Outside []string `json:"outside"`
		Found   string   `json:"found"`
		Leader  string   `json:"leader"`
		TookIn  []int    `json:"took_in"`
	}
	changes := journaled(t, "outside_change.found")
	if len(changes) != 1 {
		t.Fatalf("outside changes: %+v", changes)
	}
	change := decode[pushEntry](t, changes[0])
	if change.Result != "refused" || len(change.Outside) != 1 || change.Outside[0] != outside || change.Found != "origin/"+jobBranch || change.Leader != "leader@engineering" {
		t.Errorf("the outside change is %+v", change)
	}
	pushed := journaled(t, "job.branch.pushed")
	if len(pushed) != 2 {
		t.Fatalf("pushes: %+v", pushed)
	}
	if first := decode[pushEntry](t, pushed[0]); first.Result != "pushed" || len(first.TookIn) != 0 {
		t.Errorf("the first push is %+v", first)
	}
	if last := decode[pushEntry](t, pushed[1]); last.Commit != jobTip || len(last.TookIn) != 1 || last.TookIn[0] != change.ID {
		t.Errorf("the last push is %+v, want it at %s taking in outside change %d", last, jobTip, change.ID)
	}
}
