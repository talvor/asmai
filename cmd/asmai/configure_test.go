// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/config"
	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/statedir"
	"github.com/talvor/asmai/internal/store"
)

// runIn runs asmai in this process with stdin as the user's typing, and
// returns what it printed and its exit code.
func runIn(t *testing.T, stdin string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(args, strings.NewReader(stdin), &out, &errOut)
	return out.String(), errOut.String(), code
}

func configPath() string {
	return filepath.Join(os.Getenv("HOME"), ".config", "asmai", "config.toml")
}

func readConfig(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(configPath())
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestConfigShowPrintsTheEffectiveConfigurationWithEveryDefault(t *testing.T) {
	factoryHome(t)
	writeConfig(t, staffing+"\n[limits]\nsilence_timeout = \"45m\"\n")
	stdout, stderr, code := runAsmai(t, "config", "show")
	if code != 0 {
		t.Fatalf("asmai config show exited %d: %s", code, stderr)
	}
	for _, want := range []string{
		`silence_timeout = "45m"`, `drain_timeout = "10m"`, `free_space_floor = "5GB"`, `recording_retention = "14d"`,
		"[providers.claude]", "worker_cap = 3", "[roles.engineering]", `leader_skills = ["pr"]`, "leader_added_skills = []",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("asmai config show printed\n%s\nwant %q", stdout, want)
		}
	}
	// What it prints is a file with no problems, with the same meaning.
	cfg, problems := config.Parse([]byte(stdout))
	if len(problems) != 0 || cfg.Limits.SilenceTimeout.String() != "45m0s" {
		t.Errorf("what config show printed reads as %+v with problems %v", cfg.Limits, problems)
	}

	var shown struct {
		File          string `json:"file"`
		Configuration struct {
			Limits map[string]any `json:"limits"`
			Roles  map[string]struct {
				WorkerModel string   `json:"worker_model"`
				Skills      []string `json:"leader_skills"`
			} `json:"roles"`
		} `json:"configuration"`
	}
	asmaiJSON(t, &shown, "config", "show")
	if shown.File != configPath() || shown.Configuration.Limits["silence_timeout"] != "45m" || shown.Configuration.Roles["engineering"].WorkerModel != "opus" || len(shown.Configuration.Roles["engineering"].Skills) != 1 {
		t.Errorf("asmai config show --json printed %+v", shown)
	}

	writeConfig(t, "[limits]\nsilence_timeout = \"soon\"\n")
	if stdout, stderr, code := runAsmai(t, "config", "show"); code != 1 || stdout != "" || !strings.Contains(stderr, `line 2: silence_timeout in [limits] is "soon"`) {
		t.Errorf("config show of a file with problems exited %d printing %q, %q", code, stdout, stderr)
	}
}

// editor writes an editor that runs script on the file it is given, and sets
// EDITOR to it. Each run's number is in $RUN, counted in a file beside it.
func editor(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "editor.sh")
	body := "#!/bin/sh\nn=$(cat \"" + dir + "/n\" 2>/dev/null || echo 0)\nn=$((n+1))\necho $n > \"" + dir + "/n\"\nRUN=$n\n" + script + "\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", path)
}

func TestConfigEditSavesOnlyAFileWithNoProblem(t *testing.T) {
	factoryHome(t)
	writeConfig(t, staffing)

	// A valid edit is saved.
	editor(t, `sed -e 's/worker_model = "opus"/worker_model = "sonnet"/' "$1" > "$1.new" && mv "$1.new" "$1"`)
	stdout, stderr, code := runIn(t, "", "config", "edit")
	if code != 0 || !strings.Contains(stdout, "Saved "+configPath()) || !strings.Contains(stdout, "asmai config apply") {
		t.Fatalf("a valid edit exited %d printing %q, %q", code, stdout, stderr)
	}
	if got := readConfig(t); !strings.Contains(got, `worker_model = "sonnet"`) {
		t.Errorf("the saved file is\n%s", got)
	}

	// An invalid edit is shown its problem, and declined, leaves the file.
	before := readConfig(t)
	editor(t, `printf '\n[limits]\nsilence_timeout = "soon"\n' >> "$1"`)
	stdout, stderr, code = runIn(t, "n\n", "config", "edit")
	if code != 1 || !strings.Contains(stderr, `silence_timeout in [limits] is "soon"`) || !strings.Contains(stderr, "fix:") || !strings.Contains(stderr, "is as it was") {
		t.Errorf("an invalid edit exited %d printing %q, %q", code, stdout, stderr)
	}
	if got := readConfig(t); got != before {
		t.Errorf("a refused edit changed the file to\n%s", got)
	}

	// Asked to edit again, a second edit that fixes it is saved.
	editor(t, `if [ "$RUN" = 1 ]; then printf '\n[limits]\nsilence_timeout = "soon"\n' >> "$1"; else sed -e 's/soon/20m/' "$1" > "$1.new" && mv "$1.new" "$1"; fi`)
	stdout, stderr, code = runIn(t, "y\n", "config", "edit")
	if code != 0 || !strings.Contains(stdout, "Edit it again? [Y/n]") || !strings.Contains(stdout, "Saved ") {
		t.Fatalf("editing again exited %d printing %q, %q", code, stdout, stderr)
	}
	if got := readConfig(t); !strings.Contains(got, `silence_timeout = "20m"`) {
		t.Errorf("the saved file is\n%s", got)
	}

	// Leaving the file as it is changes nothing.
	editor(t, `true`)
	if stdout, _, code := runIn(t, "", "config", "edit"); code != 0 || !strings.Contains(stdout, "No change") {
		t.Errorf("an edit that changed nothing exited %d printing %q", code, stdout)
	}
	// With no editor there is nothing to open.
	t.Setenv("EDITOR", "")
	if _, stderr, code := runIn(t, "", "config", "edit"); code != 1 || !strings.Contains(stderr, "no editor is set") {
		t.Errorf("with no editor, config edit exited %d printing %q", code, stderr)
	}
}

func TestConfigEditRequiresAnExistingFile(t *testing.T) {
	factoryHome(t)
	if _, stderr, code := runIn(t, "", "config", "edit"); code != 1 || !strings.Contains(stderr, "asmai setup") {
		t.Errorf("config edit without a file exited %d printing %q", code, stderr)
	}
	if _, err := os.Stat(configPath()); !os.IsNotExist(err) {
		t.Errorf("config edit created a file: %v", err)
	}
}

func TestConfigEditPreservesQuotedEditorArgumentsAndConflictingEdits(t *testing.T) {
	factoryHome(t)
	writeConfig(t, staffing)
	editorPath := filepath.Join(t.TempDir(), "Editor With Spaces")
	if err := os.WriteFile(editorPath, []byte("#!/bin/sh\n[ \"$1\" = 'two words' ] || exit 9\nprintf '\\n[limits]\\nsilence_timeout = \"55m\"\\n' >> \"$2\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", quoted(editorPath)+" "+quoted("two words"))
	if _, stderr, code := runIn(t, "", "config", "edit"); code != 0 || !strings.Contains(readConfig(t), `silence_timeout = "55m"`) {
		t.Fatalf("quoted editor exited %d printing %q", code, stderr)
	}

	t.Setenv("ASMAI_CONFIG_PATH", configPath())
	editor(t, `sed -e 's/55m/41m/' "$1" > "$1.new" && mv "$1.new" "$1"; sed -e 's/55m/42m/' "$ASMAI_CONFIG_PATH" > "$ASMAI_CONFIG_PATH.new" && mv "$ASMAI_CONFIG_PATH.new" "$ASMAI_CONFIG_PATH"`)
	_, stderr, code := runIn(t, "", "config", "edit")
	marker := "your edited copy remains at "
	start := strings.Index(stderr, marker)
	if code != 1 || start < 0 {
		t.Fatalf("conflicting edit exited %d printing %q", code, stderr)
	}
	copyPath := strings.TrimSpace(stderr[start+len(marker):])
	t.Cleanup(func() { os.Remove(copyPath) })
	copyData, err := os.ReadFile(copyPath)
	if err != nil || !strings.Contains(string(copyData), `silence_timeout = "41m"`) || !strings.Contains(readConfig(t), `silence_timeout = "42m"`) {
		t.Errorf("edited copy %q: %q, %v; current file: %q", copyPath, copyData, err, readConfig(t))
	}
}

func TestConfigEditPreservesChangedCopyWhenEditorFails(t *testing.T) {
	factoryHome(t)
	writeConfig(t, staffing)
	before := readConfig(t)
	editor(t, `printf '\n[limits]\nsilence_timeout = "55m"\n' >> "$1"; exit 7`)
	_, stderr, code := runIn(t, "", "config", "edit")
	marker := "your edited copy remains at "
	start := strings.Index(stderr, marker)
	if code != 1 || start < 0 || !strings.Contains(stderr, "did not finish") {
		t.Fatalf("failed editor exited %d printing %q", code, stderr)
	}
	copyPath := strings.TrimSpace(stderr[start+len(marker):])
	t.Cleanup(func() { os.Remove(copyPath) })
	copyData, err := os.ReadFile(copyPath)
	if err != nil || !strings.Contains(string(copyData), `silence_timeout = "55m"`) {
		t.Errorf("edited copy %q: %q, %v", copyPath, copyData, err)
	}
	if got := readConfig(t); got != before {
		t.Errorf("failed edit changed the configuration to %q", got)
	}
}

func TestRoleListShowsEachRoleAndRoleSetEditsTheFileForClaudeAndRefusesCodex(t *testing.T) {
	factoryHome(t)
	writeConfig(t, "# mine\n"+staffing)

	var list struct {
		Roles []struct {
			Role        string   `json:"role"`
			Staffed     bool     `json:"staffed"`
			WorkerModel string   `json:"worker_model"`
			LeaderSkill []string `json:"leader_skills"`
		} `json:"roles"`
	}
	asmaiJSON(t, &list, "role", "list")
	if len(list.Roles) != 5 || list.Roles[0].Role != "coordination" || !list.Roles[0].Staffed || list.Roles[1].Role != "planning" || list.Roles[1].Staffed {
		t.Fatalf("asmai role list --json printed %+v, want the five roles, coordination staffed and planning not", list.Roles)
	}
	stdout, stderr, code := runAsmai(t, "role", "list")
	if code != 0 || !strings.Contains(stdout, "engineering") || !strings.Contains(stdout, "claude/opus") || !strings.Contains(stdout, "pr") {
		t.Errorf("asmai role list exited %d printing %q, %q", code, stdout, stderr)
	}

	stdout, stderr, code = runAsmai(t, "role", "set", "engineering", "--worker-model", "sonnet", "--leader-model", "opus")
	if code != 0 || !strings.Contains(stdout, "workers claude/sonnet") {
		t.Fatalf("asmai role set exited %d printing %q, %q", code, stdout, stderr)
	}
	if got, want := readConfig(t), "# mine\n"+strings.Replace(staffing, "[roles.engineering]\nleader_provider = \"claude\"\nleader_model = \"opus\"\nworker_provider = \"claude\"\nworker_model = \"opus\"", "[roles.engineering]\nleader_provider = \"claude\"\nleader_model = \"opus\"\nworker_provider = \"claude\"\nworker_model = \"sonnet\"", 1); got != want {
		t.Errorf("the file is\n%s\nwant\n%s", got, want)
	}
	if _, stderr, code := runAsmai(t, "role", "set", "quality", "--leader-provider", "claude", "--worker-provider", "claude", "--leader-model", "haiku", "--worker-model", "haiku"); code != 0 {
		t.Errorf("setting both sides exited %d: %s", code, stderr)
	}
	cfg, problems, err := config.Load(configPath())
	if err != nil || len(problems) != 0 || cfg.Roles["quality"].LeaderModel != "haiku" || cfg.Roles["quality"].WorkerModel != "haiku" {
		t.Errorf("the file reads as %+v, %v, %v", cfg.Roles["quality"], problems, err)
	}

	before := readConfig(t)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"role", "set", "engineering", "--worker-provider", "codex"}, "Codex arrives in a later asmai"},
		{[]string{"role", "set", "engineering", "--leader-provider", "codex", "--leader-model", "gpt"}, "Codex arrives in a later asmai"},
		{[]string{"role", "set", "engineering"}, "say what to set"},
		{[]string{"role", "set", "nobody", "--leader-model", "x"}, `"nobody" is no role`},
	} {
		if _, stderr, code := runAsmai(t, tc.args...); code != 1 || !strings.Contains(stderr, tc.want) {
			t.Errorf("asmai %s exited %d printing %q, want %q", strings.Join(tc.args, " "), code, stderr, tc.want)
		}
	}
	for _, flag := range []string{"--provider", "--model"} {
		if _, stderr, code := runAsmai(t, "role", "set", "quality", flag, "claude"); code != 2 || !strings.Contains(stderr, "flag provided but not defined") {
			t.Errorf("role set %s exited %d printing %q", flag, code, stderr)
		}
	}
	for _, tc := range []struct{ flag, field, other string }{
		{"--leader-provider", "leader_provider", "--worker-model"},
		{"--worker-provider", "worker_provider", "--leader-model"},
		{"--leader-model", "leader_model", "--worker-model"},
		{"--worker-model", "worker_model", "--leader-model"},
	} {
		if _, stderr, code := runAsmai(t, "role", "set", "quality", tc.flag, "", tc.other, "sonnet"); code != 1 || !strings.Contains(stderr, tc.field) {
			t.Errorf("explicit empty %s exited %d printing %q", tc.flag, code, stderr)
		}
	}
	if got := readConfig(t); got != before {
		t.Errorf("refused role sets changed the file to\n%s", got)
	}
}

func TestConfigApplyShowsWhichRunningAgentsPickUpEachChangeAndJournalsIt(t *testing.T) {
	readyFactory(t, idleLeader)
	if _, stderr, code := runAsmai(t, "start"); code != 0 {
		t.Fatalf("asmai start exited %d: %s", code, stderr)
	}
	// What the daemon started on is what there is to compare with.
	if stdout, _, code := runIn(t, "", "config", "apply"); code != 0 || !strings.Contains(stdout, "no change") {
		t.Fatalf("applying the file the factory started on exited %d printing %q, want no change", code, stdout)
	}

	// Coordination's leader is running on opus: a new model for it, and for
	// workers nobody runs, and a skill added for its leader.
	if _, stderr, code := runAsmai(t, "role", "set", "coordination", "--leader-model", "sonnet", "--worker-model", "haiku"); code != 0 {
		t.Fatal(stderr)
	}
	skills := t.TempDir()
	text := readConfig(t) + "\n[limits]\nsilence_timeout = \"45m\"\n"
	text = strings.Replace(text, "[roles.coordination]\n", "[roles.coordination]\nleader_added_skills = ["+quoted(skills)+"]\n", 1)
	writeConfig(t, text)

	// Declined, nothing is applied.
	stdout, stderr, code := runIn(t, "n\n", "config", "apply")
	if code != 1 || !strings.Contains(stderr, "not confirmed; nothing was applied") {
		t.Errorf("a declined apply exited %d printing %q, %q", code, stdout, stderr)
	}
	for _, want := range []string{
		"has 4 changes",
		`roles.coordination.leader_model: "opus" -> "sonnet"`, "picked up at the next start of: leader@coordination",
		`roles.coordination.leader_added_skills: [] -> [`, "adds skills: " + skills, "reaches sessions once per-session skill delivery lands (#89)",
		`roles.coordination.worker_model: "sonnet" -> "haiku"`,
		`limits.silence_timeout: "30m" -> "45m"`, "no running agent is affected, so it applies at once",
		"Apply it? [y/N]",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("asmai config apply printed\n%s\nwant %q", stdout, want)
		}
	}
	if got := journaled(t, "configuration.applied"); len(got) != 0 {
		t.Errorf("a declined apply journaled %d entries", len(got))
	}

	// Confirmed, it is applied and journaled with the change and its agents.
	stdout, stderr, code = runIn(t, "y\n", "config", "apply")
	if code != 0 || !strings.Contains(stdout, "Applied, and journaled as configuration.applied") {
		t.Fatalf("a confirmed apply exited %d printing %q, %q", code, stdout, stderr)
	}
	entries := journaled(t, "configuration.applied")
	if len(entries) != 1 {
		t.Fatalf("the journal holds %d applied configurations, want 1", len(entries))
	}
	var data struct {
		File          string `json:"file"`
		SHA256        string `json:"sha256"`
		Configuration string `json:"configuration"`
		Changes       []struct {
			Setting string   `json:"setting"`
			Agents  []string `json:"agents"`
			Note    string   `json:"note"`
		} `json:"changes"`
	}
	if err := json.Unmarshal(entries[0].Data, &data); err != nil {
		t.Fatal(err)
	}
	picked := map[string][]string{}
	notes := map[string]string{}
	for _, c := range data.Changes {
		picked[c.Setting] = c.Agents
		notes[c.Setting] = c.Note
	}
	if data.File != configPath() || data.Configuration != text || len(data.SHA256) != 64 || len(picked) != 4 ||
		len(picked["roles.coordination.leader_model"]) != 1 || picked["roles.coordination.leader_model"][0] != "leader@coordination" ||
		len(picked["roles.coordination.leader_added_skills"]) != 0 || !strings.Contains(notes["roles.coordination.leader_added_skills"], "#89") || len(picked["limits.silence_timeout"]) != 0 || len(picked["roles.coordination.worker_model"]) != 0 {
		t.Errorf("the journaled configuration is %+v", data)
	}

	// Applied once, it is applied.
	if stdout, _, code := runIn(t, "", "config", "apply"); code != 0 || !strings.Contains(stdout, "no change") {
		t.Errorf("applying again exited %d printing %q, want no change", code, stdout)
	}

	// A file with a problem is refused.
	writeConfig(t, text+"\n[logs]\ndaemon_files = 0\n")
	if _, stderr, code := runIn(t, "y\n", "config", "apply"); code != 1 || !strings.Contains(stderr, "daemon_files in [logs]") || !strings.Contains(stderr, "nothing was applied") {
		t.Errorf("applying a file with a problem exited %d printing %q", code, stderr)
	}
	writeConfig(t, strings.Replace(text, `"45m"`, `"50m"`, 1))
	var shown struct {
		Applied bool `json:"applied"`
		Changes []struct {
			Setting string `json:"setting"`
		} `json:"changes"`
	}
	stdout, stderr, code = runIn(t, "y\n", "config", "apply", "--json")
	if code != 0 || !strings.Contains(stderr, "Apply it? [y/N]") || json.Unmarshal([]byte(stdout), &shown) != nil {
		t.Fatalf("config apply --json exited %d printing %q, %q", code, stdout, stderr)
	}
	if !shown.Applied || len(shown.Changes) != 1 || shown.Changes[0].Setting != "limits.silence_timeout" {
		t.Errorf("config apply --json printed %+v, want the applied change", shown)
	}
	if got := journaled(t, "configuration.applied"); len(got) != 2 {
		t.Errorf("confirming JSON apply journaled %d entries, want 2", len(got))
	}
	if _, stderr, code := runIn(t, "", "config", "apply", "--yes"); code != 2 || !strings.Contains(stderr, "flag provided but not defined: -yes") {
		t.Errorf("config apply --yes exited %d printing %q", code, stderr)
	}
	writeConfig(t, strings.Replace(text, `"45m"`, `"55m"`, 1))
	if _, stderr, code := runIn(t, "y\n", "config", "apply"); code != 0 {
		t.Errorf("config apply exited %d: %s", code, stderr)
	}
	if got := journaled(t, "configuration.applied"); len(got) != 3 {
		t.Errorf("after confirmation the journal holds %d applied configurations, want 3", len(got))
	}
}

func quoted(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestConfigApplyWithNoFactoryRunningSaysThereIsNothingToPickItUp(t *testing.T) {
	factoryHome(t)
	writeConfig(t, staffing)
	stdout, stderr, code := runAsmai(t, "config", "apply")
	if code != 0 || !strings.Contains(stdout, "not running") || stderr != "" {
		t.Errorf("config apply with no daemon exited %d printing %q, %q", code, stdout, stderr)
	}
}

func TestConfigApplyRejectsRepositoryMismatchWithFactoryStopped(t *testing.T) {
	factoryHome(t)
	paths, err := statedir.Default()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(paths.Store), 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(paths.Store)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RepositoryAdded(store.Repository{Name: "fixture", Origin: "https://old.example/fixture.git", DefaultBranch: "trunk", AddedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	writeConfig(t, staffing+"\n[repositories.fixture]\norigin = \"https://new.example/fixture.git\"\ndefault_branch = \"trunk\"\n")
	for _, args := range [][]string{{"config", "apply"}, {"config", "apply", "--json"}} {
		_, stderr, code := runIn(t, "", args...)
		if code != 1 || !strings.Contains(stderr, "[repositories.fixture].origin differs") || !strings.Contains(stderr, "nothing was applied") {
			t.Errorf("%v exited %d printing %q", args, code, stderr)
		}
	}
}

func TestRepoAddAcceptsEveryNonCodexFieldOfTheEntry(t *testing.T) {
	readyFactory(t, idleLeader)
	if _, stderr, code := runAsmai(t, "start"); code != 0 {
		t.Fatalf("asmai start exited %d: %s", code, stderr)
	}
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
	gitIn(t, seed, "push", "--quiet", "origin", "trunk:develop")

	// Each refusal happens before anything is cloned or written.
	before := readConfig(t)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--default-branch", "nope"}, `origin has no branch "nope"`},
		{[]string{"--repository-skills", "nobody"}, `"nobody", which is no role`},
	} {
		_, stderr, code := runAsmai(t, append([]string{"repo", "add", origin, "--name", "fixture"}, tc.args...)...)
		if code != 1 || !strings.Contains(stderr, tc.want) {
			t.Errorf("asmai repo add %v exited %d printing %q, want %q", tc.args, code, stderr, tc.want)
		}
	}
	if got := readConfig(t); got != before {
		t.Errorf("refused registrations changed the file to\n%s", got)
	}
	if got := repositoriesListed(t); len(got) != 0 {
		t.Errorf("refused registrations left %+v", got)
	}

	var added struct {
		Repository repositoryJSON `json:"repository"`
	}
	asmaiJSON(t, &added, "repo", "add", origin, "--name", "fixture",
		"--default-branch", "develop", "--notes", "Run make test first.", "--checks-one-at-a-time", "--no-ci",
		"--commit-trailers=false", "--repository-skills", "engineering", "--repository-skills", "quality")
	if added.Repository.DefaultBranch != "develop" || added.Repository.Origin != origin {
		t.Fatalf("the registration is %+v, want it on develop", added.Repository)
	}
	cfg, problems, err := config.Load(configPath())
	if err != nil || len(problems) != 0 {
		t.Fatalf("the file reads with %v, %v", problems, err)
	}
	o := cfg.Options["fixture"]
	if cfg.Repositories["fixture"].DefaultBranch != "develop" || o.Notes != "Run make test first." || !o.ChecksOneAtATime || !o.NoCI || o.CommitTrailers ||
		strings.Join(o.RepositorySkills, ",") != "engineering,quality" {
		t.Errorf("the entry reads as %+v %+v, want every field given", cfg.Repositories["fixture"], o)
	}
	// The daemon's own write is no change to apply.
	if stdout, _, code := runIn(t, "", "config", "apply"); code != 0 || !strings.Contains(stdout, "no change") {
		t.Errorf("applying after repo add exited %d printing %q, want no change", code, stdout)
	}
	o.RepositorySkills = []string{"engineering"}
	cfg.Options["fixture"] = o
	writeConfig(t, string(config.Render(cfg)))
	var applied struct {
		Applied bool `json:"applied"`
		Changes []struct {
			Setting string   `json:"setting"`
			Agents  []string `json:"agents"`
			Note    string   `json:"note"`
		} `json:"changes"`
	}
	stdout, stderr, code := runIn(t, "y\n", "config", "apply", "--json")
	if code != 0 || json.Unmarshal([]byte(stdout), &applied) != nil || !strings.Contains(stderr, "Apply it? [y/N]") {
		t.Fatalf("applying repository skills as JSON exited %d printing %q, %q", code, stdout, stderr)
	}
	if !applied.Applied || len(applied.Changes) != 1 || applied.Changes[0].Setting != "repositories.fixture.repository_skills" || len(applied.Changes[0].Agents) != 0 || !strings.Contains(applied.Changes[0].Note, "#89") {
		t.Errorf("JSON apply reported %+v", applied)
	}
	entries := journaled(t, "configuration.applied")
	if len(entries) != 1 {
		t.Fatalf("the journal has %d applied configurations, want 1", len(entries))
	}
	var recorded struct {
		Changes []struct {
			Setting string `json:"setting"`
			Note    string `json:"note"`
		} `json:"changes"`
	}
	if err := json.Unmarshal(entries[0].Data, &recorded); err != nil || len(recorded.Changes) != 1 || recorded.Changes[0].Setting != "repositories.fixture.repository_skills" || !strings.Contains(recorded.Changes[0].Note, "#89") {
		t.Errorf("the repository skill change was not journaled with its delivery note: %+v, %v", recorded, err)
	}
}

func TestRestartReportsRepositoryRecordMismatchesWithoutChangingTheRecord(t *testing.T) {
	readyFactory(t, idleLeader)
	if _, stderr, code := runAsmai(t, "start"); code != 0 {
		t.Fatalf("starting factory: %s", stderr)
	}
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
	asmaiJSON(t, &struct{}{}, "repo", "add", origin, "--name", "fixture")
	registered := readConfig(t)
	for _, tc := range []struct {
		remove bool
		want   string
	}{
		{false, "[repositories.fixture].default_branch"},
		{true, "[repositories.fixture] is missing"},
	} {
		stopFactory(t)
		cfg, problems := config.Parse([]byte(registered))
		if len(problems) > 0 {
			t.Fatalf("registered configuration has problems: %v", problems)
		}
		if _, ok := cfg.Repositories["fixture"]; !ok {
			t.Fatal("repo add did not write [repositories.fixture]")
		}
		if tc.remove {
			delete(cfg.Repositories, "fixture")
			delete(cfg.Options, "fixture")
		} else {
			r := cfg.Repositories["fixture"]
			r.DefaultBranch = "develop"
			cfg.Repositories["fixture"] = r
		}
		writeConfig(t, string(config.Render(cfg)))
		if _, stderr, code := runAsmai(t, "config", "check"); code != 1 || !strings.Contains(stderr, tc.want) {
			t.Errorf("config check exited %d printing %q, want %q", code, stderr, tc.want)
		}
		runAsmai(t, "start")
		if stdout, stderr, code := runAsmai(t, "status"); code != 0 || !strings.Contains(stdout, tc.want) {
			t.Errorf("status exited %d printing %q, %q, want %q", code, stdout, stderr, tc.want)
		}
		if _, stderr, code := runIn(t, "y\n", "config", "apply"); code != 1 || !strings.Contains(stderr, tc.want) {
			t.Errorf("apply exited %d printing %q, want %q", code, stderr, tc.want)
		}
		if got := repositoriesListed(t); len(got) != 1 || got[0].DefaultBranch != "trunk" {
			t.Errorf("the registered repository changed: %+v", got)
		}
	}
}

func TestAnAgentCannotChangeTheConfiguration(t *testing.T) {
	factoryHome(t)
	writeConfig(t, staffing)
	t.Setenv(daemon.SessionCredential, "an-agent-session")
	before := readConfig(t)
	for _, args := range [][]string{
		{"config", "show"}, {"config", "edit"}, {"config", "apply"},
		{"role", "list"}, {"role", "set", "engineering", "--worker-model", "sonnet"},
	} {
		_, stderr, code := runIn(t, "", args...)
		if code != 1 || !strings.Contains(stderr, "an agent cannot run this command; ask the user to run `asmai "+strings.Join(args, " ")+"`") {
			t.Errorf("asmai %s as an agent exited %d printing %q", strings.Join(args, " "), code, stderr)
		}
	}
	if got := readConfig(t); got != before {
		t.Errorf("an agent changed the file to\n%s", got)
	}
}
