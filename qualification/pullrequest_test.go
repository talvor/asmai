// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGitHub puts a gh on the PATH that stands in for GitHub with a local
// bare repository as origin: it opens pull requests, as drafts when asked
// unless ignoreDraft, shows them and closes them, keeping them in a directory
// of the test's. It returns that directory.
func fakeGitHub(t *testing.T, origin string, ignoreDraft bool) string {
	t.Helper()
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	if err := os.Mkdir(state, 0o755); err != nil {
		t.Fatal(err)
	}
	draftArg := "draft=true"
	if ignoreDraft {
		draftArg = ":"
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
		--draft) ` + draftArg + ` ;;
		--title) title=$2; shift ;;
		--head) head=$2; shift ;;
		--base|--body|--repo|--body-file) shift ;;
		esac
		shift
	done
	n=$(( $(cat "$state/count" 2>/dev/null || echo 0) + 1 ))
	echo "$n" > "$state/count"
	oid=$(git ls-remote ` + origin + ` "refs/heads/$head" | cut -f1)
	printf '{"number":%d,"url":"https://github.com/o/r/pull/%d","title":"%s","isDraft":%s,"headRefName":"%s","headRefOid":"%s","state":"OPEN"}\n' "$n" "$n" "$title" "$draft" "$head" "$oid" > "$state/pr-$n.json"
	echo "Creating pull request for $head"
	echo "https://github.com/o/r/pull/$n"
	;;
"pr view")
	case "$*" in
	*--jq*) cat "$state/body-${3##*/}" ;;
	*) cat "$state/pr-${3##*/}.json" ;;
	esac
	;;
"pr edit")
	n=$3
	shift 3
	while [ $# -gt 0 ]; do
		case "$1" in
		--body-file) if [ "$2" = - ]; then cat > "$state/body-$n"; else cp "$2" "$state/body-$n"; fi; shift ;;
		--repo) shift ;;
		esac
		shift
	done
	echo "https://github.com/o/r/pull/$n"
	;;
"pr close") touch "$state/closed-${3##*/}" ;;
"repo view") echo '{"url":"https://github.com/o/r"}' ;;
*) echo "the fake gh does not do: $*" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(gh), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return state
}

func TestC42PassesWhenTheDraftIsADraftAndTheRefusedDraftOpensNotReady(t *testing.T) {
	origin := localPushFixture(t)
	usePushFixture(t, origin)
	state := fakeGitHub(t, origin, false)
	r := result(t, harness(t, signedIn, startsDirectly), "C42")
	if r.Outcome != Passed {
		t.Fatalf("C42 %s: %s", r.Outcome, r.Failure)
	}
	evidence := strings.Join(r.Evidence, "\n")
	for _, want := range []string{"as a draft, and the daemon's lookup found pull request #1 a draft", "refused at the gh boundary (simulated", "found pull request #2 not a draft, titled \"[not ready] C42:"} {
		if !strings.Contains(evidence, want) {
			t.Errorf("C42's evidence\n%s\ndoes not say %q", evidence, want)
		}
	}
	// Only two pull requests reached GitHub, the refused draft not among
	// them, and both are closed; the scratch branch is deleted from origin.
	for _, name := range []string{"closed-1", "closed-2"} {
		if _, err := os.Stat(filepath.Join(state, name)); err != nil {
			t.Errorf("pull request %s was not closed", strings.TrimPrefix(name, "closed-"))
		}
	}
	if _, err := os.Stat(filepath.Join(state, "pr-3.json")); err == nil {
		t.Error("a third pull request was opened")
	}
	calls, _ := os.ReadFile(filepath.Join(state, "calls"))
	if strings.Count(string(calls), "pr create") != 2 || strings.Contains(string(calls), "pr merge") || strings.Contains(string(calls), "pr review") {
		t.Errorf("gh ran:\n%s", calls)
	}
	out, err := exec.Command("git", "-C", origin, "branch", "--list", "asmai/*").CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "" {
		t.Errorf("origin still has branches %q (%v)", out, err)
	}
}

func TestC42FailsWhenTheDraftPullRequestIsNotADraft(t *testing.T) {
	origin := localPushFixture(t)
	usePushFixture(t, origin)
	state := fakeGitHub(t, origin, true)
	r := result(t, harness(t, signedIn, startsDirectly), "C42")
	if r.Outcome != Failed || !strings.Contains(r.Failure, "want a draft") {
		t.Errorf("C42 %s: %q, want it to fail finding the draft not a draft", r.Outcome, r.Failure)
	}
	if _, err := os.Stat(filepath.Join(state, "closed-1")); err != nil {
		t.Error("the pull request was not closed after the case failed")
	}
}

// The daemon makes the pull request writes of a delivery owner's `asmai pr
// open` and `asmai pr update`, with the code the case runs.
func TestC50PassesWhenTheDaemonsGhOpensTheDraftAndUpdatesItsBody(t *testing.T) {
	origin := localPushFixture(t)
	usePushFixture(t, origin)
	state := fakeGitHub(t, origin, false)
	r := result(t, harness(t, signedIn, startsDirectly), "C50")
	if r.Outcome != Passed {
		t.Fatalf("C50 %s: %s", r.Outcome, r.Failure)
	}
	evidence := strings.Join(r.Evidence, "\n")
	for _, want := range []string{"`gh pr create --draft`, run as the user", "found pull request #1 a draft", "`gh pr edit --body-file -` then updated the body", "leaving it a draft with its title"} {
		if !strings.Contains(evidence, want) {
			t.Errorf("C50's evidence\n%s\ndoes not say %q", evidence, want)
		}
	}
	if _, err := os.Stat(filepath.Join(state, "closed-1")); err != nil {
		t.Error("the pull request was not closed")
	}
	if body, err := os.ReadFile(filepath.Join(state, "body-1")); err != nil || !strings.Contains(string(body), "C50 updated the body") {
		t.Errorf("the pull request's body is %q (%v), want the update", body, err)
	}
	out, err := exec.Command("git", "-C", origin, "branch", "--list", "asmai/*").CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "" {
		t.Errorf("origin still has branches %q (%v)", out, err)
	}
}

func TestC50FailsWhenTheOpenedPullRequestIsNotADraft(t *testing.T) {
	origin := localPushFixture(t)
	usePushFixture(t, origin)
	state := fakeGitHub(t, origin, true)
	r := result(t, harness(t, signedIn, startsDirectly), "C50")
	if r.Outcome != Failed || !strings.Contains(r.Failure, "want a draft") {
		t.Errorf("C50 %s: %q, want it to fail finding the pull request not a draft", r.Outcome, r.Failure)
	}
	if _, err := os.Stat(filepath.Join(state, "closed-1")); err != nil {
		t.Error("the pull request was not closed after the case failed")
	}
}
