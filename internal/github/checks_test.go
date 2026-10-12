// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestViewChecksReadsTheHeadAndEachCheckOnItFromOneCall(t *testing.T) {
	calls := fakeGH(t, `echo '{"number":7,"url":"https://github.com/o/r/pull/7","title":"Add","isDraft":true,"headRefName":"asmai/job-1-add","headRefOid":"abc","state":"OPEN","statusCheckRollup":[
{"__typename":"CheckRun","name":"test","workflowName":"CI","status":"COMPLETED","conclusion":"SUCCESS","detailsUrl":"https://x/1"},
{"__typename":"CheckRun","name":"lint","workflowName":"","status":"IN_PROGRESS","conclusion":""},
{"__typename":"StatusContext","context":"deploy","state":"PENDING","targetUrl":"https://x/2"}]}'`)
	c, err := ViewChecks(context.Background(), t.TempDir(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if c.Number != 7 || !c.Draft || c.HeadCommit != "abc" || len(c.Checks) != 3 {
		t.Fatalf("the checks are %+v", c)
	}
	want := []Check{
		{Name: "CI / test", Status: "COMPLETED", Conclusion: "SUCCESS", State: CheckPassed, URL: "https://x/1"},
		{Name: "lint", Status: "IN_PROGRESS", State: CheckPending},
		{Name: "deploy", Conclusion: "PENDING", State: CheckPending, URL: "https://x/2"},
	}
	for i := range want {
		if c.Checks[i] != want[i] {
			t.Errorf("check %d is %+v, want %+v", i, c.Checks[i], want[i])
		}
	}
	if c.Verdict() != Waiting {
		t.Errorf("the verdict is %s", c.Verdict())
	}
	data, _ := os.ReadFile(calls)
	if !strings.HasSuffix(strings.TrimSpace(string(data)), ": pr view 7 --json number,url,title,isDraft,headRefName,headRefOid,isCrossRepository,state,statusCheckRollup") {
		t.Errorf("gh ran as %q", data)
	}
}

func TestAHeadWithNoChecksIsNeverGreen(t *testing.T) {
	fakeGH(t, `echo '{"number":7,"url":"u","headRefName":"b","headRefOid":"abc","state":"OPEN","statusCheckRollup":[]}'`)
	c, err := ViewChecks(context.Background(), t.TempDir(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if c.Checks == nil || len(c.Checks) != 0 || c.Verdict() != Waiting {
		t.Errorf("no checks are %#v, verdict %s", c.Checks, c.Verdict())
	}
}

func TestTheVerdictIsGreenOnlyWhenEveryCheckPassed(t *testing.T) {
	run := func(status, conclusion string) Check {
		return rollupEntry{Type: "CheckRun", Name: "n", Status: status, Conclusion: conclusion}.check()
	}
	status := func(state string) Check {
		return rollupEntry{Type: "StatusContext", Context: "c", State: state}.check()
	}
	for name, tt := range map[string]struct {
		checks []Check
		want   string
	}{
		"none":                {nil, Waiting},
		"one success":         {[]Check{run("COMPLETED", "SUCCESS")}, Green},
		"neutral and skipped": {[]Check{run("COMPLETED", "NEUTRAL"), run("COMPLETED", "SKIPPED"), status("SUCCESS")}, Green},
		"one queued":          {[]Check{run("COMPLETED", "SUCCESS"), run("QUEUED", "")}, Waiting},
		"one failure":         {[]Check{run("IN_PROGRESS", ""), run("COMPLETED", "FAILURE")}, Red},
		"cancelled":           {[]Check{run("COMPLETED", "CANCELLED")}, Red},
		"timed out":           {[]Check{run("COMPLETED", "TIMED_OUT")}, Red},
		"status error":        {[]Check{status("ERROR")}, Red},
		"status pending":      {[]Check{status("PENDING")}, Waiting},
		"unknown typename":    {[]Check{{Name: "x", State: "something else"}}, Waiting},
	} {
		if got := Verdict(tt.checks); got != tt.want {
			t.Errorf("%s: the verdict is %s, want %s", name, got, tt.want)
		}
	}
}

func TestMarkReadyReadiesADraftAndTakesNotReadyOutOfATitle(t *testing.T) {
	calls := fakeGH(t, ``)
	dir := t.TempDir()
	if err := MarkReady(context.Background(), dir, PullRequest{Number: 7, Draft: true, Title: "Add"}); err != nil {
		t.Fatal(err)
	}
	if err := MarkReady(context.Background(), dir, PullRequest{Number: 8, Title: "[not ready] Add a greeting"}); err != nil {
		t.Fatal(err)
	}
	if err := MarkReady(context.Background(), dir, PullRequest{Number: 9, Title: "Ready already"}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(calls)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 || !strings.HasSuffix(lines[0], ": pr ready 7") || !strings.HasSuffix(lines[1], ": pr edit 8 --title Add a greeting") {
		t.Errorf("gh ran as %q", lines)
	}
}

func TestMarkReadyReportsWhatGHPrintedWhenItFails(t *testing.T) {
	fakeGH(t, `echo 'GraphQL: not allowed' >&2; exit 1`)
	err := MarkReady(context.Background(), t.TempDir(), PullRequest{Number: 7, Draft: true})
	if err == nil || !strings.Contains(err.Error(), "gh pr ready: GraphQL: not allowed") {
		t.Errorf("MarkReady returned %v", err)
	}
}
