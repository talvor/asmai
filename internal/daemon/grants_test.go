// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/store"
)

func jobGrant(job int64) *store.GrantInput {
	return &store.GrantInput{Scope: store.ScopeJob, Job: job, Action: store.ActionDeploy, Approved: "deploy the build to staging", Conditions: []string{"the tests pass"}}
}

func (f twoResults) grantRequest(session string, in *store.GrantInput) Request {
	return Request{Session: session, Command: CommandDecisionRecord, Grant: in}
}

func TestACoordinationLeaderRecordsAGrantTheUserGaveInTheConversationCitingTheirWords(t *testing.T) {
	f := newTwoResults(t)
	f.withWorkers(t)
	said := f.userSays(t, "yes, you may deploy the build to staging for this job")

	// Only a leader records a grant, and only Coordination's one given in the
	// conversation; the refusals say what to do.
	for _, tt := range []struct {
		name, session string
		cites         int64
		want          string
	}{
		{"a worker", "worker0", said, "only a role's leader"},
		{"Engineering's leader", "credential", said, "only leader@coordination records a grant the user gave in the conversation"},
		{"no citation", "coordination", 0, "cites the witnessed message"},
	} {
		in := jobGrant(f.job.Number)
		in.Cites = tt.cites
		if _, err := f.d.recordGrant(f.grantRequest(tt.session, in)); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v, want a refusal naming %q", tt.name, err, tt.want)
		}
	}
	if grants, err := f.s.Grants(); err != nil || len(grants) != 0 {
		t.Fatalf("refused grants left %+v (%v)", grants, err)
	}

	in := jobGrant(f.job.Number)
	req := f.grantRequest("coordination", in)
	req.LatestWitness = true
	g, err := f.d.recordGrant(req)
	if err != nil {
		t.Fatal(err)
	}
	if g.Cites != said || g.Words != "yes, you may deploy the build to staging for this job" || g.Leader != "leader@coordination" || g.State != store.GrantActive {
		t.Errorf("grant = %+v", g)
	}

	// The job's brief lists the grant with its words and conditions.
	text := brief(f.job, store.Repository{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, []store.Grant{g})
	for _, want := range []string{"Grants (", "- grant 1 | job 1 | deploy | active | recorded by leader@coordination", "Approved: deploy the build to staging", "Conditions: the tests pass", "The user's words: yes, you may deploy"} {
		if !strings.Contains(text, want) {
			t.Errorf("the brief lacks %q:\n%s", want, text)
		}
	}
}

func TestAConsequentialActionNeedsAGrantAndTheEffectNeedsTheAuthorization(t *testing.T) {
	f := newTwoResults(t)
	f.withWorkers(t)
	check := func(session, action string, amount int64) (store.Authorization, error) {
		in := store.ActionRequest{Action: action, What: "the staging environment", Amount: amount}
		if action == store.ActionSpend {
			in.Currency = "USD"
		}
		return f.d.authorizeAction(Request{Session: session, Command: CommandAuthorize, Action: &in})
	}
	record := func(session, kind string) error {
		_, err := f.d.effect(Request{Session: session, Command: CommandEffect, Kind: kind, Ref: "staging"})
		return err
	}

	// Without a grant nothing is authorized, and the effect is refused until
	// the action was checked.
	if _, err := check("worker0", store.ActionDeploy, 0); err == nil || !strings.Contains(err.Error(), "no grant covers deploy in job 1") {
		t.Fatalf("deploying with no grant = %v", err)
	}
	if err := record("worker0", "deploy"); err == nil || !strings.Contains(err.Error(), "only after `asmai authorize") {
		t.Fatalf("recording a deployment nobody authorized = %v", err)
	}
	if _, err := check("worker0", store.ActionSpend, 500); err == nil || !strings.Contains(err.Error(), "spending is zero") {
		t.Fatalf("spending with no grant = %v", err)
	}
	// A leader names its job; a worker's is its assignment's.
	if _, err := f.d.authorizeAction(Request{Session: "credential", Command: CommandAuthorize, Action: &store.ActionRequest{Action: store.ActionDeploy, What: "x"}}); err == nil || !strings.Contains(err.Error(), "give --job") {
		t.Fatalf("a leader's action with no job = %v", err)
	}
	// A merge is never authorized, never recorded, and never granted.
	if _, err := check("worker0", "merge", 0); err == nil || !strings.Contains(err.Error(), "AsmAI never merges") {
		t.Fatalf("authorizing a merge = %v", err)
	}
	said := f.userSays(t, "you may deploy to staging")
	merge := jobGrant(f.job.Number)
	merge.Action, merge.Cites = "merge", said
	if _, err := f.d.recordGrant(f.grantRequest("coordination", merge)); err == nil || !strings.Contains(err.Error(), "no grant can authorize a merge") {
		t.Fatalf("granting a merge = %v", err)
	}

	in := jobGrant(f.job.Number)
	in.Cites = said
	g, err := f.d.recordGrant(f.grantRequest("coordination", in))
	if err != nil {
		t.Fatal(err)
	}
	a, err := check("worker1", store.ActionDeploy, 0)
	if err != nil || a.Grant != g.ID || a.Job != f.job.Number || a.Assignment != f.assignments[1].ID || a.Agent != f.assignments[1].Worker || a.Dispatch == 0 {
		t.Fatalf("authorizing under the grant = %+v, %v", a, err)
	}
	// The authorization belongs to the dispatch it was made in, and covers one
	// effect: the other worker's effect of the same kind, and a second effect
	// of this worker's, each still need a check of their own.
	if err := record("worker1", "deploy"); err != nil {
		t.Fatalf("recording the authorized deployment = %v", err)
	}
	if err := record("worker0", "deploy"); err == nil {
		t.Fatal("another dispatch's authorization covered this worker's effect")
	}
	if err := record("worker1", "deploy"); err == nil || !strings.Contains(err.Error(), "once for each effect") {
		t.Fatalf("a second effect on one authorization = %v", err)
	}
	if used, err := f.s.Authorized(g.ID); err != nil || len(used) != 1 || used[0].Effect == 0 {
		t.Fatalf("the authorization after its effect = %+v, %v", used, err)
	}
	// An effect is checked again when it is recorded: a grant that was revoked
	// after the check covers nothing, and the check stands as an earlier action.
	if _, err := check("worker1", store.ActionDeploy, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.d.endGrant(Request{Command: CommandGrantRevoke, GrantID: g.ID}); err != nil {
		t.Fatal(err)
	}
	if err := record("worker1", "deploy"); err == nil || !strings.Contains(err.Error(), "still in force") {
		t.Fatalf("an effect after the grant was revoked = %v", err)
	}
	if _, err := check("worker1", store.ActionDeploy, 0); err == nil {
		t.Fatal("a revoked grant authorized an action")
	}
}

func TestADeliveryOwnersEffectIsCheckedAgainstTheJobsGrantsToo(t *testing.T) {
	f := newTwoResults(t)
	f.withWorkers(t)
	said := f.userSays(t, "you may deploy to staging, and you may not spend or change anything destructively")
	in := jobGrant(f.job.Number)
	in.Cites = said
	if _, err := f.d.recordGrant(f.grantRequest("coordination", in)); err != nil {
		t.Fatal(err)
	}
	dispatch := f.leaderWorks(t)
	_ = dispatch
	effect := func(kind string) error {
		_, err := f.d.effect(Request{Session: "credential", Command: CommandEffect, Kind: kind, Ref: "staging"})
		return err
	}
	check := func(action string) error {
		_, err := f.d.authorizeAction(Request{Session: "credential", Command: CommandAuthorize, Action: &store.ActionRequest{Job: f.job.Number, Action: action, What: "staging"}})
		return err
	}
	if err := effect("deploy"); err == nil {
		t.Fatal("a leader's deployment was recorded unchecked")
	}
	if err := check("deploy"); err != nil {
		t.Fatal(err)
	}
	if err := effect("deploy"); err != nil {
		t.Fatalf("the leader's checked deployment = %v", err)
	}
	// A grant of deployment is no grant of destructive changes.
	if err := check("destructive"); err == nil || !strings.Contains(err.Error(), "no grant covers destructive") {
		t.Fatalf("a destructive change under a deployment grant = %v", err)
	}
}

func TestAGrantGivenInAnAnswerIsRecordedByTheLeaderThatAskedAndCarriedToItsWorkers(t *testing.T) {
	f := newTwoResults(t)
	f.withWorkers(t)
	r, _, err := f.d.decisionRequest(Request{Session: "credential", Command: CommandDecisionRequest, Job: f.job.Number, Kind: store.RequestChoice, Questions: cacheQuestion})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.d.decisionPresent(Request{Session: "coordination", Command: CommandDecisionPresent, Request: r.Request, Version: 1}); err != nil {
		t.Fatal(err)
	}
	f.userSays(t, "in-memory, and go ahead and deploy to staging whenever you need")
	record, _, _, err := f.d.conversationAnswer(Request{Session: "coordination", Command: CommandDecisionRecord, Request: r.Request, Version: 1, LatestWitness: true, Choice: "in-memory", Reading: "in-memory, and a grant to deploy to staging"})
	if err != nil {
		t.Fatal(err)
	}

	in := jobGrant(f.job.Number)
	in.Answer = record.ID
	if _, err := f.d.recordGrant(f.grantRequest("coordination", in)); err == nil || !strings.Contains(err.Error(), "asked") {
		t.Fatalf("Coordination recording a grant that was given in an answer to Engineering = %v", err)
	}
	g, err := f.d.recordGrant(f.grantRequest("credential", in))
	if err != nil {
		t.Fatal(err)
	}
	if g.Leader != "leader@engineering" || g.Answer != record.ID || !strings.Contains(g.Words, "go ahead and deploy to staging") {
		t.Errorf("grant = %+v", g)
	}

	// Each assignment's worker carries it when it fetches the assignment, and
	// is covered by it without another decision.
	for i, a := range f.assignments {
		inbox, err := f.s.Inbox(a.Worker, 0, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		var carried bool
		for _, m := range inbox {
			if m.AssignmentData != nil && len(m.AssignmentData.Grants) == 1 && m.AssignmentData.Grants[0].ID == g.ID {
				carried = true
			}
		}
		if !carried {
			t.Errorf("assignment %d's worker does not carry the grant", i)
		}
	}
}

func TestRevokingAGrantStopsLaterActionsAndTellsTheOwningLeader(t *testing.T) {
	f := newTwoResults(t)
	f.withWorkers(t)
	said := f.userSays(t, "you may deploy to staging")
	in := jobGrant(f.job.Number)
	in.Cites = said
	g, err := f.d.recordGrant(f.grantRequest("coordination", in))
	if err != nil {
		t.Fatal(err)
	}
	deploy := actionCheck(t, f, "worker0")
	if _, err := deploy(); err != nil {
		t.Fatal(err)
	}

	// An agent cannot revoke a grant; the user does.
	if _, err := f.d.endGrant(Request{Session: "credential", Command: CommandGrantRevoke, GrantID: g.ID}); err == nil || !strings.Contains(err.Error(), "ask the user to run `asmai grant revoke 1`") {
		t.Fatalf("an agent revoking = %v", err)
	}
	ending, err := f.d.endGrant(Request{Command: CommandGrantRevoke, GrantID: g.ID})
	if err != nil {
		t.Fatal(err)
	}
	if ending.Grant.State != store.GrantRevoked || len(ending.Earlier) != 1 || len(ending.Affected) != 2 || len(ending.Dispatches) != 1 || ending.Dispatches[0].Agent != "leader@engineering" {
		t.Fatalf("ending = %+v", ending)
	}
	if _, err := deploy(); err == nil || !strings.Contains(err.Error(), "grant 1 was revoked") {
		t.Fatalf("deploying after the revocation = %v", err)
	}
	if _, err := f.d.endGrant(Request{Command: CommandGrantRevoke, GrantID: g.ID}); err == nil || !strings.Contains(err.Error(), "already revoked") {
		t.Fatalf("revoking twice = %v", err)
	}
	if _, err := f.d.endGrant(Request{Command: CommandGrantRevoke}); err == nil || !strings.Contains(err.Error(), "name the grant") {
		t.Fatalf("revoking nothing = %v", err)
	}
}

// actionCheck returns a function that has session check a deployment.
func actionCheck(t *testing.T, f twoResults, session string) func() (store.Authorization, error) {
	t.Helper()
	return func() (store.Authorization, error) {
		return f.d.authorizeAction(Request{Session: session, Command: CommandAuthorize, Action: &store.ActionRequest{Action: store.ActionDeploy, What: "staging"}})
	}
}

func TestALeaderReportsThatAConditionNoLongerHoldsAndAWorkerCannot(t *testing.T) {
	f := newTwoResults(t)
	f.withWorkers(t)
	said := f.userSays(t, "you may deploy to staging while the tests pass")
	in := jobGrant(f.job.Number)
	in.Cites = said
	g, err := f.d.recordGrant(f.grantRequest("coordination", in))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.d.endGrant(Request{Session: "worker0", Command: CommandGrantInvalidate, GrantID: g.ID, Why: "tests fail"}); err == nil || !strings.Contains(err.Error(), "only a role's leader") {
		t.Fatalf("a worker invalidating = %v", err)
	}
	if _, err := f.d.endGrant(Request{Session: "credential", Command: CommandGrantInvalidate, GrantID: g.ID}); err == nil || !strings.Contains(err.Error(), "for a reason") {
		t.Fatalf("invalidating with no reason = %v", err)
	}
	ending, err := f.d.endGrant(Request{Session: "credential", Command: CommandGrantInvalidate, GrantID: g.ID, Why: "the tests fail on the job branch"})
	if err != nil {
		t.Fatal(err)
	}
	if ending.Grant.State != store.GrantInvalidated || ending.Grant.EndedBy != "leader@engineering" || ending.Grant.EndedReason != "the tests fail on the job branch" {
		t.Errorf("grant = %+v", ending.Grant)
	}
	if _, err := actionCheck(t, f, "worker0")(); err == nil || !strings.Contains(err.Error(), "new decision") {
		t.Fatalf("deploying on an invalidated grant = %v", err)
	}
}
