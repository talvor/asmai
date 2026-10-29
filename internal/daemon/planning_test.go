// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/providers"
	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/session"
	"github.com/talvor/asmai/internal/skills"
	"github.com/talvor/asmai/internal/store"
)

func TestAPlanningLeaderStartsOnDemandWithItsInstructionsAndTheIndexOfItsWorkersSkills(t *testing.T) {
	d, _, out := workerFactory(t)
	leader := roles.LeaderOf(roles.Planning)
	if l := d.leaders[leader.String()]; l != nil && l.session != nil {
		t.Fatalf("%s runs before any work for Planning arrived", leader)
	}
	d.mu.Lock()
	err := d.ensureAgent(leader)
	d.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	args := waitForFile(t, filepath.Join(out, "args"))
	for _, want := range []string{"--model\nopus", "You are Planning's leader", "## wayfinder\n", "## grilling\n", "- Invocation: starts only on request"} {
		if !strings.Contains(args, want) {
			t.Errorf("Planning's leader's command line lacks %q: %s", want, args)
		}
	}
	if strings.Contains(args, "--plugin-dir") {
		t.Errorf("Planning's leader is given skills of its own, though its list is only the index: %s", args)
	}
	agents, err := d.store.Agents()
	if err != nil || len(agents) != 1 || agents[0].Agent != leader.String() || agents[0].Role != roles.Planning {
		t.Errorf("the store records agents %+v (%v), want leader@planning", agents, err)
	}
}

func TestAPlanningWorkerRunsItsSkillsInItsWorkspace(t *testing.T) {
	d, _, out := workerFactory(t)
	a := planningAssignment(t, d, 2, nil)
	worker := roles.WorkerOf(roles.Planning, 1)
	d.mu.Lock()
	err := d.ensureAgent(worker)
	d.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	cwd, _ := filepath.EvalSymlinks(a.Workspace.Path)
	if got := waitForFile(t, filepath.Join(out, "cwd")); got != cwd {
		t.Errorf("the worker started in %s, want its workspace %s", got, cwd)
	}
	args := waitForFile(t, filepath.Join(out, "args"))
	for _, want := range []string{"--model\nsonnet", "a Planning worker", "--plugin-dir"} {
		if !strings.Contains(args, want) {
			t.Errorf("the worker's command line lacks %q: %s", want, args)
		}
	}
	plugin := filepath.Join(d.cfg.Paths.Plugins, worker.String())
	for _, name := range skills.WorkerList(roles.Planning) {
		if _, err := os.Stat(filepath.Join(plugin, skills.Dir, name, "SKILL.md")); err != nil {
			t.Errorf("Planning's worker is not given the skill %s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(plugin, skills.Dir, "implement")); err == nil {
		t.Error("Planning's worker is given Engineering's skill implement")
	}
	l := d.leaders[worker.String()]
	if l == nil || l.assignment != a.ID {
		t.Errorf("worker1@planning runs %+v, want assignment %d", l, a.ID)
	}
}

func TestAPlanningAssignmentNarrowsItsWorkersSkillsToTheListItNames(t *testing.T) {
	d, _, out := workerFactory(t)
	planningAssignment(t, d, 2, []string{"grilling", "domain-modeling"})
	d.mu.Lock()
	err := d.ensureAgent(roles.WorkerOf(roles.Planning, 1))
	d.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	waitForFile(t, filepath.Join(out, "args"))
	plugin := filepath.Join(d.cfg.Paths.Plugins, "worker1@planning", skills.Dir)
	entries, err := os.ReadDir(plugin)
	if err != nil || len(entries) != 2 {
		t.Fatalf("the worker's plugin holds %v (%v), want grilling and domain-modeling", entries, err)
	}
}

// planningAssignment gives worker1@planning, on leader@planning's behalf, the
// assignment id of job 1 in a workspace that exists, with skills as its
// narrowed list.
func planningAssignment(t *testing.T, d *daemon, id int64, narrowed []string) store.Assignment {
	t.Helper()
	workspace := filepath.Join(d.cfg.Paths.Workspaces, "job-1", "assignment-"+string(rune('0'+id)), "repo")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	const commit = "1111111111111111111111111111111111111111"
	a, _, err := d.store.AssignmentCreated(store.NewAssignment{
		ID: id, Job: 1, Owner: roles.LeaderOf(roles.Planning).String(), Worker: roles.WorkerOf(roles.Planning, 1).String(),
		Outcome: "grill the plan", Criteria: []string{"questions asked"}, Skill: "grilling", Skills: narrowed,
		Workspace: store.Workspace{Slot: 4, Path: workspace, Tmp: filepath.Join(filepath.Dir(workspace), "tmp"), Branch: "asmai/job-1/" + string(rune('0'+id)), Base: commit},
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// planningJob is a daemon with every role staffed and a pinned Claude Code
// that records how it was started and waits, a real clone of a fixture
// repository, and a job of mandate in it, with leader@planning's session
// "planning", leader@engineering's "engineering" and Coordination's
// "coordination".
type planningJob struct {
	d      *daemon
	s      *store.Store
	job    store.Job
	out    string
	clone  string
	origin string
}

func newPlanningJob(t *testing.T, mandate string) planningJob {
	t.Helper()
	return newJobFactory(t, mandate, true)
}

// newJobFactory is newPlanningJob for a job that targets the fixture
// repository, or with repository false for a job that targets none, which has
// no clone and no job branch.
func newJobFactory(t *testing.T, mandate string, repository bool) planningJob {
	t.Helper()
	return newJobFactoryFrom(t, mandate, repository, "")
}

// newJobFactoryFrom is newJobFactory for a repository cloned from origin, or
// from a remote of its own when origin is empty: the setup job of a
// repository that has no issue tracker file, and any other job of one that
// has it.
func newJobFactoryFrom(t *testing.T, mandate string, repository bool, origin string) planningJob {
	t.Helper()
	return newJobFactoryWith(t, mandate, repository, origin, factoryOptions{})
}

// factoryOptions are what a test changes of newJobFactoryFrom's factory: the
// text its configuration file ends with, and the answers its stand-in Claude
// Code gives the permission rules probe besides the rules'.
type factoryOptions struct {
	config  string
	answers []string
}

// newJobFactoryWith is newJobFactoryFrom for a factory set by options.
func newJobFactoryWith(t *testing.T, mandate string, repository bool, origin string, options factoryOptions) planningJob {
	t.Helper()
	out := filepath.Join(t.TempDir(), "out")
	if err := os.MkdirAll(out, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ASMAI_TEST_OUT", out)
	paths := stateDir(t)
	if err := os.MkdirAll(paths.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var clone string
	if repository {
		if origin == "" && mandate == store.MandateSetup {
			origin = remoteWithoutSetup(t)
		} else if origin == "" {
			origin = remote(t)
		}
		clone = filepath.Join(paths.Repositories, "fixture")
		if err := os.MkdirAll(filepath.Dir(clone), 0o700); err != nil {
			t.Fatal(err)
		}
		git(t, filepath.Dir(clone), "clone", origin, clone)
	}
	configFile := filepath.Join(paths.Dir, "config.toml")
	var config strings.Builder
	for _, role := range []string{roles.Coordination, roles.Planning, roles.Research, roles.Engineering, roles.Quality} {
		config.WriteString("[roles." + role + "]\nleader_provider = \"claude\"\nleader_model = \"opus\"\nworker_provider = \"claude\"\nworker_model = \"sonnet\"\n")
	}
	if repository {
		fmt.Fprintf(&config, "[repositories.fixture]\norigin = %q\ndefault_branch = \"trunk\"\n", origin)
	}
	config.WriteString(options.config)
	if err := os.WriteFile(configFile, []byte(config.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	provider := filepath.Join(paths.Providers, "claude-code", "v", "claude")
	if err := os.MkdirAll(filepath.Dir(provider), 0o700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" + answersTheRulesProbe(t, options.answers...) + "pwd -P > \"$ASMAI_TEST_OUT/cwd\"\nprintf '%s\\n' \"$@\" > \"$ASMAI_TEST_OUT/args.part\"\nmv \"$ASMAI_TEST_OUT/args.part\" \"$ASMAI_TEST_OUT/args\"\nexec sleep 60\n"
	if err := os.WriteFile(provider, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(paths.Store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	at := time.Now()
	if _, err := s.ProviderInstalled(store.ProviderInstall{Name: providers.ClaudeCode, Version: "v", Path: provider, SHA256: strings.Repeat("a", 64), InstalledAt: at}); err != nil {
		t.Fatal(err)
	}
	repositoryName := ""
	var job store.Job
	if mandate == store.MandateSetup {
		// The daemon opens a repository's setup job as it registers it.
		var err error
		job, _, err = s.RepositoryAddedWithSetup(store.Repository{Name: "fixture", Origin: origin, DefaultBranch: "trunk", Clone: clone, AddedAt: at}, store.SetupOpening{Words: "asmai repo add " + origin, Reading: "Set fixture up for the skills", Criteria: []string{"its setup files are on the job's pull request"}, Body: "setup"})
		if err != nil {
			t.Fatal(err)
		}
	} else {
		if repository {
			repositoryName = "fixture"
			if err := s.RepositoryAdded(store.Repository{Name: "fixture", Origin: origin, DefaultBranch: "trunk", Clone: clone, AddedAt: at}); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.ConversationEntered("leader@coordination", roles.Coordination, 1, "/dev/pts/test", at); err != nil {
			t.Fatal(err)
		}
		_, witness, err := s.ObservedWitnessed(store.Witnessed{Agent: "leader@coordination", Role: roles.Coordination, Generation: 1, Terminal: "/dev/pts/test", Text: "plan a greeting"}, "UserPromptSubmit", json.RawMessage(`{"prompt":"plan a greeting"}`), at)
		if err != nil {
			t.Fatal(err)
		}
		job, err = s.JobOpened(store.Job{Repository: repositoryName, Witness: witness, Reading: "Plan a greeting", Mandate: mandate, Criteria: []string{"a plan"}}, false, at)
		if err != nil {
			t.Fatal(err)
		}
	}
	d := &daemon{
		cfg: Config{Paths: paths, ConfigFile: configFile, Pins: testPins("v"), ClaudeVersion: reportsVersion("v"), ClaudeRecord: trustingRecord(t, paths)}, work: context.Background(), store: s,
		leaders: map[string]*leader{}, sessions: map[string]sessionRef{},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	d.applied = readApplied(configFile)
	stand := map[*session.Session]bool{}
	for _, role := range []string{roles.Coordination, roles.Planning, roles.Research, roles.Engineering, roles.Quality} {
		address := roles.LeaderOf(role)
		standIn := &session.Session{}
		stand[standIn] = true
		d.leaders[address.String()] = &leader{address: address, session: standIn, generation: 1}
		d.sessions[role] = sessionRef{address: address, generation: 1}
	}
	if repository {
		repository, err := s.Repository("fixture")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.ensureJobBranch(context.Background(), job, repository); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		d.mu.Lock()
		d.stopping = true
		var running []*session.Session
		for _, l := range d.leaders {
			if l.session != nil && !stand[l.session] {
				running = append(running, l.session)
			}
		}
		d.mu.Unlock()
		for _, s := range running {
			s.Stop(100 * time.Millisecond)
		}
		d.agents.Wait()
	})
	return planningJob{d: d, s: s, job: job, out: out, clone: clone, origin: origin}
}

func TestPlanningsLeaderGivesAWritingAssignmentToAPlanningWorkerWhoRunsTheSkillItNames(t *testing.T) {
	p := newPlanningJob(t, store.MandatePlanningOnly)
	if err := p.d.authorize(Request{Session: "planning", Command: CommandAssign}); err != nil {
		t.Fatalf("Planning's leader may not assign: %v", err)
	}
	a, dispatch, err := p.d.assign(Request{Session: "planning", Command: CommandAssign, Job: p.job.Number, Outcome: "Grill the plan", Criteria: []string{"every branch of the plan is questioned"}, Skill: "grilling"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Owner != "leader@planning" || a.Worker != "worker1@planning" || a.ReadOnly || a.Skill != "grilling" || dispatch.Agent != "worker1@planning" {
		t.Errorf("the assignment is %+v, dispatch %+v, want a writing assignment of leader@planning for worker1@planning running grilling", a, dispatch)
	}
	if want := fmt.Sprintf("asmai/test/job-%d/%d", p.job.Number, a.ID); a.Workspace.Branch != want {
		t.Errorf("the assignment's branch is %q, want %q", a.Workspace.Branch, want)
	}
	if got := git(t, a.Workspace.Path, "rev-parse", "--abbrev-ref", "HEAD"); got != a.Workspace.Branch {
		t.Errorf("the worker's workspace is on %q, want its assignment branch %q", got, a.Workspace.Branch)
	}
	args := waitForFile(t, filepath.Join(p.out, "args"))
	for _, want := range []string{"a Planning worker", "--model\nsonnet", "--plugin-dir"} {
		if !strings.Contains(args, want) {
			t.Errorf("the Planning worker's command line lacks %q: %s", want, args)
		}
	}
}

func TestOnlyTheOwningRolesLeaderAssignsItsWorkAndPlanningNeverValidates(t *testing.T) {
	p := newPlanningJob(t, store.MandatePlanningOnly)
	if _, _, err := p.d.assign(Request{Session: "planning", Command: CommandAssign, Job: p.job.Number, ReadOnly: true, Commit: "abc", Outcome: "validate", Criteria: []string{"c"}}); err == nil || !strings.Contains(err.Error(), "only leader@quality") {
		t.Errorf("Planning's leader assigning a validation returned %v, want it refused: Quality validates", err)
	}
	// Coordination's leader gives writing work for the setup of a repository
	// only, which the assignment itself checks.
	if err := p.d.authorize(Request{Session: "coordination", Command: CommandAssign}); err != nil {
		t.Errorf("Coordination's leader may not assign the work of a repository's setup: %v", err)
	}
	if _, _, err := p.d.assign(Request{Session: "coordination", Command: CommandAssign, Job: p.job.Number, Outcome: "Plan it", Criteria: []string{"planned"}}); err == nil || !strings.Contains(err.Error(), "only leader@planning may give its writing work") {
		t.Errorf("Coordination's leader assigning a planning-only job's work returned %v, want it refused", err)
	}
	for _, command := range []string{CommandAccept, CommandReject, CommandCancel} {
		if err := p.d.authorize(Request{Session: "planning", Command: command}); err != nil {
			t.Errorf("Planning's leader may not run %s: %v", command, err)
		}
	}
}

func TestCoordinationHandsAJobToPlanningAndAPlanningOnlyJobNeverToEngineering(t *testing.T) {
	tested := store.Job{Number: 1, Mandate: store.MandateTestedPR}
	planning := store.Job{Number: 2, Mandate: store.MandatePlanningOnly}
	for _, tt := range []struct {
		name             string
		sender, receiver string
		job              store.Job
		want             string
	}{
		{"Coordination to Planning, in a tested-PR job", roles.Coordination, roles.Planning, tested, ""},
		{"Coordination to Planning, in a planning-only job", roles.Coordination, roles.Planning, planning, ""},
		{"Coordination to Engineering, in a tested-PR job", roles.Coordination, roles.Engineering, tested, ""},
		{"Coordination to Engineering, in a planning-only job", roles.Coordination, roles.Engineering, planning, "authorizes only its planning outcome, never code"},
		{"Engineering to Quality", roles.Engineering, roles.Quality, tested, ""},
		{"Planning to Quality, for an early review in a tested-PR job", roles.Planning, roles.Quality, tested, ""},
		{"Planning to Quality, in a planning-only job", roles.Planning, roles.Quality, planning, "authorizes only its planning outcome: hand it to Planning"},
		{"Planning to Engineering", roles.Planning, roles.Engineering, tested, "send a handoff to another role's leader"},
		{"Coordination to Quality, for an early review", roles.Coordination, roles.Quality, tested, ""},
		{"Quality to Engineering", roles.Quality, roles.Engineering, tested, "send a handoff to another role's leader"},
		{"Engineering to Planning", roles.Engineering, roles.Planning, tested, "send a handoff to another role's leader"},
	} {
		err := checkHandoff(tt.sender, tt.receiver, tt.job)
		if tt.want == "" && err != nil || tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)) {
			t.Errorf("%s: checkHandoff returned %v, want %q", tt.name, err, tt.want)
		}
	}
}

// callWith serves req with handler on a socket of its own and returns what
// the handler answered.
func callWith(t *testing.T, handler func(*net.UnixConn, Request), req Request) Response {
	t.Helper()
	dir, err := os.MkdirTemp("", "h")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "s")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		conn, err := l.AcceptUnix()
		if err != nil {
			return
		}
		defer conn.Close()
		handler(conn, req)
	}()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestAHandoffToPlanningStartsItsLeaderOnDemandAndAPlanningOnlyJobIsNeverHandedToEngineering(t *testing.T) {
	p := newPlanningJob(t, store.MandatePlanningOnly)
	// No work for Planning has arrived, so no leader of its runs.
	delete(p.d.leaders, roles.LeaderOf(roles.Planning).String())
	delete(p.d.sessions, roles.Planning)
	handoff := func(to string) Response {
		return callWith(t, p.d.handoffCommand, Request{
			Session: "coordination", Command: CommandHandoffSend, Job: p.job.Number, Agent: to, Outcome: "A Wayfinder map of the greeting",
			Decisions: "none", Evidence: "none", Constraints: "none", Permissions: "none", Criteria: []string{"a map"},
		})
	}

	if resp := handoff("engineering"); !strings.Contains(resp.Error, "authorizes only its planning outcome, never code") || resp.Handoff != nil {
		t.Errorf("a planning-only job handed to Engineering answered %+v, want it refused", resp)
	}
	if handoffs, err := p.s.HandoffsForJob(p.job.Number); err != nil || len(handoffs) != 0 {
		t.Fatalf("the refused handoff was recorded: %+v (%v)", handoffs, err)
	}

	resp := handoff("planning")
	if resp.Error != "" || resp.Handoff == nil || resp.Handoff.Receiver != "leader@planning" || resp.Dispatch == nil || resp.Dispatch.Agent != "leader@planning" {
		t.Fatalf("a handoff to Planning answered %+v", resp)
	}
	args := waitForFile(t, filepath.Join(p.out, "args"))
	for _, want := range []string{"--model\nopus", "You are Planning's leader"} {
		if !strings.Contains(args, want) {
			t.Errorf("the Planning leader that started on demand lacks %q on its command line: %s", want, args)
		}
	}
	p.d.mu.Lock()
	l := p.d.leaders["leader@planning"]
	running := l != nil && l.session != nil
	p.d.mu.Unlock()
	if !running {
		t.Error("the handoff did not start Planning's leader")
	}
	// The job's brief shows the handoff.
	if brief, err := p.d.store.HandoffsForJob(p.job.Number); err != nil || len(brief) != 1 || brief[0].Receiver != "leader@planning" {
		t.Errorf("the job's handoffs are %+v (%v), want the one to leader@planning", brief, err)
	}
}

// userRecord records a decision of the user's in job, citing the witnessed
// message that opened it, as Coordination does for their words.
func (p planningJob) userRecord(t *testing.T, choice string) store.DecisionRecord {
	t.Helper()
	r, err := p.s.DecisionRecorded(store.DecisionRecord{
		Job: p.job.Number, Leader: "leader@coordination", Source: store.SourceHuman, Cites: p.job.Witness,
		Choice: choice, ReliedOn: "the user's answer", Rationale: "the user chose it", Evidence: "their message",
	}, 1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestAUserInvokedSkillTheMandateDoesNotCallForIsNotStartedWithoutTheUsersDecision(t *testing.T) {
	assign := func(p planningJob, session, skill string, approved int64) error {
		_, _, err := p.d.assign(Request{Session: session, Command: CommandAssign, Job: p.job.Number, Outcome: "Do it", Criteria: []string{"done"}, Skill: skill, Approved: approved})
		return err
	}
	t.Run("a planning-only mandate covers the planning skills and no other", func(t *testing.T) {
		p := newPlanningJob(t, store.MandatePlanningOnly)
		if err := assign(p, "planning", "triage", 0); err == nil || !strings.Contains(err.Error(), "triage starts only on request") || !strings.Contains(err.Error(), "planning-only") || !strings.Contains(err.Error(), "propose it to the user as a decision") {
			t.Fatalf("assigning triage in a planning-only job returned %v, want it refused and proposed to the user", err)
		}
		if assignments, err := p.s.AssignmentsForJob(p.job.Number); err != nil || len(assignments) != 0 {
			t.Fatalf("the refused assignment was recorded: %+v (%v)", assignments, err)
		}
		if err := assign(p, "planning", "wayfinder", 0); err != nil {
			t.Errorf("assigning wayfinder in a planning-only job returned %v, want it covered", err)
		}
	})
	t.Run("a tested-PR mandate covers to-spec, to-tickets, implement and code-review, not wayfinder", func(t *testing.T) {
		p := newPlanningJob(t, store.MandateTestedPR)
		if err := assign(p, "planning", "wayfinder", 0); err == nil || !strings.Contains(err.Error(), "wayfinder starts only on request") || !strings.Contains(err.Error(), "tested-pr") {
			t.Fatalf("assigning wayfinder in a tested-PR job returned %v, want it refused", err)
		}
		if err := assign(p, "engineering", "improve-codebase-architecture", 0); err == nil || !strings.Contains(err.Error(), "starts only on request") {
			t.Fatalf("assigning improve-codebase-architecture in a tested-PR job returned %v, want it refused", err)
		}
		for _, c := range []struct{ session, skill string }{{"planning", "to-spec"}, {"planning", "grilling"}, {"engineering", "implement"}, {"engineering", "tdd"}} {
			if err := assign(p, c.session, c.skill, 0); err != nil {
				t.Errorf("assigning %s in a tested-PR job returned %v, want it covered or model-invoked", c.skill, err)
			}
		}
	})
	t.Run("only the user's own decision of the job starts the skill, and it goes to the worker", func(t *testing.T) {
		p := newPlanningJob(t, store.MandatePlanningOnly)
		second, err := p.s.JobOpened(store.Job{Repository: "fixture", Witness: p.job.Witness, Reading: "Another job", Mandate: store.MandatePlanningOnly, Criteria: []string{"a plan"}}, false, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		elsewhere, err := p.s.DecisionRecorded(store.DecisionRecord{
			Job: second.Number, Leader: "leader@coordination", Source: store.SourceHuman, Cites: second.Witness,
			Choice: "start triage", ReliedOn: "the user's answer", Rationale: "the user chose it", Evidence: "their message",
		}, 1, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		delegated, err := p.s.DecisionRecorded(store.DecisionRecord{Job: p.job.Number, Leader: "leader@planning", Choice: "start triage", ReliedOn: "the mandate", Rationale: "it helps", Evidence: "none"}, 1, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		for name, c := range map[string]struct {
			id   int64
			want string
		}{
			"a delegated decision":       {delegated.ID, "not the user's own decision"},
			"a record that is not there": {999, "does not exist"},
			"another job's decision":     {elsewhere.ID, "of job"},
		} {
			if err := assign(p, "planning", "triage", c.id); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("assigning triage approved by %s returned %v, want it refused: %q", name, err, c.want)
			}
		}
		approval := p.userRecord(t, "start triage")
		a, _, err := p.d.assign(Request{Session: "planning", Command: CommandAssign, Job: p.job.Number, Outcome: "Triage the issues", Criteria: []string{"done"}, Skill: "triage", Approved: approval.ID})
		if err != nil {
			t.Fatalf("assigning triage with the user's own decision returned %v", err)
		}
		if !slices.Contains(a.Records, approval.ID) || a.Skill != "triage" {
			t.Errorf("the assignment is %+v, want it to carry the user's decision %d and name triage", a, approval.ID)
		}
	})
}

var grillingQuestions = []store.Question{
	{ID: "q1", Title: "Who are the users?", Body: "Name them.", Options: []string{"admins", "everyone"}, Recommendation: "everyone"},
	{ID: "q2", Title: "What is out of scope?"},
}

// A worker's grilling or Wayfinder question reaches the user as a Lavish
// decision request through its owning leader and Coordination, and only the
// user's own answer returns to the worker.
func TestAWorkersGrillingOrWayfinderQuestionReachesTheUserInLavishThroughItsLeaderAndOnlyTheUsersOwnAnswerReturns(t *testing.T) {
	for _, role := range []string{roles.Planning, roles.Research} {
		for _, kind := range []string{store.RequestGrilling, store.RequestWayfinder} {
			t.Run(role+"/"+kind, func(t *testing.T) {
				workersQuestionReachesTheUserThroughItsLeader(t, role, kind)
			})
		}
	}
}

func workersQuestionReachesTheUserThroughItsLeader(t *testing.T, role, kind string) {
	f := newLavishFactoryOf(t, role)
	owner := roles.LeaderOf(role).String()

	// The worker asks its owning leader, never the user, and its
	// assignment's work waits.
	asked, dispatch, err := f.d.decisionRequest(Request{Session: "worker0", Command: CommandDecisionRequest, Kind: kind, Questions: grillingQuestions, Content: "I read the spec."})
	if err != nil {
		t.Fatal(err)
	}
	if asked.Recipient != owner || asked.Kind != kind || dispatch == nil || dispatch.Agent != owner || asked.Surface != store.SurfaceLavish {
		t.Fatalf("the worker's request is %+v, dispatch %+v, want a %s request to %s", asked, dispatch, kind, owner)
	}
	if pending, err := f.s.PendingDecisions(); err != nil || len(pending) != 0 {
		t.Fatalf("the user is listed what a worker asks its leader: %+v (%v)", pending, err)
	}

	// The leader never answers it from the records or in words of its
	// own, however it relies on them.
	for name, in := range map[string]Request{
		"in words of its own":      {Answer: "everyone", ReliedOn: "the job's decisions"},
		"from a delegated record":  {Answer: "everyone", ReliedOn: "decision record 1, a delegated decision"},
		"with no answer of anyone": {},
	} {
		in.Session, in.Command, in.Request, in.Version = "credential", CommandDecisionAnswer, asked.Request, 1
		if _, _, err := f.d.decisionAnswer(in); err == nil || !strings.Contains(err.Error(), "needs the user's own answer") {
			t.Errorf("the leader's answer %s returned %v, want it refused: the user's own answer is needed", name, err)
		}
	}
	if current, err := f.s.DecisionRequest(asked.Request); err != nil || current.State != store.RequestPending {
		t.Fatalf("the worker's request is %+v (%v), want it pending still", current, err)
	}

	// The leader carries it to the user, through Coordination, in Lavish.
	carried, dispatch, err := f.d.decisionRequest(Request{Session: "credential", Command: CommandDecisionRequest, Job: f.job.Number, Kind: kind, Carries: asked.Request, Questions: grillingQuestions, Content: "I read the spec.", Blocks: []int64{f.assignments[0].ID}})
	if err != nil {
		t.Fatal(err)
	}
	if carried.Recipient != store.ToUser || carried.Surface != store.SurfaceLavish || carried.Carries != asked.Request || carried.CarriesVersion != 1 || dispatch == nil || dispatch.Agent != "leader@coordination" {
		t.Fatalf("the leader's request is %+v, dispatch %+v, want a Lavish request to the user through Coordination that carries request %d", carried, dispatch, asked.Request)
	}
	page := f.waitFor(t, carried, "the page to be opened", func(r store.DecisionRequest) bool { return r.PageURL != "" })
	html, err := os.ReadFile(page.PageFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Who are the users?", "What is out of scope?", "I read the spec."} {
		if !strings.Contains(string(html), want) {
			t.Errorf("the page lacks %q", want)
		}
	}

	// Until the user has answered, nothing returns to the worker, and
	// neither the answer nor a recorded decision of the leader's stands in.
	relay := Request{Session: "credential", Command: CommandDecisionAnswer, Request: asked.Request, Version: 1, UserAnswer: carried.Request}
	if _, _, err := f.d.decisionAnswer(relay); err == nil || !strings.Contains(err.Error(), "pending") {
		t.Fatalf("returning the user's answer before they gave it returned %v", err)
	}
	if _, _, err := f.d.decisionAnswer(Request{Session: "credential", Command: CommandDecisionAnswer, Request: asked.Request, Version: 1, UserAnswer: carried.Request, Answer: "everyone"}); err == nil || !strings.Contains(err.Error(), "needs the user's own answer") {
		t.Fatalf("typing an answer beside the user's returned %v", err)
	}
	f.userSends(t, page, false, answer(page, "q1", "admins", "only them"))
	f.waitFor(t, page, "the first answer", func(r store.DecisionRequest) bool { return len(r.Answered) == 1 })
	if _, _, err := f.d.decisionAnswer(relay); err == nil || !strings.Contains(err.Error(), "partly answered") {
		t.Fatalf("returning a partly answered page's answer returned %v", err)
	}
	f.userSends(t, page, true, answer(page, "q2", "", "billing"))
	done := f.waitFor(t, page, "the user's answer", func(r store.DecisionRequest) bool { return r.State == store.RequestAnswered })
	if done.AnsweredBy != store.ToUser || done.Cites == 0 {
		t.Fatalf("the user's answer is %+v", done)
	}

	// The leader's own decisions never answer it, and another leader
	// never returns it.
	if _, _, err := f.d.decisionAnswer(Request{Session: "coordination", Command: CommandDecisionAnswer, Request: asked.Request, Version: 1, UserAnswer: carried.Request}); err == nil || !strings.Contains(err.Error(), "goes to "+owner) {
		t.Errorf("another leader returning the user's answer returned %v", err)
	}

	// Then the leader returns it to the worker, as the user gave it.
	returned, back, err := f.d.decisionAnswer(relay)
	if err != nil {
		t.Fatal(err)
	}
	if returned.State != store.RequestAnswered || returned.AnsweredBy != owner || back.Agent != f.assignments[0].Worker {
		t.Fatalf("the returned answer is %+v, dispatch %+v", returned, back)
	}
	for _, want := range []string{"admins", "only them", "billing"} {
		if !strings.Contains(returned.Answer, want) {
			t.Errorf("the worker's answer lacks the user's words %q: %q", want, returned.Answer)
		}
	}
	if returned.Answer != done.Answer || !strings.Contains(returned.ReliedOn, "the user's own answer") || !strings.Contains(returned.ReliedOn, fmt.Sprint(done.Cites)) {
		t.Errorf("the worker's answer is %q relying on %q, want the user's words %q and the Lavish answer %d", returned.Answer, returned.ReliedOn, done.Answer, done.Cites)
	}
	inbox, err := f.s.Inbox(f.assignments[0].Worker, 0, back.ID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var reached bool
	for _, m := range inbox {
		reached = reached || m.Kind == store.MessageDecisionAnswer && strings.Contains(m.Body, "billing")
	}
	if !reached {
		t.Errorf("the worker's inbox lacks the user's answer: %+v", inbox)
	}
	if blocking, err := f.s.BlockingDecisions(f.assignments[0].ID); err != nil || len(blocking) != 0 {
		t.Errorf("the assignment still waits for %+v (%v)", blocking, err)
	}
}

func TestAnAnswerAppliesOnlyToTheVersionOfTheWorkersQuestionTheUserAnswered(t *testing.T) {
	f := newLavishFactoryOf(t, roles.Planning)
	asked, _, err := f.d.decisionRequest(Request{Session: "worker0", Command: CommandDecisionRequest, Kind: store.RequestGrilling, Questions: grillingQuestions})
	if err != nil {
		t.Fatal(err)
	}
	carried, _, err := f.d.decisionRequest(Request{Session: "credential", Command: CommandDecisionRequest, Job: f.job.Number, Kind: store.RequestGrilling, Carries: asked.Request, Questions: grillingQuestions, Blocks: []int64{f.assignments[0].ID}})
	if err != nil {
		t.Fatal(err)
	}
	page := f.waitFor(t, carried, "the page to be opened", func(r store.DecisionRequest) bool { return r.PageURL != "" })
	f.userSends(t, page, true, answer(page, "q1", "admins", ""), answer(page, "q2", "", "billing"))
	f.waitFor(t, page, "the user's answer", func(r store.DecisionRequest) bool { return r.State == store.RequestAnswered })

	// The worker asks again with new evidence before its leader returns the
	// user's answer: what the user answered was the old version.
	newer, _, err := f.d.decisionRequest(Request{Session: "worker0", Command: CommandDecisionRequest, Request: asked.Request, Kind: store.RequestGrilling, Questions: grillingQuestions, Content: "the spec changed"})
	if err != nil || newer.Version != 2 {
		t.Fatalf("a new version = %+v, %v", newer, err)
	}
	if _, _, err := f.d.decisionAnswer(Request{Session: "credential", Command: CommandDecisionAnswer, Request: asked.Request, Version: 2, UserAnswer: carried.Request}); err == nil || !strings.Contains(err.Error(), "carries version 1") {
		t.Errorf("returning the answer to the old version as the new one's returned %v", err)
	}
	if _, _, err := f.d.decisionAnswer(Request{Session: "credential", Command: CommandDecisionAnswer, Request: asked.Request, Version: 1, UserAnswer: carried.Request}); err == nil || !strings.Contains(err.Error(), "replaced") {
		t.Errorf("returning the answer to the replaced version returned %v", err)
	}
	// Carrying the new version asks the user again.
	again, _, err := f.d.decisionRequest(Request{Session: "credential", Command: CommandDecisionRequest, Job: f.job.Number, Request: carried.Request, Kind: store.RequestGrilling, Questions: grillingQuestions})
	if err == nil {
		t.Fatalf("a settled request took a new version: %+v", again)
	}
	second, _, err := f.d.decisionRequest(Request{Session: "credential", Command: CommandDecisionRequest, Job: f.job.Number, Kind: store.RequestGrilling, Carries: asked.Request, Questions: grillingQuestions, Blocks: []int64{f.assignments[0].ID}})
	if err != nil || second.CarriesVersion != 2 || second.Request == carried.Request {
		t.Fatalf("carrying the new version = %+v, %v", second, err)
	}
}

func TestOnlyALeaderCarriesAWorkersRequestAndOnlyItsOwn(t *testing.T) {
	f := newLavishFactoryOf(t, roles.Planning)
	asked, _, err := f.d.decisionRequest(Request{Session: "worker0", Command: CommandDecisionRequest, Kind: store.RequestGrilling, Questions: grillingQuestions})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.d.decisionRequest(Request{Session: "worker1", Command: CommandDecisionRequest, Kind: store.RequestGrilling, Carries: asked.Request, Questions: grillingQuestions}); err == nil || !strings.Contains(err.Error(), "carries nothing") {
		t.Errorf("a worker carrying a request returned %v, want it refused", err)
	}
	if _, _, err := f.d.decisionRequest(Request{Session: "coordination", Command: CommandDecisionRequest, Job: f.job.Number, Kind: store.RequestGrilling, Carries: asked.Request, Questions: grillingQuestions}); err == nil || !strings.Contains(err.Error(), "goes to leader@planning") {
		t.Errorf("another leader carrying the worker's request returned %v, want it refused", err)
	}
}

func TestPlanningsLeaderAcceptsItsWorkersResultMovingTheJobBranchAndNoOtherLeaderDoes(t *testing.T) {
	f := newTwoResultsOf(t, roles.Planning)
	engineering := roles.LeaderOf(roles.Engineering)
	f.d.leaders[engineering.String()] = &leader{address: engineering, session: &session.Session{}, generation: 1}
	f.d.sessions["engineering"] = sessionRef{address: engineering, generation: 1}

	a := f.assignments[0]
	if a.Owner != "leader@planning" || !strings.HasPrefix(a.Worker, "worker") || !strings.HasSuffix(a.Worker, "@planning") {
		t.Fatalf("the assignment is %+v, want one of leader@planning for a Planning worker", a)
	}
	if _, _, err := f.d.decide(Request{Session: "engineering", Command: CommandAccept, Assignment: a.ID, Reasons: []string{"it reads well"}}); err == nil || !strings.Contains(err.Error(), "is owned by leader@planning") {
		t.Fatalf("Engineering's leader accepting Planning's result returned %v, want it refused", err)
	}
	resp, err := f.decide(CommandAccept, a.ID, "the plan's notes are there")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Assignment.State != store.AssignmentAccepted || resp.JobBranch.Tip != f.commits[0] {
		t.Errorf("the acceptance answered %+v and %+v, want the assignment accepted and the job branch at %s", resp.Assignment, resp.JobBranch, f.commits[0])
	}
	if got := git(t, f.clone, "rev-parse", "refs/heads/"+jobBranchFor(f.job)); got != f.commits[0] {
		t.Errorf("the job branch is at %s, want the accepted commit %s", got, f.commits[0])
	}
}

// The skills a mandate calls for are said in the operating guide and in the
// instructions of the leaders that name skills: they say what the daemon
// enforces.
func TestWhatTheInstructionsSayEachMandateCoversIsWhatTheDaemonEnforces(t *testing.T) {
	list := func(names []string) string {
		return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
	}
	coordination, err := roles.LeaderInstructions(roles.Coordination)
	if err != nil {
		t.Fatal(err)
	}
	planning, err := roles.LeaderInstructions(roles.Planning)
	if err != nil {
		t.Fatal(err)
	}
	for mandate, phrase := range map[string]string{
		store.MandateTestedPR:     "a tested-pull-request mandate covers ",
		store.MandatePlanningOnly: "a planning-only mandate covers ",
		store.MandateResearchOnly: "a research-only mandate covers ",
	} {
		want := phrase + list(calledFor[mandate])
		for name, text := range map[string]string{"the operating guide": skills.Guide(), "Coordination's instructions": coordination} {
			if !strings.Contains(strings.ToLower(text), want) {
				t.Errorf("%s lack %q, which is what the %s mandate covers", name, want, mandate)
			}
		}
	}
	research, err := roles.LeaderInstructions(roles.Research)
	if err != nil {
		t.Fatal(err)
	}
	if want := "a planning-only mandate covers " + list(calledFor[store.MandatePlanningOnly]); !strings.Contains(planning, want) {
		t.Errorf("Planning's instructions lack %q", want)
	}
	if want := "a research-only mandate covers " + list(calledFor[store.MandateResearchOnly]); !strings.Contains(research, want) {
		t.Errorf("Research's instructions lack %q", want)
	}
	if want := "a tested-pull-request mandate covers " + list(calledFor[store.MandateTestedPR]); !strings.Contains(research, want) {
		t.Errorf("Research's instructions lack %q", want)
	}
	if want := "a tested-pull-request mandate covers " + list(calledFor[store.MandateTestedPR]); !strings.Contains(planning, want) {
		t.Errorf("Planning's instructions lack %q", want)
	}
}

func TestNarrowingToASkillThatStartsOnlyOnRequestIsRefusedAsNamingItIsAndAResultNamesOnlyTheSkillItRan(t *testing.T) {
	p := newPlanningJob(t, store.MandatePlanningOnly)
	// Narrowing to a skill that is not called for is refused, as naming it is.
	if _, _, err := p.d.assign(Request{Session: "planning", Command: CommandAssign, Job: p.job.Number, Outcome: "Grill the plan", Criteria: []string{"questioned"}, Skill: "grilling", Skills: []string{"grilling", "triage"}}); err == nil || !strings.Contains(err.Error(), "triage starts only on request") {
		t.Errorf("narrowing to triage returned %v, want it refused", err)
	}
	// The user's own decision that names it allows it on the list.
	approval := p.userRecord(t, "start triage")
	a, _, err := p.d.assign(Request{Session: "planning", Command: CommandAssign, Job: p.job.Number, Outcome: "Triage", Criteria: []string{"done"}, Skill: "triage", Skills: []string{"triage", "grilling"}, Approved: approval.ID})
	if err != nil {
		t.Fatalf("assigning triage with the user's decision returned %v", err)
	}
	// A worker's result names a skill that starts only on request only when
	// its assignment named it.
	if err := checkRan([]string{"grilling", "triage"}, a); err != nil {
		t.Errorf("a result naming the skill its assignment named returned %v", err)
	}
	other := a
	other.Skill = "grilling"
	if err := checkRan([]string{"grilling", "triage"}, other); err == nil || !strings.Contains(err.Error(), "triage starts only on request") {
		t.Errorf("a result naming triage, which its assignment did not, returned %v, want it refused", err)
	}
	if err := checkRan([]string{"grilling", "tdd"}, other); err != nil {
		t.Errorf("a result naming model-invoked skills returned %v", err)
	}
}

func TestADecisionOfTheUsersThatDoesNotNameTheSkillDoesNotStartIt(t *testing.T) {
	p := newPlanningJob(t, store.MandatePlanningOnly)
	unrelated := p.userRecord(t, "use the in-memory cache")
	if _, _, err := p.d.assign(Request{Session: "planning", Command: CommandAssign, Job: p.job.Number, Outcome: "Triage", Criteria: []string{"done"}, Skill: "triage", Approved: unrelated.ID}); err == nil || !strings.Contains(err.Error(), "does not name triage") {
		t.Errorf("starting triage with a decision about a cache returned %v, want it refused", err)
	}
}
