// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/fakeprovider"
	"github.com/talvor/asmai/internal/store"
)

// setupReading is the daemon's reading of the setup job of the fixture
// repository, from which the job branch takes its name.
const setupReading = "Set fixture up for the skills: it has no docs/agents/issue-tracker.md"

func TestRegisteringARepositoryWithoutItsIssueTrackerFileOpensASetupJobAndSaysSo(t *testing.T) {
	gitIdentity(t)
	readyFactory(t, idleLeader)
	if _, stderr, code := runAsmai(t, "start"); code != 0 {
		t.Fatal(stderr)
	}

	// With the file present, registering opens nothing.
	registerWritingFixture(t)
	if jobs := jobsListed(t); len(jobs) != 0 {
		t.Fatalf("registering a repository that has its issue tracker file opened %+v", jobs)
	}
	if _, stderr, code := runAsmai(t, "repo", "remove", "fixture"); code != 0 {
		t.Fatalf("removing it: %s", stderr)
	}

	// Without it, the user is told a job was opened, and where its questions
	// will reach them.
	work, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	origin := filepath.Join(work, "plain.git")
	gitIn(t, work, "init", "--bare", "-b", "main", origin)
	gitIn(t, work, "clone", "--quiet", origin, "plain")
	gitIn(t, filepath.Join(work, "plain"), "commit", "--allow-empty", "-m", "initial")
	gitIn(t, filepath.Join(work, "plain"), "push", "--quiet", "origin", "HEAD:main")
	stdout, stderr, code := runAsmai(t, "repo", "add", origin, "--name", "plain")
	if code != 0 {
		t.Fatalf("asmai repo add exited %d: %s", code, stderr)
	}
	for _, want := range []string{"Registered plain", "plain has no docs/agents/issue-tracker.md, so it is not set up for the skills: Coordination opened job 1 to set it up", "Its questions reach you in Lavish", "`asmai decisions`", "`asmai lavish`"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("asmai repo add printed %q, want it to say %q", stdout, want)
		}
	}
	jobs := jobsListed(t)
	if len(jobs) != 1 || jobs[0].Number != 1 || jobs[0].Repository != "plain" || jobs[0].Mandate != store.MandateSetup || jobs[0].State != store.JobOpen {
		t.Fatalf("the jobs are %+v, want the repository's setup job", jobs)
	}

	// A repository with an open job is not removed.
	if _, stderr, code := runAsmai(t, "repo", "remove", "plain"); code == 0 || !strings.Contains(stderr, "has open job 1") {
		t.Errorf("removing a repository whose setup job is open exited %d printing %q", code, stderr)
	}

	// With --json, the result holds the setup job.
	other := filepath.Join(work, "other.git")
	gitIn(t, work, "init", "--bare", "-b", "main", other)
	gitIn(t, work, "clone", "--quiet", other, "other")
	gitIn(t, filepath.Join(work, "other"), "commit", "--allow-empty", "-m", "initial")
	gitIn(t, filepath.Join(work, "other"), "push", "--quiet", "origin", "HEAD:main")
	var added struct {
		Registered bool       `json:"registered"`
		SetupJob   *store.Job `json:"setup_job"`
	}
	asmaiJSON(t, &added, "repo", "add", other, "--name", "other")
	if !added.Registered || added.SetupJob == nil || added.SetupJob.Number != 2 || added.SetupJob.Mandate != store.MandateSetup {
		t.Errorf("asmai repo add --json printed %+v, want job 2's setup", added)
	}

	// Coordination cannot open a setup job itself.
	if _, stderr, code := runAsmai(t, "job", "open", "--message", "latest", "--repository", "plain", "--reading", "x", "--mandate", "setup", "--criterion", "x"); code == 0 {
		t.Errorf("a setup job was opened by a command: %s", stderr)
	}
}

func jobsListed(t *testing.T) []store.Job {
	t.Helper()
	var listed struct {
		Jobs []store.Job `json:"jobs"`
	}
	asmaiJSON(t, &listed, "jobs")
	return listed.Jobs
}

// A repository registered without its setup is set up by Coordination end to
// end, with the fake provider and gh replaced at their boundary: the daemon
// opens the job and tells Coordination's leader, which gives it to its own
// worker; the worker commits the setup files to its assignment branch;
// Coordination's leader accepts them, asks the daemon to push the job branch
// with no Quality validation and opens a draft pull request with a short body,
// which it records. In a repository registered with --no-ci the daemon marks it
// ready straight away, the job ends, its branches stay on origin and
// Coordination's leader is told the link and that the user declared the CI.
func TestARepositoryWithoutItsSetupIsSetUpByCoordinationAndDeliveredAsASetupPullRequest(t *testing.T) {
	gitIdentity(t)
	testDir := t.TempDir()
	t.Setenv("ASMAI_TEST_DIR", testDir)
	const (
		url    = "https://github.com/talvor/asmai-fixture/pull/7"
		sha    = "[0-9a-f]{40}"
		report = "job 1: set up, pull request ready for review: " + url
	)
	jobBranch := daemon.JobBranchName("test", store.Job{Number: 1, Reading: setupReading})
	t.Setenv(fakeprovider.ScriptEnv+"_COORDINATION_WORKER", filepath.Join(testDir, "worker.jsonl"))

	worker := concat(agentSession, turn(2, "assignment",
		fakeRun(`asmai inbox --dispatch 2`, 0, "assignment 1"),
		fakeRun(`mkdir -p docs/agents && printf '# Issue tracker\n\nIssues live in GitHub.\n' > docs/agents/issue-tracker.md && printf '# Triage labels\n' > docs/agents/triage-labels.md && printf '# Domain docs\n' > docs/agents/domain.md && git add -A && git commit -q -m 'Set the repository up for the skills'`, 0, ""),
		fakeRun(`git push -q origin asmai/test/job-1/1 && asmai effect push "origin/asmai/test/job-1/1@$(git rev-parse HEAD)"`, 0, "recorded: push"),
		fakeRun(`asmai result --evidence 'the issue tracker is GitHub' --artifact docs/agents/issue-tracker.md --test none --check none --gap none --skill setup-matt-pocock-skills --pr-section 'Adds the setup files.'`, 0, "result recorded for assignment 1"),
	), []string{touchFile("w1-done"), idleSession})
	writeFiles(t, testDir, map[string][]string{"worker.jsonl": worker})

	coordination := concat(agentSession,
		turn(1, "setup",
			fakeRun(`asmai inbox --dispatch 1`, 0, "(?s)message [0-9]+ \\| dispatch 1 \\| job 1 \\| setup from daemon\n.*docs/agents/issue-tracker.md, so it is not set up for the skills.*asmai assign --job <job> --skill setup-matt-pocock-skills"),
			fakeRun(`asmai brief 1`, 0, "(?s)Job 1 \\| role: coordination \\| state: open\nRepository: fixture\nRegistration the user ran \\(journal entry\\): [0-9]+\nUser's words: asmai repo add .*Mandate: setup.*Repository setup answers.*- none"),
			// A setup job is handed to no other role.
			fakeRun(`asmai handoff send --job 1 --to engineering --outcome 'Build it' --decisions none --evidence none --constraints none --permissions none --criterion 'built'`, 1, "is the setup of repository fixture, which authorizes only its setup"),
			fakeRun(`asmai assign --job 1 --outcome 'Set fixture up for the skills' --criterion 'the setup files are on the branch' --skill setup-matt-pocock-skills`, 0, "dispatch 2"),
		),
		turn(3, "result",
			fakeRun(`asmai inbox --dispatch 3`, 0, "(?s)result [0-9]+ \\| assignment 1.*docs/agents/issue-tracker.md"),
			awaitFile("w1-done"),
			fakeRun(`asmai accept --assignment 1 --reason 'the setup files are on the branch'`, 0, "fast-forwarded"),
			// Quality is asked for nothing, and only Engineering's leader hands it a branch.
			fakeRun(`asmai handoff send --job 1 --to quality --outcome 'Validate the setup' --decisions none --evidence none --constraints none --permissions none --criterion 'done'`, 1, "only leader@engineering hands a job branch to Quality for validation"),
			fakeRun(`asmai push 1`, 0, "the daemon pushed "+jobBranch),
			fakeRun(`printf 'Setup only: this pull request changes no code.\n\n- docs/agents/issue-tracker.md, triage-labels.md and domain.md.\n' > "$ASMAI_TEST_DIR/body.md"`, 0, ""),
			fakeRun(`gh pr create --draft --base main --head `+jobBranch+` --title 'Set fixture up for the skills' --body-file "$ASMAI_TEST_DIR/body.md"`, 0, "^"+url+"\n$"),
			fakeRun(`asmai effect pull-request `+url, 0, "pull request #7 of job 1 recorded"),
			touchFile("leader-done"),
		),
		turn(4, "delivery",
			fakeRun(`asmai inbox --dispatch 4`, 0, "(?s)message [0-9]+ \\| dispatch 4 \\| job 1 \\| delivery from daemon\nJob 1 is delivered: the setup pull request #7 of repository fixture, "+url+", is ready for review at head "+sha+", straight away, as its repository is declared as having no CI\\. It holds only the repository's setup files.*Quality validated nothing.*AsmAI never merges.*with the link\\. This was the setup of fixture.*CI declaration: .*"),
			`{"screen":"`+report+`\r\n> "}`,
			touchFile("coordination-done"),
		),
		[]string{idleSession},
	)
	bin := filepath.Join(testDir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(fakeCIGH), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	readyFactory(t, coordination)
	calls := filepath.Join(testDir, "gh", "calls")
	t.Cleanup(func() {
		if t.Failed() {
			log, _, _ := runAsmai(t, "log")
			t.Logf("the daemon's log:\n%s", log)
			data, _ := os.ReadFile(calls)
			t.Logf("gh's calls:\n%s", data)
		}
	})
	if _, stderr, code := runAsmai(t, "start"); code != 0 {
		t.Fatal(stderr)
	}
	terminal := inTerminal(t, 100, 25)
	waitFor(t, "Coordination prompt", terminal.shows(">"))

	// The repository is registered with no issue tracker file, and declared
	// as having no CI, so its pull request is delivered straight away.
	fixture := registerFixtureWithoutSetup(t, nil, "--no-ci")
	if err := os.WriteFile(filepath.Join(testDir, "origin"), []byte(fixture.origin), 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "Coordination's leader recording the pull request", touched(testDir, "leader-done"))
	waitFor(t, "Coordination's leader reporting the delivery", touched(testDir, "coordination-done"))
	waitFor(t, "Coordination's report on the user's terminal", terminal.shows(report))
	waitFor(t, "the daemon removing what was local to the job", func() bool { return len(journaled(t, "job.local.removed")) == 1 })

	stateDir := filepath.Join(os.Getenv("HOME"), ".local", "state", "asmai")
	clone, err := filepath.EvalSymlinks(filepath.Join(stateDir, "repositories", "fixture"))
	if err != nil {
		t.Fatal(err)
	}
	head := gitIn(t, fixture.origin, "rev-parse", "refs/heads/"+jobBranch)

	// The job is the setup of the repository, opened by its registration.
	opened := journaled(t, "job.opened")
	if len(opened) != 1 {
		t.Fatalf("jobs opened: %+v", opened)
	}
	job := decode[store.Job](t, opened[0])
	if job.Number != 1 || job.Mandate != store.MandateSetup || job.Repository != "fixture" || job.Reading != setupReading || !strings.HasPrefix(job.Words, "asmai repo add ") {
		t.Errorf("the job is %+v, want the setup of fixture", job)
	}
	if registered := journaled(t, "repository.added"); len(registered) != 1 || job.Witness != registered[0].ID {
		t.Errorf("the job cites %d, want the registration %+v", job.Witness, registered)
	}

	// It was delivered with no check read, ended in the same step, and
	// nobody validated it.
	type deliveryEntry struct {
		Job    int           `json:"job"`
		Number int           `json:"number"`
		URL    string        `json:"url"`
		Head   string        `json:"head"`
		Checks []interface{} `json:"checks"`
		NoCI   bool          `json:"no_ci"`
	}
	delivered := journaled(t, "job.delivered")
	if len(delivered) != 1 {
		t.Fatalf("deliveries: %+v", delivered)
	}
	d := decode[deliveryEntry](t, delivered[0])
	if d.Job != 1 || d.Number != 7 || d.URL != url || d.Head != head || !d.NoCI || len(d.Checks) != 0 {
		t.Errorf("the delivery is %+v, want pull request 7 at %s with no CI", d, head)
	}
	if n := len(journaled(t, "validation.accepted")); n != 0 {
		t.Errorf("a setup job was validated %d times", n)
	}
	if n := len(journaled(t, "handoff.sent")); n != 0 {
		t.Errorf("the setup job was handed to another role %d times", n)
	}
	if n := len(journaled(t, "assignment.created")); n != 1 {
		t.Errorf("the job has %d assignments, want its worker's one", n)
	}
	if got := journaled(t, "assignment.created"); len(got) == 1 && !strings.Contains(string(got[0].Data), "worker1@coordination") {
		t.Errorf("the assignment went to %s, want Coordination's worker", got[0].Data)
	}
	if got := gitIn(t, fixture.origin, "rev-parse", "refs/heads/"+jobBranch); got != head {
		t.Errorf("origin's job branch is at %s, want %s", got, head)
	}
	for _, file := range []string{"docs/agents/issue-tracker.md", "docs/agents/triage-labels.md", "docs/agents/domain.md"} {
		if got := gitIn(t, fixture.origin, "show", "refs/heads/"+jobBranch+":"+file); got == "" {
			t.Errorf("the job branch has no %s", file)
		}
	}

	// The pull request was opened as a draft with the short body, and the
	// daemon marked it ready once, in the job's clone.
	data, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	var readies []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		dir, args, _ := strings.Cut(line, ": ")
		switch {
		case strings.HasPrefix(args, "pr create"):
			if !strings.HasPrefix(args, "pr create --draft ") {
				t.Errorf("the pull request was opened with gh %s, want a draft", args)
			}
		case strings.HasPrefix(args, "pr ready"), strings.HasPrefix(args, "pr edit"):
			readies = append(readies, args)
			if dir != clone {
				t.Errorf("the daemon ran gh %s in %s, not the job's clone", args, dir)
			}
		case strings.HasPrefix(args, "pr merge"), strings.HasPrefix(args, "pr review"):
			t.Errorf("gh ran %q", args)
		}
	}
	if len(readies) != 1 || readies[0] != "pr ready 7" {
		t.Errorf("gh marked the pull request ready with %q, want one pr ready 7", readies)
	}
	body, _ := os.ReadFile(filepath.Join(testDir, "gh", "body-7.md"))
	if !strings.Contains(string(body), "Setup only") || strings.Contains(string(body), "## Summary") || strings.Count(string(body), "\n") > 6 {
		t.Errorf("the pull request's body is %q, want a short setup-only one", body)
	}
	if _, err := os.Lstat(filepath.Join(stateDir, "views", "job-1")); err == nil {
		t.Error("the job's view is still there")
	}
}
