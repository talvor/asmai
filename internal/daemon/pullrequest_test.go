// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/talvor/asmai/internal/github"
	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/session"
	"github.com/talvor/asmai/internal/store"
)

// fakeGHDaemon puts a gh on the PATH that keeps one pull request, number 7,
// in the returned directory: `gh pr create` opens it with the body it is given
// on its standard input, refusing a draft while the directory holds
// refuse-draft, `gh pr list` shows list.json, `gh pr view` the pull request
// and `gh pr edit` keeps the body it is given. Every call is in calls.
func fakeGHDaemon(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
state=` + dir + `
printf '%s\n' "$*" >> "$state/calls"
case "$1 $2" in
"repo view") echo '{"url":"https://github.com/o/r"}' ;;
"pr list") cat "$state/list.json" 2>/dev/null || echo '[]' ;;
"pr create")
	shift 2
	draft=false title= head=
	while [ $# -gt 0 ]; do
		case "$1" in
		--draft) draft=true ;;
		--title) title=$2; shift ;;
		--head) head=$2; shift ;;
		esac
		shift
	done
	if $draft && [ -e "$state/refuse-draft" ]; then
		echo 'pull request create failed: GraphQL: Draft pull requests are not supported in this repository. (createPullRequest)' >&2
		exit 1
	fi
	cat > "$state/body"
	printf '{"number":7,"url":"https://github.com/o/r/pull/7","title":"%s","isDraft":%s,"headRefName":"%s","headRefOid":"abc","isCrossRepository":false,"state":"OPEN"}\n' "$title" "$draft" "$head" > "$state/pr7.json"
	echo "https://github.com/o/r/pull/7"
	;;
"pr view") cat "$state/pr7.json" ;;
"pr edit") cat > "$state/body-edited" ;;
*) echo "the fake gh does not do: $*" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func (f *twoResults) acceptAndPush(t *testing.T) {
	t.Helper()
	if _, err := f.decide(CommandAccept, f.assignments[0].ID, "file-1.txt is there"); err != nil {
		t.Fatal(err)
	}
	f.leaderWorks(t)
	if _, err := f.d.push(Request{Session: "credential", Command: CommandPush, Job: f.job.Number}); err != nil {
		t.Fatal(err)
	}
}

func TestTheDaemonOpensTheJobsDraftPullRequestAndRecordsItAndUpdatesOnlyItsBody(t *testing.T) {
	gitIdentity(t)
	state := fakeGHDaemon(t)
	f := newTwoResults(t)
	open := Request{Session: "credential", Command: CommandPullRequestOpen, Job: f.job.Number, Title: "Add a greeting", Body: "## Summary\n\nAdds it.\n"}

	// Only the delivery owner, working on a dispatch, with the branch pushed.
	quality := roles.LeaderOf(roles.Quality)
	f.d.leaders[quality.String()] = &leader{address: quality, session: &session.Session{}, generation: 1}
	f.d.sessions["quality"] = sessionRef{address: quality, generation: 1}
	if _, _, err := f.d.openPullRequest(Request{Session: "quality", Command: CommandPullRequestOpen, Job: f.job.Number, Title: "t", Body: "b"}); err == nil || !strings.Contains(err.Error(), "only leader@engineering") {
		t.Errorf("Quality's leader opening a pull request returned %v", err)
	}
	if _, _, err := f.d.openPullRequest(open); err == nil || !strings.Contains(err.Error(), "no current dispatch") {
		t.Errorf("opening outside a dispatch returned %v", err)
	}
	if _, err := f.decide(CommandAccept, f.assignments[0].ID, "file-1.txt is there"); err != nil {
		t.Fatal(err)
	}
	dispatch := f.leaderWorks(t)
	if _, _, err := f.d.openPullRequest(open); err == nil || !strings.Contains(err.Error(), "is not pushed") || !strings.Contains(err.Error(), "asmai push") {
		t.Errorf("opening before the push returned %v", err)
	}
	if _, err := f.d.push(Request{Session: "credential", Command: CommandPush, Job: f.job.Number}); err != nil {
		t.Fatal(err)
	}
	blank := open
	blank.Body = " "
	if _, _, err := f.d.openPullRequest(blank); err == nil || !strings.Contains(err.Error(), "needs a title and a body") {
		t.Errorf("opening with no body returned %v", err)
	}

	// It opens a draft from the job branch to the base, and records it as the
	// leader's effect, so that no second command is needed.
	p, e, err := f.d.openPullRequest(open)
	if err != nil {
		t.Fatal(err)
	}
	branch := JobBranchName(f.job)
	if p.Number != 7 || !p.Draft || p.NotReady || p.Branch != branch || p.Dispatch != dispatch || e.Kind != store.EffectPullRequest || e.Ref != "https://github.com/o/r/pull/7" {
		t.Errorf("the pull request is %+v with effect %+v", p, e)
	}
	calls, _ := os.ReadFile(filepath.Join(state, "calls"))
	if !strings.Contains(string(calls), "pr create --repo o/r --base trunk --head "+branch+" --title Add a greeting --body-file - --draft") {
		t.Errorf("gh ran %s", calls)
	}
	if body, _ := os.ReadFile(filepath.Join(state, "body")); string(body) != open.Body {
		t.Errorf("gh was given the body %q", body)
	}

	// Never a second one, and the refusal says what to do instead.
	if _, _, err := f.d.openPullRequest(open); err == nil || !strings.Contains(err.Error(), "already has pull request #7") || !strings.Contains(err.Error(), "asmai pr update") {
		t.Errorf("opening a second pull request returned %v", err)
	}

	// An update changes the body and records the pull request again.
	update := Request{Session: "credential", Command: CommandPullRequestUpdate, Job: f.job.Number, Body: "## Summary\n\nAdds it, and CI is green.\n"}
	p, e, err = f.d.updatePullRequest(update)
	if err != nil || p.Number != 7 || e.Kind != store.EffectPullRequest {
		t.Fatalf("updating returned %+v, %+v, %v", p, e, err)
	}
	if body, _ := os.ReadFile(filepath.Join(state, "body-edited")); string(body) != update.Body {
		t.Errorf("gh was given the body %q", body)
	}
	calls, _ = os.ReadFile(filepath.Join(state, "calls"))
	if !strings.Contains(string(calls), "pr edit 7 --repo o/r --body-file -\n") || strings.Contains(string(calls), "pr edit 7 --repo o/r --title") {
		t.Errorf("gh ran %s", calls)
	}
	recorded, err := f.s.PullRequests(f.job.Number)
	if err != nil || len(recorded) != 2 {
		t.Errorf("the pull requests recorded are %+v (%v), want the opening and the update", recorded, err)
	}
}

func TestWhenGitHubRefusesADraftTheDaemonOpensItNotReady(t *testing.T) {
	gitIdentity(t)
	state := fakeGHDaemon(t)
	if err := os.WriteFile(filepath.Join(state, "refuse-draft"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f := newTwoResults(t)
	f.acceptAndPush(t)
	p, _, err := f.d.openPullRequest(Request{Session: "credential", Command: CommandPullRequestOpen, Job: f.job.Number, Title: "Add a greeting", Body: "body"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Draft || !p.NotReady || p.Title != github.NotReady+" Add a greeting" {
		t.Errorf("the pull request is %+v, want one that is not a draft, with %q at the start of its title", p, github.NotReady)
	}
}

func TestTheDaemonOpensNoPullRequestWhereGitHubAlreadyHasOneFromTheBranchAndNamesIt(t *testing.T) {
	gitIdentity(t)
	state := fakeGHDaemon(t)
	if err := os.WriteFile(filepath.Join(state, "list.json"), []byte(`[{"number":3,"url":"https://github.com/o/r/pull/3","state":"OPEN"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	f := newTwoResults(t)
	f.acceptAndPush(t)
	_, _, err := f.d.openPullRequest(Request{Session: "credential", Command: CommandPullRequestOpen, Job: f.job.Number, Title: "Add a greeting", Body: "body"})
	if err == nil || !strings.Contains(err.Error(), "https://github.com/o/r/pull/3") || !strings.Contains(err.Error(), "asmai effect pull-request https://github.com/o/r/pull/3") {
		t.Fatalf("opening over an existing pull request returned %v", err)
	}
	if calls, _ := os.ReadFile(filepath.Join(state, "calls")); strings.Contains(string(calls), "pr create") {
		t.Errorf("gh opened a pull request anyway: %s", calls)
	}
	// Nothing to update where none is recorded.
	if _, _, err := f.d.updatePullRequest(Request{Session: "credential", Command: CommandPullRequestUpdate, Job: f.job.Number, Body: "body"}); err == nil || !strings.Contains(err.Error(), "has no pull request recorded") {
		t.Errorf("updating with none recorded returned %v", err)
	}
}
