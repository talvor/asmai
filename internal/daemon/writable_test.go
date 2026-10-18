// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/talvor/asmai/internal/config"
	"github.com/talvor/asmai/internal/roles"
)

// declaresWritable gives the repository fixture of a workerFactory the
// writable folders list under home, which the daemon takes ~ to mean, and has
// the stand-in Claude Code also record the GOCACHE it is started with.
func declaresWritable(t *testing.T, d *daemon, home string, list ...config.Writable) {
	t.Helper()
	t.Setenv("HOME", home)
	options := d.applied.Options["fixture"]
	options.Writable = list
	d.applied.Options["fixture"] = options
	provider := filepath.Join(d.cfg.Paths.Providers, "claude-code", "v", "claude")
	script, err := os.ReadFile(provider)
	if err != nil {
		t.Fatal(err)
	}
	script = []byte(strings.Replace(string(script), "exec sleep 60", "printf '%s' \"$GOCACHE\" > \"$ASMAI_TEST_OUT/gocache\"\nexec sleep 60", 1))
	if err := os.WriteFile(provider, script, 0o700); err != nil {
		t.Fatal(err)
	}
}

// guardOf returns the directories the settings in a session's command line
// let its commands write.
func guardOf(t *testing.T, args string) []string {
	t.Helper()
	lines := strings.Split(args, "\n")
	i := slices.Index(lines, "--settings")
	if i < 0 || i+1 >= len(lines) {
		t.Fatalf("the command line has no --settings: %s", args)
	}
	var settings struct {
		Sandbox struct {
			Filesystem struct {
				AllowWrite []string `json:"allowWrite"`
			} `json:"filesystem"`
		} `json:"sandbox"`
	}
	if err := json.Unmarshal([]byte(lines[i+1]), &settings); err != nil {
		t.Fatal(err)
	}
	return settings.Sandbox.Filesystem.AllowWrite
}

func TestAWorkerOfARepositoryWithWritableFoldersMayWriteThemAndGetsTheirVariables(t *testing.T) {
	d, a, out := workerFactory(t)
	home := t.TempDir()
	t.Setenv("GOCACHE", "/the/users/own/cache")
	declaresWritable(t, d, home,
		config.Writable{Path: "~/.cache/go-build", Env: "GOCACHE"},
		config.Writable{Path: filepath.Join(home, "other", "..", "tools")})
	worker := roles.WorkerOf(roles.Engineering, 1)
	d.mu.Lock()
	err := d.ensureAgent(worker)
	d.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	cache, tools := filepath.Join(home, ".cache", "go-build"), filepath.Join(home, "tools")
	if got := waitForFile(t, filepath.Join(out, "gocache")); got != cache {
		t.Errorf("the worker's GOCACHE is %q, want the resolved %q over the one it inherited", got, cache)
	}
	if got, want := guardOf(t, waitForFile(t, filepath.Join(out, "args"))), []string{a.Workspace.Tmp, cache, tools}; !slices.Equal(got, want) {
		t.Errorf("the worker's guard lets it write %v, want %v", got, want)
	}
	for _, folder := range []string{cache, tools} {
		if info, err := os.Stat(folder); err != nil || !info.IsDir() {
			t.Errorf("the writable folder %s is %v (%v), want it made", folder, info, err)
		}
	}
}

func TestAWorkerOfARepositoryWithNoWritableFoldersGetsNoneAndKeepsItsEnvironment(t *testing.T) {
	d, a, out := workerFactory(t)
	t.Setenv("GOCACHE", "/the/users/own/cache")
	declaresWritable(t, d, t.TempDir())
	worker := roles.WorkerOf(roles.Engineering, 1)
	d.mu.Lock()
	err := d.ensureAgent(worker)
	d.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if got := waitForFile(t, filepath.Join(out, "gocache")); got != "/the/users/own/cache" {
		t.Errorf("the worker's GOCACHE is %q, want the inherited one", got)
	}
	if got, want := guardOf(t, waitForFile(t, filepath.Join(out, "args"))), []string{a.Workspace.Tmp}; !slices.Equal(got, want) {
		t.Errorf("the worker's guard lets it write %v, want only its temporary directory %v", got, want)
	}
}

func TestAnotherRepositorysWorkerGetsNoneOfARepositorysWritableFolders(t *testing.T) {
	d, a, out := workerFactory(t)
	home := t.TempDir()
	declaresWritable(t, d, home)
	// The folders belong to a repository the worker's job is not on.
	d.applied.Options["other"] = config.RepositoryOptions{CommitTrailers: true, Writable: []config.Writable{{Path: "~/.cache/go-build", Env: "GOCACHE"}}}
	worker := roles.WorkerOf(roles.Engineering, 1)
	d.mu.Lock()
	err := d.ensureAgent(worker)
	d.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := guardOf(t, waitForFile(t, filepath.Join(out, "args"))), []string{a.Workspace.Tmp}; !slices.Equal(got, want) {
		t.Errorf("the worker's guard lets it write %v, want only %v", got, want)
	}
	if _, err := os.Stat(filepath.Join(home, ".cache")); err == nil {
		t.Error("another repository's folder was made")
	}
}

func TestALeaderGetsNoWritableFolders(t *testing.T) {
	d, _, out := workerFactory(t)
	declaresWritable(t, d, t.TempDir(), config.Writable{Path: "~/.cache/go-build", Env: "GOCACHE"})
	t.Setenv("GOCACHE", "/the/users/own/cache")
	d.mu.Lock()
	err := d.ensureAgent(roles.LeaderOf(roles.Engineering))
	d.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if got := waitForFile(t, filepath.Join(out, "gocache")); got != "/the/users/own/cache" {
		t.Errorf("the leader's GOCACHE is %q, want the inherited one", got)
	}
	if got := guardOf(t, waitForFile(t, filepath.Join(out, "args"))); len(got) != 0 {
		t.Errorf("the leader's guard lets it write %v, want only its working directory", got)
	}
}

func TestAWorkerDoesNotStartWithAWritableFolderTheBoundsRefuse(t *testing.T) {
	for name, tc := range map[string]struct {
		w    config.Writable
		want string
	}{
		"the home directory":          {config.Writable{Path: "~"}, "home directory or one of its ancestors"},
		"ssh":                         {config.Writable{Path: "~/.ssh"}, "~/.ssh"},
		"a variable of its own":       {config.Writable{Path: "~/.cache/x", Env: "ASMAI_SLOT"}, "ASMAI_SLOT"},
		"a path that is not absolute": {config.Writable{Path: "cache"}, "not an absolute path"},
	} {
		t.Run(name, func(t *testing.T) {
			d, _, _ := workerFactory(t)
			declaresWritable(t, d, t.TempDir(), tc.w)
			d.mu.Lock()
			err := d.ensureAgent(roles.WorkerOf(roles.Engineering, 1))
			d.mu.Unlock()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("starting the worker returned %v, want it refused naming %q", err, tc.want)
			}
		})
	}
}

func TestAWorkerDoesNotStartWithAWritableFolderThatIsAClone(t *testing.T) {
	d, _, _ := workerFactory(t)
	declaresWritable(t, d, t.TempDir(), config.Writable{Path: filepath.Join(d.cfg.Paths.Repositories, "fixture", "cache")})
	d.mu.Lock()
	err := d.ensureAgent(roles.WorkerOf(roles.Engineering, 1))
	d.mu.Unlock()
	if err == nil || !strings.Contains(err.Error(), "inside AsmAI's clone of fixture") {
		t.Fatalf("starting the worker returned %v, want it refused for being inside the clone", err)
	}
}

// goRemote is remote with a go.mod on trunk.
func goRemote(t *testing.T) string {
	t.Helper()
	bare := remote(t)
	seed := filepath.Join(filepath.Dir(bare), "seed")
	if err := os.WriteFile(filepath.Join(seed, "go.mod"), []byte("module example.test/fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, seed, "add", "go.mod")
	git(t, seed, "commit", "-m", "module")
	git(t, seed, "push", bare, "trunk")
	return bare
}

func TestRegisteringAGoRepositoryWritesItsBuildCacheIntoItsEntry(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	p, configFile := repoDaemon(t)
	resp, err := addRepo(t, p, goRemote(t), "gopher")
	if err != nil {
		t.Fatal(err)
	}
	want := []config.Folder{{Path: "~/.cache/go-build", Resolved: filepath.Join(home, ".cache", "go-build"), Env: "GOCACHE"}}
	if !slices.Equal(resp.Writable, want) {
		t.Errorf("registering answered with the folders %+v, want %+v", resp.Writable, want)
	}
	cfg, problems, err := config.Load(configFile)
	if err != nil || len(problems) != 0 {
		t.Fatalf("the configuration file reads as %v, %v", problems, err)
	}
	if got := cfg.Options["gopher"].Writable; !slices.Equal(got, []config.Writable{{Path: "~/.cache/go-build", Env: "GOCACHE"}}) {
		t.Errorf("the entry declares %+v, want ~/.cache/go-build exported as GOCACHE", got)
	}
	data, _ := os.ReadFile(configFile)
	if !strings.Contains(string(data), "[[repositories.gopher.writable]]\npath = \"~/.cache/go-build\"\nenv = \"GOCACHE\"\n") {
		t.Errorf("the entry is written as\n%s", data)
	}
	shown, err := Call(p.Socket, Request{Command: CommandRepoShow, Repository: "gopher"})
	if err != nil || !slices.Equal(shown.Writable, want) {
		t.Errorf("show answered with the folders %+v, %v; want %+v", shown.Writable, err, want)
	}
}

func TestRegisteringDetectsWritableFoldersFromTheSelectedBranch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p, configFile := repoDaemon(t)
	plainOrigin := remote(t)
	plainSeed := filepath.Join(filepath.Dir(plainOrigin), "seed")
	git(t, plainSeed, "checkout", "-b", "go-cache")
	if err := os.WriteFile(filepath.Join(plainSeed, "go.mod"), []byte("module example.test/fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, plainSeed, "add", "go.mod")
	git(t, plainSeed, "commit", "-m", "module")
	git(t, plainSeed, "push", plainOrigin, "go-cache")
	withGo, err := Call(p.Socket, Request{Command: CommandRepoAdd, Source: plainOrigin, Repository: "with-go", DefaultBranch: "go-cache"})
	if err != nil {
		t.Fatal(err)
	}
	want := []config.Writable{{Path: "~/.cache/go-build", Env: "GOCACHE"}}
	if withGo.Repository == nil || withGo.Repository.DefaultBranch != "go-cache" || len(withGo.Writable) != 1 || withGo.Writable[0].Path != want[0].Path || withGo.Writable[0].Env != want[0].Env {
		t.Errorf("registration on go-cache answered %+v, want its Go folder", withGo)
	}

	goOrigin := goRemote(t)
	goSeed := filepath.Join(filepath.Dir(goOrigin), "seed")
	git(t, goSeed, "branch", "plain", "trunk~1")
	git(t, goSeed, "push", goOrigin, "plain")
	withoutGo, err := Call(p.Socket, Request{Command: CommandRepoAdd, Source: goOrigin, Repository: "without-go", DefaultBranch: "plain"})
	if err != nil {
		t.Fatal(err)
	}
	if withoutGo.Repository == nil || withoutGo.Repository.DefaultBranch != "plain" || len(withoutGo.Writable) != 0 {
		t.Errorf("registration on plain answered %+v, want no Go folder", withoutGo)
	}

	cfg, problems, err := config.Load(configFile)
	if err != nil || len(problems) != 0 {
		t.Fatalf("the configuration file reads as %v, %v", problems, err)
	}
	if got := cfg.Options["with-go"].Writable; !slices.Equal(got, want) {
		t.Errorf("the selected Go branch declares %+v, want %+v", got, want)
	}
	if got := cfg.Options["without-go"].Writable; len(got) != 0 {
		t.Errorf("the selected plain branch declares %+v, want no folders", got)
	}
}

func TestARepositoryOfNoKnownLanguageGetsNoWritableFolders(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p, configFile := repoDaemon(t)
	resp, err := addRepo(t, p, remote(t), "plain")
	if err != nil || len(resp.Writable) != 0 {
		t.Fatalf("registering answered with the folders %+v, %v; want none", resp.Writable, err)
	}
	if data, _ := os.ReadFile(configFile); strings.Contains(string(data), "[[") {
		t.Errorf("the entry mentions writable folders though it has none:\n%s", data)
	}
}

func TestWritableFoldersGivenByHandReplaceTheOnesALanguageSuggests(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	p, configFile := repoDaemon(t)
	given := []config.Writable{{Path: "~/.cache/one", Env: "ONE"}, {Path: filepath.Join(home, "two")}}
	resp, err := Call(p.Socket, Request{Command: CommandRepoAdd, Source: goRemote(t), Repository: "gopher", Options: &config.RepositoryOptions{CommitTrailers: true, Writable: given}})
	if err != nil || resp.Repository == nil {
		t.Fatal(resp, err)
	}
	cfg, _, err := config.Load(configFile)
	if err != nil || !slices.Equal(cfg.Options["gopher"].Writable, given) {
		t.Errorf("the entry declares %+v, %v; want %+v", cfg.Options["gopher"].Writable, err, given)
	}
}

func TestAFolderTheBoundsRefuseStopsRegistrationBeforeTheClone(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	p, configFile := repoDaemon(t)
	origin := remote(t)
	for name, tc := range map[string]struct {
		w    config.Writable
		want string
	}{
		"the home directory":  {config.Writable{Path: home}, "your home directory or one of its ancestors"},
		"a parent of it":      {config.Writable{Path: filepath.Dir(home)}, "your home directory or one of its ancestors"},
		"ssh":                 {config.Writable{Path: "~/.ssh"}, "~/.ssh, where your SSH keys are"},
		"inside claude":       {config.Writable{Path: "~/.claude/projects"}, "inside ~/.claude"},
		"the claude record":   {config.Writable{Path: "~/.claude.json"}, "~/.claude.json"},
		"the state directory": {config.Writable{Path: p.Dir}, "AsmAI's state directory"},
		"the new clone":       {config.Writable{Path: filepath.Join(p.Repositories, "plain")}, "AsmAI's clone of plain"},
		"HOME":                {config.Writable{Path: "~/.cache/x", Env: "HOME"}, `exports "HOME"`},
		"PATH":                {config.Writable{Path: "~/.cache/x", Env: "PATH"}, `exports "PATH"`},
		"TMPDIR":              {config.Writable{Path: "~/.cache/x", Env: "TMPDIR"}, `exports "TMPDIR"`},
		"the slot variable":   {config.Writable{Path: "~/.cache/x", Env: "ASMAI_SLOT"}, "ASMAI_SLOT"},
		"a git variable":      {config.Writable{Path: "~/.cache/x", Env: "GIT_DIR"}, "GIT_DIR"},
		"not a name":          {config.Writable{Path: "~/.cache/x", Env: "1-x"}, "not an environment variable name"},
		"not absolute":        {config.Writable{Path: "cache"}, "not an absolute path"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Call(p.Socket, Request{Command: CommandRepoAdd, Source: origin, Repository: "plain", Options: &config.RepositoryOptions{CommitTrailers: true, Writable: []config.Writable{tc.w}}})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("registering returned %v, want it refused with %q", err, tc.want)
			}
		})
	}
	_, err := Call(p.Socket, Request{Command: CommandRepoAdd, Source: origin, Repository: "plain", Options: &config.RepositoryOptions{CommitTrailers: true, Writable: []config.Writable{{Path: "~/a", Env: "X"}, {Path: "~/b", Env: "X"}}}})
	if err == nil || !strings.Contains(err.Error(), `already exports`) {
		t.Errorf("two folders exporting one name returned %v, want a refusal", err)
	}
	if entries, _ := os.ReadDir(p.Repositories); len(entries) != 0 {
		t.Errorf("a refused registration left %v in the repositories directory", entries)
	}
	if data, _ := os.ReadFile(configFile); strings.Contains(string(data), "plain") {
		t.Errorf("a refused registration left an entry:\n%s", data)
	}
}

func TestConfigApplyNamesTheFoldersARepositoryOpensAndRefusesTheOnesTheBoundsClose(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	p, configFile := repoDaemon(t)
	if err := os.MkdirAll(filepath.Dir(configFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configFile, []byte(appliedStaffing), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := applyConfig(t, p.Socket, appliedStaffing, true); err != nil {
		t.Fatal(err)
	}
	if _, err := addRepo(t, p, goRemote(t), "gopher"); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	// What registering wrote is what is applied: nothing changes, and the
	// folder is named all the same.
	preview, err := applyConfig(t, p.Socket, string(content), false)
	cache := []config.RepositoryFolders{{Repository: "gopher", Folders: []config.Folder{{Path: "~/.cache/go-build", Resolved: filepath.Join(home, ".cache", "go-build"), Env: "GOCACHE"}}}}
	if err != nil || preview.ConfigApply == nil || len(preview.ConfigApply.Changes) != 0 || !reflect.DeepEqual(preview.ConfigApply.Writable, cache) {
		t.Fatalf("applying the file as it is answered %+v, %v; want no change and %+v named", preview.ConfigApply, err, cache)
	}

	// Another folder is a change, which names what it opens.
	edited := string(content) + "\n[[repositories.gopher.writable]]\npath = \"~/.cache/other\"\n"
	if err := os.WriteFile(configFile, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	preview, err = applyConfig(t, p.Socket, edited, false)
	if err != nil || preview.ConfigApply == nil || len(preview.ConfigApply.Changes) != 1 {
		t.Fatalf("applying a second folder answered %+v, %v", preview.ConfigApply, err)
	}
	if c := preview.ConfigApply.Changes[0]; c.Setting != "repositories.gopher.writable" || !c.Folders || !slices.Equal(c.Gained, []string{`{ path = "~/.cache/other" }`}) || len(c.Lost) != 0 {
		t.Errorf("the change is %+v, want the second folder gained", c)
	}
	if got := preview.ConfigApply.Writable; len(got) != 1 || len(got[0].Folders) != 2 {
		t.Errorf("the preview names %+v, want both folders of gopher", got)
	}

	// A closed place is refused, and nothing is applied.
	for _, bad := range []string{"~/.ssh", "~", p.Dir} {
		refused := string(content) + "\n[[repositories.gopher.writable]]\npath = \"" + bad + "\"\n"
		if err := os.WriteFile(configFile, []byte(refused), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := applyConfig(t, p.Socket, refused, true); err == nil || !strings.Contains(err.Error(), "[repositories.gopher] writable folder") {
			t.Errorf("applying the folder %s returned %v, want it refused", bad, err)
		}
	}
}
