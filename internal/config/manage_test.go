// SPDX-License-Identifier: Apache-2.0

package config

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestRenderWritesEveryDefaultAndReadsBackAsTheSameConfiguration(t *testing.T) {
	cfg, problems := Parse([]byte(`[limits]
silence_timeout = "45m"
free_space_floor = "1500MB"

[roles.engineering]
leader_provider = "claude"
leader_model = "opus"
worker_provider = "claude"
worker_model = "sonnet"
worker_added_skills = ["~/skills/mine"]

[repositories.otman]
location = "/home/me/src/otman"
origin = "git@github.com:me/otman.git"
default_branch = "main"
notes = "Run make \"test\"\nfirst."
no_ci = true
repository_skills = ["engineering"]
`))
	if len(problems) > 0 {
		t.Fatal(problems)
	}
	text := string(Render(cfg))
	for _, want := range []string{
		`silence_timeout = "45m"`, `drain_timeout = "10m"`, `free_space_floor = "1500MB"`,
		`recording_retention = "14d"`, "worker_cap = 3",
		`leader_skills = ["pr"]`, `leader_added_skills = []`, `worker_added_skills = ["~/skills/mine"]`,
		"commit_trailers = true", `repository_skills = ["engineering"]`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the rendered configuration lacks %q:\n%s", want, text)
		}
	}
	again, problems := Parse([]byte(text))
	if len(problems) > 0 {
		t.Fatalf("the rendered configuration has problems %v:\n%s", problems, text)
	}
	got, want := again.EffectiveSkills("engineering"), cfg.EffectiveSkills("engineering")
	if !slices.Equal(got.LeaderSkills, want.LeaderSkills) || !slices.Equal(got.WorkerSkills, want.WorkerSkills) ||
		!slices.Equal(got.LeaderAdded, want.LeaderAdded) || !slices.Equal(got.WorkerAdded, want.WorkerAdded) ||
		again.Limits != cfg.Limits || again.Logs != cfg.Logs || again.Roles["engineering"] != cfg.Roles["engineering"] ||
		again.Repositories["otman"] != cfg.Repositories["otman"] || !sameOptions(again.Options["otman"], cfg.Options["otman"]) {
		t.Errorf("the rendered configuration reads back as %+v, want %+v", again, cfg)
	}
}

func TestDurationsAndSizesAreWrittenInTheLargestExactUnit(t *testing.T) {
	for d, want := range map[time.Duration]string{14 * 24 * time.Hour: "14d", 36 * time.Hour: "36h", 10 * time.Minute: "10m", 90 * time.Second: "90s", 30 * time.Second: "30s"} {
		if got := FormatDuration(d); got != want {
			t.Errorf("FormatDuration(%s) = %q, want %q", d, got, want)
		}
		if back, err := parseDuration(FormatDuration(d)); err != nil || back != d {
			t.Errorf("%s does not read back: %v, %v", FormatDuration(d), back, err)
		}
	}
	for n, want := range map[int64]string{5 * gigabyte: "5GB", 20 * megabyte: "20MB", 1500 * megabyte: "1500MB", 1001: "1001B", 2 * terabyte: "2TB"} {
		if got := FormatSize(n); got != want {
			t.Errorf("FormatSize(%d) = %q, want %q", n, got, want)
		}
	}
}

const staffedEngineering = `# my file
[roles.engineering]
leader_provider = "claude" # the leader
leader_model = "opus"
worker_provider = "claude"
worker_model = "opus"
leader_skills = ["pr"]

[repositories.x]
origin = "o"
default_branch = "main"
`

func TestSettingAModelChangesOnlyThatValueAndKeepsTheComments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, staffedEngineering, 0o600)
	if err := SetStaffing(path, "engineering", map[string]string{"leader_model": "sonnet", "worker_model": "haiku"}); err != nil {
		t.Fatal(err)
	}
	want := strings.NewReplacer(`leader_model = "opus"`, `leader_model = "sonnet"`, `worker_model = "opus"`, `worker_model = "haiku"`).Replace(staffedEngineering)
	if got := readFile(t, path); got != want {
		t.Errorf("the file is\n%s\nwant\n%s", got, want)
	}
}

func TestSettingAFieldTheTableLacksAddsItUnderTheHeaderAndANewRoleGetsATable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, "[roles.quality]\nleader_provider = \"claude\"", 0o600)
	if err := SetStaffing(path, "quality", map[string]string{"leader_model": "opus"}); err != nil {
		t.Fatal(err)
	}
	if err := SetStaffing(path, "coordination", map[string]string{"worker_model": "opus", "worker_provider": "claude"}); err != nil {
		t.Fatal(err)
	}
	cfg, problems, err := Load(path)
	if err != nil || len(problems) > 0 {
		t.Fatal(problems, err)
	}
	if got := cfg.Roles["quality"]; got.LeaderProvider != "claude" || got.LeaderModel != "opus" {
		t.Errorf("quality is %+v", got)
	}
	if got := cfg.Roles["coordination"]; got.WorkerModel != "opus" || got.WorkerProvider != "claude" {
		t.Errorf("coordination is %+v", got)
	}
}

func TestSettingStaffingRefusesCodexAnEmptyModelAProblemFileAndARoleNotATable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	writeFile(t, path, staffedEngineering, 0o600)
	if err := SetStaffing(path, "engineering", map[string]string{"worker_provider": "codex"}); err == nil || !strings.Contains(err.Error(), "Codex arrives in a later asmai") {
		t.Errorf("setting Codex returned %v", err)
	}
	if err := SetStaffing(path, "engineering", map[string]string{"worker_model": " "}); err == nil {
		t.Error("an empty model was accepted")
	}
	if err := SetStaffing(path, "nobody", map[string]string{"worker_model": "x"}); err == nil {
		t.Error("an unknown role was accepted")
	}
	if got := readFile(t, path); got != staffedEngineering {
		t.Errorf("a refused change altered the file:\n%s", got)
	}

	broken := filepath.Join(dir, "broken.toml")
	writeFile(t, broken, "[limits]\nsilence_timeout = \"soon\"\n", 0o600)
	var problems *ProblemsError
	if err := SetStaffing(broken, "engineering", map[string]string{"worker_model": "x"}); !errors.As(err, &problems) {
		t.Errorf("a file with problems returned %v", err)
	}

	inline := filepath.Join(dir, "inline.toml")
	writeFile(t, inline, "[roles]\nengineering = { leader_provider = \"claude\" }\n", 0o600)
	if err := SetStaffing(inline, "engineering", map[string]string{"worker_model": "x"}); !errors.Is(err, ErrRoleNotATable) {
		t.Errorf("a role written inline returned %v", err)
	}
}

func TestAddingARepositoryWithOptionsWritesEachSettingNotAtItsDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	o := RepositoryOptions{Notes: "Run `make test`.", ChecksOneAtATime: true, NoCI: true, CommitTrailers: false, RepositorySkills: []string{"engineering", "quality"}}
	if err := AddRepositoryWithOptions(path, "otman", otman, o); err != nil {
		t.Fatal(err)
	}
	cfg, problems, err := Load(path)
	if err != nil || len(problems) > 0 {
		t.Fatal(problems, err)
	}
	if got := cfg.Options["otman"]; got.Notes != o.Notes || !got.ChecksOneAtATime || !got.NoCI || got.CommitTrailers || !slices.Equal(got.RepositorySkills, o.RepositorySkills) {
		t.Errorf("the entry reads back with options %+v, want %+v", got, o)
	}

	other := filepath.Join(t.TempDir(), "config.toml")
	err = AddRepositoryWithOptions(other, "otman", otman, RepositoryOptions{CommitTrailers: true, RepositorySkills: []string{"nobody"}})
	var bad *EntryProblemsError
	if !errors.As(err, &bad) || !strings.Contains(err.Error(), `"nobody"`) {
		t.Errorf("a repository skill naming no role returned %v", err)
	}
	if err := CheckRepositoryOptions(RepositoryOptions{RepositorySkills: []string{"nobody"}}); err == nil {
		t.Error("CheckRepositoryOptions accepted a role that does not exist")
	}
}

func TestDiffNamesEachChangedSettingAndWhichSkillsAndRolesItConcerns(t *testing.T) {
	parse := func(text string) Config {
		cfg, problems := Parse([]byte(text))
		if len(problems) > 0 {
			t.Fatal(problems)
		}
		return cfg
	}
	old := parse(staffedEngineering + "\n[limits]\nsilence_timeout = \"30m\"\n")
	next := parse(`[roles.engineering]
leader_provider = "claude"
leader_model = "opus"
worker_provider = "claude"
worker_model = "sonnet"
leader_skills = ["pr"]
worker_added_skills = ["~/skills/mine"]

[roles.quality]
leader_provider = "claude"
leader_model = "opus"

[repositories.x]
origin = "o"
default_branch = "main"
repository_skills = ["engineering"]

[repositories.y]
origin = "o"
default_branch = "main"

[limits]
silence_timeout = "45m"
`)
	got := map[string]Change{}
	for _, c := range Diff(old, next) {
		got[c.Setting] = c
	}
	if c := got["roles.engineering.worker_model"]; c.Before != `"opus"` || c.After != `"sonnet"` || c.Role != "engineering" || c.Side != "worker" {
		t.Errorf("the worker model change is %+v", c)
	}
	if c := got["roles.engineering.worker_added_skills"]; !c.Skills || c.Side != "worker" || !slices.Equal(c.Gained, []string{"~/skills/mine"}) {
		t.Errorf("the added skills change is %+v", c)
	}
	if c := got["roles.quality"]; !c.Added {
		t.Errorf("the new role is %+v", c)
	}
	if c := got["repositories.y"]; !c.Added {
		t.Errorf("the new repository is %+v", c)
	}
	if c := got["repositories.x.repository_skills"]; c.Repository != "x" || !slices.Equal(c.Roles, []string{"engineering"}) {
		t.Errorf("the repository skills change is %+v", c)
	}
	if c := got["limits.silence_timeout"]; c.Before != `"30m"` || c.After != `"45m"` {
		t.Errorf("the limit change is %+v", c)
	}
	if _, ok := got["roles.engineering.leader_model"]; ok {
		t.Error("an unchanged setting is reported")
	}
	if len(Diff(old, old)) != 0 {
		t.Error("a configuration differs from itself")
	}
	gone := Diff(next, old)
	var removed []string
	for _, c := range gone {
		if c.Removed {
			removed = append(removed, c.Setting)
		}
	}
	slices.Sort(removed)
	if !slices.Equal(removed, []string{"repositories.y", "roles.quality"}) {
		t.Errorf("the removed tables are %v", removed)
	}
}
