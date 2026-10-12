// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/fakeprovider"
)

// fakeCIGH is gh at its boundary for a pull request whose CI the daemon
// watches: a gh on the PATH, which the daemon and the agents both run, that
// keeps its pull requests in the test's directory, one directory each. It
// opens one with `gh pr create`, numbering them from 7, and refuses a draft
// while the test's directory holds refuse-draft. `gh pr view` shows it as
// GitHub would, with its head commit read from origin's job branch, which
// GitHub's follows each push, and when asked for statusCheckRollup with the
// checks the test's directory holds in rollup-<head> for that head, or else
// in rollup, none until the test writes some. `gh pr ready` makes it no
// longer a draft, `gh pr edit --title` retitles it and `gh pr edit
// --body-file` replaces its body. `gh run rerun` re-runs nothing itself: the
// test writes the checks that follow. `gh repo view` shows origin as
// talvor/asmai-fixture. It records every call, with where it ran, and does
// nothing else: nothing merges.
const fakeCIGH = `#!/bin/sh
state="$ASMAI_TEST_DIR/gh"
mkdir -p "$state"
printf '%s: %s\n' "$(pwd -P)" "$*" >> "$state/calls"
case "$1 $2" in
"pr create")
	shift 2
	draft=false title= head= body=
	while [ $# -gt 0 ]; do
		case "$1" in
		--draft) draft=true ;;
		--title) title=$2; shift ;;
		--head) head=$2; shift ;;
		--body-file) body=$2; shift ;;
		--base) shift ;;
		esac
		shift
	done
	if $draft && [ -e "$ASMAI_TEST_DIR/refuse-draft" ]; then
		echo 'pull request create failed: GraphQL: Draft pull requests are not supported in this repository. (createPullRequest)' >&2
		exit 1
	fi
	n=$(( $(cat "$state/count" 2>/dev/null || echo 6) + 1 ))
	echo "$n" > "$state/count"
	mkdir -p "$state/pr-$n"
	printf '%s' "$title" > "$state/pr-$n/title"
	echo "$draft" > "$state/pr-$n/draft"
	echo "$head" > "$state/pr-$n/branch"
	cp "$body" "$state/body-$n.md"
	echo "https://github.com/talvor/asmai-fixture/pull/$n"
	;;
"pr view")
	n=${3##*/}
	[ -d "$state/pr-$n" ] || { echo "no pull requests found for $3" >&2; exit 1; }
	head=$(git ls-remote "$(cat "$ASMAI_TEST_DIR/origin")" "refs/heads/$(cat "$state/pr-$n/branch")" | cut -f1)
	rollup=
	case "$5" in
	*statusCheckRollup*)
		checks="$ASMAI_TEST_DIR/rollup-$head"
		[ -e "$checks" ] || checks="$ASMAI_TEST_DIR/rollup"
		rollup=",\"statusCheckRollup\":$(cat "$checks" 2>/dev/null || echo '[]')"
		;;
	esac
	printf '{"number":%d,"url":"https://github.com/talvor/asmai-fixture/pull/%d","title":"%s","isDraft":%s,"headRefName":"%s","headRefOid":"%s","state":"OPEN"%s}\n' "$n" "$n" "$(cat "$state/pr-$n/title")" "$(cat "$state/pr-$n/draft")" "$(cat "$state/pr-$n/branch")" "$head" "$rollup"
	;;
"pr ready")
	echo false > "$state/pr-$3/draft"
	;;
"pr edit")
	case "$4" in
	--title) printf '%s' "$5" > "$state/pr-$3/title" ;;
	--body-file) cp "$5" "$state/body-$3.md" ;;
	*) echo "the fake gh edits only a title or a body" >&2; exit 1 ;;
	esac
	;;
"run rerun")
	;;
"repo view")
	echo '{"url":"https://github.com/talvor/asmai-fixture"}'
	;;
*)
	echo "the fake gh does not do: $*" >&2
	exit 1
	;;
esac
`

func TestTheDaemonMarksTheDraftPullRequestReadyWhenCIIsGreenAndCoordinationReportsTheLinkAsTheJobEnds(t *testing.T) {
	deliverJob(t, false)
}

func TestTheDaemonTakesNotReadyOutOfThePullRequestsTitleWhenCIIsGreen(t *testing.T) {
	deliverJob(t, true)
}

// deliverJob drives a job end to end with the fake provider and gh replaced
// at its boundary: a worker's accepted work, Quality's validation of the
// head, the daemon's push, and the pull request Engineering's leader opens, a
// draft, or when refuseDraft has GitHub refuse the draft one with "[not
// ready]" at the start of its title. The daemon watches the checks on the
// pushed head, with no agent: while the head has no checks, and while its
// check runs, it waits; once it is green, the daemon marks the pull request
// ready, records the delivery and ends the job, removing everything local to
// it while its branches stay on origin, and Coordination's leader reports the
// link.
func deliverJob(t *testing.T, refuseDraft bool) {
	gitIdentity(t)
	testDir := t.TempDir()
	t.Setenv("ASMAI_TEST_DIR", testDir)
	const (
		jobBranch = "asmai/job-1-add-a-greeting"
		url       = "https://github.com/talvor/asmai-fixture/pull/7"
		sha       = "[0-9a-f]{40}"
	)
	if refuseDraft {
		if err := os.WriteFile(filepath.Join(testDir, "refuse-draft"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(fakeprovider.ScriptEnv+"_WORKER", filepath.Join(testDir, "worker.jsonl"))
	t.Setenv(fakeprovider.ScriptEnv+"_QUALITY_WORKER", filepath.Join(testDir, "quality-worker.jsonl"))
	t.Setenv(fakeprovider.ScriptEnv+"_ENGINEERING", filepath.Join(testDir, "engineering.jsonl"))
	t.Setenv(fakeprovider.ScriptEnv+"_QUALITY", filepath.Join(testDir, "quality.jsonl"))

	create := `gh pr create --draft --base main --head ` + jobBranch + ` --title 'Add a greeting' --body-file "$ASMAI_TEST_DIR/body.md"`
	opened := []string{fakeRun(create, 0, "^"+url+"\n$")}
	if refuseDraft {
		opened = []string{
			fakeRun(create, 1, "Draft pull requests are not supported"),
			fakeRun(`gh pr create --base main --head `+jobBranch+` --title '[not ready] Add a greeting' --body-file "$ASMAI_TEST_DIR/body.md"`, 0, "^"+url+"\n$"),
		}
	}
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
			fakeRun(`asmai handoff send --job 1 --to quality --outcome 'Validate the job branch at its exact head' --decisions none --evidence 'result 1 accepted' --constraints none --permissions none --criterion 'greeting.txt says hello'`, 0, "handoff 2 sent to leader@quality for job 1; dispatch 5"),
		),
		turn(6, "accepted", fakeRun(`asmai inbox --dispatch 6`, 0, "accepted from leader@quality")),
		turn(9, "validation", concat(
			[]string{
				fakeRun(`asmai inbox --dispatch 9`, 0, "(?s)validation from leader@quality.*0 blocking"),
				fakeRun(`asmai push 1`, 0, "the daemon pushed "+jobBranch),
				fakeRun(`printf '## Summary\n\nAdds greeting.txt.\n\n## AsmAI\n\nJob 1: Add a greeting.\n' > "$ASMAI_TEST_DIR/body.md"`, 0, ""),
			},
			opened,
			[]string{
				fakeRun(`asmai effect pull-request `+url, 0, "pull request #7 of job 1 recorded"),
				touchFile("leader-done"),
			},
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
			awaitFile("v-done"),
			fakeRun(`asmai accept --assignment 2 --reason 'the checks re-ran at the exact head'`, 0, "delivered to leader@engineering as dispatch 9"),
		),
		[]string{idleSession},
	)
	validator := concat(agentSession, turn(7, "validate",
		fakeRun(`asmai inbox --dispatch 7`, 0, "assignment 2"),
		fakeRun(`test "$(cat greeting.txt)" = hello`, 0, ""),
		fakeRun(`asmai result --evidence 'greeting.txt says hello at the head' --check 'test greeting.txt -> passed' --gap none --finding none`, 0, "validation report recorded for assignment 2"),
	), []string{touchFile("v-done"), idleSession})
	worker := concat(agentSession, turn(3, "assignment",
		fakeRun(`asmai inbox --dispatch 3`, 0, "assignment 1"),
		fakeRun(`printf 'hello\n' > greeting.txt && git add -A && git commit -q -m 'Add greeting.txt'`, 0, ""),
		fakeRun(`git push -q origin asmai/job-1/1 && asmai effect push "origin/asmai/job-1/1@$(git rev-parse HEAD)"`, 0, "recorded: push"),
		fakeRun(`asmai result --evidence 'greeting.txt says hello' --test none --check none --gap none --pr-section 'Adds greeting.txt.'`, 0, "result recorded for assignment 1"),
	), []string{touchFile("w1-done"), idleSession})
	writeFiles(t, testDir, map[string][]string{
		"engineering.jsonl":    engineering,
		"quality.jsonl":        quality,
		"worker.jsonl":         worker,
		"quality-worker.jsonl": validator,
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
			hookStep("Stop", "", ""),
		},
		turn(2, "reply", fakeRun(`asmai inbox --dispatch 2`, 0, "accepted from leader@engineering")),
		turn(10, "delivery",
			fakeRun(`asmai inbox --dispatch 10`, 0, "(?s)message [0-9]+ \\| dispatch 10 \\| job 1 \\| delivery from daemon\nJob 1 is delivered: its pull request #7, "+url+", is ready for review at head "+sha+", with every check green \\(CI / test\\)\\. The job has ended.*AsmAI never merges.*in a line tagged with job 1, with the link"),
			fakeRun(`asmai brief 1`, 0, "(?s)Job 1 \\| role: coordination \\| state: ended.*kept on origin; the job ended.*Checks on the pull request's head.*\n- green at "+sha+".*\n  - CI / test: passed\nJudged re-runs of red checks \\(once per head\\):\n- none\nDelivery:\n- pull request #7 "+url+" marked ready for review at "+sha),
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
	waitFor(t, "Engineering's leader recording the pull request", touched(testDir, "leader-done"))

	stateDir := filepath.Join(os.Getenv("HOME"), ".local", "state", "asmai")
	clone, err := filepath.EvalSymlinks(filepath.Join(stateDir, "repositories", "fixture"))
	if err != nil {
		t.Fatal(err)
	}
	head := gitIn(t, clone, "rev-parse", "refs/heads/"+jobBranch)
	view := filepath.Join(stateDir, "views", "job-1")
	checksCall := "pr view 7 --json number,url,title,isDraft,headRefName,headRefOid,isCrossRepository,state,statusCheckRollup"
	looked := func() int {
		data, _ := os.ReadFile(calls)
		return strings.Count(string(data), ": "+checksCall+"\n")
	}
	readied := func() bool {
		data, _ := os.ReadFile(calls)
		return strings.Contains(string(data), ": pr ready ") || strings.Contains(string(data), ": pr edit ")
	}

	// A head with no checks is never green: the daemon keeps looking, and
	// marks nothing ready.
	waitFor(t, "the daemon looking at the head's checks three times", func() bool { return looked() >= 3 })
	if readied() {
		t.Fatal("the pull request was marked ready while its head had no checks")
	}
	if n := len(journaled(t, "job.delivered")) + len(journaled(t, "job.ended")); n != 0 {
		t.Fatalf("the job was delivered or ended while its head had no checks")
	}

	// While its check runs, it waits too.
	writeRollup := func(rollup string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(testDir, "rollup"), []byte(rollup), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeRollup(`[{"__typename":"CheckRun","name":"test","workflowName":"CI","status":"IN_PROGRESS","conclusion":""}]`)
	waitFor(t, "the daemon recording the running check", func() bool { return len(journaled(t, "checks.observed")) >= 2 })
	before := looked()
	waitFor(t, "the daemon looking again", func() bool { return looked() >= before+2 })
	if readied() {
		t.Fatal("the pull request was marked ready while its check was running")
	}

	// Green: the daemon marks it ready, and the job ends.
	writeRollup(`[{"__typename":"CheckRun","name":"test","workflowName":"CI","status":"COMPLETED","conclusion":"SUCCESS","detailsUrl":"https://github.com/talvor/asmai-fixture/actions/runs/1"}]`)
	waitFor(t, "Coordination's leader reporting the delivery", touched(testDir, "coordination-done"))
	waitFor(t, "Coordination's report on the user's terminal", terminal.shows(report))
	waitFor(t, "the daemon removing what was local to the job", func() bool { return len(journaled(t, "job.local.removed")) == 1 })

	// Each check's state was recorded each time it changed: none, running,
	// then green, all on the pushed head.
	type checkEntry struct {
		Name  string `json:"name"`
		State string `json:"state"`
	}
	type observationEntry struct {
		Job     int          `json:"job"`
		Number  int          `json:"number"`
		Head    string       `json:"head"`
		Verdict string       `json:"verdict"`
		Checks  []checkEntry `json:"checks"`
	}
	var verdicts []string
	for _, e := range journaled(t, "checks.observed") {
		o := decode[observationEntry](t, e)
		if o.Job != 1 || o.Number != 7 || o.Head != head {
			t.Errorf("checks observed as %+v", o)
		}
		state := o.Verdict
		for _, c := range o.Checks {
			state += " " + c.Name + "=" + c.State
		}
		verdicts = append(verdicts, state)
	}
	if strings.Join(verdicts, "; ") != "waiting; waiting CI / test=pending; green CI / test=passed" {
		t.Errorf("the checks observed are %q", verdicts)
	}

	// The delivery is recorded with the pull request, the head and the time,
	// and the job ended in the same step.
	type deliveryEntry struct {
		Job    int       `json:"job"`
		Number int       `json:"number"`
		URL    string    `json:"url"`
		Head   string    `json:"head"`
		At     time.Time `json:"at"`
	}
	delivered := journaled(t, "job.delivered")
	if len(delivered) != 1 {
		t.Fatalf("deliveries: %+v", delivered)
	}
	if d := decode[deliveryEntry](t, delivered[0]); d.Job != 1 || d.Number != 7 || d.URL != url || d.Head != head || d.At.IsZero() {
		t.Errorf("the delivery is %+v", d)
	}
	if ended := journaled(t, "job.ended"); len(ended) != 1 || ended[0].ID != delivered[0].ID+1 {
		t.Errorf("the job ended as %+v", ended)
	}

	// gh marked it ready once, as a draft or by its title, and nothing
	// merged it.
	data, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	var readies []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		dir, args, _ := strings.Cut(line, ": ")
		switch {
		case strings.HasPrefix(args, "pr ready"), strings.HasPrefix(args, "pr edit"):
			readies = append(readies, args)
			if dir != clone {
				t.Errorf("the daemon ran gh %s in %s, not the job's clone", args, dir)
			}
		case strings.HasPrefix(args, "pr merge"), strings.HasPrefix(args, "pr review"):
			t.Errorf("gh ran %q", args)
		}
	}
	want := "pr ready 7"
	if refuseDraft {
		want = "pr edit 7 --title Add a greeting"
	}
	if len(readies) != 1 || readies[0] != want {
		t.Errorf("gh marked the pull request ready with %q, want %q", readies, want)
	}
	draft, _ := os.ReadFile(filepath.Join(testDir, "gh", "pr-7", "draft"))
	title, _ := os.ReadFile(filepath.Join(testDir, "gh", "pr-7", "title"))
	if strings.TrimSpace(string(draft)) != "false" || string(title) != "Add a greeting" {
		t.Errorf("the pull request is left draft=%s, titled %q", draft, title)
	}

	// Everything local to the job is removed; its branches stay on origin.
	if _, err := os.Lstat(view); err == nil {
		t.Errorf("the job's view %s is still there", view)
	}
	if _, err := os.Lstat(filepath.Join(stateDir, "workspaces", "job-1")); err == nil {
		t.Error("the job's workspaces are still there")
	}
	if out := gitIn(t, clone, "branch", "--list", "asmai/*"); out != "" {
		t.Errorf("AsmAI's clone still has the job's branches: %s", out)
	}
	if out := gitIn(t, clone, "worktree", "list", "--porcelain"); strings.Count(out, "worktree ") != 1 {
		t.Errorf("AsmAI's clone still has checkouts:\n%s", out)
	}
	if got := gitIn(t, fixture.origin, "rev-parse", "refs/heads/"+jobBranch); got != head {
		t.Errorf("origin's job branch is at %s, want %s", got, head)
	}
	if out := gitIn(t, fixture.origin, "branch", "--list", "asmai/job-1/1"); out == "" {
		t.Error("the worker's assignment branch is gone from origin")
	}

	// Nothing about the ended job is watched afterwards.
	after := looked()
	time.Sleep(3 * time.Second)
	if got := looked(); got != after {
		t.Errorf("the daemon looked at an ended job's checks %d more times", got-after)
	}
}
