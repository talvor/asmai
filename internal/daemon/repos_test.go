// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/talvor/asmai/internal/config"
	"github.com/talvor/asmai/internal/statedir"
	"github.com/talvor/asmai/internal/store"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@example.test", "GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@example.test")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// remote makes a bare repository with a commit on trunk, standing in for a
// remote.
func remote(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bare := filepath.Join(dir, "fixture.git")
	git(t, dir, "init", "--bare", "-b", "trunk", bare)
	git(t, dir, "init", "-b", "trunk", "seed")
	git(t, filepath.Join(dir, "seed"), "commit", "--allow-empty", "-m", "first")
	git(t, filepath.Join(dir, "seed"), "push", bare, "trunk")
	return bare
}

// repoDaemon runs a daemon with a configuration file of its own.
func repoDaemon(t *testing.T) (p statedir.Paths, configFile string) {
	t.Helper()
	p = stateDir(t)
	configFile = filepath.Join(filepath.Dir(p.Dir), "config", "config.toml")
	runDaemonWith(t, Config{Paths: p, Version: "v1.2.3", ConfigFile: configFile})
	return p, configFile
}

func addRepo(t *testing.T, p statedir.Paths, source, name string) (Response, error) {
	t.Helper()
	return Call(p.Socket, Request{Command: CommandRepoAdd, Source: source, Repository: name})
}

func TestARepositoryIsRegisteredWithItsCloneItsEntryAndItsRecord(t *testing.T) {
	p, configFile := repoDaemon(t)
	origin := remote(t)

	resp, err := addRepo(t, p, origin, "")
	if err != nil {
		t.Fatal(err)
	}
	r := resp.Repository
	if r == nil || r.Name != "fixture" || r.Origin != origin || r.DefaultBranch != "trunk" || r.Location != "" || r.Clone != filepath.Join(p.Repositories, "fixture") {
		t.Fatalf("the registration is %+v, want fixture on trunk, cloned into the state directory", r)
	}
	if got, want := git(t, r.Clone, "rev-parse", "HEAD"), git(t, origin, "rev-parse", "trunk"); got != want {
		t.Errorf("the clone is at %s, want origin's %s", got, want)
	}
	cfg, problems, err := config.Load(configFile)
	if err != nil || len(problems) != 0 || cfg.Repositories["fixture"] != (config.Repository{Origin: origin, DefaultBranch: "trunk"}) {
		t.Errorf("the configuration file reads as %+v, %v, %v; want the repository's entry", cfg, problems, err)
	}
	if entries, _ := os.ReadDir(p.Repositories); len(entries) != 1 {
		t.Errorf("the repositories directory holds %v, want only the clone", entries)
	}

	listed, err := Call(p.Socket, Request{Command: CommandRepoList})
	if err != nil || len(listed.Repositories) != 1 || listed.Repositories[0].Name != "fixture" {
		t.Errorf("the list is %+v, %v", listed.Repositories, err)
	}
	shown, err := Call(p.Socket, Request{Command: CommandRepoShow, Repository: "fixture"})
	if err != nil || shown.Repository == nil || shown.Repository.Name != r.Name || shown.Repository.Clone != r.Clone || shown.Repository.Origin != r.Origin || !shown.Repository.AddedAt.Equal(r.AddedAt) {
		t.Errorf("show answered %+v, %v; want what add recorded", shown.Repository, err)
	}
	if _, err := Call(p.Socket, Request{Command: CommandRepoShow, Repository: "nope"}); err == nil || !strings.Contains(err.Error(), `no repository is registered as "nope"`) {
		t.Errorf("show of an unregistered name returned %v", err)
	}
	var journaled int
	for _, line := range journal(t, p) {
		if strings.HasPrefix(line, "repository.added ") && strings.Contains(line, `"name":"fixture"`) {
			journaled++
		}
	}
	if journaled != 1 {
		t.Errorf("the journal holds %d registrations of fixture, want 1", journaled)
	}
}

func TestARegistrationThatCannotBeCompletedLeavesNothingBehind(t *testing.T) {
	p, configFile := repoDaemon(t)
	origin := remote(t)
	if _, err := addRepo(t, p, origin, "fixture"); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(configFile)

	for name, tc := range map[string]struct{ source, name, want string }{
		"a name already registered": {origin, "fixture", `already registered as "fixture"`},
		"a name that is not valid":  {origin, "a/b", "is not a repository name"},
		"no name to be made":        {"https://example.test/.git", "", "choose one with --name"},
		"an origin that fails":      {"file://" + filepath.Join(t.TempDir(), "missing.git"), "gone", "cloning"},
		"a path that is no repo":    {t.TempDir(), "plain", "is not a git repository"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := addRepo(t, p, tc.source, tc.name)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("registering returned %v, want an error saying %q", err, tc.want)
			}
		})
	}
	after, _ := os.ReadFile(configFile)
	if string(after) != string(before) {
		t.Errorf("the refused registrations changed the configuration file to\n%s", after)
	}
	if entries, _ := os.ReadDir(p.Repositories); len(entries) != 1 {
		t.Errorf("the repositories directory holds %v, want only fixture's clone", entries)
	}
	listed, _ := Call(p.Socket, Request{Command: CommandRepoList})
	if len(listed.Repositories) != 1 {
		t.Errorf("the registry holds %+v, want only fixture", listed.Repositories)
	}
}

func TestAConfigurationFileThatCannotTakeTheEntryIsRefusedBeforeTheClone(t *testing.T) {
	p, configFile := repoDaemon(t)
	origin := remote(t)
	for name, tc := range map[string]struct{ file, want string }{
		"a problem in the file":   {"[roles.coordination]\nleader_provider = \"codex\"\n", "has problems"},
		"an entry the user wrote": {"[repositories.fixture]\norigin = \"o\"\ndefault_branch = \"main\"\n", "already has [repositories.fixture]"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.MkdirAll(filepath.Dir(configFile), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(configFile, []byte(tc.file), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := addRepo(t, p, origin, "")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("registering returned %v, want an error saying %q", err, tc.want)
			}
			if _, statErr := os.Stat(p.Repositories); statErr == nil {
				t.Error("a clone was started for a file that could not take the entry")
			}
			if got, _ := os.ReadFile(configFile); string(got) != tc.file {
				t.Errorf("the file was changed to %q", got)
			}
		})
	}
}

// openJob records a job of repository in the store, as the daemon holds it.
func openJob(t *testing.T, p statedir.Paths, repository, state string) {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+p.Store+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO jobs (repository, state) VALUES (?, ?)`, repository, state); err != nil {
		t.Fatal(err)
	}
}

func TestARepositoryIsRemovedWithItsEntryAndCloneUnlessItHasAnOpenJob(t *testing.T) {
	p, configFile := repoDaemon(t)
	origin := remote(t)
	resp, err := addRepo(t, p, origin, "")
	if err != nil {
		t.Fatal(err)
	}
	clone := resp.Repository.Clone
	openJob(t, p, "fixture", "open")
	openJob(t, p, "fixture", store.JobEnded)

	_, err = Call(p.Socket, Request{Command: CommandRepoRemove, Repository: "fixture"})
	if err == nil || !strings.Contains(err.Error(), "fixture has open job 1; it can be removed when it has ended") {
		t.Fatalf("removing a repository with an open job returned %v, want a refusal naming the job", err)
	}
	if _, statErr := os.Stat(clone); statErr != nil {
		t.Errorf("the refused removal took the clone: %v", statErr)
	}
	if cfg, _, _ := config.Load(configFile); cfg.Repositories["fixture"].Origin != origin {
		t.Error("the refused removal took the configuration entry")
	}
	if listed, _ := Call(p.Socket, Request{Command: CommandRepoList}); len(listed.Repositories) != 1 {
		t.Errorf("the refused removal took the record: %+v", listed.Repositories)
	}

	db, err := sql.Open("sqlite3", "file:"+p.Store+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE jobs SET state = ?`, store.JobEnded); err != nil {
		t.Fatal(err)
	}
	db.Close()

	resp, err = Call(p.Socket, Request{Command: CommandRepoRemove, Repository: "fixture"})
	if err != nil || resp.Repository == nil || resp.Repository.Name != "fixture" {
		t.Fatalf("removing the repository returned %+v, %v", resp.Repository, err)
	}
	if _, statErr := os.Stat(clone); !os.IsNotExist(statErr) {
		t.Errorf("the clone is still there (%v)", statErr)
	}
	if cfg, _, _ := config.Load(configFile); len(cfg.Repositories) != 0 {
		t.Errorf("the configuration file still has %+v", cfg.Repositories)
	}
	if listed, _ := Call(p.Socket, Request{Command: CommandRepoList}); len(listed.Repositories) != 0 {
		t.Errorf("the registry still holds %+v", listed.Repositories)
	}
	if _, err := Call(p.Socket, Request{Command: CommandRepoRemove, Repository: "fixture"}); err == nil || !strings.Contains(err.Error(), "no repository is registered") {
		t.Errorf("removing it again returned %v, want it not registered", err)
	}
	if _, err := addRepo(t, p, origin, ""); err != nil {
		t.Errorf("registering the same repository again after its removal failed: %v", err)
	}
}

func TestRemovingClearsWhatARegistrationLeftThatTheStoreDoesNotRecord(t *testing.T) {
	p, configFile := repoDaemon(t)
	if err := os.MkdirAll(filepath.Dir(configFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configFile, []byte("[repositories.left]\norigin = \"o\"\ndefault_branch = \"main\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	leftover := filepath.Join(p.Repositories, "left")
	if err := os.MkdirAll(leftover, 0o700); err != nil {
		t.Fatal(err)
	}

	if _, err := Call(p.Socket, Request{Command: CommandRepoRemove, Repository: "left"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(leftover); !os.IsNotExist(err) {
		t.Errorf("the leftover clone is still there (%v)", err)
	}
	if got, _ := os.ReadFile(configFile); len(got) != 0 {
		t.Errorf("the configuration file still holds %q", got)
	}
}
