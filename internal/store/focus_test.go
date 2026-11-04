// SPDX-License-Identifier: Apache-2.0

package store

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

// No job has the focus until one opens. Each job opened takes it, recorded
// with the request that opened it, and a switch moves it, recorded with the
// message that asked; the focus is read back as the store holds it.
func TestEachJobOpenedTakesTheFocusAndASwitchMovesIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	s := open(t, path)
	at := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	if f, err := s.Focus(); err != nil || f != (Focus{}) {
		t.Fatalf("with no job, the focus is %+v (%v), want none", f, err)
	}
	says := func(text string) int64 {
		t.Helper()
		_, message, err := s.ObservedWitnessed(Witnessed{Agent: "leader@coordination", Role: "coordination", Generation: 1, Terminal: "/dev/pts/3", Text: text}, "UserPromptSubmit", json.RawMessage(`{}`), at)
		if err != nil {
			t.Fatal(err)
		}
		return message
	}
	first := says("research the greeting")
	if _, err := s.JobOpened(Job{Witness: first, Reading: "Research the greeting", Mandate: MandateResearchOnly, Criteria: []string{"a report"}}, false, at); err != nil {
		t.Fatal(err)
	}
	second := says("research the farewell")
	if _, err := s.JobOpened(Job{Witness: second, Reading: "Research the farewell", Mandate: MandateResearchOnly, Criteria: []string{"a report"}}, false, at); err != nil {
		t.Fatal(err)
	}
	if f, err := s.Focus(); err != nil || f != (Focus{Job: 2, Previous: 1, Witness: second, At: at}) {
		t.Fatalf("after two jobs opened, the focus is %+v (%v), want job 2, from job 1, at its request %d", f, err, second)
	}
	switched := says("switch to 1")
	if f, moved, err := s.FocusChanged(1, 0, true, at); err != nil || !moved || f != (Focus{Job: 1, Previous: 2, Witness: switched, At: at}) {
		t.Fatalf("switching to job 1 = %+v, moved %v (%v), want job 1, from job 2, at message %d", f, moved, err, switched)
	}
	s.Close()
	s = open(t, path)
	if f, err := s.Focus(); err != nil || f.Job != 1 || f.Witness != switched {
		t.Errorf("reopened, the store's focus is %+v (%v), want job 1 at message %d", f, err, switched)
	}
	var changes int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM focus_changes`).Scan(&changes); err != nil || changes != 3 {
		t.Errorf("the store holds %d changes of focus (%v), want one for each job opened and one for the switch", changes, err)
	}
}
