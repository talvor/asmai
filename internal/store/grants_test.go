// SPDX-License-Identifier: Apache-2.0

package store

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var grantsAt = assignmentsAt.Add(time.Hour)

// spendGrant is a grant of ten dollars to the job's one spend.
func (c *conversation) spendGrant(cites int64) GrantInput {
	return GrantInput{Scope: ScopeJob, Job: c.job, Action: ActionSpend, Approved: "spend up to 10 USD on the staging database", Amount: 1000, Currency: "USD", Conditions: []string{"only for the staging database"}, Cites: cites}
}

// otherJob gives s another open job, of repository, or of none when it is empty.
func otherJob(t *testing.T, s *Store, repository string) int64 {
	t.Helper()
	var repo any
	if repository != "" {
		repo = repository
	}
	result, err := s.db.Exec(`INSERT INTO jobs(repository, state, reading) VALUES(?, ?, 'Another job')`, repo, JobOpen)
	if err != nil {
		t.Fatal(err)
	}
	job, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func TestAGrantIsRecordedAtItsScopeCitingTheUsersWords(t *testing.T) {
	c := newConversation(t)
	said := c.say(CoordinationLeader, "you may spend up to 10 dollars on the staging database for this job")

	g, err := c.s.GrantRecorded(c.spendGrant(said), CoordinationLeader, grantsAt)
	if err != nil {
		t.Fatal(err)
	}
	if g.ID == 0 || g.Scope != ScopeJob || g.Job != c.job || g.Action != ActionSpend || g.Amount != 1000 || g.Currency != "USD" || g.Leader != CoordinationLeader ||
		g.Cites != said || g.Words != "you may spend up to 10 dollars on the staging database for this job" || g.State != GrantActive || g.Spent != 0 {
		t.Errorf("grant = %+v", g)
	}
	if read, err := c.s.Grant(g.ID); err != nil || read.Words != g.Words || len(read.Conditions) != 1 {
		t.Errorf("read back = %+v, %v", read, err)
	}
	if entries := journaledKinds(t, c.s, KindGrantRecorded); len(entries) != 1 || !strings.Contains(string(entries[0].Data), `"job":1`) {
		t.Errorf("journal = %+v", entries)
	}
	text := g.Text("", "  ")
	for _, want := range []string{"grant 1 | job 1", "spend", "10.00 USD", "only for the staging database", "witnessed message"} {
		if !strings.Contains(text, want) {
			t.Errorf("the grant reads %q, missing %q", text, want)
		}
	}
}

func TestAGrantWithoutAValidCitationIsRefused(t *testing.T) {
	c := newConversation(t)
	said := c.say(CoordinationLeader, "ok, spend 10 dollars")
	toEngineering := c.say("leader@engineering", "ok, spend 10 dollars")
	nudge := c.nudge()

	for _, tt := range []struct {
		name   string
		in     func(*GrantInput)
		leader string
		want   string
	}{
		{"cites nothing", func(in *GrantInput) { in.Cites = 0 }, CoordinationLeader, "cites the witnessed message"},
		{"cites a message that does not exist", func(in *GrantInput) { in.Cites = 9999 }, CoordinationLeader, "does not exist"},
		{"cites a nudge", func(in *GrantInput) { in.Cites = nudge }, CoordinationLeader, "not a witnessed message"},
		{"cites a message typed to another agent", func(in *GrantInput) { in.Cites = toEngineering }, CoordinationLeader, "not in the conversation"},
		{"cites an entry that is not a message of the user's", func(in *GrantInput) { in.Cites = 1 }, CoordinationLeader, "not a witnessed message"},
		{"is recorded by another leader than Coordination", func(in *GrantInput) {}, owner, "only " + CoordinationLeader},
		{"cites both a message and an answer", func(in *GrantInput) { in.Answer = 1 }, CoordinationLeader, "either"},
	} {
		in := c.spendGrant(said)
		tt.in(&in)
		if _, err := c.s.GrantRecorded(in, tt.leader, grantsAt); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("a grant that %s = %v, want %q", tt.name, err, tt.want)
		}
	}
	if grants, err := c.s.Grants(); err != nil || len(grants) != 0 {
		t.Errorf("refused grants left %+v (%v)", grants, err)
	}
}

func TestAGrantOutsideTheJobsConversationIsRefusedAtJobScope(t *testing.T) {
	c := newConversation(t)
	early := c.say(CoordinationLeader, "spend 10 dollars")
	// The message came after the job's request; one before it does not belong
	// to the job, but is good for a grant at the factory's scope.
	if _, err := c.s.db.Exec(`UPDATE jobs SET witness = ? WHERE number = ?`, early+1, c.job); err != nil {
		t.Fatal(err)
	}
	if _, err := c.s.GrantRecorded(c.spendGrant(early), CoordinationLeader, grantsAt); err == nil || !strings.Contains(err.Error(), "came before job") {
		t.Fatalf("a message from before the job's request = %v", err)
	}
	factory := c.spendGrant(early)
	factory.Scope, factory.Job = ScopeFactory, 0
	if _, err := c.s.GrantRecorded(factory, CoordinationLeader, grantsAt); err != nil {
		t.Fatalf("the same words at the factory's scope = %v", err)
	}
}

func TestAGrantGivenInAnAnswerIsRecordedByTheLeaderThatAsked(t *testing.T) {
	c := newConversation(t)
	shown := c.present(c.request)
	said := c.say(CoordinationLeader, "yes, and you may spend up to 10 dollars on staging for this job")
	answer, _, _, err := c.answer(shown, said)
	if err != nil {
		t.Fatal(err)
	}

	in := c.spendGrant(0)
	in.Answer = answer.ID
	if _, err := c.s.GrantRecorded(in, CoordinationLeader, grantsAt); err == nil || !strings.Contains(err.Error(), "asked") {
		t.Errorf("Coordination recording a grant given in someone else's answer = %v", err)
	}
	if _, err := c.s.GrantRecorded(in, "leader@quality", grantsAt); err == nil || !strings.Contains(err.Error(), "asked") {
		t.Errorf("another leader recording it = %v", err)
	}
	in.Answer = 9999
	if _, err := c.s.GrantRecorded(in, owner, grantsAt); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("an answer that does not exist = %v", err)
	}
	in.Answer = answer.ID
	g, err := c.s.GrantRecorded(in, owner, grantsAt)
	if err != nil {
		t.Fatal(err)
	}
	if g.Leader != owner || g.Answer != answer.ID || g.Cites != said || g.Words != "yes, and you may spend up to 10 dollars on staging for this job" {
		t.Errorf("grant = %+v", g)
	}

	// A delegated decision is not the user's words.
	delegated, err := c.s.DecisionRecorded(DecisionRecord{Job: c.job, Leader: owner, Choice: "use redis", ReliedOn: "the mandate", Rationale: "it is there", Evidence: "it runs"}, 1, grantsAt)
	if err != nil {
		t.Fatal(err)
	}
	in.Answer = delegated.ID
	if _, err := c.s.GrantRecorded(in, owner, grantsAt); err == nil || !strings.Contains(err.Error(), "answer") {
		t.Errorf("a delegated decision cited as the user's answer = %v", err)
	}
}

func TestAGrantNamesItsActionItsScopeAndItsAmount(t *testing.T) {
	c := newConversation(t)
	said := c.say(CoordinationLeader, "go ahead")
	for _, tt := range []struct {
		name string
		in   func(*GrantInput)
		want string
	}{
		{"a merge", func(in *GrantInput) { in.Action, in.Amount, in.Currency = "merge", 0, "" }, "no grant can authorize a merge"},
		{"an action nobody knows", func(in *GrantInput) { in.Action = "teleport" }, "action"},
		{"spending without an amount", func(in *GrantInput) { in.Amount = 0 }, "amount"},
		{"spending without a currency", func(in *GrantInput) { in.Currency = "" }, "currency"},
		{"spending a negative amount", func(in *GrantInput) { in.Amount = -5 }, "amount"},
		{"an amount that is not spending", func(in *GrantInput) { in.Action = ActionDeploy }, "only spending has an amount"},
		{"no scope", func(in *GrantInput) { in.Scope = "" }, "scope"},
		{"a scope nobody knows", func(in *GrantInput) { in.Scope = "universe" }, "scope"},
		{"job scope with no job", func(in *GrantInput) { in.Job = 0 }, "names its job"},
		{"job scope naming a job that does not exist", func(in *GrantInput) { in.Job = 99 }, "does not exist"},
		{"repository scope with no repository", func(in *GrantInput) { in.Scope, in.Job = ScopeRepository, 0 }, "names its repository"},
		{"repository scope naming an unregistered repository", func(in *GrantInput) { in.Scope, in.Job, in.Repository = ScopeRepository, 0, "nowhere" }, "not registered"},
		{"factory scope naming a job", func(in *GrantInput) { in.Scope, in.Repository = ScopeFactory, "" }, "factory"},
		{"no approved action", func(in *GrantInput) { in.Approved = " " }, "approved"},
		{"a blank condition", func(in *GrantInput) { in.Conditions = []string{" "} }, "condition"},
	} {
		in := c.spendGrant(said)
		tt.in(&in)
		if _, err := c.s.GrantRecorded(in, CoordinationLeader, grantsAt); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s = %v, want %q", tt.name, err, tt.want)
		}
	}
	if grants, err := c.s.Grants(); err != nil || len(grants) != 0 {
		t.Errorf("refused grants left %+v (%v)", grants, err)
	}
}

// authorize asks to spend amount on job, as a leader in no dispatch.
func (c *conversation) authorize(job int64, action string, amount int64) (Authorization, error) {
	currency := ""
	if action == ActionSpend {
		currency = "USD"
	}
	return c.s.ActionAuthorized(ActionRequest{Job: job, Agent: owner, Action: action, What: "something", Amount: amount, Currency: currency}, grantsAt.Add(time.Minute))
}

func TestSpendingIsZeroUnlessAGrantNamesTheAmountAndScope(t *testing.T) {
	c := newConversation(t)
	if _, err := c.authorize(c.job, ActionSpend, 100); err == nil || !strings.Contains(err.Error(), "no grant covers") || !strings.Contains(err.Error(), "spending budget is zero") {
		t.Fatalf("spending with no grant = %v", err)
	}
	said := c.say(CoordinationLeader, "you may spend up to 10 dollars")
	g, err := c.s.GrantRecorded(c.spendGrant(said), CoordinationLeader, grantsAt)
	if err != nil {
		t.Fatal(err)
	}
	first, err := c.authorize(c.job, ActionSpend, 400)
	if err != nil || first.Grant != g.ID || first.Amount != 400 || first.Job != c.job {
		t.Fatalf("authorizing 4 of 10 = %+v, %v", first, err)
	}
	if _, err := c.authorize(c.job, ActionSpend, 400); err != nil {
		t.Fatalf("authorizing 4 more = %v", err)
	}
	if _, err := c.authorize(c.job, ActionSpend, 400); err == nil || !strings.Contains(err.Error(), "leaves 2.00 USD of 10.00 USD") {
		t.Fatalf("authorizing past the amount = %v", err)
	}
	if _, err := c.s.ActionAuthorized(ActionRequest{Job: c.job, Agent: owner, Action: ActionSpend, What: "x", Amount: 100, Currency: "EUR"}, grantsAt); err == nil || !strings.Contains(err.Error(), "USD") {
		t.Fatalf("authorizing another currency = %v", err)
	}
	if _, err := c.authorize(c.job, ActionSpend, 0); err == nil || !strings.Contains(err.Error(), "amount") {
		t.Fatalf("authorizing to spend nothing = %v", err)
	}
	if read, _ := c.s.Grant(g.ID); read.Spent != 800 {
		t.Errorf("spent = %d, want 800", read.Spent)
	}
	// Spending is the only action that has an amount.
	if _, err := c.authorize(c.job, ActionDeploy, 100); err == nil || !strings.Contains(err.Error(), "only spending has an amount") {
		t.Fatalf("a deployment with an amount = %v", err)
	}
	if n := len(journaledKinds(t, c.s, KindActionAuthorized)); n != 2 {
		t.Errorf("%d authorizations journaled, want 2", n)
	}
}

func TestNoGrantCanAuthorizeAMerge(t *testing.T) {
	c := newConversation(t)
	said := c.say(CoordinationLeader, "merge it whenever it is green")
	for _, action := range []string{"merge", "Merge"} {
		in := c.spendGrant(said)
		in.Action, in.Amount, in.Currency, in.Approved = action, 0, "", "merge the pull request"
		if _, err := c.s.GrantRecorded(in, CoordinationLeader, grantsAt); err == nil || !strings.Contains(err.Error(), "AsmAI never merges") {
			t.Errorf("a grant of %q = %v", action, err)
		}
	}
	// Even a grant of everything else leaves a merge refused.
	for _, action := range []string{ActionDeploy, ActionDestructive, ActionDecision} {
		in := c.spendGrant(said)
		in.Action, in.Amount, in.Currency, in.Approved = action, 0, "", "everything"
		if _, err := c.s.GrantRecorded(in, CoordinationLeader, grantsAt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.s.ActionAuthorized(ActionRequest{Job: c.job, Agent: owner, Action: "merge", What: "pull request 3"}, grantsAt); err == nil || !strings.Contains(err.Error(), "AsmAI never merges") {
		t.Errorf("authorizing a merge = %v", err)
	}
}

func TestAJobsApprovalStaysWithThatJobUnlessGrantedAtAWiderScope(t *testing.T) {
	c := newConversation(t)
	sameRepository := otherJob(t, c.s, "fixture")
	elsewhere := otherJob(t, c.s, "")
	if err := c.s.RepositoryAdded(Repository{Name: "second", Origin: "/o2", DefaultBranch: "main", Clone: "/c2", AddedAt: assignmentsAt}); err != nil {
		t.Fatal(err)
	}
	otherRepository := otherJob(t, c.s, "second")
	said := c.say(CoordinationLeader, "deploying is fine")
	grant := func(scope, repository string, job int64) Grant {
		t.Helper()
		g, err := c.s.GrantRecorded(GrantInput{Scope: scope, Job: job, Repository: repository, Action: ActionDeploy, Approved: "deploy to staging", Cites: said}, CoordinationLeader, grantsAt)
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	covered := func(job int64) bool {
		_, err := c.authorize(job, ActionDeploy, 0)
		return err == nil
	}

	g := grant(ScopeJob, "", c.job)
	if !covered(c.job) || covered(sameRepository) || covered(elsewhere) || covered(otherRepository) {
		t.Fatal("a grant at a job's scope covers that job only")
	}
	// A changed scope needs a new decision, which the refusal says.
	if _, err := c.authorize(sameRepository, ActionDeploy, 0); err == nil || !strings.Contains(err.Error(), "grant 1 is of job 1, not job 2") || !strings.Contains(err.Error(), "new decision") {
		t.Errorf("the refusal reads %v", err)
	}
	if err := ended(c, g.ID); err != nil {
		t.Fatal(err)
	}

	grant(ScopeRepository, "fixture", 0)
	if !covered(c.job) || !covered(sameRepository) || covered(elsewhere) || covered(otherRepository) {
		t.Fatal("a grant at a repository's scope covers that repository's jobs only")
	}
	grant(ScopeFactory, "", 0)
	if !covered(elsewhere) || !covered(otherRepository) {
		t.Fatal("a grant at the factory's scope covers every job")
	}
}

// ended revokes grant id, as the user does.
func ended(c *conversation, id int64) error {
	_, err := c.s.GrantEnded(id, GrantRevoked, "user", "", grantsAt.Add(time.Hour))
	return err
}

func TestAGrantIsReusedThroughAssignmentsHandoffsAndARestartWhileItsConditionsHold(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	s := open(t, path)
	c := &conversation{t: t, s: s, job: jobWithBranch(t, s)}
	opening := c.say(CoordinationLeader, "deploy the thing")
	if _, err := s.db.Exec(`UPDATE jobs SET witness = ? WHERE number = ?`, opening, c.job); err != nil {
		t.Fatal(err)
	}
	said := c.say(CoordinationLeader, "you may deploy to staging while the tests pass")
	in := GrantInput{Scope: ScopeJob, Job: c.job, Action: ActionDeploy, Approved: "deploy to staging", Conditions: []string{"the tests pass"}, Cites: said}
	g, err := s.GrantRecorded(in, CoordinationLeader, grantsAt)
	if err != nil {
		t.Fatal(err)
	}

	// An assignment and a handoff carry the grants in force, as they stand
	// when the receiver fetches them.
	_, d := newAssignment(t, s, c.job)
	h, hd, err := s.SendHandoff(Handoff{Job: c.job, Sender: "leader@coordination", Receiver: "leader@quality", Outcome: "validate", Decisions: "none", Evidence: "none", Constraints: "none", Permissions: "deploy to staging", Criteria: []string{"done"}}, grantsAt)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		agent    string
		dispatch int64
		get      func(Message) []Grant
	}{
		{"worker1@engineering", d.ID, func(m Message) []Grant { return m.AssignmentData.Grants }},
		{"leader@quality", hd.ID, func(m Message) []Grant { return m.HandoffData.Grants }},
	} {
		inbox, err := s.Inbox(tt.agent, tt.dispatch, grantsAt)
		if err != nil || len(inbox) != 1 {
			t.Fatalf("%s's inbox = %+v, %v", tt.agent, inbox, err)
		}
		if grants := tt.get(inbox[0]); len(grants) != 1 || grants[0].ID != g.ID {
			t.Errorf("%s carries grants %+v, want grant %d", tt.agent, grants, g.ID)
		}
	}
	_ = h

	// A restart alone changes nothing: the store holds the grant, and the
	// action it covers is authorized again.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = open(t, path)
	if grants, err := s.GrantsInForce(c.job); err != nil || len(grants) != 1 || grants[0].ID != g.ID {
		t.Fatalf("after a restart the grants in force are %+v, %v", grants, err)
	}
	if _, err := s.ActionAuthorized(ActionRequest{Job: c.job, Agent: owner, Action: ActionDeploy, What: "deploy build 7"}, grantsAt); err != nil {
		t.Fatalf("authorizing after a restart = %v", err)
	}

	// An invalidated condition needs a new decision, which a handoff or a
	// restart alone never does.
	ending, err := s.GrantEnded(g.ID, GrantInvalidated, owner, "the tests no longer pass", grantsAt)
	if err != nil {
		t.Fatal(err)
	}
	if ending.Grant.State != GrantInvalidated || ending.Grant.EndedBy != owner || ending.Grant.EndedReason != "the tests no longer pass" {
		t.Errorf("ending = %+v", ending)
	}
	if _, err := s.ActionAuthorized(ActionRequest{Job: c.job, Agent: owner, Action: ActionDeploy, What: "deploy build 8"}, grantsAt); err == nil || !strings.Contains(err.Error(), "invalidated") || !strings.Contains(err.Error(), "the tests no longer pass") || !strings.Contains(err.Error(), "new decision") {
		t.Fatalf("authorizing on an invalidated grant = %v", err)
	}
	fresh, err := s.GrantRecorded(in, CoordinationLeader, grantsAt)
	if err != nil {
		t.Fatal(err)
	}
	if a, err := s.ActionAuthorized(ActionRequest{Job: c.job, Agent: owner, Action: ActionDeploy, What: "deploy build 8"}, grantsAt); err != nil || a.Grant != fresh.ID {
		t.Fatalf("authorizing on the new decision = %+v, %v", a, err)
	}
}

func TestAGrantIsNotUsedOnceTheJobIsNotOpen(t *testing.T) {
	c := newConversation(t)
	said := c.say(CoordinationLeader, "deploy it")
	if _, err := c.s.GrantRecorded(GrantInput{Scope: ScopeJob, Job: c.job, Action: ActionDeploy, Approved: "deploy", Cites: said}, CoordinationLeader, grantsAt); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{JobPaused, "cancelled", "ended"} {
		if _, err := c.s.db.Exec(`UPDATE jobs SET state = ? WHERE number = ?`, state, c.job); err != nil {
			t.Fatal(err)
		}
		if _, err := c.authorize(c.job, ActionDeploy, 0); err == nil || !strings.Contains(err.Error(), "is "+state) {
			t.Errorf("authorizing in a %s job = %v", state, err)
		}
	}
}

func TestRevokingAGrantStopsLaterActionsAndReportsEarlierOnesAsTheyStand(t *testing.T) {
	c := newConversation(t)
	said := c.say(CoordinationLeader, "you may spend up to 10 dollars")
	g, err := c.s.GrantRecorded(c.spendGrant(said), CoordinationLeader, grantsAt)
	if err != nil {
		t.Fatal(err)
	}
	// One assignment is in flight; another has ended.
	live, _ := newAssignment(t, c.s, c.job)
	earlier, err := c.authorize(c.job, ActionSpend, 400)
	if err != nil {
		t.Fatal(err)
	}

	ending, err := c.s.GrantEnded(g.ID, GrantRevoked, "user", "", grantsAt.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if ending.Grant.State != GrantRevoked || ending.Grant.EndedBy != "user" {
		t.Errorf("grant = %+v", ending.Grant)
	}
	// What the grant already authorized stands, and is reported, never undone.
	if len(ending.Earlier) != 1 || ending.Earlier[0].ID != earlier.ID || ending.Earlier[0].Amount != 400 {
		t.Errorf("earlier = %+v", ending.Earlier)
	}
	// The owning leader of the assignment in flight is told.
	if len(ending.Affected) != 1 || ending.Affected[0].ID != live.ID || len(ending.Dispatches) != 1 || ending.Dispatches[0].Agent != owner {
		t.Fatalf("affected = %+v, dispatches = %+v", ending.Affected, ending.Dispatches)
	}
	inbox, err := c.s.Inbox(owner, ending.Dispatches[0].ID, grantsAt)
	if err != nil || len(inbox) != 1 {
		t.Fatalf("inbox = %+v, %v", inbox, err)
	}
	for _, want := range []string{"Grant 1", "revoked", "Assignment 1", "nothing is undone", "fresh decision"} {
		if !strings.Contains(inbox[0].Body, want) {
			t.Errorf("the leader's message lacks %q:\n%s", want, inbox[0].Body)
		}
	}

	// Later actions are refused, the grant is no longer in force, and the
	// revocation is journaled and cannot be repeated.
	if _, err := c.authorize(c.job, ActionSpend, 100); err == nil || !strings.Contains(err.Error(), "grant 1 was revoked") {
		t.Errorf("authorizing after the revocation = %v", err)
	}
	if grants, err := c.s.GrantsInForce(c.job); err != nil || len(grants) != 0 {
		t.Errorf("in force = %+v, %v", grants, err)
	}
	if all, err := c.s.Grants(); err != nil || len(all) != 1 || all[0].State != GrantRevoked || all[0].Spent != 400 {
		t.Errorf("all = %+v, %v", all, err)
	}
	if n := len(journaledKinds(t, c.s, KindGrantRevoked)); n != 1 {
		t.Errorf("%d revocations journaled, want 1", n)
	}
	if _, err := c.s.GrantEnded(g.ID, GrantRevoked, "user", "", grantsAt); err == nil || !strings.Contains(err.Error(), "already revoked") {
		t.Errorf("revoking twice = %v", err)
	}
	if _, err := c.s.GrantEnded(99, GrantRevoked, "user", "", grantsAt); err == nil || !errors.Is(err, ErrNoGrant) {
		t.Errorf("revoking a grant that does not exist = %v", err)
	}
}

func TestAmountsAreWrittenAndReadToTwoDecimalPlaces(t *testing.T) {
	for _, tt := range []struct {
		text string
		want int64
	}{{"10", 1000}, {"12.5", 1250}, {"12.50", 1250}, {"0.05", 5}, {"0", 0}} {
		if got, err := ParseAmount(tt.text); err != nil || got != tt.want {
			t.Errorf("ParseAmount(%q) = %d, %v, want %d", tt.text, got, err, tt.want)
		}
	}
	for _, text := range []string{"", "-1", "1.234", "ten", "1,000", "1e3", ".5", "+4"} {
		if got, err := ParseAmount(text); err == nil {
			t.Errorf("ParseAmount(%q) = %d, want a refusal", text, got)
		}
	}
	if got := FormatAmount(1205, "USD"); got != "12.05 USD" {
		t.Errorf("FormatAmount = %q", got)
	}
}
