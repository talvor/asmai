// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/talvor/asmai/internal/fakeprovider"
)

// TestRedCIIsReRunOnceOnAJudgedDecisionThenAFixsNewerPushReplacesTheWait
// drives a job end to end with the fake provider and gh replaced at its
// boundary, through Quality's validation, the daemon's push and the draft
// pull request, to red CI. The daemon tells Engineering's leader the failing
// checks on the pushed head; the leader judges the failure unrelated to the
// change and records that as a delegated decision, on which the daemon
// re-runs the failed jobs once, and a second re-run of the same head is
// refused. Red again, the leader assigns a fix, which is accepted, validated
// by Quality at the new head and pushed; the daemon drops its wait on the old
// head for the new one, and once that is green marks the pull request ready.
// The leader updates the body each time the evidence changes, recording each
// update as an effect.
func TestRedCIIsReRunOnceOnAJudgedDecisionThenAFixsNewerPushReplacesTheWait(t *testing.T) {
	gitIdentity(t)
	testDir := t.TempDir()
	t.Setenv("ASMAI_TEST_DIR", testDir)
	const (
		jobBranch = "asmai/test/job-1-add-a-greeting"
		url       = "https://github.com/talvor/asmai-fixture/pull/7"
		runURL    = "https://github.com/talvor/asmai-fixture/actions/runs/41/job/1"
		sha       = "[0-9a-f]{40}"
	)
	// Each worker session plays whatever its script file holds when it
	// starts, so the leaders put the script of the next one in place.
	t.Setenv(fakeprovider.ScriptEnv+"_WORKER", filepath.Join(testDir, "worker.jsonl"))
	t.Setenv(fakeprovider.ScriptEnv+"_QUALITY_WORKER", filepath.Join(testDir, "quality-worker.jsonl"))
	t.Setenv(fakeprovider.ScriptEnv+"_ENGINEERING", filepath.Join(testDir, "engineering.jsonl"))
	t.Setenv(fakeprovider.ScriptEnv+"_QUALITY", filepath.Join(testDir, "quality.jsonl"))

	handoffToQuality := func(evidence string, more ...string) string {
		return `asmai handoff send --job 1 --to quality --outcome 'Validate the job branch at its exact head' --decisions none --evidence '` + evidence + `' --constraints none --permissions none --criterion 'greeting.txt says hello' ` + strings.Join(more, " ")
	}
	updateBody := func(text string) []string {
		return []string{
			fakeRun(`printf '## Summary\n\nAdds greeting.txt.\n\n## AsmAI\n\nJob 1: Add a greeting. `+text+`\n' > "$ASMAI_TEST_DIR/body.md"`, 0, ""),
			fakeRun(`gh pr edit 7 --body-file "$ASMAI_TEST_DIR/body.md"`, 0, ""),
			fakeRun(`asmai effect pull-request `+url, 0, "pull request #7 of job 1 recorded"),
		}
	}
	rerun := `asmai decision record --job 1 --choice rerun-failed --rationale 'the runner lost its network, which the change cannot cause' --evidence 'the log of CI / test shows a DNS failure fetching a dependency'`
	engineering := concat(agentSession,
		turn(1, "handoff",
			fakeRun(`asmai inbox --dispatch 1`, 0, "Add a greeting"),
			fakeRun(`asmai handoff accept --handoff 1`, 0, "reply dispatch 2"),
			fakeRun(`asmai assign --job 1 --outcome 'Add greeting.txt' --criterion 'greeting.txt says hello'`, 0, "dispatch 3"),
		),
		turn(4, "result1",
			fakeRun(`asmai inbox --dispatch 4`, 0, "result [0-9]+ \\| assignment 1"),
			awaitFile("w1-done"),
			fakeRun(`asmai accept --assignment 1 --reason 'greeting.txt says hello'`, 0, "fast-forwarded"),
			fakeRun(handoffToQuality("result 1 accepted"), 0, "handoff 2 sent to leader@quality for job 1; dispatch 5"),
		),
		turn(6, "accepted", fakeRun(`asmai inbox --dispatch 6`, 0, "accepted from leader@quality")),
		turn(9, "validation", concat(
			[]string{
				fakeRun(`asmai inbox --dispatch 9`, 0, "(?s)validation from leader@quality.*0 blocking"),
				// No head is pushed and watched yet, so there is nothing to
				// re-run.
				fakeRun(rerun, 1, "no pushed head with a pull request"),
				fakeRun(`asmai push 1`, 0, "the daemon pushed "+jobBranch),
				fakeRun(`printf '## Summary\n\nAdds greeting.txt.\n\n## AsmAI\n\nJob 1: Add a greeting. CI has not reported yet.\n' > "$ASMAI_TEST_DIR/body.md"`, 0, ""),
				fakeRun(`gh pr create --draft --base main --head `+jobBranch+` --title 'Add a greeting' --body-file "$ASMAI_TEST_DIR/body.md"`, 0, "^"+url+"\n$"),
				fakeRun(`asmai effect pull-request `+url, 0, "pull request #7 of job 1 recorded"),
				// The checks are not red yet.
				fakeRun(rerun, 1, "are not red, so there is nothing to re-run"),
			},
			[]string{touchFile("opened")},
		)...),
		// Red: the leader judges the failure unrelated to the change, and the
		// daemon re-runs the failed jobs once on that delegated decision.
		turn(10, "red1", concat(
			[]string{
				fakeRun(`asmai inbox --dispatch 10`, 0, "(?s)dispatch 10 \\| job 1 \\| checks-red from daemon\nThe checks on head "+sha+" of job 1's pull request #7 are red\\. Failing: CI / test \\("+runURL+"\\)\\. If you judge the failure unrelated to the change, you may re-run the failed jobs once for this head.*asmai decision record --job 1 --choice rerun-failed"),
				fakeRun(`asmai brief 1`, 0, "(?s)- red at "+sha+".*\n  - CI / test: failed\nJudged re-runs of red checks \\(once per head\\):\n- none"),
				fakeRun(`asmai decision record --job 1 --choice merge-anyway --rationale x --evidence y`, 1, "needs --relied-on"),
				fakeRun(`asmai decision record --job 1 --choice merge-anyway --relied-on 'the job mandate' --rationale x --evidence y`, 1, "give --assumption, or --assumption none"),
				fakeRun(`asmai decision record --job 1 --choice rerun-failed --rationale x`, 1, "needs --evidence"),
				fakeRun(rerun, 0, "(?s)job 1: decision 1 recorded, delegated by leader@engineering: rerun-failed\nthe daemon re-ran the failed jobs of head "+sha+" once, in workflow runs 41: CI / test\nnext:.*a second re-run of this head is refused"),
				fakeRun(rerun, 1, "re-run once already, on decision 1; a second re-run of the same head is refused"),
				fakeRun(`asmai brief 1`, 0, "(?s)Judged re-runs of red checks \\(once per head\\):\n- the failed jobs of "+sha+" re-run once on decision 1, [^:]+:[^:]+:[^:]+: CI / test\n.*Decisions:\n- decision 1 \\| job 1 \\| delegated \\| accountable leader leader@engineering \\| [^\n]+\n  Choice: rerun-failed\n  Relied on: the job's mandate\n  Rationale: the runner lost its network, which the change cannot cause\n  Evidence: the log of CI / test shows a DNS failure[^\n]*\n  Assumptions: none\n  Reopen when: none\n"),
			},
			[]string{
				// Any other decision that affects later work is recorded with
				// the same fields, and needs no re-run.
				fakeRun(`asmai decision record --job 1 --choice 'fix CI / test with a new assignment if the re-run fails again' --relied-on 'the job mandate' --rationale 'a second red run of the same head is not a runner fault' --evidence 'the log of CI / test' --assumption 'the failure is in the change' --reopen-when 'CI / test fails on a second head'`, 0, "(?s)job 1: decision 2 recorded, delegated by leader@engineering: fix CI / test.*--record 2"),
				fakeRun(`asmai brief 1`, 0, "(?s)- decision 2 \\| job 1 \\| delegated \\| accountable leader leader@engineering \\| [^\n]+\n  Choice: fix CI / test with a new assignment if the re-run fails again\n  Relied on: the job mandate\n  Rationale: a second red run of the same head is not a runner fault\n  Evidence: the log of CI / test\n  Assumptions: the failure is in the change\n  Reopen when: CI / test fails on a second head\n"),
			},
			updateBody("CI: CI / test failed; its failed jobs are re-run once on decision 1."),
			[]string{touchFile("rerun-done")},
		)...),
		// Red again: a second re-run is refused, so the leader assigns a fix.
		turn(11, "red2",
			fakeRun(`asmai inbox --dispatch 11`, 0, "(?s)checks-red from daemon\nThe checks on head "+sha+" of job 1's pull request #7 are red\\. Failing: CI / test.*re-run once already, on decision 1, and failed again: a second re-run of this head is refused\\. Assign a fix"),
			fakeRun(rerun, 1, "a second re-run of the same head is refused"),
			fakeRun(`cp "$ASMAI_TEST_DIR/worker-2.jsonl" "$ASMAI_TEST_DIR/worker.jsonl"`, 0, ""),
			fakeRun(`asmai assign --job 1 --outcome 'Fix the failing check CI / test' --criterion 'CI / test passes' --record 99`, 1, "decision record 99 does not exist"),
			fakeRun(`asmai assign --job 1 --outcome 'Fix the failing check CI / test' --criterion 'CI / test passes' --record 2`, 0, "(?s)assignment 3 of job 1 given to worker1@engineering; dispatch 12.*decision records passed on: 2"),
		),
		turn(13, "result3",
			fakeRun(`asmai inbox --dispatch 13`, 0, "result [0-9]+ \\| assignment 3"),
			awaitFile("w2-done"),
			fakeRun(`asmai accept --assignment 3 --reason 'CI / test passes with the fix'`, 0, "fast-forwarded"),
			fakeRun(handoffToQuality("result 3 accepted, fixing CI / test", "--record 2"), 0, "(?s)handoff 3 sent to leader@quality for job 1; dispatch 14.*decision records passed on: 2"),
		),
		turn(15, "accepted2", fakeRun(`asmai inbox --dispatch 15`, 0, "accepted from leader@quality")),
		// Quality passes the new head: it is pushed, and the body says so.
		turn(18, "validation2", concat(
			[]string{
				fakeRun(`asmai inbox --dispatch 18`, 0, "(?s)validation from leader@quality.*0 blocking"),
				fakeRun(`asmai brief 1`, 0, "(?s)- 3 worker1@engineering \\(owner leader@engineering\\) accepted on branch asmai/test/job-1/3: Fix the failing check CI / test \\[decisions passed on: 2\\]\n"),
				fakeRun(`asmai push 1`, 0, "the daemon pushed "+jobBranch),
			},
			updateBody("A fix of CI / test is validated at the new head; CI has not reported on it yet."),
			[]string{touchFile("leader-done")},
		)...),
		[]string{idleSession},
	)
	quality := concat(agentSession,
		turn(5, "handoff",
			fakeRun(`asmai inbox --dispatch 5`, 0, "(?s)handoff 2 .*Head to validate: "+sha),
			fakeRun(`asmai handoff accept --handoff 2`, 0, "reply dispatch 6"),
			fakeRun(`asmai assign --read-only --commit `+headOf(5)+` --job 1 --outcome 'Validate the job branch at its exact head' --criterion 'the checks re-ran at the commit'`, 0, "assignment 2 of job 1 given to worker1@quality; dispatch 7"),
		),
		turn(8, "report",
			fakeRun(`asmai inbox --dispatch 8`, 0, "Validation report at commit "+sha),
			awaitFile("v1-done"),
			fakeRun(`asmai accept --assignment 2 --reason 'the checks re-ran at the exact head'`, 0, "delivered to leader@engineering as dispatch 9"),
		),
		turn(14, "handoff3",
			fakeRun(`asmai inbox --dispatch 14`, 0, "(?s)handoff 3 .*Head to validate: "+sha+"\nReview: changes since "+sha+"[^\n]*\nDecision records passed with the handoff:\n  decision 2 \\| job 1 \\| delegated .*Assumptions: the failure is in the change"),
			fakeRun(`asmai handoff accept --handoff 3`, 0, "reply dispatch 15"),
			fakeRun(`cp "$ASMAI_TEST_DIR/quality-worker-2.jsonl" "$ASMAI_TEST_DIR/quality-worker.jsonl"`, 0, ""),
			fakeRun(`asmai assign --read-only --commit `+headOf(14)+` --job 1 --outcome 'Validate the new head' --criterion 'the checks re-ran at the commit'`, 0, "assignment 4 of job 1 given to worker1@quality; dispatch 16"),
		),
		turn(17, "report2",
			fakeRun(`asmai inbox --dispatch 17`, 0, "Validation report at commit "+sha),
			awaitFile("v2-done"),
			fakeRun(`asmai accept --assignment 4 --reason 'the checks re-ran at the new head'`, 0, "delivered to leader@engineering as dispatch 18"),
		),
		[]string{idleSession},
	)
	validator := func(dispatch int, assignment, done string) []string {
		return concat(agentSession, turn(dispatch, "validate",
			fakeRun(`asmai inbox --dispatch `+strconv.Itoa(dispatch), 0, "assignment "+assignment),
			fakeRun(`grep -q hello greeting.txt`, 0, ""),
			fakeRun(`asmai result --evidence 'greeting.txt says hello at the head' --check 'grep hello greeting.txt -> passed' --gap none --finding none`, 0, "validation report recorded for assignment "+assignment),
		), []string{touchFile(done), idleSession})
	}
	worker1 := concat(agentSession, turn(3, "assignment",
		fakeRun(`asmai inbox --dispatch 3`, 0, "assignment 1"),
		fakeRun(`printf 'hello\n' > greeting.txt && git add -A && git commit -q -m 'Add greeting.txt'`, 0, ""),
		fakeRun(`git push -q origin asmai/test/job-1/1 && asmai effect push "origin/asmai/test/job-1/1@$(git rev-parse HEAD)"`, 0, "recorded: push"),
		fakeRun(`asmai result --evidence 'greeting.txt says hello' --test none --check none --gap none --pr-section 'Adds greeting.txt.'`, 0, "result recorded for assignment 1"),
	), []string{touchFile("w1-done"), idleSession})
	worker2 := concat(agentSession, turn(12, "fix",
		fakeRun(`asmai inbox --dispatch 12`, 0, "(?s)assignment 3.*Fix the failing check CI / test.*Decision records passed to you with this assignment:\n  decision 2 \\| job 1 \\| delegated .*Reopen when: CI / test fails on a second head"),
		fakeRun(`printf 'hello\n' > greeting.txt && printf 'ok\n' > ci-fixed.txt && git add -A && git commit -q -m 'Fix CI / test'`, 0, ""),
		fakeRun(`git push -q origin asmai/test/job-1/3 && asmai effect push "origin/asmai/test/job-1/3@$(git rev-parse HEAD)"`, 0, "recorded: push"),
		fakeRun(`asmai result --evidence 'CI / test passes' --test none --check 'grep hello greeting.txt -> passed' --gap none --pr-section 'Fixes CI / test.'`, 0, "result recorded for assignment 3"),
	), []string{touchFile("w2-done"), idleSession})
	writeFiles(t, testDir, map[string][]string{
		"engineering.jsonl":      engineering,
		"quality.jsonl":          quality,
		"worker.jsonl":           worker1,
		"worker-2.jsonl":         worker2,
		"quality-worker.jsonl":   validator(7, "2", "v1-done"),
		"quality-worker-2.jsonl": validator(16, "4", "v2-done"),
	})

	report := "job 1: delivered, ready for review: " + url
	coordination := concat(
		[]string{
			hookStep("SessionStart", "", ""),
			`{"screen":"> "}`,
			`{"expect":"Please add a greeting\r"}`,
			hookStep("UserPromptSubmit", "", "Please add a greeting"),
			fakeRun(`asmai job open --message latest --repository fixture --reading 'Add a greeting' --criterion 'Greeting is printed'`, 0, "Opened job 1"),
			fakeRun(`asmai handoff send --job 1 --to engineering --outcome 'Add a greeting' --decisions none --evidence none --constraints none --permissions none --criterion 'Greeting is printed'`, 0, "dispatch 1"),
			fakeRun(`asmai decision record --job 1 --choice rerun-failed --rationale x --evidence y`, 1, "has no current dispatch to tag the decision with"),
			hookStep("Stop", "", ""),
		},
		turn(2, "reply", fakeRun(`asmai inbox --dispatch 2`, 0, "accepted from leader@engineering")),
		turn(19, "delivery",
			fakeRun(`asmai inbox --dispatch 19`, 0, "(?s)delivery from daemon\nJob 1 is delivered: its pull request #7, "+url+", is ready for review at head "+sha+", with every check green \\(CI / test\\)"),
			`{"screen":"`+report+`\r\n> "}`,
			touchFile("coordination-done"),
		),
		[]string{idleSession},
	)
	bin := filepath.Join(testDir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(fakeCIGH), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	readyFactory(t, coordination)
	calls := filepath.Join(testDir, "gh", "calls")
	t.Cleanup(func() {
		if t.Failed() {
			log, _, _ := runAsmai(t, "log")
			t.Logf("the daemon's log:\n%s", log)
			data, _ := os.ReadFile(calls)
			t.Logf("gh's calls:\n%s", data)
		}
	})
	if _, stderr, code := runAsmai(t, "start"); code != 0 {
		t.Fatal(stderr)
	}
	fixture := registerWritingFixture(t)
	if err := os.WriteFile(filepath.Join(testDir, "origin"), []byte(fixture.origin), 0o600); err != nil {
		t.Fatal(err)
	}
	terminal := inTerminal(t, 100, 25)
	waitFor(t, "Coordination prompt", terminal.shows(">"))
	terminal.ptmx.WriteString("Please add a greeting\r")
	waitFor(t, "Engineering's leader opening the pull request", touched(testDir, "opened"))

	stateDir := filepath.Join(os.Getenv("HOME"), ".local", "state", "asmai")
	clone, err := filepath.EvalSymlinks(filepath.Join(stateDir, "repositories", "fixture"))
	if err != nil {
		t.Fatal(err)
	}
	first := gitIn(t, clone, "rev-parse", "refs/heads/"+jobBranch)
	writeChecks := func(head, rollup string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(testDir, "rollup-"+head), []byte(rollup), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	const (
		running = `[{"__typename":"CheckRun","name":"test","workflowName":"CI","status":"IN_PROGRESS","conclusion":"","detailsUrl":"` + runURL + `"}]`
		red     = `[{"__typename":"CheckRun","name":"test","workflowName":"CI","status":"COMPLETED","conclusion":"FAILURE","detailsUrl":"` + runURL + `"}]`
		green   = `[{"__typename":"CheckRun","name":"test","workflowName":"CI","status":"COMPLETED","conclusion":"SUCCESS","detailsUrl":"https://github.com/talvor/asmai-fixture/actions/runs/42/job/2"}]`
	)
	ghCalls := func() []string {
		data, _ := os.ReadFile(calls)
		return strings.Split(strings.TrimSpace(string(data)), "\n")
	}
	reruns := func() []string {
		var found []string
		for _, line := range ghCalls() {
			if dir, args, _ := strings.Cut(line, ": "); strings.HasPrefix(args, "run rerun") {
				found = append(found, args)
				if dir != clone {
					t.Errorf("the daemon ran gh %s in %s, not the job's clone", args, dir)
				}
			}
		}
		return found
	}

	type checkEntry struct {
		Name  string `json:"name"`
		State string `json:"state"`
	}
	type observationEntry struct {
		Head    string       `json:"head"`
		Verdict string       `json:"verdict"`
		Checks  []checkEntry `json:"checks"`
	}
	lastVerdict := func() string {
		o := journaled(t, "checks.observed")
		if len(o) == 0 {
			return ""
		}
		return decode[observationEntry](t, o[len(o)-1]).Verdict
	}
	waitFor(t, "the daemon reading the pushed head with no checks", func() bool { return lastVerdict() == "waiting" })

	// Red: Engineering's leader is told, and re-runs the failed jobs once.
	writeChecks(first, red)
	waitFor(t, "the leader's judged re-run", touched(testDir, "rerun-done"))
	if got := reruns(); !slices.Equal(got, []string{"run rerun 41 --failed"}) {
		t.Fatalf("gh re-ran %q, want the failed jobs of run 41 once", got)
	}
	// The re-run runs, and fails again.
	writeChecks(first, running)
	waitFor(t, "the daemon recording the re-run running", func() bool { return lastVerdict() == "waiting" })
	writeChecks(first, red)
	waitFor(t, "the leader assigning a fix, validated and pushed", touched(testDir, "leader-done"))

	// A newer push replaces the wait on the old head.
	second := gitIn(t, clone, "rev-parse", "refs/heads/"+jobBranch)
	if second == first {
		t.Fatal("the fix did not move the job branch")
	}
	waitFor(t, "the daemon dropping the wait on the old head", func() bool { return len(journaled(t, "ci.wait.replaced")) == 1 })
	type replacedEntry struct {
		Job      int
		From, To string
	}
	if r := decode[replacedEntry](t, journaled(t, "ci.wait.replaced")[0]); r.Job != 1 || r.From != first || r.To != second {
		t.Errorf("the wait was replaced as %+v, want from %s to %s", r, first, second)
	}
	writeChecks(second, green)
	waitFor(t, "Coordination's leader reporting the delivery", touched(testDir, "coordination-done"))
	waitFor(t, "Coordination's report on the user's terminal", terminal.shows(report))

	// Each check's state on each head was recorded each time it changed: the
	// old head's red, re-run, and red again, then the new head's green.
	var seen []string
	for _, e := range journaled(t, "checks.observed") {
		o := decode[observationEntry](t, e)
		state := map[string]string{first: "first", second: "second"}[o.Head] + " " + o.Verdict
		for _, c := range o.Checks {
			state += " " + c.Name + "=" + c.State
		}
		seen = append(seen, state)
	}
	if want := "first waiting; first red CI / test=failed; first waiting CI / test=pending; first red CI / test=failed; second waiting; second green CI / test=passed"; strings.Join(seen, "; ") != want {
		t.Errorf("the checks observed are %q, want %q", strings.Join(seen, "; "), want)
	}

	// The re-run is recorded once, with the delegated decision it rests on,
	// beside the other decision the leader recorded, which re-ran nothing.
	type decisionEntry struct {
		ID          int      `json:"id"`
		Job         int      `json:"job"`
		Leader      string   `json:"leader"`
		Source      string   `json:"source"`
		ReliedOn    string   `json:"relied_on"`
		Choice      string   `json:"choice"`
		Rationale   string   `json:"rationale"`
		Evidence    string   `json:"evidence"`
		Assumptions []string `json:"assumptions"`
		ReopenWhen  []string `json:"reopen_when"`
	}
	decisions := journaled(t, "decision.recorded")
	if len(decisions) != 2 {
		t.Fatalf("decisions: %+v", decisions)
	}
	if d := decode[decisionEntry](t, decisions[1]); d.ID != 2 || d.ReliedOn != "the job mandate" || !slices.Equal(d.Assumptions, []string{"the failure is in the change"}) || !slices.Equal(d.ReopenWhen, []string{"CI / test fails on a second head"}) {
		t.Errorf("the second decision is %+v", d)
	}
	if d := decode[decisionEntry](t, decisions[0]); d.ID != 1 || d.Job != 1 || d.Leader != "leader@engineering" || d.Source != "delegated" || d.Choice != "rerun-failed" || !strings.Contains(d.Rationale, "the runner lost its network") || !strings.Contains(d.Evidence, "DNS failure") {
		t.Errorf("the decision is %+v", d)
	}
	type rerunEntry struct {
		Job      int     `json:"job"`
		Head     string  `json:"head"`
		Runs     []int64 `json:"runs"`
		Decision int     `json:"decision"`
	}
	rerunsJournaled := journaled(t, "checks.rerun")
	if len(rerunsJournaled) != 1 {
		t.Fatalf("re-runs: %+v", rerunsJournaled)
	}
	if r := decode[rerunEntry](t, rerunsJournaled[0]); r.Job != 1 || r.Head != first || !slices.Equal(r.Runs, []int64{41}) || r.Decision != 1 {
		t.Errorf("the re-run is %+v", r)
	}
	if got := reruns(); len(got) != 1 {
		t.Errorf("gh re-ran %q, want once", got)
	}

	// The pull request was recorded when it opened and each time its body
	// was updated, each as an effect, and the body says the latest evidence.
	if n := len(journaled(t, "pull_request.recorded")); n != 3 {
		t.Errorf("the pull request was recorded %d times, want 3", n)
	}
	type effectEntry struct {
		Kind  string `json:"kind"`
		Agent string `json:"agent"`
	}
	effects := 0
	for _, e := range journaled(t, "effect.recorded") {
		if f := decode[effectEntry](t, e); f.Kind == "pull-request" && f.Agent == "leader@engineering" {
			effects++
		}
	}
	if effects != 3 {
		t.Errorf("%d pull-request effects, want 3", effects)
	}
	body, _ := os.ReadFile(filepath.Join(testDir, "gh", "body-7.md"))
	if !strings.Contains(string(body), "A fix of CI / test is validated at the new head") {
		t.Errorf("the pull request's body is %q", body)
	}

	// The new head was delivered, and gh marked the draft ready once.
	type deliveryEntry struct {
		Head string `json:"head"`
	}
	if delivered := journaled(t, "job.delivered"); len(delivered) != 1 || decode[deliveryEntry](t, delivered[0]).Head != second {
		t.Errorf("deliveries: %+v", delivered)
	}
	var readies []string
	for _, line := range ghCalls() {
		_, args, _ := strings.Cut(line, ": ")
		switch {
		case strings.HasPrefix(args, "pr ready"):
			readies = append(readies, args)
		case strings.HasPrefix(args, "pr merge"), strings.HasPrefix(args, "pr review"):
			t.Errorf("gh ran %q", args)
		}
	}
	if !slices.Equal(readies, []string{"pr ready 7"}) {
		t.Errorf("gh marked the pull request ready with %q", readies)
	}
}
