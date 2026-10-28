// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/session"
	"github.com/talvor/asmai/internal/store"
)

// leaderWorksAs has the leader owner of f work on the next dispatch waiting
// for it, as its fetch does, and returns the dispatch.
func (f *twoResults) leaderWorksAs(t *testing.T, owner roles.Address) int64 {
	t.Helper()
	d, err := f.s.NextDispatch(owner.String())
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.ChangeDispatch(d.ID, 1, d.State, store.DispatchWorking, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	f.d.leaders[owner.String()].currentDispatch = d.ID
	return d.ID
}

// addLeader makes the session credential name the running leader of role.
func (f *twoResults) addLeader(role, credential string) roles.Address {
	address := roles.LeaderOf(role)
	f.d.leaders[address.String()] = &leader{address: address, session: &session.Session{}, generation: 1}
	f.d.sessions[credential] = sessionRef{address: address, generation: 1}
	return address
}

func TestTheDeliveryOwnerOfAJobIsTheLeaderWhoseNotesTheyAre(t *testing.T) {
	for _, tt := range []struct {
		name string
		j    store.Job
		want roles.Address
	}{
		{"a tested pull request", store.Job{Repository: "fixture", Mandate: store.MandateTestedPR}, roles.LeaderOf(roles.Engineering)},
		{"research notes in a repository", store.Job{Repository: "fixture", Mandate: store.MandateResearchOnly}, roles.LeaderOf(roles.Research)},
		{"planning notes in a repository", store.Job{Repository: "fixture", Mandate: store.MandatePlanningOnly}, roles.LeaderOf(roles.Planning)},
		{"the setup of a repository", store.Job{Repository: "fixture", Mandate: store.MandateSetup}, roles.LeaderOf(roles.Coordination)},
	} {
		if got := deliveryOwner(tt.j); got != tt.want {
			t.Errorf("the delivery owner of %s is %s, want %s", tt.name, got, tt.want)
		}
	}
	for _, role := range []string{roles.Engineering, roles.Planning, roles.Research, roles.Coordination} {
		if !isDeliveryOwner(roles.LeaderOf(role)) {
			t.Errorf("the leader of %s cannot own a delivery", role)
		}
	}
	for _, address := range []roles.Address{roles.LeaderOf(roles.Quality), roles.WorkerOf(roles.Research, 1), roles.WorkerOf(roles.Coordination, 1)} {
		if isDeliveryOwner(address) {
			t.Errorf("%s can own a delivery", address)
		}
	}
}

func TestTheDaemonPushesANotesJobsBranchForItsOwnLeaderWithNoQualityValidation(t *testing.T) {
	f := newTwoResultsOf(t, roles.Research)
	branch := jobBranchFor(f.job)
	push := Request{Session: "credential", Command: CommandPush, Job: f.job.Number}
	if _, err := f.decide(CommandAccept, f.assignments[0].ID, "file-1.txt holds the notes"); err != nil {
		t.Fatal(err)
	}

	// Only the leader whose notes they are asks, and while working on a
	// dispatch.
	f.addLeader(roles.Planning, "planning")
	f.addLeader(roles.Engineering, "engineering")
	for _, credential := range []string{"planning", "engineering"} {
		if _, err := f.d.push(Request{Session: credential, Command: CommandPush, Job: f.job.Number}); err == nil {
			t.Errorf("the %s leader asked for a push of a research job", credential)
		}
	}
	f.d.leaders[roles.LeaderOf(roles.Planning).String()].currentDispatch = 99
	if _, err := f.d.push(Request{Session: "planning", Command: CommandPush, Job: f.job.Number}); err == nil || !strings.Contains(err.Error(), "leader@research owns the delivery of job 1") {
		t.Errorf("Planning's leader asking for a push of a research job returned %v", err)
	}
	if _, err := f.d.push(push); err == nil || !strings.Contains(err.Error(), "no current dispatch") {
		t.Errorf("a push asked for outside a dispatch returned %v", err)
	}
	dispatch := f.leaderWorksAs(t, roles.LeaderOf(roles.Research))

	// Another assignment of the job is still live: the notes are not all in.
	if _, err := f.d.push(push); err == nil || !strings.Contains(err.Error(), "assignment 2 of job 1 has not ended") {
		t.Fatalf("a push with an assignment still live returned %v", err)
	}
	if got := git(t, f.origin, "branch", "--list", branch); got != "" {
		t.Fatalf("the job branch reached origin with an assignment still live: %s", got)
	}
	if _, err := f.decide(CommandCancel, f.assignments[1].ID, "its notes are not wanted"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.AssignmentCancelled(f.assignments[1].ID, store.Cancellation{Commit: f.commits[1], Pushed: true}, time.Now()); err != nil {
		t.Fatal(err)
	}

	// With every assignment ended, the job branch is pushed with no Quality
	// validation, as the daemon's own effect, tagged with the leader's dispatch.
	got, err := f.d.push(push)
	if err != nil {
		t.Fatal(err)
	}
	if got.Result != store.PushPushed || got.Commit != f.commits[0] || got.Branch != branch || got.Dispatch != dispatch || got.Leader != "leader@research" || got.Effect == 0 {
		t.Errorf("the push is %+v", got)
	}
	if on := git(t, f.origin, "rev-parse", "refs/heads/"+branch); on != f.commits[0] {
		t.Errorf("origin's job branch is at %s, want the accepted notes %s", on, f.commits[0])
	}
	if err := f.s.Deliverable(f.job.Number, f.commits[0]); err != nil {
		t.Errorf("the pushed head of a notes job is not deliverable: %v", err)
	}
}

func TestAPrototypeAcceptedToStayOnItsBranchMovesNoJobBranchAndItsOriginBranchStays(t *testing.T) {
	f := newTwoResults(t)
	branchRef := "refs/heads/" + jobBranchFor(f.job)
	prototype := f.assignments[0]

	// Nothing but the prototype's own branch on origin holds it.
	git(t, f.origin, "update-ref", "-d", "refs/heads/"+prototype.Workspace.Branch)
	accept := Request{Session: "credential", Command: CommandAccept, Assignment: prototype.ID, Reasons: []string{"it answers the question"}, Prototype: true}
	resp, _, err := f.d.decide(accept)
	if err == nil || !strings.Contains(err.Error(), "origin does not have the assignment branch") {
		t.Fatalf("a prototype with no branch on origin was accepted: %+v, %v", resp, err)
	}
	if got, _ := f.s.Assignment(prototype.ID); got.State != store.AssignmentSubmitted {
		t.Fatalf("a refused prototype left its assignment %s", got.State)
	}
	git(t, f.clone, "push", "origin", prototype.Workspace.Branch)

	for name, req := range map[string]Request{
		"with --ends-job": {Session: "credential", Command: CommandAccept, Assignment: prototype.ID, Reasons: []string{"x"}, Prototype: true, EndsJob: true},
		"of a rejection":  {Session: "credential", Command: CommandReject, Assignment: prototype.ID, Reasons: []string{"x"}, Prototype: true},
	} {
		if _, _, err := f.d.decide(req); err == nil {
			t.Errorf("a prototype %s was accepted", name)
		}
	}

	resp, _, err = f.d.decide(accept)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Assignment.State != store.AssignmentAccepted || resp.Prototype == nil || resp.Prototype.Branch != prototype.Workspace.Branch || resp.Prototype.Commit != f.commits[0] || resp.Decision.Commit != f.commits[0] {
		t.Errorf("the acceptance answered %+v, %+v and %+v", resp.Assignment, resp.Decision, resp.Prototype)
	}
	if got := git(t, f.clone, "rev-parse", branchRef); got != f.started {
		t.Errorf("the job branch is at %s, want it still at %s: a prototype stays on its own branch", got, f.started)
	}
	if b, _ := f.s.JobBranch(f.job.Number); b.Tip != f.started || resp.JobBranch.Tip != f.started {
		t.Errorf("the recorded job branch is at %s, want %s", b.Tip, f.started)
	}
	if _, err := os.Stat(prototype.Workspace.Path); !os.IsNotExist(err) {
		t.Errorf("the prototype's workspace is still there (%v)", err)
	}
	if got := git(t, f.origin, "rev-parse", "refs/heads/"+prototype.Workspace.Branch); got != f.commits[0] {
		t.Errorf("origin's assignment branch is at %s, want the prototype kept at %s", got, f.commits[0])
	}
	if out := git(t, f.origin, "branch", "--list", "asmai/test/job-1-*"); out != "" {
		t.Errorf("the job branch was pushed: %s", out)
	}
	listed, err := f.s.AssignmentsForJob(f.job.Number)
	if err != nil {
		t.Fatal(err)
	}
	if brief := brief(f.job, store.Repository{}, nil, nil, nil, nil, listed, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, ""); !strings.Contains(brief, "accepted as a prototype that stays on its own branch "+prototype.Workspace.Branch+" at "+f.commits[0]+" on origin, not on the job branch") {
		t.Errorf("the brief does not name where the prototype stays:\n%s", brief)
	}

	// The verdict note is another assignment's work on the job branch: the
	// other result is still a fast-forward of the tip the prototype left.
	resp, err = f.decide(CommandAccept, f.assignments[1].ID, "the verdict note names the prototype's branch")
	if err != nil {
		t.Fatal(err)
	}
	if resp.JobBranch.Tip != f.commits[1] {
		t.Errorf("the job branch is at %s after the verdict note, want %s", resp.JobBranch.Tip, f.commits[1])
	}
}

func TestCoordinationIsToldANotesPullRequestIsReadyAndThatNoOneValidatedIt(t *testing.T) {
	j := store.Job{Number: 3, Repository: "fixture", Mandate: store.MandateResearchOnly}
	green := deliveryBody(j, store.Delivery{Number: 7, URL: "https://github.com/o/r/pull/7", Head: "bbb", Checks: []store.CheckState{{Name: "CI / test"}}})
	for _, want := range []string{"Job 3 is delivered", "its notes pull request #7, https://github.com/o/r/pull/7", "head bbb, with every check green (CI / test)", "Quality validated nothing", "The job has ended", "AsmAI never merges", "in a line tagged with job 3, with the link"} {
		if !strings.Contains(green, want) {
			t.Errorf("the delivery message lacks %q: %s", want, green)
		}
	}
	straightAway := deliveryBody(j, store.Delivery{Number: 7, URL: "https://github.com/o/r/pull/7", Head: "bbb", NoCI: true})
	if !strings.Contains(straightAway, "head bbb, straight away, as its repository is declared as having no CI") || strings.Contains(straightAway, "every check green") {
		t.Errorf("the no-CI delivery message is: %s", straightAway)
	}
}

func TestTheDeliveryOwnerOfANotesJobIsToldRedChecksWithAFixThatNeedsNoQuality(t *testing.T) {
	o := store.ChecksObservation{Number: 7, Head: "bbb", Verdict: "red", Checks: []store.CheckState{{Name: "CI / docs", State: "failed"}}}
	got := redBody(store.Job{Number: 3, Repository: "fixture", Mandate: store.MandatePlanningOnly}, o, nil)
	for _, want := range []string{"The checks on head bbb of job 3's pull request #7 are red. Failing: CI / docs.", "accept its result, and ask for the push with `asmai push`", "a notes job has no Quality validation to wait for"} {
		if !strings.Contains(got, want) {
			t.Errorf("the red checks message lacks %q: %s", want, got)
		}
	}
	if strings.Contains(got, "hand validation") {
		t.Errorf("a notes job's red checks message sends the fix to Quality: %s", got)
	}
}

func TestTheBriefSaysAPullRequestWithNoCIWasMarkedReadyStraightAway(t *testing.T) {
	j := store.Job{Number: 1, Role: "coordination", State: store.JobEnded, Repository: "fixture", Mandate: store.MandateResearchOnly}
	at := time.Date(2026, 10, 12, 1, 2, 3, 0, time.UTC)
	delivery := &store.Delivery{Job: 1, Number: 7, URL: "https://github.com/o/r/pull/7", Head: "bbb", NoCI: true, At: at}
	got := brief(j, store.Repository{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, delivery, nil, nil, nil, nil, nil, "")
	want := "Delivery:\n- pull request #7 https://github.com/o/r/pull/7 marked ready for review at bbb, 2026-10-12T01:02:03Z, straight away, as the repository is declared as having no CI; the job ended. AsmAI never merges\n"
	if !strings.Contains(got, want) {
		t.Errorf("the brief lacks %q:\n%s", want, got)
	}
}
