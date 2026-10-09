// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/providers"
	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/session"
	"github.com/talvor/asmai/internal/store"
)

func TestAJobBranchIsNamedForTheJobAndASlugOfItsTitle(t *testing.T) {
	for _, tt := range []struct {
		number  int64
		reading string
		want    string
	}{
		{1, "Add a greeting", "asmai/job-1-add-a-greeting"},
		{12, "  Fix: the  parser's --flag handling!! ", "asmai/job-12-fix-the-parser-s-flag-handling"},
		{3, "Ünïcode only: 日本語", "asmai/job-3-n-code-only"},
		{4, "", "asmai/job-4-job"},
		{5, "!!!", "asmai/job-5-job"},
		{6, strings.Repeat("word ", 20), "asmai/job-6-word-word-word-word-word-word-word-word"},
	} {
		got := JobBranchName(store.Job{Number: tt.number, Reading: tt.reading})
		if got != tt.want {
			t.Errorf("job %d titled %q is named %q, want %q", tt.number, tt.reading, got, tt.want)
		}
	}
	if s := slug(strings.Repeat("abcd ", 30)); len(s) > maxSlug || strings.HasSuffix(s, "-") {
		t.Errorf("a long title makes the slug %q, want at most %d characters and no hyphen last", s, maxSlug)
	}
}

func TestAJobBranchCanBeRecordedAfterItsViewCreationIsRetried(t *testing.T) {
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
	name := JobBranchName(job)
	startedFrom := git(t, clone, "rev-parse", "refs/heads/"+name)
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	d.cfg.Paths.Views = paths.Views
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

// workerFactory is a daemon with Engineering staffed, a pinned Claude Code
// that records where and how it was started and waits, and a store holding a
// job with a branch and one assignment for worker1@engineering, whose
// workspace exists.
func workerFactory(t *testing.T) (d *daemon, a store.Assignment, out string) {
	t.Helper()
	root := t.TempDir()
	out = filepath.Join(root, "out")
	if err := os.MkdirAll(out, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ASMAI_TEST_OUT", out)
	configFile := filepath.Join(root, "config.toml")
	var config strings.Builder
	for _, role := range []string{roles.Coordination, roles.Engineering, roles.Quality} {
		config.WriteString("[roles." + role + "]\nleader_provider = \"claude\"\nleader_model = \"opus\"\nworker_provider = \"claude\"\nworker_model = \"sonnet\"\n")
	}
	if err := os.WriteFile(configFile, []byte(config.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	paths := stateDir(t)
	provider := filepath.Join(paths.Providers, "claude-code", "v", "claude")
	if err := os.MkdirAll(filepath.Dir(provider), 0o700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\npwd -P > \"$ASMAI_TEST_OUT/cwd\"\nprintf '%s' \"$ASMAI_SLOT\" > \"$ASMAI_TEST_OUT/slot\"\nprintf '%s' \"$TMPDIR\" > \"$ASMAI_TEST_OUT/tmpdir\"\nprintf '%s' \"$ASMAI_SESSION\" > \"$ASMAI_TEST_OUT/credential\"\nprintf '%s\\n' \"$@\" > \"$ASMAI_TEST_OUT/args\"\nexec sleep 60\n"
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
	if _, err := s.JobBranchMade(store.JobBranch{Job: job.Number, Repository: "fixture", Name: JobBranchName(job), StartedFrom: commit, View: "/view"}, at); err != nil {
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
	d = &daemon{cfg: Config{Paths: paths, ConfigFile: configFile, Pins: providers.Pins{ClaudeCode: providers.Pin{Version: "v"}}}, leaders: map[string]*leader{}, sessions: map[string]sessionRef{}, store: s, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	t.Cleanup(func() {
		d.mu.Lock()
		d.stopping = true
		var running []*session.Session
		for _, l := range d.leaders {
			if l.session != nil {
				running = append(running, l.session)
			}
		}
		d.mu.Unlock()
		for _, s := range running {
			s.Stop(100 * time.Millisecond)
		}
		d.agents.Wait()
	})
	return d, a, out
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
