// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/store"
)

// restartCase is a worker the previous daemon left, with the assignment it
// carried in the state the case says.
type restartCase struct {
	name string
	// native is the provider's session of the worker and whether its
	// transcript is still on disk; with no native session the provider
	// reported none.
	native, transcript bool
	// leave puts the worker's dispatch in the state the stop found it in.
	leave string
	// blocked has the dispatch end in a blocked report, and noWorkspace
	// leaves the assignment's workspace missing.
	blocked     bool
	noWorkspace bool
	// cancel has the owning leader cancel the assignment after its worker's
	// first turn stopped, so that leave is the state of the cancellation's
	// dispatch.
	cancel bool
}

// restartStore makes a store the previous daemon left: job 1 with a branch and
// one assignment for each case, numbered from 1, whose worker fetched its
// assignment and was left as the case says.
func restartStore(t *testing.T, cases []restartCase) (*store.Store, map[string]int64) {
	t.Helper()
	paths := stateDir(t)
	if err := paths.Prepare(); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(paths.Store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	at := time.Now().Add(-time.Hour)
	if err := s.RepositoryAdded(store.Repository{Name: "fixture", Origin: "/origin", DefaultBranch: "main", Clone: "/clone", AddedAt: at}); err != nil {
		t.Fatal(err)
	}
	if err := s.ConversationEntered("leader@coordination", roles.Coordination, 1, "/dev/pts/test", at); err != nil {
		t.Fatal(err)
	}
	_, witness, err := s.ObservedWitnessed(store.Witnessed{Agent: "leader@coordination", Role: roles.Coordination, Generation: 1, Terminal: "/dev/pts/test", Text: "add a greeting"}, "UserPromptSubmit", json.RawMessage(`{"prompt":"add a greeting"}`), at)
	if err != nil {
		t.Fatal(err)
	}
	job, err := s.JobOpened(store.Job{Repository: "fixture", Witness: witness, Reading: "Add a greeting", Mandate: store.MandateTestedPR, Criteria: []string{"it greets"}}, false, at)
	if err != nil {
		t.Fatal(err)
	}
	const tip = "1111111111111111111111111111111111111111"
	if _, err := s.JobBranchMade(store.JobBranch{Job: job.Number, Repository: "fixture", Name: "asmai/job-1-add-a-greeting", StartedFrom: tip, View: "/views/job-1"}, at); err != nil {
		t.Fatal(err)
	}
	ids := map[string]int64{}
	for _, c := range cases {
		id, err := s.NextAssignmentID()
		if err != nil {
			t.Fatal(err)
		}
		number, slot, err := s.Allocate(roles.Engineering)
		if err != nil {
			t.Fatal(err)
		}
		worker := roles.WorkerOf(roles.Engineering, number)
		workspace := filepath.Join(t.TempDir(), c.name)
		if !c.noWorkspace {
			if err := os.MkdirAll(workspace, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		a, dispatch, err := s.AssignmentCreated(store.NewAssignment{
			ID: id, Job: job.Number, Owner: "leader@engineering", Worker: worker.String(), Outcome: "Add greeting.txt", Criteria: []string{"it says hello"},
			Workspace: store.Workspace{Slot: slot, Path: workspace, Tmp: "/tmp/" + c.name, Branch: "asmai/job-1/" + c.name, Base: tip},
		}, at)
		if err != nil {
			t.Fatal(err)
		}
		ids[c.name] = a.ID
		generation, err := s.SessionStarted(store.SessionStart{Agent: worker.String(), Role: roles.Engineering, Provider: "claude-code", Version: "2.1.292", Model: "opus", Executable: "/p", Args: []string{}, Dir: "/ws/" + c.name, PID: 7, At: at, Assignment: a.ID})
		if err != nil {
			t.Fatal(err)
		}
		if c.native {
			path := ""
			if c.transcript {
				path = filepath.Join(t.TempDir(), "session.jsonl")
				if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				path = "/gone/session.jsonl"
			}
			if err := s.SessionIdentified(worker.String(), generation, "native-"+c.name, path, at); err != nil {
				t.Fatal(err)
			}
		}
		if c.cancel {
			if _, err := s.Inbox(worker.String(), dispatch.ID, at); err != nil {
				t.Fatal(err)
			}
			if err := s.WorkFetchedDispatch(dispatch.ID, worker.String(), generation, at); err != nil {
				t.Fatal(err)
			}
			if _, err := s.StopDispatchIfWorking(dispatch.ID, generation, "", at); err != nil {
				t.Fatal(err)
			}
			if _, dispatch, err = s.CancellationRequested(a.ID, "leader@engineering", []string{"not wanted"}, at); err != nil {
				t.Fatal(err)
			}
			if c.leave != store.DispatchCreated {
				// Nudged in the worker's session, as it is before it fetches.
				if err := s.ChangeDispatch(dispatch.ID, generation, store.DispatchCreated, store.DispatchNudged, "", at); err != nil {
					t.Fatal(err)
				}
			}
		}
		if c.leave == store.DispatchCreated {
			continue
		}
		if _, err := s.Inbox(worker.String(), dispatch.ID, at); err != nil {
			t.Fatal(err)
		}
		if err := s.WorkFetchedDispatch(dispatch.ID, worker.String(), generation, at); err != nil {
			t.Fatal(err)
		}
		if c.leave == store.DispatchStopped {
			if c.blocked {
				if _, err := s.EffectRecorded(worker.String(), generation, dispatch.ID, "push", "origin/asmai/job-1/"+c.name, at); err != nil {
					t.Fatal(err)
				}
				sub := store.Submission{Agent: worker.String(), Generation: generation, Dispatch: dispatch.ID, Kind: store.ReportBlocked, Commit: tip, JobTip: tip, TookInTip: true, Input: store.ReportInput{Reason: "which greeting?"}}
				if _, _, err := s.ReportSubmitted(sub, at); err != nil {
					t.Fatal(err)
				}
			}
			if stopped, err := s.StopDispatchIfWorking(dispatch.ID, generation, "", at); err != nil || !stopped {
				t.Fatalf("%s: stopping its dispatch: %v, %v", c.name, stopped, err)
			}
		}
		// Every session ended with the previous daemon.
		if err := s.SessionEnded(worker.String(), generation, "signal: terminated", "asmai stop", at); err != nil {
			t.Fatal(err)
		}
	}
	return s, ids
}

func recoveringDaemon(s *store.Store) *daemon {
	return &daemon{store: s, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func assignmentState(t *testing.T, s *store.Store, id int64) string {
	t.Helper()
	a, err := s.Assignment(id)
	if err != nil {
		t.Fatal(err)
	}
	return a.State
}

var restartCases = []restartCase{
	{name: "stopped", native: true, transcript: true, leave: store.DispatchStopped},
	{name: "nosession", leave: store.DispatchStopped},
	{name: "notranscript", native: true, leave: store.DispatchStopped},
	{name: "noworkspace", native: true, transcript: true, leave: store.DispatchStopped, noWorkspace: true},
	{name: "working", native: true, transcript: true, leave: store.DispatchWorking},
	{name: "blocked", native: true, transcript: true, leave: store.DispatchStopped, blocked: true},
	{name: "unfetched", native: true, transcript: true, leave: store.DispatchCreated},
	{name: "cancelstopped", native: true, transcript: true, leave: store.DispatchStopped, cancel: true},
	{name: "cancelworking", native: true, transcript: true, leave: store.DispatchWorking, cancel: true},
}

func TestAStartAfterACleanStopResumesWhatStoppedAtABoundaryAndHoldsTheRest(t *testing.T) {
	s, ids := restartStore(t, restartCases)
	d := recoveringDaemon(s)

	d.recoverWork(true)

	want := map[string]string{
		"stopped":      store.AssignmentActive,
		"nosession":    store.AssignmentNeedsReconciliation,
		"notranscript": store.AssignmentNeedsReconciliation,
		"noworkspace":  store.AssignmentNeedsReconciliation,
		"working":      store.AssignmentNeedsReconciliation,
		"blocked":      store.AssignmentActive,
		"unfetched":    store.AssignmentActive,
		// A cancellation under way is continued like any assignment's work.
		"cancelstopped": store.AssignmentCancelling,
		"cancelworking": store.AssignmentNeedsReconciliation,
	}
	for name, state := range want {
		if got := assignmentState(t, s, ids[name]); got != state {
			t.Errorf("assignment %q is %s after the restart, want %s", name, got, state)
		}
	}
	for name, resumed := range map[string]bool{"stopped": true, "cancelstopped": true} {
		w, err := s.WorkerDispatch(ids[name])
		if err != nil {
			t.Fatal(err)
		}
		if got := w.Kind == store.MessageResumption; got != resumed {
			t.Errorf("assignment %q's latest dispatch is a %s", name, w.Kind)
		}
		if resumed && (w.Dispatch.NativeSession != "native-"+name || w.Dispatch.Resumes == 0) {
			t.Errorf("assignment %q's resumption is %+v, want it to resume its stopped dispatch in native-%s", name, w.Dispatch, name)
		}
	}
	for _, name := range []string{"nosession", "notranscript", "noworkspace", "blocked", "unfetched", "working"} {
		if w, err := s.WorkerDispatch(ids[name]); err != nil || w.Kind != store.MessageAssignment {
			t.Errorf("assignment %q's latest dispatch is %+v (%v), want its assignment, not resumed", name, w, err)
		}
	}
	// A turn that was cut off has an unknown outcome; one that stopped keeps
	// the state the provider reported.
	if w, _ := s.WorkerDispatch(ids["working"]); w.Dispatch.State != store.DispatchUnknown {
		t.Errorf("the cut off dispatch is %s, want unknown", w.Dispatch.State)
	}
	if w, _ := s.WorkerDispatch(ids["nosession"]); w.Dispatch.State != store.DispatchStopped {
		t.Errorf("a dispatch that stopped is %s, want it left as it was", w.Dispatch.State)
	}

	// The owning leader is told of each that needs reconciliation, and each
	// leader with open work to load its job's brief.
	var told []int64
	entries, err := s.Journal()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Kind != store.KindAssignmentNeedsReconciliation {
			continue
		}
		var h struct {
			Assignment int64  `json:"assignment"`
			Owner      string `json:"owner"`
			Reason     string `json:"reason"`
		}
		if err := json.Unmarshal(e.Data, &h); err != nil {
			t.Fatal(err)
		}
		told = append(told, h.Assignment)
		if h.Owner != "leader@engineering" || !strings.Contains(h.Reason, "worker") {
			t.Errorf("assignment %d was held as %+v", h.Assignment, h)
		}
	}
	if wantTold := []int64{ids["nosession"], ids["notranscript"], ids["noworkspace"], ids["working"], ids["cancelworking"]}; !slices.Equal(told, wantTold) {
		t.Errorf("the assignments held for reconciliation are %v, want %v", told, wantTold)
	}
	work, err := s.OpenLeaderWork()
	if err != nil || len(work) != 2 {
		t.Fatalf("the open work is %+v (%v)", work, err)
	}
	pending, err := s.PendingLeaders()
	if err != nil {
		t.Fatal(err)
	}
	// The worker with its resumption, the unfetched assignment's worker, and
	// the leaders told: no worker whose assignment is held.
	for _, name := range []string{"worker1@engineering", "worker7@engineering", "worker8@engineering", "leader@coordination", "leader@engineering"} {
		if !slices.Contains(pending, name) {
			t.Errorf("%s has no message waiting among %v", name, pending)
		}
	}
	for _, name := range []string{"worker2@engineering", "worker3@engineering", "worker4@engineering", "worker5@engineering", "worker9@engineering"} {
		if slices.Contains(pending, name) {
			t.Errorf("%s, whose assignment is held for reconciliation, has a message waiting", name)
		}
	}

	// Recovering again, as a start that could not run its agents is followed
	// by another, resumes nothing a second time and tells no one twice.
	before := len(entries)
	d.recoverWork(true)
	if after, _ := s.Journal(); len(after) != before {
		t.Errorf("a second recovery journaled %d more entries", len(after)-before)
	}
}

func TestAStartAfterAnUncleanStopRestoresTheLeadersAndLeavesTheWorkersAlone(t *testing.T) {
	s, ids := restartStore(t, restartCases)
	d := recoveringDaemon(s)

	d.recoverWork(false)

	for name, id := range ids {
		want, kind := store.AssignmentActive, store.MessageAssignment
		if strings.HasPrefix(name, "cancel") {
			want, kind = store.AssignmentCancelling, store.MessageCancellation
		}
		if got := assignmentState(t, s, id); got != want {
			t.Errorf("assignment %q is %s after an unclean stop, want it left as it was, %s", name, got, want)
		}
		if w, _ := s.WorkerDispatch(id); w.Kind != kind {
			t.Errorf("assignment %q's latest dispatch is a %s after an unclean stop, want it left as it was, a %s", name, w.Kind, kind)
		}
	}
	work, err := s.OpenLeaderWork()
	if err != nil || len(work) != 2 {
		t.Fatalf("the open work is %+v (%v)", work, err)
	}
	pending, err := s.PendingLeaders()
	if err != nil || !slices.Contains(pending, "leader@engineering") || !slices.Contains(pending, "leader@coordination") {
		t.Errorf("the agents with a message waiting are %v (%v), want both leaders told to load the brief", pending, err)
	}
}

func TestAHookOfASessionFromBeforeAReplacementIsJournaledAndChangesNothing(t *testing.T) {
	s, ids := restartStore(t, []restartCase{{name: "stopped", native: true, transcript: true, leave: store.DispatchStopped}})
	d := recoveringDaemon(s)
	d.leaders = map[string]*leader{}
	d.sessions = map[string]sessionRef{}
	worker := roles.WorkerOf(roles.Engineering, 1)
	d.recoverWork(true)
	generation, err := s.SessionStarted(store.SessionStart{Agent: worker.String(), Role: roles.Engineering, Provider: "claude-code", Version: "2.1.292", Model: "opus", Executable: "/p", Args: []string{}, Dir: "/ws", PID: 8, At: time.Now(), Assignment: ids["stopped"], Resumes: "native-stopped"})
	if err != nil || generation != 2 {
		t.Fatalf("the replacement session: %d, %v", generation, err)
	}
	// Generation 1's session is still known to this daemon, as a hook that
	// arrives late is from a session it started; generation 2 is current.
	d.sessions["old"] = sessionRef{address: worker, generation: 1}
	d.sessions["new"] = sessionRef{address: worker, generation: 2}
	d.leaders[worker.String()] = &leader{address: worker, generation: 2, assignment: ids["stopped"]}
	w, err := s.WorkerDispatch(ids["stopped"])
	if err != nil || w.Kind != store.MessageResumption || w.Dispatch.State != store.DispatchCreated {
		t.Fatalf("the worker's latest dispatch is %+v (%v)", w, err)
	}
	before, _ := s.Journal()

	// A late Stop of generation 1, correlated to nothing the new session
	// was given.
	payload := json.RawMessage(`{"hook_event_name":"Stop","prompt_id":"first","session_id":"native-stopped"}`)
	if err := d.observe("old", payload); err != nil {
		t.Fatal(err)
	}
	after, _ := s.Journal()
	observed := 0
	for _, e := range after[len(before):] {
		if e.Kind == store.KindObservation {
			observed++
			if !strings.Contains(string(e.Data), `"generation":1`) {
				t.Errorf("the late hook was journaled as %s, want it as generation 1's", e.Data)
			}
		}
	}
	if observed != 1 {
		t.Errorf("%d observations were journaled for the late hook, want 1", observed)
	}
	got, err := s.WorkerDispatch(ids["stopped"])
	if err != nil || got.Dispatch.ID != w.Dispatch.ID || got.Dispatch.State != store.DispatchCreated {
		t.Errorf("the resumption is %+v (%v) after the late hook, want it untouched", got, err)
	}
}

func TestARestoredLeaderIsToldOnceToLoadTheBriefOfEachJobItHasOpenWorkIn(t *testing.T) {
	s, _ := restartStore(t, []restartCase{{name: "unfetched", native: true, transcript: true, leave: store.DispatchCreated}})
	// A second open job, which only Coordination has any work in.
	witness := int64(0)
	entries, err := s.Journal()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Kind == store.KindMessageWitnessed {
			witness = e.ID
		}
	}
	second, err := s.JobOpened(store.Job{Repository: "fixture", Witness: witness, Reading: "Add a farewell", Mandate: store.MandateTestedPR, Criteria: []string{"it says goodbye"}}, false, time.Now())
	if err != nil || second.Number != 2 {
		t.Fatalf("opening a second job: %+v (%v)", second, err)
	}
	d := recoveringDaemon(s)

	d.recoverWork(true)

	messages, err := s.Inbox("leader@coordination", 0, time.Now())
	if err != nil || len(messages) != 1 {
		t.Fatalf("Coordination's inbox holds %+v (%v), want one message for both jobs", messages, err)
	}
	m := messages[0]
	if m.Kind != store.MessageRestoration || m.Job != 1 || m.Sender != store.Daemon {
		t.Errorf("the message is %+v, want a restoration about the first job", m)
	}
	for _, want := range []string{"`asmai brief 1`", "`asmai brief 2`", "asmai inbox", "leader@coordination"} {
		if !strings.Contains(m.Body, want) {
			t.Errorf("the restoration says %q, want it to say %q", m.Body, want)
		}
	}
	// Engineering has open work in the first job alone.
	messages, err = s.Inbox("leader@engineering", 0, time.Now())
	var restorations []store.Message
	for _, m := range messages {
		if m.Kind == store.MessageRestoration {
			restorations = append(restorations, m)
		}
	}
	if err != nil || len(restorations) != 1 || strings.Contains(restorations[0].Body, "asmai brief 2") || !strings.Contains(restorations[0].Body, "Load `asmai brief 1` for job 1") {
		t.Errorf("Engineering's restorations are %+v (%v), want one for job 1 only", restorations, err)
	}
}
