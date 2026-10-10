// SPDX-License-Identifier: Apache-2.0

package store

import (
	"path/filepath"
	"strings"
	"testing"
)

const qualityOwner = "leader@quality"

// newValidation records a read-only assignment of job for the next free
// worker of Quality, fixed at the job branch's tip, and returns it with its
// dispatch, which is working.
func newValidation(t *testing.T, s *Store, job int64) (Assignment, Dispatch) {
	t.Helper()
	id, err := s.NextAssignmentID()
	if err != nil {
		t.Fatal(err)
	}
	number, slot, err := s.Allocate("quality")
	if err != nil {
		t.Fatal(err)
	}
	worker := "worker" + string(rune('0'+number)) + "@quality"
	a, d, err := s.AssignmentCreated(NewAssignment{
		ID: id, Job: job, Owner: qualityOwner, Worker: worker, ReadOnly: true,
		Outcome: "Validate the job branch", Criteria: []string{"the checks re-ran at the commit"},
		Workspace: Workspace{Slot: slot, Path: "/ws/validation", Tmp: "/ws/validation-tmp", Base: tipCommit},
	}, assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	working(t, s, d)
	return a, d
}

// validationOf is a validation report of a worker of Quality at commit.
func validationOf(a Assignment, d Dispatch, commit string, v ValidationInput) Submission {
	return Submission{
		Agent: a.Worker, Generation: 1, Dispatch: d.ID, Kind: ReportResult, Commit: commit, JobTip: tipCommit, TookInTip: true,
		Input: ReportInput{
			Evidence:   []string{"./check.sh -> passed at " + commit},
			Checks:     []Check{{Command: "./check.sh", Outcome: "passed"}},
			Gaps:       []string{},
			Validation: &v,
		},
	}
}

func TestAReadOnlyAssignmentIsFixedAtTheTipHasNoBranchAndCarriesTheJobsIntent(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	job := jobWithBranch(t, s)
	if _, err := s.db.Exec(`UPDATE jobs SET words = ?, mandate = ?, criteria = ? WHERE number = ?`, "Please add a greeting", MandateTestedPR, `["Greeting is printed"]`, job); err != nil {
		t.Fatal(err)
	}
	a, d := newValidation(t, s, job)
	if !a.ReadOnly || a.Commit != tipCommit || a.Workspace.Branch != "" || a.Workspace.Base != tipCommit || a.Worker != "worker1@quality" || a.Owner != qualityOwner {
		t.Errorf("the validation is %+v", a)
	}
	if i := a.Intent; i == nil || i.Words != "Please add a greeting" || i.Reading != "Add a greeting" || i.Mandate != MandateTestedPR || strings.Join(i.Criteria, "|") != "Greeting is printed" {
		t.Errorf("the validation is given the intent %+v", a.Intent)
	}
	if d.Agent != "worker1@quality" {
		t.Errorf("the validation is dispatched to %s", d.Agent)
	}
	got, err := s.Assignment(a.ID)
	if err != nil || !got.ReadOnly || got.Commit != tipCommit || got.Intent == nil || got.Intent.Mandate != MandateTestedPR {
		t.Errorf("the validation reads back as %+v (%v)", got, err)
	}
	if live, err := s.LiveAssignmentOf("worker1@quality"); err != nil || live.ID != a.ID {
		t.Errorf("the Quality worker carries %+v (%v)", live, err)
	}

	// A writing assignment of the same job is no read-only one, and carries no
	// intent.
	writing, _ := newAssignment(t, s, job)
	if writing.ReadOnly || writing.Intent != nil || writing.Commit != "" {
		t.Errorf("the writing assignment is %+v", writing)
	}

	// One validation of a job runs at a time, and a workspace has a branch
	// exactly when it is a writing one.
	next, err := s.NextAssignmentID()
	if err != nil {
		t.Fatal(err)
	}
	base := NewAssignment{ID: next, Job: job, Owner: qualityOwner, Worker: "worker2@quality", Outcome: "o", Criteria: []string{"c"}, ReadOnly: true,
		Workspace: Workspace{Slot: 9, Path: "/p", Tmp: "/t", Base: tipCommit}}
	if _, _, err := s.AssignmentCreated(base, assignmentsAt); err == nil || !strings.Contains(err.Error(), "already has validation") {
		t.Errorf("a second validation of the job returned %v", err)
	}
	withBranch := base
	withBranch.Workspace.Branch = "asmai/job-1/x"
	if _, _, err := s.AssignmentCreated(withBranch, assignmentsAt); err == nil {
		t.Error("a read-only workspace was given a branch")
	}
	noBranch := NewAssignment{ID: next, Job: job, Owner: owner, Worker: "worker2@engineering", Outcome: "o", Criteria: []string{"c"},
		Workspace: Workspace{Slot: 9, Path: "/p", Tmp: "/t", Base: tipCommit}}
	if _, _, err := s.AssignmentCreated(noBranch, assignmentsAt); err == nil {
		t.Error("a writing workspace was made with no branch")
	}
}

func TestAValidationReportNamesItsCommitItsChecksAndEachFindingWithItsKind(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	job := jobWithBranch(t, s)
	a, d := newValidation(t, s, job)

	for name, mutate := range map[string]func(*Submission){
		"its findings":         func(s *Submission) { s.Input.Validation = nil },
		"evidence":             func(s *Submission) { s.Input.Evidence = nil },
		"nonblank evidence":    func(s *Submission) { s.Input.Evidence = []string{" "} },
		"the commit it is at":  func(s *Submission) { s.Commit = otherCommit },
		"a known finding kind": func(s *Submission) { s.Input.Validation.Findings = []FindingInput{{Kind: "minor", Text: "x"}} },
		"finding text":         func(s *Submission) { s.Input.Validation.Findings = []FindingInput{{Kind: FindingBlocking, Text: " "}} },
	} {
		sub := validationOf(a, d, tipCommit, ValidationInput{Findings: []FindingInput{{Kind: FindingBlocking, Text: "it fails"}}})
		mutate(&sub)
		if _, _, err := s.ReportSubmitted(sub, assignmentsAt); err == nil {
			t.Errorf("a validation report without %s was recorded", name)
		}
	}
	if got, _ := s.Assignment(a.ID); got.State != AssignmentActive {
		t.Fatalf("a refused report moved the assignment to %s", got.State)
	}

	// No push is recorded, for there is no branch to push, and the report need
	// not have anything of the job branch's tip in its history.
	sub := validationOf(a, d, tipCommit, ValidationInput{Findings: []FindingInput{
		{Kind: FindingBlocking, Text: "greeting.txt does not say hello"},
		{Kind: FindingAdvisory, Text: "check.sh has no comment"},
		{Kind: FindingNeedsYou, Text: "the criterion says greet the user, but the request said greet the world"},
	}})
	sub.TookInTip = false
	r, dispatch, err := s.ReportSubmitted(sub, assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	if r.Commit != tipCommit || r.Validation == nil || len(r.Validation.Findings) != 3 || len(r.Checks) != 1 || r.Checks[0].Outcome != "passed" || len(r.Effects) != 0 {
		t.Errorf("the validation report is %+v", r)
	}
	if dispatch.Agent != qualityOwner {
		t.Errorf("the report is sent to %s, want its owning leader %s", dispatch.Agent, qualityOwner)
	}
	if got, _ := s.Assignment(a.ID); got.State != AssignmentSubmitted {
		t.Errorf("the assignment is %s, want submitted", got.State)
	}
	// Until Quality's leader accepts the report, no finding is recorded.
	if findings, err := s.Findings(job); err != nil || len(findings) != 0 {
		t.Errorf("the job has findings %+v (%v) before the report is accepted", findings, err)
	}

	// A writing assignment's result has no findings.
	writing, wd := newAssignment(t, s, job)
	working(t, s, wd)
	if _, err := s.EffectRecorded(writing.Worker, 1, wd.ID, "push", "origin/x", assignmentsAt); err != nil {
		t.Fatal(err)
	}
	bad := submission(wd, ReportResult)
	bad.Input.Validation = &ValidationInput{Findings: []FindingInput{}}
	if _, _, err := s.ReportSubmitted(bad, assignmentsAt); err == nil || !strings.Contains(err.Error(), "findings belong to a validation") {
		t.Errorf("a writing result with findings returned %v", err)
	}
}

func TestAcceptingAValidationRecordsItsFindingsAndOnlyAReValidationClearsABlockingOne(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	job := jobWithBranch(t, s)
	a, d := newValidation(t, s, job)
	if _, _, err := s.ReportSubmitted(validationOf(a, d, tipCommit, ValidationInput{Findings: []FindingInput{
		{Kind: FindingBlocking, Text: "greeting.txt does not say hello"},
		{Kind: FindingAdvisory, Text: "check.sh has no comment"},
	}}), assignmentsAt); err != nil {
		t.Fatal(err)
	}

	// Only the owning leader accepts, and only a validation is accepted so.
	if _, _, err := s.ValidationAccepted(Acceptance{Assignment: a.ID, Leader: owner, Reasons: []string{"x"}}, owner, assignmentsAt); err == nil {
		t.Error("Engineering's leader accepted Quality's validation")
	}
	if _, _, err := s.ValidationAccepted(Acceptance{Assignment: a.ID, Leader: qualityOwner}, owner, assignmentsAt); err == nil {
		t.Error("a validation was accepted with no reasons")
	}
	decision, dispatch, err := s.ValidationAccepted(Acceptance{Assignment: a.ID, Leader: qualityOwner, Reasons: []string{"the checks re-ran at the head"}}, owner, assignmentsAt)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Kind != DecisionAccepted || decision.Commit != tipCommit || dispatch.Agent != owner {
		t.Errorf("the acceptance is %+v delivered by %+v", decision, dispatch)
	}
	if got, _ := s.Assignment(a.ID); got.State != AssignmentAccepted {
		t.Errorf("the validation is %s, want accepted", got.State)
	}
	if b, _ := s.JobBranch(job); b.Tip != tipCommit {
		t.Errorf("accepting a validation moved the job branch to %s", b.Tip)
	}
	findings, err := s.Findings(job)
	if err != nil || len(findings) != 2 || findings[0].Kind != FindingBlocking || findings[0].Commit != tipCommit || findings[1].Kind != FindingAdvisory || !findings[0].Open() || findings[1].Open() {
		t.Fatalf("the job's findings are %+v (%v)", findings, err)
	}
	if open, _ := s.OpenFindings(job); len(open) != 1 || open[0].ID != findings[0].ID {
		t.Errorf("the open blocking findings are %+v", open)
	}

	// The report goes to Engineering's leader with its findings and their IDs.
	messages, err := s.Inbox(owner, 0, assignmentsAt)
	if err != nil || len(messages) != 1 || messages[0].Kind != MessageValidation || messages[0].ReportData == nil || len(messages[0].ReportData.Findings) != 2 || messages[0].ReportData.Findings[0].ID != findings[0].ID {
		t.Fatalf("Engineering's inbox holds %+v (%v)", messages, err)
	}
	if !strings.Contains(messages[0].Body, "1 blocking, 1 advisory, 0 needs-you") {
		t.Errorf("the validation is summarized as %q", messages[0].Body)
	}

	// The next validation is told which blocking findings are open, and must
	// say for each whether it is resolved.
	b, bd := newValidation(t, s, job)
	messages, err = s.Inbox("worker1@quality", bd.ID, assignmentsAt)
	if err != nil || len(messages) != 1 || len(messages[0].AssignmentData.OpenFindings) != 1 || messages[0].AssignmentData.OpenFindings[0].ID != findings[0].ID {
		t.Fatalf("the re-validation's worker is given %+v (%v)", messages, err)
	}
	id := findings[0].ID
	for name, v := range map[string]ValidationInput{
		"an unexplained finding":     {Findings: []FindingInput{}},
		"a finding that is no one's": {Findings: []FindingInput{}, Resolved: []int64{id, 99}},
		"the advisory one":           {Findings: []FindingInput{}, Resolved: []int64{id, findings[1].ID}},
		"a finding named twice":      {Findings: []FindingInput{}, Resolved: []int64{id}, StillOpen: []int64{id}},
	} {
		if _, _, err := s.ReportSubmitted(validationOf(b, bd, tipCommit, v), assignmentsAt); err == nil {
			t.Errorf("a re-validation with %s was recorded", name)
		}
	}
	if _, _, err := s.ReportSubmitted(validationOf(b, bd, tipCommit, ValidationInput{Findings: []FindingInput{}}), assignmentsAt); err == nil || !strings.Contains(err.Error(), "greeting.txt does not say hello") {
		t.Errorf("an unexplained finding returned %v, want it named", err)
	}
	// Still open keeps it open, and the finding is not cleared by anything but
	// a validation that says it is resolved.
	if _, _, err := s.ReportSubmitted(validationOf(b, bd, tipCommit, ValidationInput{Findings: []FindingInput{}, StillOpen: []int64{id}}), assignmentsAt); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ValidationAccepted(Acceptance{Assignment: b.ID, Leader: qualityOwner, Reasons: []string{"x"}}, owner, assignmentsAt); err != nil {
		t.Fatal(err)
	}
	if open, _ := s.OpenFindings(job); len(open) != 1 {
		t.Fatalf("a validation that found it still open left %+v open", open)
	}

	c, cd := newValidation(t, s, job)
	if _, _, err := s.ReportSubmitted(validationOf(c, cd, tipCommit, ValidationInput{Findings: []FindingInput{{Kind: FindingAdvisory, Text: "still no comment"}}, Resolved: []int64{id}}), assignmentsAt); err != nil {
		t.Fatal(err)
	}
	if open, _ := s.OpenFindings(job); len(open) != 1 {
		t.Fatalf("a report that is not accepted yet cleared a finding: %+v", open)
	}
	if _, _, err := s.ValidationAccepted(Acceptance{Assignment: c.ID, Leader: qualityOwner, Reasons: []string{"x"}}, owner, assignmentsAt); err != nil {
		t.Fatal(err)
	}
	if open, _ := s.OpenFindings(job); len(open) != 0 {
		t.Errorf("the re-validation left %+v open", open)
	}
	findings, _ = s.Findings(job)
	if len(findings) != 3 || findings[0].ClearedBy == 0 || findings[0].Open() {
		t.Errorf("the findings after the re-validation are %+v, want the first cleared by its report", findings)
	}
	cleared, err := s.Reports(c.ID)
	if err != nil || len(cleared) != 1 || findings[0].ClearedBy != cleared[0].ID {
		t.Errorf("the finding was cleared by %d, want report %+v (%v)", findings[0].ClearedBy, cleared, err)
	}
	if got := strings.Join(kinds(t, s), ","); strings.Count(got, KindValidationAccepted) != 3 {
		t.Errorf("the journal holds %v, want three accepted validations", got)
	}
}

func TestCancellingAValidationNeedsNoPush(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	job := jobWithBranch(t, s)
	a, _ := newValidation(t, s, job)
	if _, _, err := s.CancellationRequested(a.ID, qualityOwner, []string{"no longer wanted"}, assignmentsAt); err != nil {
		t.Fatal(err)
	}
	ended, err := s.AssignmentCancelled(a.ID, Cancellation{Commit: tipCommit}, assignmentsAt)
	if err != nil || ended.State != AssignmentCancelled {
		t.Fatalf("cancelling the validation returned %+v (%v)", ended, err)
	}
	// Its worker number is free again.
	if n, _, err := s.Allocate("quality"); err != nil || n != 1 {
		t.Errorf("the next Quality worker is %d (%v), want 1", n, err)
	}
}

func TestAHandoffToQualityNamesTheFinishedJobBranchsTipAndOnlyWhileNothingIsLive(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	job := jobWithBranch(t, s)
	h := Handoff{Job: job, Sender: owner, Receiver: qualityOwner, Outcome: "Validate", Decisions: "none", Evidence: "none", Constraints: "none", Permissions: "none", Criteria: []string{"checks re-run"}, Head: otherCommit}
	if _, _, err := s.SendHandoff(h, assignmentsAt); err == nil || !strings.Contains(err.Error(), "tip is "+tipCommit) {
		t.Errorf("a handoff naming a commit that is not the tip returned %v", err)
	}
	h.Head = tipCommit
	a, _ := newAssignment(t, s, job)
	if _, _, err := s.SendHandoff(h, assignmentsAt); err == nil || !strings.Contains(err.Error(), "has not ended") {
		t.Errorf("a handoff while an assignment is live returned %v", err)
	}
	if _, _, err := s.CancellationRequested(a.ID, owner, []string{"not wanted"}, assignmentsAt); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AssignmentCancelled(a.ID, Cancellation{Commit: tipCommit, Pushed: true}, assignmentsAt); err != nil {
		t.Fatal(err)
	}
	sent, d, err := s.SendHandoff(h, assignmentsAt)
	if err != nil || sent.Head != tipCommit || d.Agent != qualityOwner {
		t.Fatalf("the handoff returned %+v, %+v (%v)", sent, d, err)
	}
	got, err := s.Handoff(sent.ID)
	if err != nil || got.Head != tipCommit {
		t.Errorf("the handoff reads back as %+v (%v)", got, err)
	}
	// A handoff that is no request for validation names no head.
	h.Head = ""
	if plain, _, err := s.SendHandoff(h, assignmentsAt); err != nil || plain.Head != "" {
		t.Errorf("a handoff without a head returned %+v (%v)", plain, err)
	}
}
