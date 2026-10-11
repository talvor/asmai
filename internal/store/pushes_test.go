// SPDX-License-Identifier: Apache-2.0

package store

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// leaderWorking gives s a job whose assignment's result is accepted, with the
// job branch at otherCommit, and returns the job with the dispatch Engineering's
// leader is working on.
func leaderWorking(t *testing.T, s *Store) (int64, Dispatch) {
	t.Helper()
	job := jobWithBranch(t, s)
	_, d := newAssignment(t, s, job)
	working(t, s, d)
	if _, err := s.EffectRecorded("worker1@engineering", 1, d.ID, "push", "origin/asmai/job-1/1@"+otherCommit, assignmentsAt); err != nil {
		t.Fatal(err)
	}
	_, leader, err := s.ReportSubmitted(submission(d, ReportResult), assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	working(t, s, leader)
	if _, err := s.ResultAccepted(Acceptance{Assignment: 1, Leader: owner, Reasons: []string{"x"}, FromTip: tipCommit}, assignmentsAt); err != nil {
		t.Fatal(err)
	}
	return job, leader
}

func TestTheDaemonsPushIsItsOwnEffectAndARefusalRecordsTheOutsideChangeUntilAPushTakesItIn(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	job, d := leaderWorking(t, s)
	const branch = "asmai/job-1-add-a-greeting"
	outside := []string{"3333333333333333333333333333333333333333", "4444444444444444444444444444444444444444"}

	// A refusal records the outside commits and where they were found.
	refused, err := s.OutsideChangeFound(Push{Job: job, Branch: branch, Commit: otherCommit, Outside: outside, Leader: owner, Dispatch: d.ID}, 1, assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	if refused.ID == 0 || refused.Result != PushRefused || refused.Found != "origin/"+branch || !slices.Equal(refused.Outside, outside) || refused.Effect != 0 || refused.MergedIn != "" {
		t.Errorf("the refusal is %+v", refused)
	}

	// The push after the merge is the daemon's effect, tagged with the
	// leader's dispatch, and records that it took the outside change in.
	merged := "5555555555555555555555555555555555555555"
	if _, err := s.db.Exec(`UPDATE job_branches SET tip = ? WHERE job = ?`, merged, job); err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]Push{
		"the job branch's recorded tip": {Job: job, Branch: branch, Commit: otherCommit, Leader: owner, Dispatch: d.ID},
		"the job branch":                {Job: job, Branch: "asmai/job-1/1", Commit: merged, Leader: owner, Dispatch: d.ID},
		"the leader's current dispatch": {Job: job, Branch: branch, Commit: merged, Leader: "leader@quality", Dispatch: d.ID},
	} {
		if _, err := s.JobBranchPushed(p, 1, []int64{refused.ID}, assignmentsAt); err == nil {
			t.Errorf("a push not of %s was recorded", name)
		}
	}
	if _, err := s.JobBranchPushed(Push{Job: job, Branch: branch, Commit: merged, Leader: owner, Dispatch: d.ID}, 1, []int64{refused.ID + 7}, assignmentsAt); err == nil {
		t.Error("a push recorded taking in an outside change that does not exist")
	}
	pushed, err := s.JobBranchPushed(Push{Job: job, Branch: branch, Commit: merged, Leader: owner, Dispatch: d.ID}, 1, []int64{refused.ID}, assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	if pushed.Result != PushPushed || pushed.Commit != merged || pushed.Effect == 0 || len(pushed.Outside) != 0 {
		t.Errorf("the push is %+v", pushed)
	}
	effects, err := dispatchEffects(s.db, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(effects) != 1 || effects[0].ID != pushed.Effect || effects[0].Agent != Daemon || effects[0].Kind != "push" || effects[0].Ref != "origin/"+branch+"@"+merged || effects[0].Job != job || effects[0].Assignment != 0 {
		t.Errorf("the leader's dispatch has the effects %+v", effects)
	}

	pushes, err := s.Pushes(job)
	if err != nil {
		t.Fatal(err)
	}
	if len(pushes) != 2 || pushes[0].ID != refused.ID || pushes[0].MergedIn != merged || pushes[1].ID != pushed.ID || pushes[1].Result != PushPushed {
		t.Errorf("the job's pushes are %+v", pushes)
	}

	// The journal has the outside change, and the push with its effect.
	journal, err := s.Journal()
	if err != nil {
		t.Fatal(err)
	}
	var entries []string
	for _, e := range journal {
		switch e.Kind {
		case KindOutsideChange, KindJobBranchPushed:
			entries = append(entries, e.Kind)
			var data struct {
				Job     int64    `json:"job"`
				Outside []string `json:"outside"`
				TookIn  []int64  `json:"took_in"`
			}
			if err := json.Unmarshal(e.Data, &data); err != nil {
				t.Fatal(err)
			}
			if data.Job != job || (e.Kind == KindOutsideChange && len(data.Outside) != 2) || (e.Kind == KindJobBranchPushed && !slices.Equal(data.TookIn, []int64{refused.ID})) {
				t.Errorf("%s is journaled as %s", e.Kind, e.Data)
			}
		}
	}
	if strings.Join(entries, ",") != KindOutsideChange+","+KindJobBranchPushed {
		t.Errorf("the journal holds %v", entries)
	}

	// An outside change once taken in is not taken in again.
	if _, err := s.JobBranchPushed(Push{Job: job, Branch: branch, Commit: merged, Leader: owner, Dispatch: d.ID}, 1, []int64{refused.ID}, assignmentsAt); err == nil {
		t.Error("an outside change was taken in twice")
	}
}
