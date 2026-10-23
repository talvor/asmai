// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGH puts a gh on the PATH that records its arguments and working
// directory in the test's directory and runs script.
func fakeGH(t *testing.T, script string) (calls string) {
	t.Helper()
	dir := t.TempDir()
	calls = filepath.Join(dir, "calls")
	gh := "#!/bin/sh\necho \"$PWD: $*\" >> " + calls + "\n" + script + "\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(gh), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return calls
}

func TestViewPullRequestReadsTheNumberDraftStateAndHeadGHShows(t *testing.T) {
	calls := fakeGH(t, `[ "$GH_PROMPT_DISABLED" = 1 ] || exit 3
echo '{"number":7,"url":"https://github.com/o/r/pull/7","title":"[not ready] Add a greeting","isDraft":false,"headRefName":"asmai/job-1-add","headRefOid":"abc","state":"OPEN"}'`)
	dir := t.TempDir()
	p, err := ViewPullRequest(context.Background(), dir, "https://github.com/o/r/pull/7")
	if err != nil {
		t.Fatal(err)
	}
	if p.Number != 7 || p.Draft || !p.IsNotReady() || p.HeadBranch != "asmai/job-1-add" || p.HeadCommit != "abc" || p.State != "OPEN" {
		t.Errorf("the pull request is %+v", p)
	}
	data, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(data)); !strings.HasSuffix(got, ": pr view https://github.com/o/r/pull/7 --json number,url,title,isDraft,headRefName,headRefOid,isCrossRepository,state") || !strings.Contains(got, filepath.Base(dir)) {
		t.Errorf("gh ran as %q", got)
	}
}

func TestAPullRequestIsInARepositoryOnlyFromOneOfItsBranches(t *testing.T) {
	const repository = "https://github.com/o/r"
	for name, tt := range map[string]struct {
		p    PullRequest
		want bool
	}{
		"its own":            {PullRequest{URL: repository + "/pull/7"}, true},
		"another repository": {PullRequest{URL: "https://github.com/o/r2/pull/7"}, false},
		"another owner":      {PullRequest{URL: "https://github.com/x/r/pull/7"}, false},
		"from a fork":        {PullRequest{URL: repository + "/pull/7", CrossRepository: true}, false},
	} {
		if got := tt.p.In(repository); got != tt.want {
			t.Errorf("%s: In(%s) = %v, want %v", name, repository, got, tt.want)
		}
	}
}

func TestRepositoryURLIsTheOneGHShowsInTheDirectory(t *testing.T) {
	calls := fakeGH(t, `echo '{"url":"https://github.com/o/r"}'`)
	dir := t.TempDir()
	url, err := RepositoryURL(context.Background(), dir)
	if err != nil || url != "https://github.com/o/r" {
		t.Fatalf("RepositoryURL = %q, %v", url, err)
	}
	data, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(data)); !strings.HasSuffix(got, ": repo view --json url") || !strings.Contains(got, filepath.Base(dir)) {
		t.Errorf("gh ran as %q", got)
	}
	fakeGH(t, `echo '{}'`)
	if _, err := RepositoryURL(context.Background(), dir); err == nil || !strings.Contains(err.Error(), "no repository URL") {
		t.Errorf("RepositoryURL without a URL = %v", err)
	}
}

func TestADraftIsNotNotReady(t *testing.T) {
	fakeGH(t, `echo '{"number":3,"title":"Add a greeting","isDraft":true,"headRefName":"b"}'`)
	p, err := ViewPullRequest(context.Background(), t.TempDir(), "3")
	if err != nil {
		t.Fatal(err)
	}
	if !p.Draft || p.IsNotReady() {
		t.Errorf("the pull request is %+v", p)
	}
}

func TestViewPullRequestFailsWithWhatGHSaid(t *testing.T) {
	for name, tt := range map[string]struct{ script, ref, want string }{
		"gh fails":      {`echo 'no pull requests found for branch "x"' >&2; exit 1`, "9", `gh pr view: no pull requests found for branch "x"`},
		"no number":     {`echo '{"headRefName":"b"}'`, "9", "no pull request number"},
		"not JSON":      {`echo nope`, "9", "reading what gh pr view printed"},
		"a flag as ref": {`exit 0`, "--web", "does not name a pull request"},
		"no ref":        {`exit 0`, " ", "does not name a pull request"},
	} {
		t.Run(name, func(t *testing.T) {
			fakeGH(t, tt.script)
			if _, err := ViewPullRequest(context.Background(), t.TempDir(), tt.ref); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("ViewPullRequest(%q) = %v, want it to say %q", tt.ref, err, tt.want)
			}
		})
	}
}

func TestViewPullRequestSaysWhenGHIsMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if _, err := ViewPullRequest(context.Background(), t.TempDir(), "1"); err == nil || !strings.Contains(err.Error(), "gh is not installed") {
		t.Errorf("ViewPullRequest without gh = %v", err)
	}
}

func TestAPullRequestIsOpenedWithGHInTheCloneWithItsBodyOnStandardInput(t *testing.T) {
	dir := t.TempDir()
	calls := fakeGH(t, `cat > "$PWD/body-seen"
echo "Creating pull request for asmai/job-1-add into main in o/r"
echo "https://github.com/o/r/pull/9"`)
	url, err := CreatePullRequest(context.Background(), dir, Create{
		Repository: "o/r", Base: "main", Head: "asmai/job-1-add", Title: "Add a greeting", Body: "## Summary\n\nAdds it.\n", Draft: true,
	})
	if err != nil || url != "https://github.com/o/r/pull/9" {
		t.Fatalf("CreatePullRequest = %q, %v", url, err)
	}
	data, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(data)); !strings.HasSuffix(got, ": pr create --repo o/r --base main --head asmai/job-1-add --title Add a greeting --body-file - --draft") || !strings.Contains(got, filepath.Base(dir)) {
		t.Errorf("gh ran as %q", got)
	}
	if body, err := os.ReadFile(filepath.Join(dir, "body-seen")); err != nil || string(body) != "## Summary\n\nAdds it.\n" {
		t.Errorf("gh was given the body %q (%v)", body, err)
	}

	// Without a draft, and with a gh that prints no URL.
	calls = fakeGH(t, `echo "https://github.com/o/r/pull/10"`)
	if _, err := CreatePullRequest(context.Background(), dir, Create{Repository: "o/r", Base: "main", Head: "b", Title: "t", Body: "x"}); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(calls)
	if strings.Contains(string(data), "--draft") {
		t.Errorf("a pull request that is not a draft was opened with --draft: %s", data)
	}
	fakeGH(t, `echo done`)
	if _, err := CreatePullRequest(context.Background(), dir, Create{Repository: "o/r", Base: "main", Head: "b", Title: "t", Body: "x"}); err == nil || !strings.Contains(err.Error(), "no pull request URL") {
		t.Errorf("a gh that printed no URL returned %v", err)
	}
}

func TestGitHubsRefusalOfADraftIsToldApartFromOtherFailures(t *testing.T) {
	fakeGH(t, `echo 'pull request create failed: GraphQL: Draft pull requests are not supported in this repository. (createPullRequest)' >&2; exit 1`)
	_, err := CreatePullRequest(context.Background(), t.TempDir(), Create{Repository: "o/r", Base: "main", Head: "b", Title: "t", Body: "x", Draft: true})
	if !DraftsRefused(err) {
		t.Errorf("DraftsRefused(%v) = false", err)
	}
	fakeGH(t, `echo 'HTTP 502' >&2; exit 1`)
	_, err = CreatePullRequest(context.Background(), t.TempDir(), Create{Repository: "o/r", Base: "main", Head: "b", Title: "t", Body: "x", Draft: true})
	if err == nil || DraftsRefused(err) {
		t.Errorf("a failure that is not the refusal of a draft is %v", err)
	}
}

func TestOnlyTheBodyOfAPullRequestIsEditedAndTheRepositoryIsNamedForGH(t *testing.T) {
	dir := t.TempDir()
	calls := fakeGH(t, `cat > "$PWD/body-seen"`)
	if err := EditBody(context.Background(), dir, "o/r", 7, "new body\n"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(calls)
	if got := strings.TrimSpace(string(data)); !strings.HasSuffix(got, ": pr edit 7 --repo o/r --body-file -") {
		t.Errorf("gh ran as %q", got)
	}
	if body, _ := os.ReadFile(filepath.Join(dir, "body-seen")); string(body) != "new body\n" {
		t.Errorf("gh was given the body %q", body)
	}

	fakeGH(t, `echo '{"url":"https://github.com/o/r"}'`)
	if got, err := Repository(context.Background(), dir); err != nil || got != "o/r" {
		t.Errorf("Repository = %q, %v, want o/r", got, err)
	}
	fakeGH(t, `echo '{"url":"https://ghe.example.test/o/r"}'`)
	if got, err := Repository(context.Background(), dir); err != nil || got != "ghe.example.test/o/r" {
		t.Errorf("Repository = %q, %v, want the host named too", got, err)
	}
}

func TestThePullRequestsFromABranchAreListedInAnyState(t *testing.T) {
	calls := fakeGH(t, `echo '[{"number":3,"url":"https://github.com/o/r/pull/3","state":"CLOSED"}]'`)
	found, err := PullRequestsFrom(context.Background(), t.TempDir(), "o/r", "asmai/job-1-add")
	if err != nil || len(found) != 1 || found[0].Number != 3 || found[0].State != "CLOSED" {
		t.Fatalf("PullRequestsFrom = %+v, %v", found, err)
	}
	data, _ := os.ReadFile(calls)
	if got := strings.TrimSpace(string(data)); !strings.HasSuffix(got, ": pr list --repo o/r --head asmai/job-1-add --state all --json number,url,state") {
		t.Errorf("gh ran as %q", got)
	}
}
