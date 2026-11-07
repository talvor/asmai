// SPDX-License-Identifier: Apache-2.0

package store

import (
	"path/filepath"
	"testing"
	"time"
)

// A dispatch's recording is journaled with where it is and, once finished,
// how large it is. A dispatch typed again keeps its recording.
func TestADispatchsRecordingIsJournaledWithWhereItIs(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	j := researchJob(t, s)
	a, d := scratchAssignment(t, s, j.Number)
	at := assignmentsAt.Add(time.Hour)

	r, err := s.RecordingStarted(d.ID, "/rec/dispatch-1.cast.gz", at)
	if err != nil || r.Dispatch != d.ID || r.Agent != a.Worker || r.Job != j.Number || r.Path != "/rec/dispatch-1.cast.gz" || !r.Started.Equal(at) {
		t.Fatalf("RecordingStarted = %+v (%v)", r, err)
	}
	again, err := s.RecordingStarted(d.ID, "/elsewhere", at.Add(time.Minute))
	if err != nil || again != r {
		t.Fatalf("a dispatch recorded again = %+v (%v), want the recording it had, %+v", again, err, r)
	}
	if err := s.RecordingFinished(d.ID, 123, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := s.Recording(d.ID); err != nil || !ok || got.Size != 123 || got.Path != r.Path {
		t.Errorf("Recording = %+v, %v (%v), want it finished at 123 bytes", got, ok, err)
	}
	if _, ok, err := s.Recording(d.ID + 100); err != nil || ok {
		t.Errorf("a dispatch that was never recorded has a recording: %v (%v)", ok, err)
	}
	if err := s.RecordingFinished(d.ID+100, 1, at); err == nil {
		t.Error("finishing a recording that was never started was accepted")
	}
	if started := entriesOf(t, s, KindRecordingStarted); len(started) != 1 || started[0]["path"] != r.Path || started[0]["dispatch"] != float64(d.ID) {
		t.Errorf("the journal holds recording starts %v, want one with the path", started)
	}
	if finished := entriesOf(t, s, KindRecordingFinished); len(finished) != 1 || finished[0]["size"] != float64(123) {
		t.Errorf("the journal holds recording ends %v, want one of 123 bytes", finished)
	}
}

// A recording expires once its job has ended for the retention period, and
// not before, nor while its job is open.
func TestARecordingExpiresOnlyAfterItsJobEnded(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	open1 := researchJob(t, s)
	_, openDispatch := scratchAssignment(t, s, open1.Number)

	ending := researchJob(t, s)
	a, d := scratchAssignment(t, s, ending.Number)
	for _, id := range []int64{openDispatch.ID, d.ID} {
		if _, err := s.RecordingStarted(id, filepath.Join("/rec", "d", string(rune('0'+id))), assignmentsAt); err != nil {
			t.Fatal(err)
		}
	}
	scratchResult(t, s, a, d)
	endedAt := assignmentsAt.Add(24 * time.Hour)
	if _, _, err := s.ScratchResultAccepted(Acceptance{Assignment: a.ID, Leader: researcher, Reasons: []string{"cited"}, EndsJob: true}, "ended", endedAt); err != nil {
		t.Fatal(err)
	}

	far := endedAt.Add(1000 * time.Hour)
	if got, err := s.ExpiredRecordings(endedAt.Add(-time.Second)); err != nil || len(got) != 0 {
		t.Errorf("before its job ended, %v expired (%v)", got, err)
	}
	got, err := s.ExpiredRecordings(endedAt)
	if err != nil || len(got) != 1 || got[0].Dispatch != d.ID || got[0].Job != ending.Number {
		t.Fatalf("when its job ended, ExpiredRecordings = %+v (%v), want only job %d's recording", got, err, ending.Number)
	}
	if got, _ := s.ExpiredRecordings(far); len(got) != 1 {
		t.Errorf("an open job's recording expired: %+v", got)
	}

	if err := s.RecordingDeleted(d.ID, far); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordingDeleted(d.ID, far); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ExpiredRecordings(far); err != nil || len(got) != 0 {
		t.Errorf("a deleted recording still expires: %+v (%v)", got, err)
	}
	if rec, ok, err := s.Recording(d.ID); err != nil || !ok || rec.Deleted.IsZero() {
		t.Errorf("a deleted recording = %+v, %v (%v), want it known with when it was deleted", rec, ok, err)
	}
	if deleted := entriesOf(t, s, KindRecordingDeleted); len(deleted) != 1 {
		t.Errorf("the journal holds %d deletions, want one", len(deleted))
	}
}
