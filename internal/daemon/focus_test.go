// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/store"
)

// A job opened takes the conversation's focus, which the status line shows,
// and a switch at the user's word moves it, recorded with the witnessed
// message that made it.
func TestTheStatusLineShowsTheFocusedJobAsAJobOpensAndAsTheUserSwitches(t *testing.T) {
	f := newTwoResults(t)
	empty := statusLine{prompts: []string{}, decisions: []string{}, jobs: []int64{}}
	if got := f.d.statusLine(empty); got.focus != f.job.Number {
		t.Fatalf("with job %d just opened, the status line's focus is job %d, want it", f.job.Number, got.focus)
	}
	second, err := f.s.JobOpened(store.Job{Repository: "fixture", Witness: f.userSays(t, "add a farewell"), Reading: "Add a farewell", Mandate: store.MandateTestedPR, Criteria: []string{"done"}}, false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got := f.d.statusLine(empty); got.focus != second.Number {
		t.Fatalf("with job %d opened from a new request, the status line's focus is job %d, want it", second.Number, got.focus)
	}

	switched := f.userSays(t, "switch to 1")
	focus, j, moved, err := f.d.focus(Request{Command: CommandFocus, Job: f.job.Number, LatestWitness: true})
	if err != nil || !moved || focus.Job != f.job.Number || focus.Previous != second.Number || focus.Witness != switched || j.Number != f.job.Number {
		t.Fatalf("switching to job %d = %+v, job %d, moved %v (%v), want it moved from job %d at witnessed message %d", f.job.Number, focus, j.Number, moved, err, second.Number, switched)
	}
	if got := f.d.statusLine(empty); got.focus != f.job.Number {
		t.Errorf("after the switch, the status line's focus is job %d, want job %d", got.focus, f.job.Number)
	}
	entries, err := f.s.Journal()
	if err != nil {
		t.Fatal(err)
	}
	last := entries[len(entries)-1]
	var change store.Focus
	if err := json.Unmarshal(last.Data, &change); err != nil || last.Kind != store.KindFocusChanged || change.Job != f.job.Number || change.Previous != second.Number || change.Witness != switched {
		t.Errorf("the switch is journaled as %s %s (%v), want %s of job %d from job %d citing witnessed message %d", last.Kind, last.Data, err, store.KindFocusChanged, f.job.Number, second.Number, switched)
	}

	// Switching to the job that has the focus records nothing.
	f.userSays(t, "switch to 1 again")
	if again, _, moved, err := f.d.focus(Request{Command: CommandFocus, Job: f.job.Number, LatestWitness: true}); err != nil || moved || again != focus {
		t.Errorf("switching to the focused job = %+v, moved %v (%v), want the focus as it was and nothing moved", again, moved, err)
	}
	if after, err := f.s.Journal(); err != nil || after[len(after)-1].Kind == store.KindFocusChanged {
		t.Errorf("switching to the focused job journaled %+v (%v), want no change of focus", after[len(after)-1], err)
	}
}

// A switch needs the job and the user's witnessed message to Coordination:
// a job that does not exist, a nudge or observation, a message to another
// agent and a message from before the one that last moved the focus are
// refused, and the focus stays where it is. Only Coordination's leader moves
// it.
func TestASwitchIsRefusedWithoutTheUsersOwnLaterMessageToCoordination(t *testing.T) {
	f := newTwoResults(t)
	before := f.userSays(t, "an earlier message")
	second, err := f.s.JobOpened(store.Job{Repository: "fixture", Witness: f.userSays(t, "add a farewell"), Reading: "Add a farewell", Mandate: store.MandateTestedPR, Criteria: []string{"done"}}, false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	observation, _, err := f.s.ObservedWitnessed(store.Witnessed{Agent: "leader@coordination", Role: roles.Coordination, Generation: 1, Terminal: "/dev/pts/test", Text: "switch to 1"}, "UserPromptSubmit", json.RawMessage(`{}`), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_, elsewhere, err := f.s.ObservedWitnessed(store.Witnessed{Agent: "leader@engineering", Role: roles.Engineering, Generation: 1, Terminal: "/dev/pts/test", Text: "switch to 1"}, "UserPromptSubmit", json.RawMessage(`{}`), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		req  Request
		want string
	}{
		"no job":               {Request{Command: CommandFocus, LatestWitness: true}, "--message latest"},
		"no message":           {Request{Command: CommandFocus, Job: f.job.Number}, "--message latest"},
		"a job that is absent": {Request{Command: CommandFocus, Job: 9, LatestWitness: true}, "job 9 does not exist"},
		"an observation":       {Request{Command: CommandFocus, Job: f.job.Number, Witness: observation}, "not a witnessed message"},
		"another agent's":      {Request{Command: CommandFocus, Job: f.job.Number, Witness: elsewhere}, "not submitted to Coordination's leader"},
		"an older message":     {Request{Command: CommandFocus, Job: f.job.Number, Witness: before}, "came before message"},
	} {
		if _, _, _, err := f.d.focus(c.req); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("a switch with %s = %v, want it refused, saying %q", name, err, c.want)
		}
	}
	if focus, err := f.s.Focus(); err != nil || focus.Job != second.Number {
		t.Errorf("after the refused switches, the focus is %+v (%v), want it still on job %d", focus, err, second.Number)
	}

	// The fixture's credential is Engineering's leader's.
	if err := f.d.authorize(Request{Session: "credential", Command: CommandFocus, Job: f.job.Number, LatestWitness: true}); err == nil || !strings.Contains(err.Error(), "only leader@coordination may run asmai focus") {
		t.Errorf("Engineering's leader moving the focus = %v, want it refused", err)
	}
	if err := f.d.authorize(Request{Command: CommandFocus, Job: f.job.Number, LatestWitness: true}); err == nil || !strings.Contains(err.Error(), "agent command") {
		t.Errorf("the user moving the focus with a command = %v, want it refused as an agent command", err)
	}
}
