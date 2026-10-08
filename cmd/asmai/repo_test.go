// SPDX-License-Identifier: Apache-2.0

package main

import (
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	_ "github.com/ncruces/go-sqlite3/driver"
	_ "github.com/ncruces/go-sqlite3/embed"

	"github.com/talvor/asmai/internal/config"
)

// gitIn runs git in dir as a user with an identity and no configuration of
// the host's, and returns what it printed. The tests may themselves run in a
// git hook, whose variables are dropped.
func gitIn(t *testing.T, dir string, args ...string) string {
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

type repositoryJSON struct {
	Name          string `json:"name"`
	Location      string `json:"location"`
	Origin        string `json:"origin"`
	DefaultBranch string `json:"default_branch"`
	Clone         string `json:"clone"`
}

// journaledRepositories returns the repositories the journal's entries of
// kind record.
func journaledRepositories(t *testing.T, kind string) []repositoryJSON {
	t.Helper()
	var repositories []repositoryJSON
	for _, e := range journaled(t, kind) {
		var r repositoryJSON
		if err := json.Unmarshal(e.Data, &r); err != nil {
			t.Fatal(err)
		}
		repositories = append(repositories, r)
	}
	return repositories
}

func repositoriesListed(t *testing.T) []repositoryJSON {
	t.Helper()
	var list struct {
		Repositories []repositoryJSON `json:"repositories"`
	}
	asmaiJSON(t, &list, "repo", "list")
	return list.Repositories
}

// checkoutState is everything about a checkout that using it would change.
func checkoutState(t *testing.T, checkout string) string {
	t.Helper()
	return strings.Join([]string{
		gitIn(t, checkout, "for-each-ref"),
		gitIn(t, checkout, "status", "--porcelain", "--untracked-files=all"),
		gitIn(t, checkout, "config", "--local", "--list"),
		gitIn(t, checkout, "rev-parse", "HEAD"),
	}, "\n--\n")
}

func TestRepositoriesAreRegisteredListedShownAndRemovedEndToEnd(t *testing.T) {
	stateDir := readyFactory(t, idleLeader)
	if _, stderr, code := runAsmai(t, "start"); code != 0 {
		t.Fatalf("asmai start exited %d: %s", code, stderr)
	}
	home := os.Getenv("HOME")
	configFile := filepath.Join(home, ".config", "asmai", "config.toml")

	// A local bare repository stands in for origin, whose default branch is
	// not main. The user's checkout of it has a commit nowhere else and an
	// untracked file: AsmAI's clone must have neither.
	work, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	origin := filepath.Join(work, "fixture.git")
	gitIn(t, work, "init", "--bare", "-b", "trunk", origin)
	gitIn(t, work, "clone", "--quiet", origin, "fixture")
	checkout := filepath.Join(work, "fixture")
	gitIn(t, checkout, "checkout", "-b", "trunk")
	gitIn(t, checkout, "commit", "--allow-empty", "-m", "published")
	gitIn(t, checkout, "push", "--quiet", "origin", "trunk")
	published := gitIn(t, checkout, "rev-parse", "HEAD")
	gitIn(t, checkout, "commit", "--allow-empty", "-m", "mine alone")
	if err := os.WriteFile(filepath.Join(checkout, "draft.txt"), []byte("not committed"), 0o644); err != nil {
		t.Fatal(err)
	}
	untouched := checkoutState(t, checkout)

	// A relative path is the user's own: it means where they ran the command.
	t.Chdir(work)
	var added struct {
		Registered bool           `json:"registered"`
		Repository repositoryJSON `json:"repository"`
	}
	asmaiJSON(t, &added, "repo", "add", "fixture")
	clone := filepath.Join(stateDir, "repositories", "fixture")
	want := repositoryJSON{Name: "fixture", Location: checkout, Origin: origin, DefaultBranch: "trunk", Clone: clone}
	if !added.Registered || added.Repository != want {
		t.Fatalf("asmai repo add printed %+v, want fixture registered as %+v", added, want)
	}

	// It is recorded in the configuration file, beside what was there, and
	// in the store.
	text, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(text), staffing) {
		t.Errorf("the configuration file is\n%s\nwant the staffing it had untouched, then the entry", text)
	}
	cfg, problems, err := config.Load(configFile)
	if err != nil || len(problems) != 0 || cfg.Repositories["fixture"] != (config.Repository{Location: checkout, Origin: origin, DefaultBranch: "trunk"}) || cfg.Roles["quality"].WorkerModel != "opus" {
		t.Errorf("the configuration file reads as %+v, %v, %v; want the entry and the staffing", cfg, problems, err)
	}
	if got := repositoriesListed(t); len(got) != 1 || got[0] != want {
		t.Errorf("asmai repo list shows %+v, want %+v", got, want)
	}
	if got := journaledRepositories(t, "repository.added"); len(got) != 1 || got[0] != want {
		t.Errorf("the journal holds the registrations %+v, want %+v", got, want)
	}

	// AsmAI's clone is its own, fetched from origin: it holds what origin
	// holds, not what the user's checkout holds beyond it. The checkout is
	// as it was.
	if got := gitIn(t, clone, "rev-parse", "HEAD"); got != published {
		t.Errorf("the clone is at %s, want origin's %s", got, published)
	}
	if got := gitIn(t, clone, "config", "--get", "remote.origin.url"); got != origin {
		t.Errorf("the clone's origin is %s, want %s", got, origin)
	}
	if got := gitIn(t, clone, "status", "--porcelain", "--untracked-files=all"); got != "" {
		t.Errorf("the clone has %q, want a clean checkout of origin", got)
	}
	if got := checkoutState(t, checkout); got != untouched {
		t.Errorf("registering changed the user's checkout:\n%s\nwas\n%s", got, untouched)
	}

	// The same repository, by URL and under another name, has no checkout.
	var byURL struct {
		Repository repositoryJSON `json:"repository"`
	}
	asmaiJSON(t, &byURL, "repo", "add", "file://"+origin, "--name", "second")
	second := repositoryJSON{Name: "second", Origin: "file://" + origin, DefaultBranch: "trunk", Clone: filepath.Join(stateDir, "repositories", "second")}
	if byURL.Repository != second {
		t.Errorf("registering by URL recorded %+v, want %+v", byURL.Repository, second)
	}
	if got := repositoriesListed(t); len(got) != 2 || got[0] != want || got[1] != second {
		t.Errorf("asmai repo list shows %+v, want fixture and second", got)
	}

	// The user's tables show what was recorded, and the clone's path.
	stdout, stderr, code := runAsmai(t, "repo", "list")
	if code != 0 {
		t.Fatalf("asmai repo list exited %d: %s", code, stderr)
	}
	for _, text := range []string{"NAME", "fixture", "second", origin, checkout, clone, "trunk"} {
		if !strings.Contains(stdout, text) {
			t.Errorf("asmai repo list printed\n%s\nwant it to show %s", stdout, text)
		}
	}
	stdout, stderr, code = runAsmai(t, "repo", "show", "fixture")
	if code != 0 {
		t.Fatalf("asmai repo show exited %d: %s", code, stderr)
	}
	var shown []string
	for line := range strings.Lines(stdout) {
		shown = append(shown, strings.Join(strings.Fields(line), " "))
	}
	for _, line := range []string{"name fixture", "location " + checkout, "origin " + origin, "default branch trunk", "clone " + clone} {
		if !slices.Contains(shown, line) {
			t.Errorf("asmai repo show printed\n%s\nwant the line %q", stdout, line)
		}
	}
	if _, stderr, code := runAsmai(t, "repo", "show", "nope"); code != 1 || !strings.Contains(stderr, `no repository is registered as "nope"`) {
		t.Errorf("asmai repo show of an unregistered name exited %d printing %q", code, stderr)
	}

	// A name registers once.
	if _, stderr, code := runAsmai(t, "repo", "add", "fixture"); code != 1 || !strings.Contains(stderr, `already registered as "fixture"`) {
		t.Errorf("registering fixture again exited %d printing %q, want it refused", code, stderr)
	}

	// The registry survives the factory stopping and starting.
	if _, stderr, code := runAsmai(t, "stop"); code != 0 {
		t.Fatalf("asmai stop exited %d: %s", code, stderr)
	}
	if _, stderr, code := runAsmai(t, "start"); code != 0 {
		t.Fatalf("asmai start exited %d: %s", code, stderr)
	}
	if got := repositoriesListed(t); len(got) != 2 {
		t.Errorf("after a restart asmai repo list shows %+v, want both repositories", got)
	}

	// Removal is refused while the repository has an open job, and takes
	// the record, the entry and the clone once it has none.
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(stateDir, "store.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO jobs (repository, state) VALUES ('fixture', 'open')`); err != nil {
		t.Fatal(err)
	}
	if _, stderr, code := runAsmai(t, "repo", "remove", "fixture"); code != 1 || !strings.Contains(stderr, "fixture has open job 1") {
		t.Errorf("removing a repository with an open job exited %d printing %q, want it refused", code, stderr)
	}
	if _, err := os.Stat(clone); err != nil {
		t.Errorf("the refused removal took the clone: %v", err)
	}
	if got := repositoriesListed(t); len(got) != 2 {
		t.Errorf("the refused removal left %+v", got)
	}
	if _, err := db.Exec(`UPDATE jobs SET state = 'ended'`); err != nil {
		t.Fatal(err)
	}

	var removed struct {
		Removed    bool           `json:"removed"`
		Repository repositoryJSON `json:"repository"`
	}
	asmaiJSON(t, &removed, "repo", "remove", "fixture")
	if !removed.Removed || removed.Repository.Name != "fixture" {
		t.Errorf("asmai repo remove printed %+v", removed)
	}
	if _, err := os.Stat(clone); !os.IsNotExist(err) {
		t.Errorf("the clone is still there (%v)", err)
	}
	cfg, problems, err = config.Load(configFile)
	if err != nil || len(problems) != 0 || len(cfg.Repositories) != 1 || cfg.Repositories["second"].Origin != "file://"+origin {
		t.Errorf("after the removal the configuration file reads as %+v, %v, %v; want only second's entry", cfg, problems, err)
	}
	if got := repositoriesListed(t); len(got) != 1 || got[0] != second {
		t.Errorf("after the removal asmai repo list shows %+v, want only second", got)
	}
	if got := journaledRepositories(t, "repository.removed"); len(got) != 1 || got[0] != want {
		t.Errorf("the journal holds the removals %+v, want %+v", got, want)
	}
	if got := checkoutState(t, checkout); got != untouched {
		t.Errorf("removing changed the user's checkout:\n%s\nwas\n%s", got, untouched)
	}
	if _, stderr, code := runAsmai(t, "repo", "remove", "fixture"); code != 1 || !strings.Contains(stderr, "no repository is registered") {
		t.Errorf("removing fixture again exited %d printing %q", code, stderr)
	}
}

func TestTheRepoCommandsNeedTheFactoryRunningAndTheirArguments(t *testing.T) {
	factoryHome(t)
	for _, args := range [][]string{{"repo", "list"}, {"repo", "add", t.TempDir()}, {"repo", "show", "x"}, {"repo", "remove", "x"}} {
		if _, stderr, code := runAsmai(t, args...); code != 1 || !strings.Contains(stderr, "the factory is not running") {
			t.Errorf("asmai %s exited %d printing %q, want the factory not running", strings.Join(args, " "), code, stderr)
		}
	}
	for _, args := range [][]string{{"repo"}, {"repo", "frobnicate"}, {"repo", "add"}, {"repo", "show"}, {"repo", "remove"}, {"repo", "list", "extra"}} {
		if _, stderr, code := runAsmai(t, args...); code != 2 || !strings.Contains(stderr, "asmai repo") {
			t.Errorf("asmai %s exited %d printing %q, want usage", strings.Join(args, " "), code, stderr)
		}
	}
}
