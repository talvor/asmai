// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/config"
	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/skills"
	"github.com/talvor/asmai/internal/store"
)

// messagesOfKind returns the bodies of the journaled messages of kind that
// the daemon sent, as the journal holds them.
func messagesOfKind(t *testing.T, lines []string, kind string) []string {
	t.Helper()
	var got []string
	for _, line := range lines {
		if strings.HasPrefix(line, "message.created ") && strings.Contains(line, `"kind":"`+kind+`"`) {
			got = append(got, line)
		}
	}
	return got
}

func TestARepositoryRegisteredWithoutItsIssueTrackerFileOpensASetupJobAtOnce(t *testing.T) {
	p, _ := repoDaemon(t)
	origin := remoteWithoutSetup(t)

	resp, err := addRepo(t, p, origin, "")
	if err != nil {
		t.Fatal(err)
	}
	j := resp.Job
	if j == nil || j.Number != 1 || j.Repository != "fixture" || j.Mandate != store.MandateSetup || j.State != store.JobOpen || j.Role != "coordination" {
		t.Fatalf("registering a repository with no issue tracker file returned the job %+v", j)
	}
	if !strings.Contains(j.Reading, "docs/agents/issue-tracker.md") || len(j.Criteria) == 0 {
		t.Errorf("the setup job's reading is %q with criteria %v; want it to say why the job is open", j.Reading, j.Criteria)
	}

	jobs, err := Call(p.Socket, Request{Command: CommandJobs})
	if err != nil || len(jobs.Jobs) != 1 || jobs.Jobs[0].Number != j.Number || jobs.Jobs[0].Mandate != store.MandateSetup {
		t.Fatalf("the daemon holds the jobs %+v, %v; want the setup job", jobs.Jobs, err)
	}

	// Its job branch is made as the job opens, so that its brief points to
	// the code from the start.
	branch := jobBranchFor(*j)
	if got := git(t, resp.Repository.Clone, "rev-parse", "--verify", "refs/heads/"+branch); got != git(t, origin, "rev-parse", "trunk") {
		t.Errorf("the setup job's branch %s is at %s, want origin's trunk", branch, got)
	}

	// Coordination's leader is told in a message of its own.
	lines := journal(t, p)
	told := messagesOfKind(t, lines, store.MessageSetup)
	if len(told) != 1 || !strings.Contains(told[0], `"recipient":"leader@coordination"`) || !strings.Contains(told[0], `"job":1`) {
		t.Errorf("the journal holds %v, want one setup message to leader@coordination about job 1", told)
	}
}

func TestARepositoryRegisteredWithItsIssueTrackerFileOpensNothing(t *testing.T) {
	p, _ := repoDaemon(t)
	origin := remote(t)

	resp, err := addRepo(t, p, origin, "")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Job != nil {
		t.Errorf("registering a repository that has its issue tracker file opened %+v", resp.Job)
	}
	if jobs, err := Call(p.Socket, Request{Command: CommandJobs}); err != nil || len(jobs.Jobs) != 0 {
		t.Errorf("the daemon holds the jobs %+v, %v; want none", jobs.Jobs, err)
	}
	if told := messagesOfKind(t, journal(t, p), store.MessageSetup); len(told) != 0 {
		t.Errorf("Coordination was told %v", told)
	}
}

func TestTheIssueTrackerFileAloneDecidesWhetherARepositoryNeedsSetup(t *testing.T) {
	// Its other setup files, or an agent instructions block, do not stand in
	// for it (08 rule 28).
	p, _ := repoDaemon(t)
	origin := remoteWith(t, map[string]string{
		"docs/agents/triage-labels.md": "labels",
		"docs/agents/domain.md":        "domain",
		"AGENTS.md":                    "## Agent skills\n",
	})
	resp, err := addRepo(t, p, origin, "")
	if err != nil || resp.Job == nil || resp.Job.Mandate != store.MandateSetup {
		t.Fatalf("a repository with some of its setup files returned %+v, %v; want a setup job", resp.Job, err)
	}
}

func TestARegistrationThatFailsOpensNoSetupJob(t *testing.T) {
	p, _ := repoDaemon(t)
	if _, err := addRepo(t, p, filepath.Join(t.TempDir(), "missing.git"), "nope"); err == nil {
		t.Fatal("registering a repository that does not exist succeeded")
	}
	if jobs, err := Call(p.Socket, Request{Command: CommandJobs}); err != nil || len(jobs.Jobs) != 0 {
		t.Errorf("a failed registration left the jobs %+v, %v", jobs.Jobs, err)
	}
	if entries, _ := os.ReadDir(p.Repositories); len(entries) != 0 {
		t.Errorf("the repositories directory holds %v", entries)
	}
}

func TestCoordinationsLeaderGivesASetupJobsWorkToItsOwnWorkerWhoRunsTheSetupSkill(t *testing.T) {
	p := newJobFactory(t, store.MandateSetup, true)
	if err := p.d.authorize(Request{Session: "coordination", Command: CommandAssign}); err != nil {
		t.Fatalf("Coordination's leader may not assign: %v", err)
	}
	a, dispatch, err := p.d.assign(Request{Session: "coordination", Command: CommandAssign, Job: p.job.Number, Outcome: "Set fixture up for the skills", Criteria: []string{"the setup files are on the branch"}, Skill: "setup-matt-pocock-skills"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Owner != "leader@coordination" || a.Worker != "worker1@coordination" || a.ReadOnly || a.Scratch || a.Skill != "setup-matt-pocock-skills" || dispatch.Agent != "worker1@coordination" {
		t.Errorf("the assignment is %+v, dispatch %+v, want a writing assignment of leader@coordination for worker1@coordination", a, dispatch)
	}
	if want := "asmai/test/job-1/" + string(rune('0'+a.ID)); a.Workspace.Branch != want || git(t, a.Workspace.Path, "rev-parse", "--abbrev-ref", "HEAD") != want {
		t.Errorf("the worker works on %q, want %q", a.Workspace.Branch, want)
	}
	args := waitForFile(t, filepath.Join(p.out, "args"))
	for _, want := range []string{"a Coordination worker", "--model\nsonnet", "--plugin-dir", "dontAsk"} {
		if !strings.Contains(args, want) {
			t.Errorf("the Coordination worker's command line lacks %q: %s", want, args)
		}
	}
	plugin := filepath.Join(p.d.cfg.Paths.Plugins, "worker1@coordination", skills.Dir)
	if _, err := os.Stat(filepath.Join(plugin, "setup-matt-pocock-skills", "SKILL.md")); err != nil {
		t.Errorf("Coordination's worker is not given its skill: %v", err)
	}
	if _, err := os.Stat(filepath.Join(plugin, "implement")); err == nil {
		t.Error("Coordination's worker is given Engineering's skill implement")
	}
	// Nobody answers a native prompt in a worker's terminal, even Coordination's.
	if got := p.d.waitingPrompts(); len(got) != 0 {
		t.Errorf("waiting prompts: %+v", got)
	}
}

func TestOnlyCoordinationsLeaderGivesASetupJobsWorkAndOnlyForASetupJob(t *testing.T) {
	setup := newJobFactory(t, store.MandateSetup, true)
	for _, session := range []string{"planning", "research", "engineering"} {
		if _, _, err := setup.d.assign(Request{Session: session, Command: CommandAssign, Job: setup.job.Number, Outcome: "Set it up", Criteria: []string{"done"}}); err == nil || !strings.Contains(err.Error(), "authorizes only its setup outcome: only leader@coordination may give its writing work") {
			t.Errorf("the %s leader assigning a setup job's work returned %v, want it refused", session, err)
		}
	}
	if assignments, err := setup.s.AssignmentsForJob(setup.job.Number); err != nil || len(assignments) != 0 {
		t.Errorf("a refused assignment was recorded: %+v (%v)", assignments, err)
	}
	tested := newJobFactory(t, store.MandateTestedPR, true)
	if _, _, err := tested.d.assign(Request{Session: "coordination", Command: CommandAssign, Job: tested.job.Number, Outcome: "Build it", Criteria: []string{"built"}}); err == nil || !strings.Contains(err.Error(), "gives writing work only for the setup of a repository") {
		t.Errorf("Coordination's leader assigning a tested-PR job's work returned %v, want it refused", err)
	}
}

func TestASetupJobIsNeverHandedToAnotherRole(t *testing.T) {
	job := store.Job{Number: 4, Repository: "fixture", Mandate: store.MandateSetup}
	for _, receiver := range []string{roles.Planning, roles.Research, roles.Engineering} {
		if err := checkHandoff(roles.Coordination, receiver, job); err == nil || !strings.Contains(err.Error(), "setup of repository fixture") {
			t.Errorf("a handoff of a setup job to %s returned %v, want it refused", receiver, err)
		}
	}
}

// setupAnswered records answers of the user to the setup questions of job's
// repository, as their Lavish answer to a request of Coordination's leader.
func setupAnswered(t *testing.T, s *store.Store, job store.Job, answers ...store.QuestionAnswer) {
	t.Helper()
	questions := make([]store.Question, len(answers))
	for i, a := range answers {
		questions[i] = store.Question{ID: a.Question, Title: "Question " + a.Question}
	}
	at := time.Now()
	r, _, err := s.DecisionRequested(store.DecisionRequestInput{Job: job.Number, Requester: store.CoordinationLeader, Recipient: store.ToUser, Kind: store.RequestQuestions, Questions: questions}, at)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.LavishPageOpened(r.ID, "/pages/setup.html", "http://127.0.0.1:1/session/1", at); err != nil {
		t.Fatal(err)
	}
	var prompts []json.RawMessage
	for _, a := range answers {
		prompt, err := json.Marshal(answer(r, a.Question, a.Choice, a.Notes))
		if err != nil {
			t.Fatal(err)
		}
		prompts = append(prompts, prompt)
	}
	if _, err := s.LavishCollected(r.ID, prompts, at); err != nil {
		t.Fatal(err)
	}
}

// secondJob opens a job of the fixture repository with mandate, which the
// user asked for after its setup job was opened.
func secondJob(t *testing.T, p planningJob, mandate string) store.Job {
	t.Helper()
	at := time.Now()
	if err := p.s.ConversationEntered("leader@coordination", roles.Coordination, 1, "/dev/pts/test", at); err != nil {
		t.Fatal(err)
	}
	_, witness, err := p.s.ObservedWitnessed(store.Witnessed{Agent: "leader@coordination", Role: roles.Coordination, Generation: 1, Terminal: "/dev/pts/test", Text: "add a greeting"}, "UserPromptSubmit", json.RawMessage(`{"prompt":"add a greeting"}`), at)
	if err != nil {
		t.Fatal(err)
	}
	job, err := p.s.JobOpened(store.Job{Repository: "fixture", Witness: witness, Reading: "Add a greeting", Mandate: mandate, Criteria: []string{"done"}}, false, at)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

// engineeringWorkerArgs gives job an assignment from Engineering's leader and
// returns the command line the worker it starts is given.
func engineeringWorkerArgs(t *testing.T, p planningJob, job store.Job) string {
	t.Helper()
	if _, _, err := p.d.assign(Request{Session: "engineering", Command: CommandAssign, Job: job.Number, Outcome: "Add a greeting", Criteria: []string{"it greets"}}); err != nil {
		t.Fatal(err)
	}
	return waitForFile(t, filepath.Join(p.out, "args"))
}

func TestAJobThatStartsBeforeTheSetupPullRequestMergesIsGivenTheUsersSetupAnswersAsRepositoryInstructions(t *testing.T) {
	// The repository has no issue tracker file yet: its setup pull request
	// has not merged.
	p := newJobFactory(t, store.MandateSetup, true)
	setupAnswered(t, p.s, p.job,
		store.QuestionAnswer{Question: "tracker", Choice: "github"},
		store.QuestionAnswer{Question: "ci", Choice: "no", Notes: "only a nightly script"},
	)
	args := engineeringWorkerArgs(t, p, secondJob(t, p, store.MandateTestedPR))
	for _, want := range []string{"The user's answers to the setup of fixture", "Question tracker (tracker): github", "Question ci (ci): no (only a nightly script)", "the repository's instructions"} {
		if !strings.Contains(args, want) {
			t.Errorf("an engineering worker started before the setup merged lacks %q in %s", want, args)
		}
	}
}

func TestAJobThatStartsBeforeAnyAnswerIsGivenNoSetupAnswers(t *testing.T) {
	p := newJobFactory(t, store.MandateSetup, true)
	if args := engineeringWorkerArgs(t, p, secondJob(t, p, store.MandateTestedPR)); strings.Contains(args, "answers to the setup") {
		t.Errorf("a worker is given setup answers the user has not given: %s", args)
	}
}

func TestAJobThatStartsAfterTheSetupPullRequestMergedIsGivenNoSetupAnswersForTheRepositoryHasItsFiles(t *testing.T) {
	// The repository has its issue tracker file: the setup pull request
	// merged, and its files carry what the answers said.
	p := newJobFactoryFrom(t, store.MandateSetup, true, remote(t))
	setupAnswered(t, p.s, p.job, store.QuestionAnswer{Question: "tracker", Choice: "github"})
	if args := engineeringWorkerArgs(t, p, secondJob(t, p, store.MandateTestedPR)); strings.Contains(args, "answers to the setup") {
		t.Errorf("a worker started after the setup merged is given the answers: %s", args)
	}
}

func TestTheBriefOfAJobInARepositoryListsTheUsersSetupAnswersAndWhatTheirCIAnswerAsksOfThem(t *testing.T) {
	p := newJobFactory(t, store.MandateSetup, true)
	other := secondJob(t, p, store.MandateTestedPR)
	brief := func(job store.Job) string {
		t.Helper()
		resp := callWith(t, p.d.jobCommand, Request{Command: CommandBrief, Job: job.Number})
		if resp.Error != "" {
			t.Fatal(resp.Error)
		}
		return resp.Brief
	}

	// Before any answer the setup job's brief says so and asks nothing of the
	// user, and another job of the repository has no section.
	if got := brief(p.job); !strings.Contains(got, "Repository setup answers") || !strings.Contains(got, "- none") || !strings.Contains(got, "has not answered whether fixture has CI") {
		t.Errorf("the setup job's brief before any answer is %s", got)
	}
	if got := brief(other); strings.Contains(got, "Repository setup answers") {
		t.Errorf("a job's brief lists setup answers there are none of: %s", got)
	}
	if got := brief(p.job); !strings.Contains(got, "Registration the user ran (journal entry): ") || strings.Contains(got, "Witnessed message") {
		t.Errorf("the setup job's brief says it cites a message: %s", got)
	}

	// The user says the repository has no CI: every job of the repository
	// reads their answers, and the CI answer is a change for the user to make
	// in the configuration, which only they change.
	setupAnswered(t, p.s, p.job,
		store.QuestionAnswer{Question: "tracker", Choice: "github"},
		store.QuestionAnswer{Question: "ci", Choice: "no"},
	)
	for _, job := range []store.Job{p.job, other} {
		got := brief(job)
		for _, want := range []string{
			"Repository setup answers", "Question tracker (tracker): github", "Question ci (ci): no",
			"has no CI, and the configuration does not declare it yet",
			"set `no_ci = true` in the [repositories.fixture] table of " + p.d.cfg.ConfigFile,
			"`asmai config apply`", "only the user changes it",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("the brief of job %d lacks %q: %s", job.Number, want, got)
			}
		}
	}

	// Once the user has declared it, there is nothing left to give them.
	p.d.configMu.Lock()
	p.d.applied.Options = map[string]config.RepositoryOptions{"fixture": {NoCI: true}}
	p.d.configMu.Unlock()
	if got := brief(p.job); !strings.Contains(got, "the configuration already declares it (no_ci = true)") || strings.Contains(got, "give them this change to make") {
		t.Errorf("the brief after the user declared it is %s", got)
	}
}

func TestTheUserWhoSaysTheRepositoryHasCIHasNothingToDeclare(t *testing.T) {
	p := newJobFactory(t, store.MandateSetup, true)
	setupAnswered(t, p.s, p.job, store.QuestionAnswer{Question: "ci", Choice: "yes"})
	resp := callWith(t, p.d.jobCommand, Request{Command: CommandBrief, Job: p.job.Number})
	if !strings.Contains(resp.Brief, "has CI, so there is nothing to declare") || strings.Contains(resp.Brief, "no_ci") {
		t.Errorf("the brief of a repository with CI is %s", resp.Brief)
	}
}

var setupQuestions = []store.Question{
	{ID: "tracker", Title: "Where do issues live?", Body: "The git remote points to GitHub.", Options: []string{"github", "gitlab", "local", "other"}, Recommendation: "github"},
	{ID: "labels", Title: "Keep the default triage labels?", Options: []string{"keep the defaults", "use other labels"}, Recommendation: "keep the defaults"},
	{ID: "ci", Title: "Does the repository have CI?", Options: []string{"yes", "no"}, Recommendation: "yes"},
}

// A setup worker's questions reach the user in Lavish through Coordination's
// leader, which owns the worker and carries them itself; the user's answers
// are recorded for the repository the moment they send them, and only their
// own words return to the worker.
func TestASetupWorkersQuestionsReachTheUserInLavishThroughCoordinationsLeaderAndTheAnswersAreRecordedAtOnce(t *testing.T) {
	f := newLavishFactoryOf(t, roles.Coordination)
	owner := roles.LeaderOf(roles.Coordination).String()

	// The worker asks its owning leader, which is Coordination's own, never
	// the user, and its assignment's work waits.
	asked, dispatch, err := f.d.decisionRequest(Request{Session: "worker0", Command: CommandDecisionRequest, Kind: store.RequestQuestions, Questions: setupQuestions, Content: "It has a CLAUDE.md and no docs/agents."})
	if err != nil {
		t.Fatal(err)
	}
	if asked.Recipient != owner || dispatch == nil || dispatch.Agent != owner || asked.HasPage() {
		t.Fatalf("the worker's request is %+v, dispatch %+v, want it to go to %s", asked, dispatch, owner)
	}

	// Coordination's leader carries it to the user, in Lavish, even a single
	// choice of the setup.
	carried, dispatch, err := f.d.decisionRequest(Request{Session: "credential", Command: CommandDecisionRequest, Job: f.job.Number, Kind: store.RequestQuestions, Carries: asked.Request, Questions: setupQuestions, Content: "It has a CLAUDE.md and no docs/agents.", Blocks: []int64{f.assignments[0].ID}})
	if err != nil {
		t.Fatal(err)
	}
	if carried.Recipient != store.ToUser || carried.Surface != store.SurfaceLavish || carried.Carries != asked.Request || dispatch != nil {
		t.Fatalf("Coordination's request is %+v, dispatch %+v, want a Lavish request to the user that it presents itself", carried, dispatch)
	}
	page := f.waitFor(t, carried, "the page to be opened", func(r store.DecisionRequest) bool { return r.PageURL != "" })
	html, err := os.ReadFile(page.PageFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Where do issues live?", "Does the repository have CI?", "It has a CLAUDE.md and no docs/agents."} {
		if !strings.Contains(string(html), want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	if pending, err := f.s.PendingDecisions(); err != nil || len(pending) != 1 || pending[0].PageURL == "" {
		t.Errorf("the user is listed %+v (%v), want the one request with its page", pending, err)
	}

	// Nothing is recorded for the repository before the user answers.
	if recorded, err := f.s.SetupAnswers("fixture"); err != nil || len(recorded) != 0 {
		t.Fatalf("setup answers before the user answered: %+v (%v)", recorded, err)
	}

	// What the user sends is recorded for the repository at once, though the
	// page is partly answered and the version stays pending.
	f.userSends(t, page, false, answer(page, "tracker", "github", ""), answer(page, "ci", "no", "a nightly script only"))
	f.waitFor(t, page, "the first answers", func(r store.DecisionRequest) bool { return len(r.Answered) == 2 })
	recorded, err := f.s.SetupAnswers("fixture")
	if err != nil || len(recorded) != 2 || recorded[0].Question != "tracker" || recorded[1].Question != "ci" || recorded[1].Choice != "no" {
		t.Fatalf("setup answers after the first send: %+v (%v)", recorded, err)
	}
	brief := callWith(t, f.d.jobCommand, Request{Command: CommandBrief, Job: f.job.Number}).Brief
	for _, want := range []string{"Where do issues live? (tracker): github", "Does the repository have CI? (ci): no (a nightly script only)", "set `no_ci = true` in the [repositories.fixture] table"} {
		if !strings.Contains(brief, want) {
			t.Errorf("the setup job's brief lacks %q: %s", want, brief)
		}
	}

	// Coordination's leader is told at once what the user must declare, and not
	// only when it reads the brief.
	var notices []string
	for _, m := range f.inbox(t, owner) {
		if m.Kind == store.MessageSetup && strings.Contains(m.Body, "CI declaration") {
			notices = append(notices, m.Body)
		}
	}
	if len(notices) != 1 || !strings.Contains(notices[0], "set `no_ci = true` in the [repositories.fixture] table") || !strings.Contains(notices[0], "tagged with job 1") {
		t.Errorf("Coordination was told %q of the CI declaration, want the change once", notices)
	}

	f.userSends(t, page, true, answer(page, "labels", "keep the defaults", ""))
	done := f.waitFor(t, page, "the user's answer", func(r store.DecisionRequest) bool { return r.State == store.RequestAnswered })
	if recorded, err := f.s.SetupAnswers("fixture"); err != nil || len(recorded) != 3 || recorded[2].Question != "labels" || recorded[2].Cites != done.Cites {
		t.Fatalf("setup answers after the last send: %+v (%v), want the third to cite Lavish answer %d", recorded, err, done.Cites)
	}

	// Coordination's leader returns it to the worker, as the user gave it.
	returned, back, err := f.d.decisionAnswer(Request{Session: "credential", Command: CommandDecisionAnswer, Request: asked.Request, Version: 1, UserAnswer: carried.Request})
	if err != nil {
		t.Fatal(err)
	}
	if returned.State != store.RequestAnswered || returned.AnsweredBy != owner || back.Agent != f.assignments[0].Worker || returned.Answer != done.Answer {
		t.Fatalf("the returned answer is %+v, dispatch %+v", returned, back)
	}
	for _, want := range []string{"github", "a nightly script only", "keep the defaults"} {
		if !strings.Contains(returned.Answer, want) {
			t.Errorf("the worker's answer lacks the user's words %q: %q", want, returned.Answer)
		}
	}
	if _, _, err := f.d.decisionAnswer(Request{Session: "credential", Command: CommandDecisionAnswer, Request: asked.Request, Version: 1, Answer: "github"}); err == nil {
		t.Error("Coordination's leader answered a setup question in words of its own")
	}
}

func TestASetupJobsSingleChoiceIsAnsweredInLavishToo(t *testing.T) {
	f := newLavishFactoryOf(t, roles.Coordination)
	r, _, err := f.d.decisionRequest(Request{Session: "credential", Command: CommandDecisionRequest, Job: f.job.Number, Kind: store.RequestChoice, Questions: setupQuestions[2:]})
	if err != nil {
		t.Fatal(err)
	}
	if r.Surface != store.SurfaceLavish {
		t.Errorf("a single choice of a setup job is answered in %s", r.Surface)
	}
	page := f.waitFor(t, r, "the page to be opened", func(r store.DecisionRequest) bool { return r.PageURL != "" })
	f.userSends(t, page, true, answer(page, "ci", "yes", ""))
	f.waitFor(t, page, "the user's answer", func(r store.DecisionRequest) bool { return r.State == store.RequestAnswered })
	if recorded, err := f.s.SetupAnswers("fixture"); err != nil || len(recorded) != 1 || recorded[0].Choice != "yes" {
		t.Errorf("setup answers = %+v (%v)", recorded, err)
	}
	// A repository with CI has nothing to declare, so nothing is said of it.
	for _, m := range f.inbox(t, "leader@coordination") {
		if m.Kind == store.MessageSetup && strings.Contains(m.Body, "CI declaration") {
			t.Errorf("Coordination was told to give the user a change: %s", m.Body)
		}
	}
}

func TestCoordinationsLeaderPushesASetupJobsBranchAsItsDeliveryOwnerWithNoQualityValidation(t *testing.T) {
	f := newTwoResultsOf(t, roles.Coordination)
	branch := jobBranchFor(f.job)
	push := Request{Session: "credential", Command: CommandPush, Job: f.job.Number}
	if err := f.d.authorize(push); err != nil {
		t.Fatalf("Coordination's leader may not push: %v", err)
	}
	if err := f.d.authorize(Request{Session: "credential", Command: CommandEffect}); err != nil {
		t.Fatalf("Coordination's leader may not record the pull request: %v", err)
	}
	for _, command := range []string{CommandAccept, CommandReject, CommandCancel} {
		if err := f.d.authorize(Request{Session: "credential", Command: command}); err != nil {
			t.Errorf("Coordination's leader may not run %s: %v", command, err)
		}
	}
	if _, err := f.decide(CommandAccept, f.assignments[0].ID, "file-1.txt holds the setup"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.decide(CommandCancel, f.assignments[1].ID, "its files are not wanted"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.AssignmentCancelled(f.assignments[1].ID, store.Cancellation{Commit: f.commits[1], Pushed: true}, time.Now()); err != nil {
		t.Fatal(err)
	}

	// Only while working on a dispatch, as every delivery owner.
	if _, err := f.d.push(push); err == nil || !strings.Contains(err.Error(), "no current dispatch") {
		t.Errorf("a push asked for outside a dispatch returned %v", err)
	}
	dispatch := f.leaderWorksAs(t, roles.LeaderOf(roles.Coordination))
	got, err := f.d.push(push)
	if err != nil {
		t.Fatal(err)
	}
	if got.Result != store.PushPushed || got.Commit != f.commits[0] || got.Branch != branch || got.Dispatch != dispatch || got.Leader != "leader@coordination" || got.Effect == 0 {
		t.Errorf("the push is %+v", got)
	}
	if on := git(t, f.origin, "rev-parse", "refs/heads/"+branch); on != f.commits[0] {
		t.Errorf("origin's job branch is at %s, want the accepted setup %s", on, f.commits[0])
	}
	if err := f.s.Deliverable(f.job.Number, f.commits[0]); err != nil {
		t.Errorf("the pushed head of a setup job is not deliverable: %v", err)
	}
}

func TestTheDeliveryOfASetupJobTellsCoordinationTheLinkAndWhatTheUserStillDoesAboutCI(t *testing.T) {
	p := newJobFactory(t, store.MandateSetup, true)
	delivery := store.Delivery{Number: 7, URL: "https://github.com/o/fixture/pull/7", Head: "bbb", NoCI: true}
	body := deliveryBody(p.job, delivery)
	for _, want := range []string{
		"the setup pull request #7 of repository fixture, https://github.com/o/fixture/pull/7, is ready for review at head bbb, straight away, as its repository is declared as having no CI",
		"setup files", "Quality validated nothing", "ready is not merged", "tagged with job 1, with the link",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the delivery message lacks %q: %s", want, body)
		}
	}
	if strings.Contains(body, "notes") {
		t.Errorf("the delivery message of a setup calls it notes: %s", body)
	}

	// The user said it has no CI and has not declared it: the message gives
	// the change again.
	setupAnswered(t, p.s, p.job, store.QuestionAnswer{Question: "ci", Choice: "no"})
	got := body + p.d.setupDeliveryBody(p.job)
	for _, want := range []string{"the setup of fixture", "repository instructions until the pull request merges", "set `no_ci = true` in the [repositories.fixture] table", "`asmai config apply`"} {
		if !strings.Contains(got, want) {
			t.Errorf("the setup delivery message lacks %q: %s", want, got)
		}
	}
}

func TestTheInstructionsAndTheDaemonNameTheSetupTheSameWay(t *testing.T) {
	leader, err := roles.LeaderInstructions(roles.Coordination)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := roles.WorkerInstructions(roles.Coordination)
	if err != nil {
		t.Fatal(err)
	}
	ci := "`" + store.CIQuestion + "`"
	for name, text := range map[string]string{"Coordination's leader": leader, "Coordination's worker": worker} {
		if !strings.Contains(text, ci) {
			t.Errorf("%s is not told the CI question's id %s", name, ci)
		}
	}
	if want := "message of kind `" + store.MessageSetup + "`"; !strings.Contains(leader, want) {
		t.Errorf("Coordination's leader is not told the message of its setup job: %q", want)
	}
	if want := "the setup mandate"; !strings.Contains(leader, want) || store.MandateSetup != "setup" {
		t.Errorf("Coordination's leader is not told the setup mandate")
	}
	body := setupBody(store.Repository{Name: "fixture"})
	for _, want := range []string{"docs/agents/issue-tracker.md", "setup-matt-pocock-skills", `"` + store.CIQuestion + `"`, "asmai push <job>", "asmai brief <job>"} {
		if !strings.Contains(body, want) {
			t.Errorf("the daemon's message to Coordination lacks %q: %s", want, body)
		}
	}
	// The change the daemon gives for CI is the one the instructions say.
	p := newJobFactory(t, store.MandateSetup, true)
	change := p.d.ciInstruction("fixture", []store.SetupAnswer{{Question: store.CIQuestion, Choice: "no"}})
	for _, want := range []string{"no_ci = true", "asmai config apply", "asmai config edit"} {
		if !strings.Contains(change, want) {
			t.Errorf("the CI change lacks %q: %s", want, change)
		}
	}
	for _, want := range []string{"no_ci = true", "asmai config apply"} {
		if !strings.Contains(leader, want) {
			t.Errorf("Coordination's leader is not told %q", want)
		}
	}
}
