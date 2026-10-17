// SPDX-License-Identifier: Apache-2.0

package config

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/roles"
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
		"an unknown table": {"[telemetry]\nenabled = true\n", 1,
			"[telemetry] is not a table this asmai reads", "remove [telemetry]"},
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
		`there is no [roles.quality], so Quality is not staffed / add [roles.quality] with leader_provider = "claude", leader_model = "opus", worker_provider = "claude" and worker_model = "opus"`,
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
		"a misspelt field": {"[repositories.otman]\norigin = \"o\"\nnote = \"x\"\n", 3,
			"note in [repositories.otman] is not a setting this asmai reads; the fields are location, origin, default_branch, notes,", "remove note"},
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

// The file `asmai init` writes staffs every role M1 runs, points to the
// README's section on the file, and registers no repository.
func TestTheDefaultFileStaffsEveryRoleAndRegistersNoRepository(t *testing.T) {
	data := Default("opus")
	cfg, problems := Parse(data)
	if len(problems) != 0 {
		t.Fatalf("the default file has problems: %v", problems)
	}
	if missing := cfg.MissingStaffing(roles.Staffed); len(missing) != 0 {
		t.Errorf("the default file leaves staffing out: %v", missing)
	}
	for _, role := range roles.Staffed {
		if got := cfg.Roles[role]; got != exampleStaffing() {
			t.Errorf("the default file staffs %s as %+v, want %+v", role, got, exampleStaffing())
		}
	}
	if len(cfg.Repositories) != 0 {
		t.Errorf("the default file registers %v", cfg.Repositories)
	}
	if got := cfg.Providers["claude"].DefaultModel; got != "opus" {
		t.Errorf("the default file gives Claude model %q, want opus", got)
	}
	if sonnet, problems := Parse(Default("sonnet")); len(problems) != 0 || sonnet.Providers["claude"].DefaultModel != "sonnet" {
		t.Errorf("the Sonnet default file has model %q and problems %v", sonnet.Providers["claude"].DefaultModel, problems)
	}
	if !strings.HasPrefix(string(data), "# ") || !strings.Contains(string(data), "README") || !strings.Contains(string(data), "#the-configuration-file") {
		t.Errorf("the default file opens\n%s\nwant a comment pointing to the README's section on the file", data)
	}
}

func TestCreateDefaultWritesOnlyWhenThereIsNoFile(t *testing.T) {
	path := DefaultPath(t.TempDir())
	created, err := CreateDefault(path, "opus")
	if err != nil || !created {
		t.Fatalf("CreateDefault of a missing file = %t, %v, want it written", created, err)
	}
	if got, _ := os.ReadFile(path); string(got) != string(Default("opus")) {
		t.Errorf("the file holds\n%s\nwant the default", got)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("the file's mode is %v (%v), want 0600", info.Mode(), err)
	}
	mine := []byte("# mine\n[roles.coordination]\nleader_provider = \"codex\"\n")
	if err := os.WriteFile(path, mine, 0o644); err != nil {
		t.Fatal(err)
	}
	created, err = CreateDefault(path, "sonnet")
	if err != nil || created {
		t.Errorf("CreateDefault of an existing file = %t, %v, want nothing written", created, err)
	}
	if got, _ := os.ReadFile(path); string(got) != string(mine) {
		t.Errorf("an existing file became\n%s\nwant it unchanged", got)
	}
}

func TestEveryFieldOfTheSchemaIsReadWithItsDefault(t *testing.T) {
	cfg, problems := Parse([]byte("[roles.engineering]\nleader_model = \"opus\"\n[repositories.otman]\norigin = \"o\"\n"))
	if len(problems) != 0 {
		t.Fatal(problems)
	}
	if cfg.Limits != DefaultLimits() || cfg.Logs != DefaultLogs() {
		t.Errorf("defaults are %+v and %+v", cfg.Limits, cfg.Logs)
	}
	if got := cfg.Providers[Claude]; got != (Provider{WorkerCap: 3}) {
		t.Errorf("[providers.claude] defaults to %+v", got)
	}
	if got := cfg.Options["otman"]; !got.CommitTrailers || got.Notes != "" || got.NoCI || got.ChecksOneAtATime || got.RepositorySkills != nil {
		t.Errorf("[repositories.otman] defaults to %+v", got)
	}
	d := DefaultLimits()
	if d.LeaderIdleGrace != 10*time.Minute || d.FreeSpaceFloor != 5_000_000_000 || DefaultLogs().RecordingRetention != 14*24*time.Hour || DefaultLogs().DaemonFileSize != 20_000_000 {
		t.Errorf("defaults differ from rule 33: %+v %+v", d, DefaultLogs())
	}
}

func TestEveryFieldIsRead(t *testing.T) {
	cfg, problems := Parse([]byte(`[limits]
leader_idle_grace = "1h"
leader_restarts = 0
leader_restart_window = "30m"
silence_timeout = "45s"
drain_timeout = "2d"
side_effect_free_retries = 4
provider_error_resumes = 6
provider_error_wait = "1m"
provider_error_wait_max = "20m"
recheck_interval = "10m"
dispatch_limit = 7
failed_validations = 2
first_check_wait = "5m"
free_space_floor = "10GB"

[logs]
daemon_files = 9
daemon_file_size = "1MB"
recording_retention = "7d"

[providers.claude]
worker_cap = 8
default_model = "sonnet"

[roles.engineering]
leader_skills = ["pr"]
worker_skills = []
leader_added_skills = ["/opt/skills/a", "~/skills/b"]
worker_added_skills = ["/opt/skills/c"]

[repositories.otman]
notes = "Run make test."
checks_one_at_a_time = true
no_ci = true
commit_trailers = false
repository_skills = ["engineering", "quality"]
`))
	if len(problems) != 0 {
		t.Fatal(problems)
	}
	wantLimits := Limits{time.Hour, 0, 30 * time.Minute, 45 * time.Second, 48 * time.Hour, 4, 6, time.Minute, 20 * time.Minute, 10 * time.Minute, 7, 2, 5 * time.Minute, 10_000_000_000}
	if cfg.Limits != wantLimits {
		t.Errorf("limits are %+v, want %+v", cfg.Limits, wantLimits)
	}
	if cfg.Logs != (Logs{9, 1_000_000, 7 * 24 * time.Hour}) {
		t.Errorf("logs are %+v", cfg.Logs)
	}
	if cfg.Providers[Claude] != (Provider{8, "sonnet"}) {
		t.Errorf("claude is %+v", cfg.Providers[Claude])
	}
	skills := cfg.Skills["engineering"]
	if !slices.Equal(skills.LeaderSkills, []string{"pr"}) || skills.WorkerSkills == nil || len(skills.WorkerSkills) != 0 ||
		!slices.Equal(skills.LeaderAdded, []string{"/opt/skills/a", "~/skills/b"}) || !slices.Equal(skills.WorkerAdded, []string{"/opt/skills/c"}) {
		t.Errorf("skills are %+v", skills)
	}
	o := cfg.Options["otman"]
	if o.Notes != "Run make test." || !o.ChecksOneAtATime || !o.NoCI || o.CommitTrailers || !slices.Equal(o.RepositorySkills, []string{"engineering", "quality"}) {
		t.Errorf("options are %+v", o)
	}
}

func TestEveryProblemIsReportedWithItsLine(t *testing.T) {
	file := `[limits]
leader_idle_grace = "ten minutes"
leader_restarts = "3"
dispatch_limit = 0
free_space_floor = "5 GB"
provider_error_wait = "10m"
provider_error_wait_max = "1m"
unknown = 1

[logs]
daemon_files = true
recording_retention = "0d"

[providers.codex]
worker_cap = 3

[providers.gpt]
worker_cap = 1

[providers.claude]
default_model = ""

[roles.engineering]
worker_provider = "codex"
leader_skills = "pr"
worker_skills = ["a b", "tdd", "tdd"]
leader_added_skills = ["relative/dir"]

[repositories.otman]
notes = 4
no_ci = "yes"
repository_skills = ["testing"]
`
	_, problems := Parse([]byte(file))
	want := []struct {
		line int
		text string
	}{
		{2, "leader_idle_grace in [limits] is \"ten minutes\", which is not a duration"},
		{3, "leader_restarts in [limits] is a string, not an integer"},
		{4, "dispatch_limit in [limits] is 0, which is below the least, 1"},
		{5, "free_space_floor in [limits] is \"5 GB\", which is not a size"},
		{8, "unknown in [limits] is not a setting"},
		{11, "daemon_files in [logs] is a boolean, not an integer"},
		{12, "recording_retention in [logs] is \"0d\", which is not a duration"},
		{14, "[providers.codex] names Codex, which arrives in a later asmai"},
		{17, "[providers.gpt] names no provider"},
		{21, "default_model in [providers.claude] is empty"},
		{24, "worker_provider in [roles.engineering] is \"codex\", but this asmai runs only \"claude\" (Codex arrives in a later asmai)"},
		{25, "leader_skills in [roles.engineering] is a string, not a list of strings"},
		{26, "worker_skills in [roles.engineering] names \"a b\", which is not a skill name"},
		{27, "leader_added_skills in [roles.engineering] has \"relative/dir\", which is not an absolute path"},
		{30, "notes in [repositories.otman] is a number, not a string"},
		{31, "no_ci in [repositories.otman] is a string, not a boolean"},
		{32, "repository_skills in [repositories.otman] names \"testing\", which is no role"},
		{7, "provider_error_wait_max in [limits] (1m0s) is shorter than provider_error_wait (10m0s)"},
	}
	if len(problems) != len(want) {
		t.Fatalf("Parse found %d problems, want %d:\n%v", len(problems), len(want), problems)
	}
	for i, w := range want {
		if problems[i].Line != w.line || !strings.Contains(problems[i].Problem, w.text) || problems[i].Fix == "" {
			t.Errorf("problem %d is line %d: %q (fix %q); want line %d: %q", i, problems[i].Line, problems[i].Problem, problems[i].Fix, w.line, w.text)
		}
	}
}

func TestTheExampleConfigurationPassesItsOwnCheck(t *testing.T) {
	data := ExampleConfiguration()
	cfg, problems := Parse(data)
	if len(problems) != 0 {
		t.Fatalf("the example has problems: %v", problems)
	}
	if missing := cfg.MissingStaffing(roles.Staffed); len(missing) != 0 {
		t.Errorf("the example leaves staffing out: %v", missing)
	}
	if cfg.Limits != DefaultLimits() || cfg.Logs != DefaultLogs() {
		t.Errorf("the example's limits and logs are not the defaults")
	}
	for _, field := range []string{"[limits]", "[logs]", "[providers.claude]", "[roles.engineering]", "[repositories.otman]", "commit_trailers = true", "repository_skills = []"} {
		if !strings.Contains(string(data), field) {
			t.Errorf("the example lacks %s", field)
		}
	}
	if strings.Contains(strings.ToLower(string(data)), "codex") {
		t.Error("the example names Codex, which this asmai refuses")
	}
}

func TestDurationsAndSizesAreReadAsTheFileWritesThem(t *testing.T) {
	for text, want := range map[string]time.Duration{"30s": 30 * time.Second, "10m": 10 * time.Minute, "14d": 14 * 24 * time.Hour, "2h": 2 * time.Hour} {
		if got, err := parseDuration(text); err != nil || got != want {
			t.Errorf("parseDuration(%q) = %v, %v; want %v", text, got, err, want)
		}
	}
	for _, text := range []string{"", "10", "m", "1.5h", "-1m", "0s", "10 m", "10M", "99999999999999999999d"} {
		if _, err := parseDuration(text); err == nil {
			t.Errorf("parseDuration(%q) was accepted", text)
		}
	}
	for text, want := range map[string]int64{"5GB": 5_000_000_000, "20MB": 20_000_000, "1TB": 1_000_000_000_000, "512B": 512, "3KB": 3000} {
		if got, err := parseSize(text); err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v; want %d", text, got, err, want)
		}
	}
	for _, text := range []string{"", "5", "GB", "5gb", "0GB", "1.5GB"} {
		if _, err := parseSize(text); err == nil {
			t.Errorf("parseSize(%q) was accepted", text)
		}
	}
}
