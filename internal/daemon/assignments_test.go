// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/providers"
	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/session"
	"github.com/talvor/asmai/internal/statedir"
	"github.com/talvor/asmai/internal/store"
)

func TestAJobBranchIsNamedForTheJobAndASlugOfItsTitle(t *testing.T) {
	for _, tt := range []struct {
		number  int64
		reading string
		want    string
	}{
		{1, "Add a greeting", "asmai/abc123/job-1-add-a-greeting"},
		{12, "  Fix: the  parser's --flag handling!! ", "asmai/abc123/job-12-fix-the-parser-s-flag-handling"},
		{3, "Ünïcode only: 日本語", "asmai/abc123/job-3-n-code-only"},
		{4, "", "asmai/abc123/job-4-job"},
		{5, "!!!", "asmai/abc123/job-5-job"},
		{6, strings.Repeat("word ", 20), "asmai/abc123/job-6-word-word-word-word-word-word-word-word"},
	} {
		got := JobBranchName("abc123", store.Job{Number: tt.number, Reading: tt.reading})
		if got != tt.want {
			t.Errorf("job %d titled %q is named %q, want %q", tt.number, tt.reading, got, tt.want)
		}
	}
	if s := slug(strings.Repeat("abcd ", 30)); len(s) > maxSlug || strings.HasSuffix(s, "-") {
		t.Errorf("a long title makes the slug %q, want at most %d characters and no hyphen last", s, maxSlug)
	}
}

func TestAJobBranchRetryChecksOriginBeforeReusingTheLocalBranch(t *testing.T) {
	paths := stateDir(t)
	origin := remote(t)
	clone := filepath.Join(paths.Repositories, "fixture")
	if err := os.MkdirAll(filepath.Dir(clone), 0o700); err != nil {
		t.Fatal(err)
	}
	git(t, filepath.Dir(clone), "clone", origin, clone)

	s, err := store.Open(paths.Store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	at := time.Now()
	if err := s.RepositoryAdded(store.Repository{Name: "fixture", Origin: origin, DefaultBranch: "trunk", Clone: clone, AddedAt: at}); err != nil {
		t.Fatal(err)
	}
	if err := s.ConversationEntered("leader@coordination", roles.Coordination, 1, "/dev/pts/test", at); err != nil {
		t.Fatal(err)
	}
	_, witness, err := s.ObservedWitnessed(store.Witnessed{Agent: "leader@coordination", Role: roles.Coordination, Generation: 1, Terminal: "/dev/pts/test", Text: "open a test job"}, "UserPromptSubmit", json.RawMessage(`{"prompt":"open a test job"}`), at)
	if err != nil {
		t.Fatal(err)
	}
	job, err := s.JobOpened(store.Job{Repository: "fixture", Witness: witness, Reading: "Recover branch", Mandate: store.MandateTestedPR, Criteria: []string{"complete"}}, false, at)
	if err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(paths.Dir, "views-blocker")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	d := &daemon{cfg: Config{Paths: paths}, store: s, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	d.cfg.Paths.Views = blocker
	repository, err := s.Repository("fixture")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := d.ensureJobBranch(ctx, job, repository); err == nil {
		t.Fatal("branch creation succeeded despite the blocked view path")
	}
	name := jobBranchFor(job)
	startedFrom := git(t, clone, "rev-parse", "refs/heads/"+name)
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	d.cfg.Paths.Views = paths.Views
	git(t, clone, "push", "origin", "refs/heads/"+name+":refs/heads/"+name)
	if _, err := d.ensureJobBranch(ctx, job, repository); err == nil || !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "branch_prefix") {
		t.Fatalf("retrying while origin has %s gave %v, want a branch_prefix refusal", name, err)
	}
	if _, err := s.JobBranch(job.Number); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("a job branch was recorded despite the refusal: %v", err)
	}
	if _, err := os.Stat(filepath.Join(paths.Views, "job-1")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a view was made despite the refusal: %v", err)
	}
	if got := git(t, clone, "rev-parse", "refs/heads/"+name); got != startedFrom {
		t.Errorf("the local branch moved to %s, want %s", got, startedFrom)
	}
	git(t, clone, "push", "origin", ":refs/heads/"+name)
	branch, err := d.ensureJobBranch(ctx, job, repository)
	if err != nil {
		t.Fatalf("retrying the job branch failed: %v", err)
	}
	if branch.Name != name || branch.StartedFrom != startedFrom {
		t.Errorf("the recovered branch is %+v, want %s started from %s", branch, name, startedFrom)
	}
	if got := git(t, filepath.Join(paths.Views, "job-1"), "rev-parse", "HEAD"); got != startedFrom {
		t.Errorf("the recovered view is at %s, want %s", got, startedFrom)
	}
}

func TestJobOpenRefusesAnOriginBranchInItsJobNumberNamespace(t *testing.T) {
	for _, branchAtOrigin := range []struct {
		name   string
		branch string
	}{
		{"same job branch", "asmai/test/job-1-clash"},
		{"another slug", "asmai/test/job-1-other-title"},
		{"assignment branch", "asmai/test/job-1/1"},
	} {
		t.Run(branchAtOrigin.name, func(t *testing.T) {
			paths := stateDir(t)
			origin := remote(t)
			clone := filepath.Join(paths.Repositories, "fixture")
			if err := os.MkdirAll(filepath.Dir(clone), 0o700); err != nil {
				t.Fatal(err)
			}
			git(t, filepath.Dir(clone), "clone", origin, clone)
			git(t, clone, "push", "origin", "refs/remotes/origin/trunk:refs/heads/"+branchAtOrigin.branch)
			s, err := store.Open(paths.Store)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { s.Close() })
			at := time.Now()
			if err := s.RepositoryAdded(store.Repository{Name: "fixture", Origin: origin, DefaultBranch: "trunk", Clone: clone, AddedAt: at}); err != nil {
				t.Fatal(err)
			}
			if err := s.ConversationEntered("leader@coordination", roles.Coordination, 1, "/dev/pts/test", at); err != nil {
				t.Fatal(err)
			}
			_, witness, err := s.ObservedWitnessed(store.Witnessed{Agent: "leader@coordination", Role: roles.Coordination, Generation: 1, Terminal: "/dev/pts/test", Text: "open a test job"}, "UserPromptSubmit", json.RawMessage(`{"prompt":"open a test job"}`), at)
			if err != nil {
				t.Fatal(err)
			}
			owner := roles.LeaderOf(roles.Coordination)
			d := &daemon{
				cfg: Config{Paths: paths}, work: context.Background(), store: s,
				leaders:  map[string]*leader{owner.String(): {address: owner, session: &session.Session{}, generation: 1}},
				sessions: map[string]sessionRef{"credential": {address: owner, generation: 1}},
				log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
			}
			req := Request{Command: CommandJobOpen, Session: "credential", Repository: "fixture", Witness: witness, Reading: "Clash", Mandate: store.MandateTestedPR, Criteria: []string{"complete"}}
			resp := callWith(t, d.jobCommand, req)
			proposed := JobBranchName(testBranchPrefix, store.Job{Number: 1, Reading: req.Reading})
			if resp.Job != nil || !strings.Contains(resp.Error, proposed) || !strings.Contains(resp.Error, branchAtOrigin.branch) || !strings.Contains(resp.Error, "branch_prefix") {
				t.Fatalf("job open answered %+v, want a refusal naming both branches and branch_prefix", resp)
			}
			if jobs, err := s.Jobs(); err != nil || len(jobs) != 0 {
				t.Fatalf("job open recorded %+v (%v), want no jobs", jobs, err)
			}
			if branches := git(t, clone, "branch", "--list", proposed); branches != "" {
				t.Errorf("job open left a local branch: %s", branches)
			}
			if _, err := os.Stat(filepath.Join(paths.Views, "job-1")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("job open left a view: %v", err)
			}
			git(t, clone, "push", "origin", ":refs/heads/"+branchAtOrigin.branch)
			resp = callWith(t, d.jobCommand, req)
			if resp.Error != "" || resp.Job == nil || resp.Job.Number != 1 {
				t.Fatalf("job open after removing the conflict answered %+v", resp)
			}
			if branch, err := s.JobBranch(resp.Job.Number); err != nil || branch.Name != proposed {
				t.Errorf("job open recorded branch %+v (%v), want %s", branch, err, proposed)
			}
		})
	}
}

func TestJobOpenRecordsTheJobWhenOriginLookupFails(t *testing.T) {
	p := newPlanningJob(t, store.MandatePlanningOnly)
	missing := filepath.Join(t.TempDir(), "unavailable.git")
	git(t, p.clone, "remote", "set-url", "origin", missing)
	req := Request{Command: CommandJobOpen, Session: "coordination", Repository: "fixture", Witness: p.job.Witness, Reading: "Continue planning", Mandate: store.MandatePlanningOnly, Criteria: []string{"a plan"}}
	resp := callWith(t, p.d.jobCommand, req)
	if resp.Error != "" || resp.Job == nil || resp.Job.Number != p.job.Number+1 {
		t.Fatalf("job open during an origin outage answered %+v", resp)
	}
	if branch, err := p.s.JobBranch(resp.Job.Number); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("job open recorded branch %+v (%v), want its first assignment to retry", branch, err)
	}
	git(t, p.clone, "remote", "set-url", "origin", p.origin)
	repository, err := p.s.Repository("fixture")
	if err != nil {
		t.Fatal(err)
	}
	branch, err := p.d.ensureJobBranch(context.Background(), *resp.Job, repository)
	if err != nil || branch.Name != jobBranchFor(*resp.Job) {
		t.Errorf("the job's branch retry returned %+v (%v)", branch, err)
	}
}

func TestLegacyAssignmentWorksWhileOriginIsUnavailable(t *testing.T) {
	p := newPlanningJob(t, store.MandatePlanningOnly)
	previous, err := p.s.JobBranchDropped(p.job.Number, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	oldName := fmt.Sprintf("asmai/job-%d-plan-a-greeting", p.job.Number)
	git(t, p.clone, "branch", oldName, previous.Tip)
	if _, err := p.s.JobBranchMade(store.JobBranch{Job: p.job.Number, Repository: "fixture", Name: oldName, StartedFrom: previous.Tip, View: previous.View}, time.Now()); err != nil {
		t.Fatal(err)
	}
	git(t, p.clone, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "unavailable.git"))
	_, _, err = p.d.assign(Request{Session: "planning", Command: CommandAssign, Job: p.job.Number, Outcome: "Review the plan", Criteria: []string{"a plan"}, Skill: "grilling"})
	if err != nil && !strings.Contains(err.Error(), "starting worker") {
		t.Fatalf("assigning legacy job while origin is unavailable: %v", err)
	}
	assignments, err := p.s.AssignmentsForJob(p.job.Number)
	if err != nil || len(assignments) != 1 {
		t.Fatalf("legacy job recorded assignments %+v (%v), want one", assignments, err)
	}
	a := assignments[0]
	want := fmt.Sprintf("asmai/job-%d/%d", p.job.Number, a.ID)
	if a.Workspace.Branch != want || git(t, a.Workspace.Path, "rev-parse", "--abbrev-ref", "HEAD") != want {
		t.Errorf("legacy assignment uses branch %s, want %s", a.Workspace.Branch, want)
	}
}

func TestRunningDaemonKeepsALegacyJobAssignmentLocalDuringOriginOutage(t *testing.T) {
	p := newJobFactory(t, store.MandateSetup, true)
	previous, err := p.s.JobBranchDropped(p.job.Number, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	oldName := "asmai/job-1-setup"
	git(t, p.clone, "branch", oldName, previous.Tip)
	if _, err := p.s.JobBranchMade(store.JobBranch{Job: p.job.Number, Repository: "fixture", Name: oldName, StartedFrom: previous.Tip, View: previous.View}, time.Now()); err != nil {
		t.Fatal(err)
	}
	git(t, p.clone, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "unavailable.git"))
	credential := runFactoryWithCredential(t, p)
	_, err = Call(p.d.cfg.Paths.Socket, Request{Command: CommandAssign, Session: credential, Job: p.job.Number, Outcome: "Set up the repository", Criteria: []string{"setup is complete"}})
	if err != nil && !strings.Contains(err.Error(), "starting worker") {
		t.Fatalf("assigning the legacy setup job through the running daemon: %v", err)
	}
	assignments, err := p.s.AssignmentsForJob(p.job.Number)
	if err != nil || len(assignments) != 1 {
		t.Fatalf("legacy setup job recorded assignments %+v (%v), want one", assignments, err)
	}
	branch := "asmai/job-1/" + fmt.Sprint(assignments[0].ID)
	if assignments[0].Workspace.Branch != branch || git(t, assignments[0].Workspace.Path, "rev-parse", "--abbrev-ref", "HEAD") != branch {
		t.Fatalf("legacy assignment branch is %s, want %s", assignments[0].Workspace.Branch, branch)
	}
	t.Logf("running daemon made legacy assignment branch %s while origin was unavailable", branch)
}

func TestRunningDaemonChecksOriginWhenRetryingAnUnrecordedLocalJobBranch(t *testing.T) {
	p := newJobFactory(t, store.MandateSetup, true)
	previous, err := p.s.JobBranchDropped(p.job.Number, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	git(t, p.clone, "push", "origin", "refs/heads/"+previous.Name+":refs/heads/"+previous.Name)
	credential := runFactoryWithCredential(t, p)
	_, err = Call(p.d.cfg.Paths.Socket, Request{Command: CommandAssign, Session: credential, Job: p.job.Number, Outcome: "Set up the repository", Criteria: []string{"setup is complete"}})
	if err == nil || !strings.Contains(err.Error(), previous.Name) || !strings.Contains(err.Error(), "branch_prefix") {
		t.Fatalf("retrying the unrecorded local branch through the running daemon gave %v, want a collision refusal", err)
	}
	if assignments, err := p.s.AssignmentsForJob(p.job.Number); err != nil || len(assignments) != 0 {
		t.Fatalf("retrying the collided branch recorded assignments %+v (%v), want none", assignments, err)
	}
	if _, err := p.s.JobBranch(p.job.Number); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("retrying the collided branch recorded a job branch: %v", err)
	}
	t.Logf("running daemon refused retry of %s because origin now has the name", previous.Name)
}

func runFactoryWithCredential(t *testing.T, p planningJob) string {
	t.Helper()
	provider := filepath.Join(p.d.cfg.Paths.Providers, "claude-code", "v", "claude")
	script, err := os.ReadFile(provider)
	if err != nil {
		t.Fatal(err)
	}
	script = []byte(strings.Replace(string(script), "exec sleep 60", "printf '%s' \"$ASMAI_SESSION\" > \"$ASMAI_TEST_OUT/credential\"\nexec sleep 60", 1))
	if err := os.WriteFile(provider, script, 0o700); err != nil {
		t.Fatal(err)
	}
	trusted := map[string]map[string]bool{}
	for _, dir := range []string{p.d.cfg.Paths.Agents, p.d.cfg.Paths.Workspaces, p.clone} {
		folder, _ := providers.TrustFolder(dir)
		trusted[folder] = map[string]bool{"hasTrustDialogAccepted": true}
	}
	record, err := json.Marshal(map[string]any{"projects": trusted})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.d.cfg.ClaudeRecord, record, 0o600); err != nil {
		t.Fatal(err)
	}
	runDaemonWith(t, p.d.cfg)
	credentialPath := filepath.Join(p.out, "credential")
	var credential string
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if data, err := os.ReadFile(credentialPath); err == nil && len(data) != 0 {
			credential = string(data)
			break
		}
	}
	if credential == "" {
		log, _ := os.ReadFile(p.d.cfg.Paths.Log)
		t.Fatalf("the running daemon did not start its coordination leader:\n%s", log)
	}
	return strings.TrimSpace(credential)
}

func TestAssignReturnsAnErrorWhenTheWorkerCannotStart(t *testing.T) {
	paths := stateDir(t)
	origin := remote(t)
	clone := filepath.Join(paths.Repositories, "fixture")
	if err := os.MkdirAll(filepath.Dir(clone), 0o700); err != nil {
		t.Fatal(err)
	}
	git(t, filepath.Dir(clone), "clone", origin, clone)
	s, err := store.Open(paths.Store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	at := time.Now()
	if err := s.RepositoryAdded(store.Repository{Name: "fixture", Origin: origin, DefaultBranch: "trunk", Clone: clone, AddedAt: at}); err != nil {
		t.Fatal(err)
	}
	if err := s.ConversationEntered("leader@coordination", roles.Coordination, 1, "/dev/pts/test", at); err != nil {
		t.Fatal(err)
	}
	_, witness, err := s.ObservedWitnessed(store.Witnessed{Agent: "leader@coordination", Role: roles.Coordination, Generation: 1, Terminal: "/dev/pts/test", Text: "open a test job"}, "UserPromptSubmit", json.RawMessage(`{"prompt":"open a test job"}`), at)
	if err != nil {
		t.Fatal(err)
	}
	job, err := s.JobOpened(store.Job{Repository: "fixture", Witness: witness, Reading: "Start worker", Mandate: store.MandateTestedPR, Criteria: []string{"complete"}}, false, at)
	if err != nil {
		t.Fatal(err)
	}
	var config strings.Builder
	for _, role := range []string{roles.Coordination, roles.Planning, roles.Research, roles.Engineering, roles.Quality} {
		config.WriteString("[roles." + role + "]\nleader_provider = \"claude\"\nleader_model = \"opus\"\nworker_provider = \"claude\"\nworker_model = \"sonnet\"\n")
	}
	fmt.Fprintf(&config, "[repositories.fixture]\norigin = %q\ndefault_branch = \"trunk\"\n", origin)
	configFile := filepath.Join(paths.Dir, "config.toml")
	if err := os.WriteFile(configFile, []byte(config.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	provider := filepath.Join(paths.Providers, "claude-code", "v", "claude")
	if err := os.MkdirAll(filepath.Dir(provider), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(provider, []byte("#!/missing/interpreter\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProviderInstalled(store.ProviderInstall{Name: providers.ClaudeCode, Version: "v", Path: provider, SHA256: strings.Repeat("a", 64), InstalledAt: at}); err != nil {
		t.Fatal(err)
	}
	owner := roles.LeaderOf(roles.Engineering)
	d := &daemon{
		cfg: Config{Paths: paths, ConfigFile: configFile, Pins: testPins("v"), ClaudeVersion: reportsVersion("v"), ClaudeRecord: trustingRecord(t, paths)}, work: context.Background(), store: s,
		leaders:  map[string]*leader{owner.String(): {address: owner, session: &session.Session{}, generation: 1}},
		sessions: map[string]sessionRef{"credential": {address: owner, generation: 1}},
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	d.applied = readApplied(configFile)
	_, _, err = d.assign(Request{Session: "credential", Job: job.Number, Outcome: "Implement it", Criteria: []string{"complete"}})
	if err == nil || !strings.Contains(err.Error(), "starting worker worker1@engineering") {
		t.Fatalf("assign returned %v, want the worker startup error", err)
	}
	assignments, err := s.AssignmentsForJob(job.Number)
	if err != nil || len(assignments) != 1 || assignments[0].Worker != "worker1@engineering" || assignments[0].State != store.AssignmentActive {
		t.Errorf("the recorded assignment is %+v (%v), want one active worker1 assignment", assignments, err)
	}
}

// workerFactory is a daemon with Engineering staffed, a pinned Claude Code
// that records where and how it was started and waits, and a store holding a
// job with a branch and one assignment for worker1@engineering, whose
// workspace exists.
func workerFactory(t *testing.T) (d *daemon, a store.Assignment, out string) {
	t.Helper()
	return workerFactoryAt(t, stateDir(t))
}

func workerFactoryAt(t *testing.T, paths statedir.Paths) (d *daemon, a store.Assignment, out string) {
	t.Helper()
	if err := os.MkdirAll(paths.Providers, 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(paths.Store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	configFile, out := recordingClaude(t, s, paths, "/origin", "main")
	at := time.Now()
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
	job, err := s.JobOpened(store.Job{Repository: "fixture", Witness: witness, Reading: "Add a greeting", Mandate: store.MandateTestedPR, Criteria: []string{"done"}}, false, at)
	if err != nil {
		t.Fatal(err)
	}
	const commit = "1111111111111111111111111111111111111111"
	if _, err := s.JobBranchMade(store.JobBranch{Job: job.Number, Repository: "fixture", Name: jobBranchFor(job), StartedFrom: commit, View: "/view"}, at); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(paths.Workspaces, "job-1", "assignment-1", "repo")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	a, _, err = s.AssignmentCreated(store.NewAssignment{
		ID: 1, Job: job.Number, Owner: "leader@engineering", Worker: "worker1@engineering", Outcome: "outcome", Criteria: []string{"done"},
		Workspace: store.Workspace{Slot: 3, Path: workspace, Tmp: filepath.Join(filepath.Dir(workspace), "tmp"), Branch: "asmai/job-1/1", Base: commit},
	}, at)
	if err != nil {
		t.Fatal(err)
	}
	d = &daemon{leaders: map[string]*leader{}, sessions: map[string]sessionRef{}, store: s, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	startsRecordingClaude(t, d, paths, configFile)
	return d, a, out
}

// recordingClaude installs in s a pinned Claude Code that records where and
// how it was started in the directory out, a worker's arguments also in
// args-<its slot>, and waits, and writes the
// configuration file of a factory with every role staffed on it and fixture
// registered with origin and defaultBranch.
func recordingClaude(t *testing.T, s *store.Store, paths statedir.Paths, origin, defaultBranch string) (configFile, out string) {
	t.Helper()
	root := t.TempDir()
	out = filepath.Join(root, "out")
	if err := os.MkdirAll(out, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ASMAI_TEST_OUT", out)
	configFile = filepath.Join(root, "config.toml")
	var config strings.Builder
	for _, role := range []string{roles.Coordination, roles.Planning, roles.Research, roles.Engineering, roles.Quality} {
		config.WriteString("[roles." + role + "]\nleader_provider = \"claude\"\nleader_model = \"opus\"\nworker_provider = \"claude\"\nworker_model = \"sonnet\"\n")
	}
	config.WriteString("[repositories.fixture]\norigin = \"" + origin + "\"\ndefault_branch = \"" + defaultBranch + "\"\n")
	if err := os.WriteFile(configFile, []byte(config.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	provider := filepath.Join(paths.Providers, "claude-code", "v", "claude")
	if err := os.MkdirAll(filepath.Dir(provider), 0o700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" + answersTheRulesProbe(t) + "pwd -P > \"$ASMAI_TEST_OUT/cwd\"\nprintf '%s' \"$ASMAI_SLOT\" > \"$ASMAI_TEST_OUT/slot\"\nprintf '%s' \"$TMPDIR\" > \"$ASMAI_TEST_OUT/tmpdir\"\nprintf '%s' \"$ASMAI_SESSION\" > \"$ASMAI_TEST_OUT/credential\"\nprintf '%s\\n' \"$@\" > \"$ASMAI_TEST_OUT/args.tmp\" && mv \"$ASMAI_TEST_OUT/args.tmp\" \"$ASMAI_TEST_OUT/args\"\n[ -n \"$ASMAI_SLOT\" ] && printf '%s\\n' \"$@\" > \"$ASMAI_TEST_OUT/args-$ASMAI_SLOT.tmp\" && mv \"$ASMAI_TEST_OUT/args-$ASMAI_SLOT.tmp\" \"$ASMAI_TEST_OUT/args-$ASMAI_SLOT\"\nexec sleep 60\n"
	script = strings.Replace(script, "exec sleep 60\n", "[ -n \"$ASMAI_SLOT\" ] && printf '%s' \"$TMPDIR\" > \"$ASMAI_TEST_OUT/tmpdir-$ASMAI_SLOT\"\nexec sleep 60\n", 1)
	if err := os.WriteFile(provider, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProviderInstalled(store.ProviderInstall{Name: providers.ClaudeCode, Version: "v", Path: provider, SHA256: strings.Repeat("a", 64), InstalledAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	return configFile, out
}

// startsRecordingClaude has d start its agents on the Claude Code
// recordingClaude installed, with the configuration file configFile, and stop
// them when the test ends.
func startsRecordingClaude(t *testing.T, d *daemon, paths statedir.Paths, configFile string) {
	t.Helper()
	d.cfg.Paths, d.cfg.ConfigFile, d.cfg.Pins, d.cfg.ClaudeVersion, d.cfg.ClaudeRecord = paths, configFile, testPins("v"), reportsVersion("v"), trustingRecord(t, paths)
	d.applied = readApplied(configFile)
	// The sessions a test gave its agents before are not real ones to stop.
	given := map[*session.Session]bool{}
	for _, l := range d.leaders {
		given[l.session] = true
	}
	t.Cleanup(func() {
		d.mu.Lock()
		d.stopping = true
		var running []*session.Session
		for _, l := range d.leaders {
			if l.session != nil && !given[l.session] {
				running = append(running, l.session)
			}
		}
		d.mu.Unlock()
		for _, s := range running {
			s.Stop(100 * time.Millisecond)
		}
		d.agents.Wait()
	})
}

func waitForFile(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
			return strings.TrimSpace(string(data))
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s was never written", path)
	return ""
}

func TestAWorkerStartsInItsWorkspaceWithItsSlotAndItsOwnTemporaryDirectory(t *testing.T) {
	d, a, out := workerFactory(t)
	trustWorkerFolder(t, d.cfg.ClaudeRecord, d.cfg.Paths, a.Workspace.Path)
	worker := roles.WorkerOf(roles.Engineering, 1)
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
	if got := waitForFile(t, filepath.Join(out, "slot")); got != "3" {
		t.Errorf("ASMAI_SLOT is %q, want 3", got)
	}
	if got := waitForFile(t, filepath.Join(out, "tmpdir")); got != a.Workspace.Tmp {
		t.Errorf("TMPDIR is %q, want %q", got, a.Workspace.Tmp)
	}
	if info, err := os.Stat(a.Workspace.Tmp); err != nil || !info.IsDir() {
		t.Errorf("the worker's temporary directory is %v (%v), want a directory", info, err)
	}
	args := waitForFile(t, filepath.Join(out, "args"))
	for _, want := range []string{"--model\nsonnet", "an Engineering worker"} {
		if !strings.Contains(args, want) {
			t.Errorf("the worker's command line lacks %q: %s", want, args)
		}
	}
	credential := waitForFile(t, filepath.Join(out, "credential"))
	d.mu.Lock()
	ref, ok := d.sessions[credential]
	l := d.leaders[worker.String()]
	d.mu.Unlock()
	if !ok || ref.address != worker || ref.generation != 1 || l.assignment != a.ID {
		t.Errorf("the worker's credential belongs to %+v, running assignment %d; want %s generation 1 running assignment %d", ref, l.assignment, worker, a.ID)
	}
	agents, err := d.store.Agents()
	if err != nil || len(agents) != 1 || agents[0].Agent != worker.String() || agents[0].Model != "sonnet" || agents[0].Role != roles.Engineering {
		t.Errorf("the store records agents %+v (%v), want worker1@engineering on its worker model", agents, err)
	}

	// Starting it again while it runs changes nothing; another assignment
	// cannot take its place while it carries this one.
	d.mu.Lock()
	err = d.ensureAgent(worker)
	d.mu.Unlock()
	if err != nil || l.generation != 1 {
		t.Errorf("starting a running worker returned %v at generation %d", err, l.generation)
	}
	d.mu.Lock()
	l.assignment = 99
	err = d.ensureAgent(worker)
	d.mu.Unlock()
	if err == nil || !strings.Contains(err.Error(), "still running assignment 99") {
		t.Errorf("starting a worker still running another assignment returned %v", err)
	}
}

func TestAWorkerIsStartedOnlyForAnAssignmentItCarries(t *testing.T) {
	d, _, _ := workerFactory(t)
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, address := range []roles.Address{roles.WorkerOf(roles.Engineering, 2), roles.WorkerOf(roles.Quality, 1), {Name: "bob", Role: roles.Engineering}} {
		if err := d.ensureAgent(address); err == nil {
			t.Errorf("%s was started with no assignment to carry", address)
		}
	}
}

// testBranchPrefix is the branch prefix the test daemons run under.
const testBranchPrefix = "test"

// jobBranchFor names the branch a test daemon makes for j.
func jobBranchFor(j store.Job) string { return JobBranchName(testBranchPrefix, j) }

func TestAnAssignmentBranchSitsBesideItsJobBranchUnderTheSamePrefix(t *testing.T) {
	for _, tt := range []struct{ jobBranch, want string }{
		{"asmai/abc123/job-7-add-a-greeting", "asmai/abc123/job-7/3"},
		{"asmai/my-team/job-7-add-a-greeting", "asmai/my-team/job-7/3"},
		// A job made before branches had a prefix keeps its old names.
		{"asmai/job-7-add-a-greeting", "asmai/job-7/3"},
	} {
		if got := AssignmentBranchName(tt.jobBranch, 7, 3); got != tt.want {
			t.Errorf("an assignment of the job branch %s is named %s, want %s", tt.jobBranch, got, tt.want)
		}
	}
}

func TestTheBranchPrefixIsTheConfiguredOneElseTheKeptIDAcrossRestarts(t *testing.T) {
	paths := statedir.At(filepath.Join(t.TempDir(), "state"))
	d := &daemon{cfg: Config{Paths: paths}}
	first, err := d.branchPrefix()
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := regexp.MatchString(`^[a-z0-9]{6}$`, first); !ok {
		t.Errorf("the made ID is %q, want 6 lower-case letters and digits", first)
	}
	// A restarted daemon reads the kept ID.
	if again, err := (&daemon{cfg: Config{Paths: paths}}).branchPrefix(); err != nil || again != first {
		t.Errorf("after a restart the prefix is %q (%v), want %q", again, err, first)
	}
	// The configuration's prefix overrides the ID, which stays as it was.
	d.applied.BranchPrefix = "mine"
	if got, err := d.branchPrefix(); err != nil || got != "mine" {
		t.Errorf("with branch_prefix set the prefix is %q (%v), want mine", got, err)
	}
	if kept, err := paths.ReadBranchID(); err != nil || kept != first {
		t.Errorf("the kept ID is %q (%v), want %q", kept, err, first)
	}
}

func TestAJobBranchOriginAlreadyHasIsRefusedNamingItAndNoBranchIsMade(t *testing.T) {
	paths := stateDir(t)
	origin := remote(t)
	clone := filepath.Join(paths.Repositories, "fixture")
	if err := os.MkdirAll(filepath.Dir(clone), 0o700); err != nil {
		t.Fatal(err)
	}
	git(t, filepath.Dir(clone), "clone", origin, clone)

	s, err := store.Open(paths.Store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	at := time.Now()
	if err := s.RepositoryAdded(store.Repository{Name: "fixture", Origin: origin, DefaultBranch: "trunk", Clone: clone, AddedAt: at}); err != nil {
		t.Fatal(err)
	}
	if err := s.ConversationEntered("leader@coordination", roles.Coordination, 1, "/dev/pts/test", at); err != nil {
		t.Fatal(err)
	}
	_, witness, err := s.ObservedWitnessed(store.Witnessed{Agent: "leader@coordination", Role: roles.Coordination, Generation: 1, Terminal: "/dev/pts/test", Text: "open a test job"}, "UserPromptSubmit", json.RawMessage(`{"prompt":"open a test job"}`), at)
	if err != nil {
		t.Fatal(err)
	}
	job, err := s.JobOpened(store.Job{Repository: "fixture", Witness: witness, Reading: "Clash", Mandate: store.MandateTestedPR, Criteria: []string{"complete"}}, false, at)
	if err != nil {
		t.Fatal(err)
	}
	// Another install on this origin already made the name.
	name := jobBranchFor(job)
	git(t, clone, "push", "origin", "refs/remotes/origin/trunk:refs/heads/"+name)
	git(t, clone, "fetch", "origin")

	d := &daemon{cfg: Config{Paths: paths}, store: s, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	repository, err := s.Repository("fixture")
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.ensureJobBranch(context.Background(), job, repository)
	if err == nil || !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "branch_prefix") {
		t.Fatalf("opening a job whose branch origin has gave %v, want an error naming %s and branch_prefix", err, name)
	}
	if out := git(t, clone, "branch", "--list", name); out != "" {
		t.Errorf("a branch was made despite the refusal: %q", out)
	}
	if _, err := s.JobBranch(job.Number); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("a job branch was recorded despite the refusal: %v", err)
	}
	// Another prefix opens the job.
	d.applied.BranchPrefix = "other"
	branch, err := d.ensureJobBranch(context.Background(), job, repository)
	if err != nil || branch.Name != JobBranchName("other", job) {
		t.Errorf("under another prefix the branch is %+v (%v), want %s", branch, err, JobBranchName("other", job))
	}
}
