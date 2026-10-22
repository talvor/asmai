// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/skills"
	"github.com/talvor/asmai/internal/store"
)

func TestAResearchLeaderStartsOnDemandWithItsInstructionsAndTheIndexOfItsWorkersSkills(t *testing.T) {
	p := newJobFactory(t, store.MandateResearchOnly, false)
	leader := roles.LeaderOf(roles.Research)
	delete(p.d.leaders, leader.String())
	p.d.mu.Lock()
	err := p.d.ensureAgent(leader)
	p.d.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	args := waitForFile(t, filepath.Join(p.out, "args"))
	for _, want := range []string{"--model\nopus", "You are Research's leader", "## research\n", "## to-questionnaire\n", "tied to a cited source"} {
		if !strings.Contains(args, want) {
			t.Errorf("Research's leader's command line lacks %q: %s", want, args)
		}
	}
	if strings.Contains(args, "--plugin-dir") {
		t.Errorf("Research's leader is given skills of its own, though its list is only the index: %s", args)
	}
	if agents, err := p.s.Agents(); err != nil || len(agents) != 1 || agents[0].Agent != leader.String() || agents[0].Role != roles.Research {
		t.Errorf("the store records agents %+v (%v), want leader@research", agents, err)
	}
}

func TestAResearchJobWithNoRepositoryGivesItsWorkerAnEmptyScratchWorkspaceWithNoCloneAndNoBranch(t *testing.T) {
	p := newJobFactory(t, store.MandateResearchOnly, false)
	if err := p.d.authorize(Request{Session: "research", Command: CommandAssign}); err != nil {
		t.Fatalf("Research's leader may not assign: %v", err)
	}
	for _, command := range []string{CommandAccept, CommandReject, CommandCancel} {
		if err := p.d.authorize(Request{Session: "research", Command: command}); err != nil {
			t.Errorf("Research's leader may not run %s: %v", command, err)
		}
	}
	if err := p.d.authorize(Request{Session: "coordination", Command: CommandAssign}); err == nil {
		t.Error("Coordination's leader may assign work")
	}

	a, dispatch, err := p.d.assign(Request{Session: "research", Command: CommandAssign, Job: p.job.Number, Outcome: "Find how the greeting is spelled", Criteria: []string{"the answer names its source"}, Skill: "research"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Owner != "leader@research" || a.Worker != "worker1@research" || !a.Scratch || a.ReadOnly || a.Skill != "research" || dispatch.Agent != "worker1@research" {
		t.Errorf("the assignment is %+v, dispatch %+v, want a scratch one of leader@research for worker1@research running research", a, dispatch)
	}
	if want := filepath.Join(p.d.cfg.Paths.Workspaces, "job-1", "assignment-1", "scratch"); a.Workspace.Path != want {
		t.Errorf("the scratch workspace is %s, want %s", a.Workspace.Path, want)
	}
	if a.Workspace.Branch != "" || a.Workspace.Base != "" || a.JobBranch != "" {
		t.Errorf("the scratch workspace has a branch %q, a commit %q and a job branch %q, want none", a.Workspace.Branch, a.Workspace.Base, a.JobBranch)
	}
	if entries, err := os.ReadDir(a.Workspace.Path); err != nil || len(entries) != 0 {
		t.Errorf("the scratch workspace holds %v (%v), want it empty: no clone, no checkout", entries, err)
	}
	if _, err := p.s.JobBranch(p.job.Number); err == nil {
		t.Error("a job with no repository was given a job branch")
	}
	cwd, _ := filepath.EvalSymlinks(a.Workspace.Path)
	if got := waitForFile(t, filepath.Join(p.out, "cwd")); got != cwd {
		t.Errorf("the worker started in %s, want its scratch workspace %s", got, cwd)
	}
	args := waitForFile(t, filepath.Join(p.out, "args"))
	for _, want := range []string{"--model\nsonnet", "a Research worker", "--plugin-dir"} {
		if !strings.Contains(args, want) {
			t.Errorf("the Research worker's command line lacks %q: %s", want, args)
		}
	}
	plugin := filepath.Join(p.d.cfg.Paths.Plugins, "worker1@research")
	for _, name := range skills.WorkerList(roles.Research) {
		if _, err := os.Stat(filepath.Join(plugin, skills.Dir, name, "SKILL.md")); err != nil {
			t.Errorf("Research's worker is not given the skill %s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(plugin, skills.Dir, "implement")); err == nil {
		t.Error("Research's worker is given Engineering's skill implement")
	}

	// A second assignment gets a scratch workspace of its own.
	b, _, err := p.d.assign(Request{Session: "research", Command: CommandAssign, Job: p.job.Number, Outcome: "Find who maintains it", Criteria: []string{"named"}})
	if err != nil {
		t.Fatal(err)
	}
	if b.Workspace.Path == a.Workspace.Path || b.Worker != "worker2@research" || b.Workspace.Slot == a.Workspace.Slot {
		t.Errorf("the second assignment is %+v, want its own worker, slot and scratch workspace", b)
	}

	// There is no commit for Quality to examine.
	if _, _, err := p.d.assign(Request{Session: "quality", Command: CommandAssign, Job: p.job.Number, ReadOnly: true, Commit: "abc", Outcome: "validate", Criteria: []string{"c"}}); err == nil || !strings.Contains(err.Error(), "no repository") {
		t.Errorf("a validation of a job with no repository returned %v, want it refused", err)
	}
	if brief := p.briefOf(t); !strings.Contains(brief, "Repository: none") || !strings.Contains(brief, "scratch workspace "+a.Workspace.Path) {
		t.Errorf("the brief is %s, want it to say the job has no repository and where each assignment works", brief)
	}
}

// briefOf returns the job's canonical brief.
func (p planningJob) briefOf(t *testing.T) string {
	t.Helper()
	resp := callWith(t, p.d.jobCommand, Request{Command: CommandBrief, Job: p.job.Number})
	if resp.Error != "" {
		t.Fatal(resp.Error)
	}
	return resp.Brief
}

func TestAResearchOnlyMandateAuthorizesOnlyResearchAndNeverTheOtherRoles(t *testing.T) {
	tested := store.Job{Number: 1, Mandate: store.MandateTestedPR}
	research := store.Job{Number: 2, Mandate: store.MandateResearchOnly}
	planning := store.Job{Number: 3, Mandate: store.MandatePlanningOnly}
	for _, tt := range []struct {
		name             string
		sender, receiver string
		job              store.Job
		want             string
	}{
		{"Coordination to Research, in a research-only job", roles.Coordination, roles.Research, research, ""},
		{"Coordination to Research, in a tested-PR job", roles.Coordination, roles.Research, tested, ""},
		{"Coordination to Engineering, in a research-only job", roles.Coordination, roles.Engineering, research, "authorizes only its research outcome, never code"},
		{"Coordination to Planning, in a research-only job", roles.Coordination, roles.Planning, research, "authorizes only its research outcome: hand it to Research"},
		{"Coordination to Research, in a planning-only job", roles.Coordination, roles.Research, planning, "authorizes only its planning outcome: hand it to Planning"},
		{"Coordination to Engineering, in a planning-only job", roles.Coordination, roles.Engineering, planning, "authorizes only its planning outcome, never code"},
		{"Research to Engineering", roles.Research, roles.Engineering, research, "send a handoff to another role's leader"},
		{"Research to Quality", roles.Research, roles.Quality, research, "send a handoff to another role's leader"},
		{"Engineering to Research", roles.Engineering, roles.Research, tested, "send a handoff to another role's leader"},
	} {
		err := checkHandoff(tt.sender, tt.receiver, tt.job)
		if tt.want == "" && err != nil || tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)) {
			t.Errorf("%s: checkHandoff returned %v, want %q", tt.name, err, tt.want)
		}
	}
}

func TestAHandoffToResearchStartsItsLeaderOnDemandAndAResearchOnlyJobIsNeverHandedToEngineering(t *testing.T) {
	p := newJobFactory(t, store.MandateResearchOnly, false)
	delete(p.d.leaders, roles.LeaderOf(roles.Research).String())
	delete(p.d.sessions, roles.Research)
	handoff := func(to string) Response {
		return callWith(t, p.d.handoffCommand, Request{
			Session: "coordination", Command: CommandHandoffSend, Job: p.job.Number, Agent: to, Outcome: "How the greeting is spelled, with sources",
			Decisions: "none", Evidence: "none", Constraints: "none", Permissions: "none", Criteria: []string{"cited"},
		})
	}
	if resp := handoff("engineering"); !strings.Contains(resp.Error, "authorizes only its research outcome") || resp.Handoff != nil {
		t.Errorf("a research-only job handed to Engineering answered %+v, want it refused", resp)
	}
	resp := handoff("research")
	if resp.Error != "" || resp.Handoff == nil || resp.Handoff.Receiver != "leader@research" || resp.Dispatch == nil || resp.Dispatch.Agent != "leader@research" {
		t.Fatalf("a handoff to Research answered %+v", resp)
	}
	args := waitForFile(t, filepath.Join(p.out, "args"))
	if !strings.Contains(args, "You are Research's leader") {
		t.Errorf("the Research leader that started on demand lacks its instructions: %s", args)
	}
}

func TestAResearchOnlyMandateCoversResearchAndToQuestionnaire(t *testing.T) {
	p := newJobFactory(t, store.MandateResearchOnly, false)
	for _, skill := range []string{"research", "to-questionnaire"} {
		if _, _, err := p.d.assign(Request{Session: "research", Command: CommandAssign, Job: p.job.Number, Outcome: "Find it", Criteria: []string{"cited"}, Skill: skill}); err != nil {
			t.Errorf("assigning %s in a research-only job returned %v, want it covered", skill, err)
		}
	}
	if !slices.Equal(calledFor[store.MandateResearchOnly], []string{"research", "to-questionnaire"}) {
		t.Errorf("a research-only mandate calls for %v, want research and to-questionnaire", calledFor[store.MandateResearchOnly])
	}
}

// scratchWorker makes the worker of assignment a, started by assign, the
// session worker credential "worker" is, working on dispatch, as its inbox
// fetch does.
func (p planningJob) scratchWorker(t *testing.T, a store.Assignment, dispatch store.Dispatch) {
	t.Helper()
	address, err := roles.ParseAddress(a.Worker)
	if err != nil {
		t.Fatal(err)
	}
	p.d.mu.Lock()
	l := p.d.leaders[a.Worker]
	l.currentDispatch = dispatch.ID
	p.d.sessions["worker"] = sessionRef{address: address, generation: l.generation}
	generation := l.generation
	p.d.mu.Unlock()
	if err := p.s.ChangeDispatch(dispatch.ID, generation, store.DispatchCreated, store.DispatchWorking, "", time.Now()); err != nil {
		t.Fatal(err)
	}
}

// scratchResult has the worker of a submit findings it wrote in its scratch
// workspace.
func (p planningJob) scratchResult(t *testing.T, a store.Assignment, dispatch store.Dispatch, findings string) store.Report {
	t.Helper()
	p.scratchWorker(t, a, dispatch)
	if err := os.WriteFile(filepath.Join(a.Workspace.Path, "findings.md"), []byte(findings), 0o600); err != nil {
		t.Fatal(err)
	}
	r, _, err := p.d.report(Request{Session: "worker", Command: CommandResult, Report: &store.ReportInput{
		Evidence: []string{"https://example.test/spec#greeting"}, Artifacts: []string{"findings.md"}, Gaps: []string{}, SkillsRun: []string{"research"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestAResearchOnlyJobWithNoRepositoryRunsInScratchWorkspacesAndEndsOnTheLeadersAcceptanceOfTheOutcome(t *testing.T) {
	p := newJobFactory(t, store.MandateResearchOnly, false)
	first, firstDispatch, err := p.d.assign(Request{Session: "research", Command: CommandAssign, Job: p.job.Number, Outcome: "Find how the greeting is spelled", Criteria: []string{"cited"}, Skill: "research"})
	if err != nil {
		t.Fatal(err)
	}
	r := p.scratchResult(t, first, firstDispatch, "# Findings\nThe greeting is spelled hello.\n")
	if r.Commit != "" || len(r.Artifacts) != 2 || r.Artifacts[0] != "scratch workspace "+first.Workspace.Path {
		t.Errorf("the result is %+v, want no commit and its scratch workspace named first among its artifacts", r)
	}

	decide := func(command string, a store.Assignment, endsJob bool) (Response, string, error) {
		return p.d.decide(Request{Session: "research", Command: command, Assignment: a.ID, Reasons: []string{"the spelling is cited"}, EndsJob: endsJob})
	}
	// A result that is rejected, or accepted without the outcome, ends
	// nothing but its assignment.
	if _, _, err := p.d.decide(Request{Session: "research", Command: CommandReject, Assignment: first.ID, EndsJob: true, Reasons: []string{"x"}}); err == nil || !strings.Contains(err.Error(), "--ends-job belongs to asmai accept") {
		t.Errorf("rejecting with --ends-job returned %v, want it refused", err)
	}
	resp, notify, err := decide(CommandAccept, first, false)
	if err != nil || resp.Assignment == nil || resp.Assignment.State != store.AssignmentAccepted || resp.Dispatch != nil || notify != "" {
		t.Fatalf("accepting a result returned %+v, notify %q, %v", resp, notify, err)
	}
	if j, _ := p.s.Job(p.job.Number); j.State != store.JobOpen {
		t.Fatalf("accepting one result ended the job, which is %s", j.State)
	}
	if _, err := os.Stat(filepath.Join(first.Workspace.Path, "findings.md")); err != nil {
		t.Errorf("accepting the result removed the findings from the scratch workspace: %v", err)
	}
	if _, err := os.Stat(first.Workspace.Tmp); err == nil {
		t.Errorf("the accepted assignment's temporary directory %s is still there", first.Workspace.Tmp)
	}

	// Only the owning leader's acceptance of the outcome ends the job.
	second, secondDispatch, err := p.d.assign(Request{Session: "research", Command: CommandAssign, Job: p.job.Number, Outcome: "Find who maintains it", Criteria: []string{"named"}, Skill: "research"})
	if err != nil {
		t.Fatal(err)
	}
	p.scratchResult(t, second, secondDispatch, "# Maintainers\nThe Hello team.\n")
	resp, notify, err = decide(CommandAccept, second, true)
	if err != nil || resp.Job == nil || resp.Job.State != store.JobEnded || resp.Dispatch == nil || resp.Dispatch.Agent != "leader@coordination" || notify != "leader@coordination" {
		t.Fatalf("accepting the outcome returned %+v, notify %q, %v, want the job ended and Coordination told", resp, notify, err)
	}

	// Coordination is told where the results are.
	messages, err := p.s.Inbox("leader@coordination", 0, time.Now())
	if err != nil || len(messages) != 1 || messages[0].Kind != store.MessageDelivery {
		t.Fatalf("Coordination's inbox holds %+v (%v), want the message that the job ended", messages, err)
	}
	for _, want := range []string{"Job 1 has ended", "leader@research", "research-only mandate", "the results are in " + first.Workspace.Path, "the results are in " + second.Workspace.Path, "artifact: findings.md", "evidence: https://example.test/spec#greeting", "`asmai job clean 1`"} {
		if !strings.Contains(messages[0].Body, want) {
			t.Errorf("Coordination's message lacks %q: %s", want, messages[0].Body)
		}
	}

	// What stays is the scratch workspaces, with their results, kept after the
	// job has ended, even across the daemon's cleaning of ended jobs.
	p.d.removeEndedJobsLocal()
	for _, a := range []store.Assignment{first, second} {
		if _, err := os.Stat(filepath.Join(a.Workspace.Path, "findings.md")); err != nil {
			t.Errorf("the ended job's scratch workspace %s was not kept: %v", a.Workspace.Path, err)
		}
	}
	if removed, err := p.s.LocalRemoved(p.job.Number); err != nil || removed {
		t.Errorf("the daemon removed what was local to a job with no repository (%v, %v)", removed, err)
	}
	if brief := p.briefOf(t); !strings.Contains(brief, "state: ended") || !strings.Contains(brief, "asmai job clean 1") {
		t.Errorf("the ended job's brief is %s, want it to say the job ended and how its scratch workspaces are removed", brief)
	}
}

func TestAJobCleanRemovesTheScratchWorkspacesOfAJobThatEndedAndIsRefusedWhileItIsOpen(t *testing.T) {
	p := newJobFactory(t, store.MandateResearchOnly, false)
	a, dispatch, err := p.d.assign(Request{Session: "research", Command: CommandAssign, Job: p.job.Number, Outcome: "Find it", Criteria: []string{"cited"}, Skill: "research"})
	if err != nil {
		t.Fatal(err)
	}
	p.scratchResult(t, a, dispatch, "# Findings\n")
	clean := func(job int64) Response {
		return callWith(t, p.d.jobCommand, Request{Command: CommandJobClean, Job: job})
	}

	if resp := clean(p.job.Number); !strings.Contains(resp.Error, "job 1 is still open") || len(resp.Removed) != 0 {
		t.Errorf("cleaning an open job answered %+v, want it refused", resp)
	}
	if _, err := os.Stat(filepath.Join(a.Workspace.Path, "findings.md")); err != nil {
		t.Fatalf("a refused clean removed the results: %v", err)
	}
	// Submitted and accepted without the outcome, it is still open.
	if _, _, err := p.d.decide(Request{Session: "research", Command: CommandAccept, Assignment: a.ID, Reasons: []string{"cited"}}); err != nil {
		t.Fatal(err)
	}
	if resp := clean(p.job.Number); !strings.Contains(resp.Error, "still open") {
		t.Errorf("cleaning a job whose assignments ended but whose outcome is not accepted answered %+v, want it refused", resp)
	}
	b, bDispatch, err := p.d.assign(Request{Session: "research", Command: CommandAssign, Job: p.job.Number, Outcome: "Find more", Criteria: []string{"cited"}, Skill: "research"})
	if err != nil {
		t.Fatal(err)
	}
	p.scratchResult(t, b, bDispatch, "# More\n")
	if _, _, err := p.d.decide(Request{Session: "research", Command: CommandAccept, Assignment: b.ID, Reasons: []string{"cited"}, EndsJob: true}); err != nil {
		t.Fatal(err)
	}

	resp := clean(p.job.Number)
	jobDir := filepath.Join(p.d.cfg.Paths.Workspaces, "job-1")
	if resp.Error != "" || !slices.Equal(resp.Removed, []string{jobDir}) {
		t.Fatalf("cleaning an ended job answered %+v, want its directory %s removed", resp, jobDir)
	}
	if _, err := os.Stat(jobDir); err == nil {
		t.Errorf("%s is still there after the clean", jobDir)
	}
	if cleaned, err := p.s.CleanedScratch(p.job.Number); err != nil || !cleaned {
		t.Errorf("the journal does not hold the cleaning (%v, %v)", cleaned, err)
	}
	// Cleaning again removes nothing and journals nothing more.
	if again := clean(p.job.Number); again.Error != "" || len(again.Removed) != 0 {
		t.Errorf("cleaning a job twice answered %+v, want nothing to remove", again)
	}
	if resp := clean(99); !strings.Contains(resp.Error, "does not exist") {
		t.Errorf("cleaning a job that does not exist answered %+v", resp)
	}
	if err := p.d.authorize(Request{Session: "research", Command: CommandJobClean}); err == nil || !strings.Contains(err.Error(), "asmai job clean") {
		t.Errorf("an agent running asmai job clean returned %v, want it told to ask the user", err)
	}
}

func TestAJobCleanRefusesAJobThatHasARepository(t *testing.T) {
	p := newJobFactory(t, store.MandateResearchOnly, true)
	if resp := callWith(t, p.d.jobCommand, Request{Command: CommandJobClean, Job: p.job.Number}); !strings.Contains(resp.Error, "repository fixture") {
		t.Errorf("cleaning a repository job answered %+v, want it refused: its workspaces are removed by the daemon when it ends", resp)
	}
}

func TestACancelledScratchAssignmentEndsWithNothingToPushAndKeepsWhatItLeft(t *testing.T) {
	p := newJobFactory(t, store.MandateResearchOnly, false)
	a, dispatch, err := p.d.assign(Request{Session: "research", Command: CommandAssign, Job: p.job.Number, Outcome: "Find it", Criteria: []string{"cited"}, Skill: "research"})
	if err != nil {
		t.Fatal(err)
	}
	p.scratchWorker(t, a, dispatch)
	if err := os.WriteFile(filepath.Join(a.Workspace.Path, "partial.md"), []byte("half"), 0o600); err != nil {
		t.Fatal(err)
	}
	resp, _, err := p.d.decide(Request{Session: "research", Command: CommandCancel, Assignment: a.ID, Reasons: []string{"no longer wanted"}})
	if err != nil || resp.Dispatch == nil || resp.Dispatch.Agent != a.Worker {
		t.Fatalf("cancelling returned %+v, %v", resp, err)
	}
	p.d.mu.Lock()
	l := p.d.leaders[a.Worker]
	p.d.mu.Unlock()
	ref := sessionRef{address: l.address, generation: l.generation}
	p.d.cancelled(ref, a.ID)
	got, err := p.s.Assignment(a.ID)
	if err != nil || got.State != store.AssignmentCancelled {
		t.Fatalf("the assignment is %+v (%v), want it cancelled", got, err)
	}
	if _, err := os.Stat(filepath.Join(a.Workspace.Path, "partial.md")); err != nil {
		t.Errorf("the cancelled assignment's scratch workspace was not kept: %v", err)
	}
}

func TestAScratchAssignmentsCancellationAfterARestartHasNoBranchToPush(t *testing.T) {
	body := cancellationResumption(store.Assignment{ID: 3, Job: 1, Scratch: true, Workspace: store.Workspace{Path: "/ws/scratch"}}, 9)
	if strings.Contains(body, "git push") || strings.Contains(body, "assignment branch") || !strings.Contains(body, "scratch workspace with no branch") {
		t.Errorf("the resumption of a cancelled scratch assignment is %q, want it to name no branch to push", body)
	}
	repository := cancellationResumption(store.Assignment{ID: 3, Job: 1, Workspace: store.Workspace{Branch: "asmai/job-1/3"}}, 9)
	if !strings.Contains(repository, "git push origin asmai/job-1/3") {
		t.Errorf("the resumption of a cancelled repository assignment is %q, want it to push its branch", repository)
	}
}

func TestOnlyTheLeaderOfTheRoleAMandateAuthorizesGivesWritingWorkToItsJob(t *testing.T) {
	research := newJobFactory(t, store.MandateResearchOnly, false)
	if _, _, err := research.d.assign(Request{Session: "planning", Command: CommandAssign, Job: research.job.Number, Outcome: "Plan it", Criteria: []string{"planned"}, Skill: "grilling"}); err == nil || !strings.Contains(err.Error(), "authorizes only its research outcome: only leader@research may give its writing work") {
		t.Errorf("Planning's leader assigning work of a research-only job returned %v, want it refused", err)
	}
	if _, _, err := research.d.assign(Request{Session: "engineering", Command: CommandAssign, Job: research.job.Number, Outcome: "Build it", Criteria: []string{"built"}}); err == nil || !strings.Contains(err.Error(), "only leader@research may give its writing work") {
		t.Errorf("Engineering's leader assigning work of a research-only job returned %v, want it refused", err)
	}
	if assignments, err := research.s.AssignmentsForJob(research.job.Number); err != nil || len(assignments) != 0 {
		t.Errorf("a refused assignment was recorded: %+v (%v)", assignments, err)
	}
	planning := newPlanningJob(t, store.MandatePlanningOnly)
	if _, _, err := planning.d.assign(Request{Session: "research", Command: CommandAssign, Job: planning.job.Number, Outcome: "Find it", Criteria: []string{"found"}, Skill: "research"}); err == nil || !strings.Contains(err.Error(), "authorizes only its planning outcome: only leader@planning may give its writing work") {
		t.Errorf("Research's leader assigning work of a planning-only job returned %v, want it refused", err)
	}
}
