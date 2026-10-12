// SPDX-License-Identifier: Apache-2.0

package store

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestTheDeliveryOwnersPullRequestIsRecordedWithItsEffectAndItsDraftState(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	job, d := leaderWorking(t, s)
	const (
		branch = "asmai/job-1-add-a-greeting"
		url    = "https://github.com/talvor/asmai-fixture/pull/7"
	)
	draft := PullRequest{Job: job, Number: 7, URL: url, Title: "Add a greeting", Draft: true, Branch: branch, Head: otherCommit, State: "OPEN", Leader: owner, Dispatch: d.ID}

	for name, p := range map[string]PullRequest{
		"from the job branch":          {Job: job, Number: 7, URL: url, Branch: "asmai/job-1/1", State: "OPEN", Leader: owner, Dispatch: d.ID},
		"of the dispatch's job":        {Job: job + 1, Number: 7, URL: url, Branch: branch, State: "OPEN", Leader: owner, Dispatch: d.ID},
		"of the leader's dispatch":     {Job: job, Number: 7, URL: url, Branch: branch, State: "OPEN", Leader: "leader@quality", Dispatch: d.ID},
		"with a pull request's number": {Job: job, URL: url, Branch: branch, State: "OPEN", Leader: owner, Dispatch: d.ID},
		"open":                         {Job: job, Number: 7, URL: url, Branch: branch, State: "MERGED", Leader: owner, Dispatch: d.ID},
	} {
		if _, _, err := s.PullRequestRecorded(p, 1, url, assignmentsAt); err == nil {
			t.Errorf("a pull request not %s was recorded", name)
		}
	}

	recorded, e, err := s.PullRequestRecorded(draft, 1, url, assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	if recorded.ID == 0 || recorded.Effect != e.ID || !recorded.Draft || recorded.NotReady || recorded.Number != 7 {
		t.Errorf("the pull request is %+v", recorded)
	}
	if e.Agent != owner || e.Kind != EffectPullRequest || e.Ref != url || e.Dispatch != d.ID || e.Job != job {
		t.Errorf("the effect is %+v", e)
	}

	// Recording it again, once it opened normally as "[not ready]", makes
	// that the record followed.
	notReady := draft
	notReady.Number, notReady.Draft, notReady.NotReady, notReady.Title = 8, false, true, "[not ready] Add a greeting"
	if _, _, err := s.PullRequestRecorded(notReady, 1, url, assignmentsAt); err != nil {
		t.Fatal(err)
	}
	prs, err := s.PullRequests(job)
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != 2 || prs[0].ID != recorded.ID || !prs[0].Draft || prs[1].Number != 8 || prs[1].Draft || !prs[1].NotReady || prs[1].Title != notReady.Title {
		t.Errorf("the job's pull requests are %+v", prs)
	}
	if got, err := s.DispatchJob(d.ID); err != nil || got != job {
		t.Errorf("dispatch %d is of job %d (%v), want %d", d.ID, got, err, job)
	}

	journal, err := s.Journal()
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, entry := range journal {
		switch entry.Kind {
		case KindPullRequestRecorded:
			var data PullRequest
			if err := json.Unmarshal(entry.Data, &data); err != nil {
				t.Fatal(err)
			}
			if data.Job != job || data.Effect == 0 {
				t.Errorf("the pull request is journaled as %s", entry.Data)
			}
			fallthrough
		case KindEffectRecorded:
			kinds = append(kinds, entry.Kind)
		}
	}
	// leaderWorking's worker push is the first effect.
	want := []string{KindEffectRecorded, KindEffectRecorded, KindPullRequestRecorded, KindEffectRecorded, KindPullRequestRecorded}
	if len(kinds) != len(want) {
		t.Fatalf("the journal holds %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Errorf("the journal holds %v, want %v", kinds, want)
			break
		}
	}
}
