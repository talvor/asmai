// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The checks fakeCI's GitHub reports on a pull request's head, each in turn.
const (
	noChecks     = `[]`
	checkRunning = `[{"__typename":"CheckRun","name":"test","workflowName":"CI","status":"IN_PROGRESS","conclusion":""}]`
	checkGreen   = `[{"__typename":"CheckRun","name":"test","workflowName":"CI","status":"COMPLETED","conclusion":"SUCCESS"}]`
	checkRed     = `[{"__typename":"CheckRun","name":"test","workflowName":"CI","status":"COMPLETED","conclusion":"FAILURE"}]`
	// A red check on a head forced red, and its re-run, a new check run of
	// the same workflow run, red again or green.
	runRed     = `[{"__typename":"CheckRun","name":"test","workflowName":"CI","status":"COMPLETED","conclusion":"FAILURE","detailsUrl":"https://github.com/o/r/actions/runs/41/job/1"}]`
	rerunning  = `[{"__typename":"CheckRun","name":"test","workflowName":"CI","status":"IN_PROGRESS","conclusion":"","detailsUrl":"https://github.com/o/r/actions/runs/41/job/2"}]`
	rerunRed   = `[{"__typename":"CheckRun","name":"test","workflowName":"CI","status":"COMPLETED","conclusion":"FAILURE","detailsUrl":"https://github.com/o/r/actions/runs/41/job/2"}]`
	rerunGreen = `[{"__typename":"CheckRun","name":"test","workflowName":"CI","status":"COMPLETED","conclusion":"SUCCESS","detailsUrl":"https://github.com/o/r/actions/runs/41/job/2"}]`
)

// fakeCI puts a gh on the PATH that stands in for GitHub with a local bare
// repository as origin: it opens pull requests, shows them, with the checks
// on their head when asked for statusCheckRollup, readies them, retitles them
// and closes them, keeping them in a directory of the test's, which it
// returns. A pull request's head is origin's branch, as GitHub's follows each
// push. Each pull request's checks are rollups, one per look, the last
// repeated. gh run rerun re-runs nothing itself: the rollups say what follows.
// With ignoreReady, gh pr ready does nothing.
func fakeCI(t *testing.T, origin string, ignoreReady bool, rollups ...string) string {
	t.Helper()
	ciWait, ciPoll = 5*time.Second, 10*time.Millisecond
	t.Cleanup(func() { ciWait, ciPoll = 10*time.Minute, 10*time.Second })
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	if err := os.Mkdir(state, 0o755); err != nil {
		t.Fatal(err)
	}
	for i, rollup := range rollups {
		if err := os.WriteFile(filepath.Join(state, "rollup-"+strconv.Itoa(i+1)), []byte(rollup), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ready := `echo false > "$state/pr-$3/draft"`
	if ignoreReady {
		ready = ":"
	}
	gh := `#!/bin/sh
state=` + state + `
echo "$*" >> "$state/calls"
case "$1 $2" in
"pr create")
	shift 2
	draft=false title= head=
	while [ $# -gt 0 ]; do
		case "$1" in
		--draft) draft=true ;;
		--title) title=$2; shift ;;
		--head) head=$2; shift ;;
		--base|--body) shift ;;
		esac
		shift
	done
	n=$(( $(cat "$state/count" 2>/dev/null || echo 0) + 1 ))
	echo "$n" > "$state/count"
	mkdir "$state/pr-$n"
	printf '%s' "$title" > "$state/pr-$n/title"
	echo "$draft" > "$state/pr-$n/draft"
	echo "$head" > "$state/pr-$n/branch"
	echo "Creating pull request for $head"
	echo "https://github.com/o/r/pull/$n"
	;;
"pr view")
	n=${3##*/}
	rollup=
	case "$5" in
	*statusCheckRollup*)
		look=$(( $(cat "$state/pr-$n/looks" 2>/dev/null || echo 0) + 1 ))
		echo "$look" > "$state/pr-$n/looks"
		while [ "$look" -gt 1 ] && [ ! -e "$state/rollup-$look" ]; do look=$((look - 1)); done
		rollup=",\"statusCheckRollup\":$(cat "$state/rollup-$look")"
		;;
	esac
	head=$(git ls-remote ` + origin + ` "refs/heads/$(cat "$state/pr-$n/branch")" | cut -f1)
	[ -n "$head" ] && printf '%s %s\n' "$head" "$(git --git-dir=` + origin + ` show "$head:.ci-mode" 2>/dev/null)" >> "$state/heads"
	s=OPEN
	[ -e "$state/closed-$n" ] && s=CLOSED
	printf '{"number":%d,"url":"https://github.com/o/r/pull/%d","title":"%s","isDraft":%s,"headRefName":"%s","headRefOid":"%s","state":"%s"%s}\n' "$n" "$n" "$(cat "$state/pr-$n/title")" "$(cat "$state/pr-$n/draft")" "$(cat "$state/pr-$n/branch")" "$head" "$s" "$rollup"
	;;
"pr ready") ` + ready + ` ;;
"pr edit") printf '%s' "$5" > "$state/pr-$3/title" ;;
"pr close") touch "$state/closed-${3##*/}" ;;
"run rerun") ;;
*) echo "the fake gh does not do: $*" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(gh), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return state
}

// leftNothing fails t unless every pull request the case opened is closed,
// none was merged or reviewed, and the scratch branch is gone from origin.
func leftNothing(t *testing.T, state, origin string, opened int) {
	t.Helper()
	for n := 1; n <= opened; n++ {
		if _, err := os.Stat(filepath.Join(state, "closed-"+string(rune('0'+n)))); err != nil {
			t.Errorf("pull request %d was not closed", n)
		}
	}
	if _, err := os.Stat(filepath.Join(state, "pr-"+string(rune('1'+opened)))); err == nil {
		t.Error("another pull request was opened")
	}
	calls, _ := os.ReadFile(filepath.Join(state, "calls"))
	if strings.Contains(string(calls), "pr merge") || strings.Contains(string(calls), "pr review") {
		t.Errorf("gh ran:\n%s", calls)
	}
	out, err := exec.Command("git", "-C", origin, "branch", "--list", "asmai/*").CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "" {
		t.Errorf("origin still has branches %q (%v)", out, err)
	}
}

// c43Path is the checks C43's pull request has at each look: green on the
// first head, red on the newer one forced red, its re-run red again, and
// green on the fix.
var c43Path = []string{noChecks, noChecks, checkRunning, checkGreen, checkRunning, runRed, runRed, rerunning, rerunRed, checkRunning, checkGreen}

func TestC43PassesGreenThenRedWithOneReRunThenANewerPushReplacingTheWait(t *testing.T) {
	origin := localPushFixture(t)
	usePushFixture(t, origin)
	state := fakeCI(t, origin, false, c43Path...)
	r := result(t, harness(t, signedIn, startsDirectly), "C43")
	if r.Outcome != Passed {
		t.Fatalf("C43 %s: %s\n%s", r.Outcome, r.Failure, strings.Join(r.Evidence, "\n"))
	}
	evidence := strings.Join(r.Evidence, "\n")
	for _, want := range []string{
		"as: waiting: no checks; waiting: CI / test pending; green: CI / test passed",
		"still a draft: watching changes nothing",
		"replaced the wait on",
		"the new head as: waiting: CI / test pending; red: CI / test failed",
		"re-ran the failed jobs of the red checks CI / test failed once, in workflow runs [41]",
		"the re-run reported on",
		"red: CI / test failed; waiting: CI / test pending; red: CI / test failed; it is the head's one re-run, so a fix follows",
		"the fix's newer push of",
		"waiting: CI / test pending; green: CI / test passed, and the pull request is still a draft",
	} {
		if !strings.Contains(evidence, want) {
			t.Errorf("C43's evidence\n%s\ndoes not say %q", evidence, want)
		}
	}
	calls, _ := os.ReadFile(filepath.Join(state, "calls"))
	if strings.Contains(string(calls), "pr ready") || strings.Contains(string(calls), "pr edit") {
		t.Errorf("C43 marked the pull request ready:\n%s", calls)
	}
	if n := strings.Count(string(calls), "run rerun 41 --failed\n"); n != 1 {
		t.Errorf("C43 re-ran the failed jobs %d times, want once:\n%s", n, calls)
	}
	// The checks were read on three heads in turn, each pushed after the
	// last: the green one, the newer one forced red, and the fix.
	data, _ := os.ReadFile(filepath.Join(state, "heads"))
	var heads, modes []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		head, mode, _ := strings.Cut(line, " ")
		if len(heads) == 0 || heads[len(heads)-1] != head {
			heads, modes = append(heads, head), append(modes, mode)
		}
	}
	if len(heads) != 3 || !slices.Equal(modes, []string{"", "red", ""}) {
		t.Errorf("the checks were read on heads %q with .ci-mode %q, want three heads in turn: green, forced red, the fix", heads, modes)
	}
	leftNothing(t, state, origin, 1)
}

func TestC43FailsWhenTheReRunNeverReports(t *testing.T) {
	origin := localPushFixture(t)
	usePushFixture(t, origin)
	state := fakeCI(t, origin, false, checkGreen, runRed)
	r := result(t, harness(t, signedIn, startsDirectly), "C43")
	if r.Outcome != Failed || !strings.Contains(r.Failure, "were not re-run within") {
		t.Errorf("C43 %s: %q, want it to fail waiting for the re-run", r.Outcome, r.Failure)
	}
	leftNothing(t, state, origin, 1)
}

func TestC43FailsWhenTheReRunOfAHeadForcedRedIsGreen(t *testing.T) {
	origin := localPushFixture(t)
	usePushFixture(t, origin)
	state := fakeCI(t, origin, false, checkGreen, runRed, rerunGreen)
	r := result(t, harness(t, signedIn, startsDirectly), "C43")
	if r.Outcome != Failed || !strings.Contains(r.Failure, "which its .ci-mode forces red, is green") {
		t.Errorf("C43 %s: %q, want it to fail on a green re-run", r.Outcome, r.Failure)
	}
	leftNothing(t, state, origin, 1)
}

func TestC43FailsWhenARedCheckCannotBeReRun(t *testing.T) {
	origin := localPushFixture(t)
	usePushFixture(t, origin)
	state := fakeCI(t, origin, false, checkGreen, checkRed)
	r := result(t, harness(t, signedIn, startsDirectly), "C43")
	if r.Outcome != Failed || !strings.Contains(r.Failure, "is a GitHub Actions run to re-run") {
		t.Errorf("C43 %s: %q, want it to fail with nothing to re-run", r.Outcome, r.Failure)
	}
	leftNothing(t, state, origin, 1)
}

func TestC43FailsWhenTheChecksAreRed(t *testing.T) {
	origin := localPushFixture(t)
	usePushFixture(t, origin)
	state := fakeCI(t, origin, false, checkRunning, checkRed)
	r := result(t, harness(t, signedIn, startsDirectly), "C43")
	if r.Outcome != Failed || !strings.Contains(r.Failure, "the fixture's CI is red") {
		t.Errorf("C43 %s: %q, want it to fail on red CI", r.Outcome, r.Failure)
	}
	leftNothing(t, state, origin, 1)
}

func TestC43FailsWhenTheHeadNeverHasChecks(t *testing.T) {
	origin := localPushFixture(t)
	usePushFixture(t, origin)
	state := fakeCI(t, origin, false, noChecks)
	r := result(t, harness(t, signedIn, startsDirectly), "C43")
	if r.Outcome != Failed || !strings.Contains(r.Failure, "were not green within") || !strings.Contains(r.Failure, "waiting: no checks") {
		t.Errorf("C43 %s: %q, want it to fail with a head that has no checks never green", r.Outcome, r.Failure)
	}
	leftNothing(t, state, origin, 1)
}

func TestC45PassesWhenTheDraftIsReadiedAndNotReadyIsTakenOutOfTheTitleOnceGreen(t *testing.T) {
	origin := localPushFixture(t)
	usePushFixture(t, origin)
	state := fakeCI(t, origin, false, checkRunning, checkGreen)
	r := result(t, harness(t, signedIn, startsDirectly), "C45")
	if r.Outcome != Passed {
		t.Fatalf("C45 %s: %s", r.Outcome, r.Failure)
	}
	evidence := strings.Join(r.Evidence, "\n")
	for _, want := range []string{"marked the draft https://github.com/o/r/pull/1 ready, and its lookup found pull request #1 not a draft and open, not merged", "took \"[not ready]\" out of the title of https://github.com/o/r/pull/2", "titled \"C45: a pull request whose draft was refused c45-"} {
		if !strings.Contains(evidence, want) {
			t.Errorf("C45's evidence\n%s\ndoes not say %q", evidence, want)
		}
	}
	calls, _ := os.ReadFile(filepath.Join(state, "calls"))
	if !strings.Contains(string(calls), "pr ready 1\n") || !strings.Contains(string(calls), "pr edit 2 --title C45: a pull request whose draft was refused") {
		t.Errorf("gh ran:\n%s", calls)
	}
	leftNothing(t, state, origin, 2)
}

func TestC45FailsWhenTheDraftIsStillADraftAfterItIsMarkedReady(t *testing.T) {
	origin := localPushFixture(t)
	usePushFixture(t, origin)
	state := fakeCI(t, origin, true, checkGreen)
	r := result(t, harness(t, signedIn, startsDirectly), "C45")
	if r.Outcome != Failed || !strings.Contains(r.Failure, "want no longer a draft") {
		t.Errorf("C45 %s: %q, want it to fail finding the draft still a draft", r.Outcome, r.Failure)
	}
	leftNothing(t, state, origin, 1)
}
