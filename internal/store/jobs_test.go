// SPDX-License-Identifier: Apache-2.0

package store

import (
	"path/filepath"
	"slices"
	"testing"
)

// The jobs that run are the open ones, in order: a paused, an ended and a
// cancelled job do not.
func TestOpenJobsAreTheJobsNeitherPausedNorEnded(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	if jobs, err := s.OpenJobs(); err != nil || jobs == nil || len(jobs) != 0 {
		t.Fatalf("with no jobs, the open jobs are %v (%v), want none", jobs, err)
	}
	for _, state := range []string{JobOpen, JobPaused, JobOpen, JobEnded, JobCancelled, JobOpen} {
		if _, err := s.db.Exec(`INSERT INTO jobs(repository, state) VALUES(NULL, ?)`, state); err != nil {
			t.Fatal(err)
		}
	}
	if jobs, err := s.OpenJobs(); err != nil || !slices.Equal(jobs, []int64{1, 3, 6}) {
		t.Errorf("the open jobs are %v (%v), want 1, 3 and 6", jobs, err)
	}
}
