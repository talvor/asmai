// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"slices"
	"strings"
	"testing"

	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/session"
)

// An agent's credential cannot take: the command is the user's.
func TestAnAgentsCredentialCannotTakeAnAgentsInput(t *testing.T) {
	engineering := roles.LeaderOf(roles.Engineering)
	d := &daemon{
		leaders:  map[string]*leader{engineering.String(): {address: engineering, session: &session.Session{}, generation: 1}},
		sessions: map[string]sessionRef{"credential": {address: engineering, generation: 1}},
	}
	err := d.authorize(Request{Command: CommandTake, Agent: "worker1@engineering", Session: "credential"})
	if err == nil || !strings.Contains(err.Error(), "ask the user to run `asmai take`") {
		t.Errorf("an agent's take is refused with %v, want it told to ask the user", err)
	}
	if err := d.authorize(Request{Command: CommandTake, Agent: "worker1@engineering"}); err != nil {
		t.Errorf("the user's take is refused with %v, want it allowed", err)
	}
}

// While the user holds an agent's input, or a take waits for its turn
// boundary, nothing automated is typed into it: its messages wait.
func TestNothingAutomatedIsTypedWhileTheUserHoldsOrIsTakingTheInput(t *testing.T) {
	address := roles.LeaderOf(roles.Engineering)
	d := &daemon{leaders: map[string]*leader{}}
	l := &leader{address: address, session: &session.Session{}, generation: 1, ready: true}
	d.leaders[address.String()] = l
	if !d.mayNudge(l) {
		t.Fatal("an idle agent that nobody holds may not be nudged")
	}
	l.held = &inputHold{terminal: "/dev/pts/1"}
	if d.mayNudge(l) {
		t.Error("an agent a take waits for is nudged, want its messages to wait")
	}
	l.held.taken = true
	if d.mayNudge(l) {
		t.Error("an agent the user holds is nudged, want its messages to wait")
	}
	l.held = nil
	if !d.mayNudge(l) {
		t.Error("an agent nobody holds any more may not be nudged")
	}
}

// A turn is over only at a boundary: with nothing submitted or being
// submitted, and the session ready.
func TestATurnBoundaryIsAReadySessionWithNothingSubmitted(t *testing.T) {
	for name, tc := range map[string]struct {
		l       leader
		working bool
	}{
		"ready and idle":          {leader{ready: true}, false},
		"not yet ready":           {leader{}, true},
		"a dispatch is working":   {leader{ready: true, currentDispatch: 3}, true},
		"a nudge is being typed":  {leader{ready: true, pendingDispatch: 3}, true},
		"its input state unknown": {leader{ready: true, inputUnknown: true}, true},
	} {
		if got := tc.l.working(); got != tc.working {
			t.Errorf("%s: working is %v, want %v", name, got, tc.working)
		}
	}
}

// The status line shows each agent whose input the user holds once the
// handover happened, not while a take only waits, and not for a stopped
// session.
func TestTheStatusLineShowsTheAgentsWhoseInputTheUserHolds(t *testing.T) {
	engineering, quality := roles.LeaderOf(roles.Engineering), roles.LeaderOf(roles.Quality)
	d := &daemon{leaders: map[string]*leader{
		engineering.String(): {address: engineering, session: &session.Session{}, held: &inputHold{terminal: "/dev/pts/1", taken: true}},
		quality.String():     {address: quality, session: &session.Session{}, held: &inputHold{terminal: "/dev/pts/2"}},
		"worker1@research":   {session: nil, held: &inputHold{terminal: "/dev/pts/3", taken: true}},
	}}
	got := d.heldInputs()
	if want := []string{"you hold leader@engineering's input"}; !slices.Equal(got, want) {
		t.Fatalf("the status line shows %q, want %q", got, want)
	}

	was := statusLine{prompts: []string{}, decisions: []string{}, jobs: []int64{}}
	now := statusLine{prompts: []string{}, decisions: []string{}, jobs: []int64{}, holds: got}
	f, changed := now.frame(was, false)
	if !changed || f.Holds == nil || !slices.Equal(*f.Holds, got) || f.Prompts != nil {
		t.Errorf("the frame for a new hold is %+v (changed %v), want only the holds", f, changed)
	}
	if _, changed := now.frame(now, false); changed {
		t.Error("an unchanged hold makes a frame")
	}
	if f, _ := was.frame(was, true); f.Holds == nil {
		t.Error("the first frame leaves out the holds")
	}
}
