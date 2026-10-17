// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/talvor/asmai/internal/config"
)

func TestConfigCheckNamesEveryFolderARepositoryOpensAndRefusesTheClosedOnes(t *testing.T) {
	factoryHome(t)
	home, _ := os.UserHomeDir()
	staffing := string(config.Default("opus"))
	writeConfig(t, staffing+`
[repositories.otman]
origin = "o"
default_branch = "main"

[[repositories.otman.writable]]
path = "~/.cache/go-build"
env = "GOCACHE"

[[repositories.otman.writable]]
path = "/var/cache/tools"
`)
	stdout, stderr, code := runAsmai(t, "config", "check")
	if code != 0 || !strings.Contains(stdout, "has no problems") {
		t.Fatalf("asmai config check exited %d: %s%s", code, stdout, stderr)
	}
	for _, want := range []string{"outside their workspaces", "otman: " + filepath.Join(home, ".cache", "go-build") + " (exported as GOCACHE)", "otman: /var/cache/tools\n"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("asmai config check printed\n%s\nwant %q", stdout, want)
		}
	}
	var report struct {
		Valid    bool `json:"valid"`
		Writable []struct {
			Repository string `json:"repository"`
			Folders    []struct {
				Path     string `json:"path"`
				Resolved string `json:"resolved"`
				Env      string `json:"env"`
			} `json:"folders"`
		} `json:"writable"`
	}
	asmaiJSON(t, &report, "config", "check", "--json")
	if !report.Valid || len(report.Writable) != 1 || len(report.Writable[0].Folders) != 2 || report.Writable[0].Folders[0].Resolved != filepath.Join(home, ".cache", "go-build") || report.Writable[0].Folders[0].Env != "GOCACHE" {
		t.Errorf("asmai config check --json reported %+v", report)
	}

	for path, want := range map[string]string{
		"~":                    "your home directory or one of its ancestors",
		"/":                    "your home directory or one of its ancestors",
		"~/.ssh":               "~/.ssh, where your SSH keys are",
		"~/.claude":            "~/.claude",
		"~/.local/state/asmai": "AsmAI's state directory",
	} {
		writeConfig(t, staffing+"\n[repositories.otman]\norigin = \"o\"\ndefault_branch = \"main\"\n\n[[repositories.otman.writable]]\npath = \""+path+"\"\n")
		_, stderr, code := runAsmai(t, "config", "check")
		if code != 1 || !strings.Contains(stderr, `[repositories.otman] writable folder "`+path+`"`) || !strings.Contains(stderr, want) {
			t.Errorf("asmai config check of %s exited %d printing %q, want a refusal naming %q", path, code, stderr, want)
		}
	}
	writeConfig(t, staffing+"\n[repositories.otman]\norigin = \"o\"\ndefault_branch = \"main\"\n\n[[repositories.otman.writable]]\npath = \"~/.cache/x\"\nenv = \"PATH\"\n")
	if _, stderr, code := runAsmai(t, "config", "check"); code != 1 || !strings.Contains(stderr, `exports "PATH"`) {
		t.Errorf("asmai config check of a PATH folder exited %d printing %q", code, stderr)
	}
}

func TestRepoAddSuggestsAGoRepositorysBuildCacheAndTakesFoldersByHand(t *testing.T) {
	readyFactory(t, idleLeader)
	if _, stderr, code := runAsmai(t, "start"); code != 0 {
		t.Fatalf("asmai start exited %d: %s", code, stderr)
	}
	home, _ := os.UserHomeDir()
	work, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	origin := filepath.Join(work, "fixture.git")
	gitIn(t, work, "init", "--bare", "-b", "trunk", origin)
	gitIn(t, work, "clone", "--quiet", origin, "seed")
	seed := filepath.Join(work, "seed")
	gitIn(t, seed, "checkout", "-b", "trunk")
	gitIn(t, seed, "commit", "--allow-empty", "-m", "first")
	gitIn(t, seed, "push", "--quiet", "origin", "trunk")
	plain := filepath.Join(work, "plain.git")
	gitIn(t, work, "clone", "--quiet", "--bare", origin, plain)
	if err := os.WriteFile(filepath.Join(seed, "go.mod"), []byte("module example.test/fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, seed, "add", "go.mod")
	gitIn(t, seed, "commit", "-m", "module")
	gitIn(t, seed, "push", "--quiet", "origin", "trunk")

	// A refusal happens before anything is cloned or written.
	before := readConfig(t)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--writable", "~/.ssh"}, "~/.ssh, where your SSH keys are"},
		{[]string{"--writable", "~"}, "your home directory or one of its ancestors"},
		{[]string{"--writable", "~/.cache/x=HOME"}, `exports "HOME"`},
		{[]string{"--writable", "~/.cache/x=GIT_DIR"}, `exports "GIT_DIR"`},
		{[]string{"--writable", "~/.cache/x=ASMAI_SLOT"}, `exports "ASMAI_SLOT"`},
		{[]string{"--writable", "~/.cache/x=A", "--writable", "~/.cache/y=A"}, `already exports`},
		{[]string{"--writable", "relative"}, "not an absolute path"},
		{[]string{"--writable", ""}, "--writable: a writable folder is empty"},
	} {
		_, stderr, code := runAsmai(t, append([]string{"repo", "add", origin, "--name", "fixture"}, tc.args...)...)
		if code != 1 || !strings.Contains(stderr, tc.want) {
			t.Errorf("asmai repo add %v exited %d printing %q, want %q", tc.args, code, stderr, tc.want)
		}
	}
	if got := readConfig(t); got != before {
		t.Errorf("refused registrations changed the file to\n%s", got)
	}

	// A Go repository gets its build cache without being asked.
	var added struct {
		Writable []config.Folder `json:"writable"`
	}
	asmaiJSON(t, &added, "repo", "add", origin, "--name", "fixture")
	cache := config.Folder{Path: "~/.cache/go-build", Resolved: filepath.Join(home, ".cache", "go-build"), Env: "GOCACHE"}
	if len(added.Writable) != 1 || added.Writable[0] != cache {
		t.Errorf("repo add answered with the folders %+v, want %+v", added.Writable, cache)
	}
	cfg, problems, err := config.Load(configPath())
	if err != nil || len(problems) != 0 {
		t.Fatalf("the file reads with %v, %v", problems, err)
	}
	if got := cfg.Options["fixture"].Writable; len(got) != 1 || got[0] != (config.Writable{Path: "~/.cache/go-build", Env: "GOCACHE"}) {
		t.Errorf("the entry declares %+v, want ~/.cache/go-build exported as GOCACHE", got)
	}
	stdout, _, code := runAsmai(t, "repo", "show", "fixture")
	if code != 0 || !strings.Contains(stdout, "writable") || !strings.Contains(stdout, cache.Resolved+" (exported as GOCACHE)") {
		t.Errorf("asmai repo show exited %d printing %q, want the folder", code, stdout)
	}
	var shown struct {
		Writable []config.Folder `json:"writable"`
	}
	asmaiJSON(t, &shown, "repo", "show", "fixture", "--json")
	if len(shown.Writable) != 1 || shown.Writable[0] != cache {
		t.Errorf("asmai repo show --json answered with %+v", shown.Writable)
	}
	// The daemon's own write is no change to apply, and the folder is named.
	if stdout, _, code := runIn(t, "", "config", "apply"); code != 0 || !strings.Contains(stdout, "no change") || !strings.Contains(stdout, "fixture: "+cache.Resolved+" (exported as GOCACHE)") {
		t.Errorf("applying after repo add exited %d printing %q, want no change and the folder", code, stdout)
	}

	// A repository of no known language gets none; --writable sets them, and
	// is repeated for each.
	asmaiJSON(t, &added, "repo", "add", plain, "--name", "plain", "--writable", "~/.cache/one=ONE", "--writable", filepath.Join(home, "two"))
	cfg, _, _ = config.Load(configPath())
	want := []config.Writable{{Path: "~/.cache/one", Env: "ONE"}, {Path: filepath.Join(home, "two")}}
	if got := cfg.Options["plain"].Writable; len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("the entry declares %+v, want %+v", got, want)
	}

	// Another folder is a change to apply, which says what it opens.
	o := cfg.Options["plain"]
	o.Writable = append(o.Writable, config.Writable{Path: "~/.cache/three"})
	cfg.Options["plain"] = o
	writeConfig(t, string(config.Render(cfg)))
	stdout, stderr, code := runIn(t, "n\n", "config", "apply")
	if code != 1 || !strings.Contains(stdout, "repositories.plain.writable") || !strings.Contains(stdout, "opens for plain's workers to write: ~/.cache/three") ||
		!strings.Contains(stdout, "plain: "+filepath.Join(home, ".cache", "three")) || !strings.Contains(stderr, "not confirmed") {
		t.Errorf("applying a third folder exited %d printing %q, %q", code, stdout, stderr)
	}
}
