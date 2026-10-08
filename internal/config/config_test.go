// SPDX-License-Identifier: Apache-2.0

package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const staffed = `# The factory's staffing.
[roles.coordination]
leader_provider = "claude"
leader_model = "opus"
worker_provider = "claude"
worker_model = "sonnet"

[roles.engineering]
leader_provider = "claude"
leader_model = "opus"
worker_provider = "claude"
worker_model = "opus"

[roles]
quality = { leader_provider = "claude", leader_model = "opus", worker_provider = "claude", worker_model = "haiku" }
`

func TestTheStaffingOfEachRoleIsRead(t *testing.T) {
	cfg, problems := Parse([]byte(staffed))
	if len(problems) != 0 {
		t.Fatalf("Parse found %v", problems)
	}
	want := map[string]Staffing{
		"coordination": {"claude", "opus", "claude", "sonnet"},
		"engineering":  {"claude", "opus", "claude", "opus"},
		"quality":      {"claude", "opus", "claude", "haiku"},
	}
	for role, s := range want {
		if cfg.Roles[role] != s {
			t.Errorf("[roles.%s] is %+v, want %+v", role, cfg.Roles[role], s)
		}
	}
	if missing := cfg.MissingStaffing([]string{"coordination", "engineering", "quality"}); len(missing) != 0 {
		t.Errorf("MissingStaffing found %v", missing)
	}
}

func TestAFileThisAsmaiCannotHonourNamesTheLineAndTheFix(t *testing.T) {
	for name, tc := range map[string]struct {
		file    string
		line    int
		problem string
		fix     string
	}{
		"codex": {"[roles.coordination]\n\nleader_provider = \"codex\"\n", 3,
			`leader_provider in [roles.coordination] is "codex", but this asmai runs only "claude"`, `write leader_provider = "claude"`},
		"an unknown provider": {"[roles.quality]\nworker_provider = \"gpt\"\n", 2,
			`"gpt" is not a provider`, `write worker_provider = "claude"`},
		"a misspelt field": {"[roles.engineering]\nleader_model = \"opus\"\nleader_providr = \"claude\"\n", 3,
			"leader_providr in [roles.engineering] is not a setting this asmai reads", "correct its name"},
		"an unknown role": {"\n[roles.testing]\nleader_model = \"opus\"\n", 2,
			"[roles.testing] names no role", "such as [roles.coordination]"},
		"a later table": {"[limits]\nleader_restarts = 3\n", 1,
			"[limits] is not a table this asmai reads", "remove [limits]"},
		"not a string": {"[roles.coordination]\nleader_model = 4\n", 2,
			"leader_model in [roles.coordination] is a number, not a string", `such as leader_model = "opus"`},
		"an empty model": {"[roles.coordination]\nleader_model = \" \"\n", 2,
			"leader_model in [roles.coordination] is empty", "write the model"},
		"not TOML": {"[roles.coordination]\nleader_model = opus\n", 2,
			"the file is not valid TOML", "correct the TOML on that line"},
		"a key defined twice": {"[roles.coordination]\nleader_model = \"a\"\nleader_model = \"b\"\n", 3,
			"the file is not valid TOML", "correct the TOML on that line"},
		"a top-level key": {"leader_model = \"opus\"\n", 1,
			"leader_model is not a setting this asmai reads", "remove leader_model"},
	} {
		t.Run(name, func(t *testing.T) {
			_, problems := Parse([]byte(tc.file))
			if len(problems) != 1 {
				t.Fatalf("Parse found %v, want one problem", problems)
			}
			p := problems[0]
			if p.Line != tc.line || !strings.Contains(p.Problem, tc.problem) || !strings.Contains(p.Fix, tc.fix) {
				t.Errorf("the problem is line %d: %q, fix %q; want line %d: %q, fix %q", p.Line, p.Problem, p.Fix, tc.line, tc.problem, tc.fix)
			}
		})
	}
}

func TestMissingStaffingNamesEachFieldAndRole(t *testing.T) {
	cfg, problems := Parse([]byte("[roles.coordination]\nleader_provider = \"claude\"\nleader_model = \"opus\"\nworker_provider = \"claude\"\n"))
	if len(problems) != 0 {
		t.Fatal(problems)
	}
	var got []string
	for _, p := range cfg.MissingStaffing([]string{"coordination", "quality"}) {
		got = append(got, p.Problem+" / "+p.Fix)
	}
	want := []string{
		`[roles.coordination] has no worker_model / add worker_model under [roles.coordination], such as worker_model = "opus"`,
		`there is no [roles.quality], so Quality is not staffed / add [roles.quality] with leader_provider = "claude", leader_model, worker_provider = "claude" and worker_model`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("MissingStaffing found\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestLoadReadsTheFileOrSaysItIsMissing(t *testing.T) {
	path := DefaultPath(t.TempDir())
	if _, _, err := Load(path); !errors.Is(err, ErrMissing) {
		t.Errorf("Load of a missing file returned %v, want ErrMissing", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(staffed), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, problems, err := Load(path)
	if err != nil || len(problems) != 0 || cfg.Roles["quality"].WorkerModel != "haiku" {
		t.Errorf("Load returned %+v, %v, %v", cfg, problems, err)
	}
	if !strings.HasSuffix(path, filepath.Join(".config", "asmai", "config.toml")) {
		t.Errorf("the configuration file is %s, want ~/.config/asmai/config.toml", path)
	}
}

func TestTheRepositoriesOfTheFileAreRead(t *testing.T) {
	cfg, problems := Parse([]byte(`[repositories.otman]
location = "/home/me/src/otman"
origin = "git@github.com:me/otman.git"
default_branch = "main"

[repositories.fixture]
location = ""
origin = 'https://example.test/fixture.git'
default_branch = "trunk"

[repositories]
inline = { origin = "o", default_branch = "main" }
`))
	if len(problems) != 0 {
		t.Fatalf("Parse found %v", problems)
	}
	want := map[string]Repository{
		"otman":   {"/home/me/src/otman", "git@github.com:me/otman.git", "main"},
		"fixture": {"", "https://example.test/fixture.git", "trunk"},
		"inline":  {"", "o", "main"},
	}
	for name, r := range want {
		if cfg.Repositories[name] != r {
			t.Errorf("[repositories.%s] is %+v, want %+v", name, cfg.Repositories[name], r)
		}
	}
}

func TestARepositoryEntryThisAsmaiCannotHonourNamesTheLineAndTheFix(t *testing.T) {
	for name, tc := range map[string]struct {
		file    string
		line    int
		problem string
		fix     string
	}{
		"a later field": {"[repositories.otman]\norigin = \"o\"\nnotes = \"x\"\n", 3,
			"notes in [repositories.otman] is not a setting this asmai reads; the fields are location, origin and default_branch", "remove notes"},
		"not a string": {"[repositories.otman]\ndefault_branch = 4\n", 2,
			"default_branch in [repositories.otman] is a number, not a string", "in double quotes"},
		"an empty origin": {"[repositories.otman]\norigin = \"\"\n", 2,
			"origin in [repositories.otman] is empty", "asmai repo add"},
		"a name that cannot be a directory": {"[repositories.\"a/b\"]\norigin = \"o\"\n", 1,
			`"a/b" is not a repository name`, "rename the repository"},
		"a nested table": {"[repositories.otman.extra]\norigin = \"o\"\n", 1,
			"[repositories.otman.extra] is not a table this asmai reads", "remove [repositories.otman.extra]"},
	} {
		t.Run(name, func(t *testing.T) {
			_, problems := Parse([]byte(tc.file))
			if len(problems) != 1 {
				t.Fatalf("Parse found %v, want one problem", problems)
			}
			p := problems[0]
			if p.Line != tc.line || !strings.Contains(p.Problem, tc.problem) || !strings.Contains(p.Fix, tc.fix) {
				t.Errorf("the problem is line %d: %q, fix %q; want line %d: %q, fix %q", p.Line, p.Problem, p.Fix, tc.line, tc.problem, tc.fix)
			}
		})
	}
}

func TestRepositoryNames(t *testing.T) {
	for _, name := range []string{"otman", "my-repo_2", "A1"} {
		if err := CheckRepositoryName(name); err != nil {
			t.Errorf("CheckRepositoryName(%q) = %v, want it valid", name, err)
		}
	}
	for _, name := range []string{"", "a.b", "a/b", "-a", "_a", "a b", "..", "é"} {
		if err := CheckRepositoryName(name); err == nil {
			t.Errorf("CheckRepositoryName(%q) is valid, want it refused", name)
		}
	}
}

func TestScratchConfigFileOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	t.Setenv("ASMAI_CONFIG_FILE", path)
	got, err := ActivePath(t.TempDir())
	if err != nil || got != path {
		t.Fatalf("ActivePath() = %q, %v; want %q", got, err, path)
	}
	t.Setenv("ASMAI_CONFIG_FILE", "relative.toml")
	if _, err := ActivePath(t.TempDir()); err == nil {
		t.Fatal("relative config path was accepted")
	}
}
