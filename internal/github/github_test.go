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
	if got := strings.TrimSpace(string(data)); !strings.HasSuffix(got, ": pr view https://github.com/o/r/pull/7 --json number,url,title,isDraft,headRefName,headRefOid,state") || !strings.Contains(got, filepath.Base(dir)) {
		t.Errorf("gh ran as %q", got)
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
