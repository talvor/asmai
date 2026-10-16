// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/config"
	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/session"
	"github.com/talvor/asmai/internal/store"
)

const appliedStaffing = `[roles.coordination]
leader_provider = "claude"
leader_model = "opus"
worker_provider = "claude"
worker_model = "opus"

[roles.engineering]
leader_provider = "claude"
leader_model = "opus"
worker_provider = "claude"
worker_model = "opus"

[roles.quality]
leader_provider = "claude"
leader_model = "opus"
worker_provider = "claude"
worker_model = "opus"
`

func applyConfig(t *testing.T, p string, text string, confirm bool) (Response, error) {
	t.Helper()
	preview, err := Call(p, Request{Command: CommandConfigApply, Configuration: text})
	if err != nil || !confirm {
		return preview, err
	}
	return Call(p, Request{Command: CommandConfigApply, Configuration: text, Confirm: true, Preview: preview.ConfigApply})
}

func TestConfigApplyRequiresThePreviewedAgentSet(t *testing.T) {
	p, configFile := repoDaemon(t)
	if err := os.MkdirAll(filepath.Dir(configFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configFile, []byte(appliedStaffing), 0o600); err != nil {
		t.Fatal(err)
	}
	preview, err := applyConfig(t, p.Socket, appliedStaffing, false)
	if err != nil || preview.ConfigApply == nil || len(preview.ConfigApply.Changes) == 0 {
		t.Fatalf("preview returned %+v, %v", preview.ConfigApply, err)
	}
	if _, err := Call(p.Socket, Request{Command: CommandConfigApply, Configuration: appliedStaffing, Confirm: true}); err == nil || !strings.Contains(err.Error(), "changed since the preview") {
		t.Errorf("confirming without a preview returned %v", err)
	}
	s, err := store.Open(p.Store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionStarted(store.SessionStart{Agent: "worker@coordination", Role: "coordination", Provider: "claude", Model: "opus", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Call(p.Socket, Request{Command: CommandConfigApply, Configuration: appliedStaffing, Confirm: true, Preview: preview.ConfigApply}); err == nil || !strings.Contains(err.Error(), "affected agents changed since the preview") {
		t.Errorf("confirming after a worker starts returned %v", err)
	}
	if got := countJournaled(t, p.Socket, store.KindConfigurationApplied); got != 0 {
		t.Errorf("stale confirmation journaled %d configurations", got)
	}
	fresh, err := applyConfig(t, p.Socket, appliedStaffing, false)
	if err != nil || fresh.ConfigApply == nil || !slices.ContainsFunc(fresh.ConfigApply.Changes, func(c ConfigChange) bool { return slices.Contains(c.Agents, "worker@coordination") }) {
		t.Fatalf("new preview returned %+v, %v", fresh.ConfigApply, err)
	}
	if applied, err := Call(p.Socket, Request{Command: CommandConfigApply, Configuration: appliedStaffing, Confirm: true, Preview: fresh.ConfigApply}); err != nil || applied.ConfigApply == nil || !applied.ConfigApply.Applied {
		t.Fatalf("confirming the fresh preview returned %+v, %v", applied.ConfigApply, err)
	}
}

func TestAConfigurationIsShownThenAppliedAndJournaledOnce(t *testing.T) {
	p, configFile := repoDaemon(t)
	if err := os.MkdirAll(filepath.Dir(configFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configFile, []byte(appliedStaffing), 0o600); err != nil {
		t.Fatal(err)
	}
	// What the daemon started on is the baseline, which is nothing at all
	// here: the file did not exist, so every setting is a change.
	if resp, err := applyConfig(t, p.Socket, appliedStaffing, true); err != nil || !resp.ConfigApply.Applied {
		t.Fatalf("applying the first file returned %+v, %v", resp.ConfigApply, err)
	}

	changed := strings.Replace(appliedStaffing, `worker_model = "opus"`, `worker_model = "sonnet"`, 1)
	if err := os.WriteFile(configFile, []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	resp, err := applyConfig(t, p.Socket, changed, false)
	if err != nil {
		t.Fatal(err)
	}
	var settings []string
	for _, c := range resp.ConfigApply.Changes {
		settings = append(settings, c.Setting)
		if len(c.Agents) != 0 {
			t.Errorf("%s is picked up by %v, but no agent is running", c.Setting, c.Agents)
		}
	}
	if !slices.Equal(settings, []string{"roles.coordination.worker_model"}) || resp.ConfigApply.Applied {
		t.Fatalf("showing the change reported %v, applied %v, want only the coordination worker model", settings, resp.ConfigApply.Applied)
	}
	if got := countJournaled(t, p.Socket, store.KindConfigurationApplied); got != 1 {
		t.Errorf("showing the change journaled it: %d entries, want the first apply's 1", got)
	}

	resp, err = applyConfig(t, p.Socket, changed, true)
	if err != nil || !resp.ConfigApply.Applied {
		t.Fatalf("applying returned %+v, %v", resp.ConfigApply, err)
	}
	if got := countJournaled(t, p.Socket, store.KindConfigurationApplied); got != 2 {
		t.Errorf("the journal holds %d applied configurations, want 2", got)
	}
	entries, err := Call(p.Socket, Request{Command: CommandExport})
	if err != nil {
		t.Fatal(err)
	}
	last := entries.Journal[len(entries.Journal)-1]
	if last.Kind != store.KindConfigurationApplied || !strings.Contains(string(last.Data), `roles.coordination.worker_model`) || !strings.Contains(string(last.Data), `"sha256"`) {
		t.Errorf("the last journal entry is %s %s, want the applied configuration with its change", last.Kind, last.Data)
	}

	resp, err = applyConfig(t, p.Socket, changed, true)
	if err != nil || len(resp.ConfigApply.Changes) != 0 || resp.ConfigApply.Applied {
		t.Errorf("applying the same file again returned %+v, %v, want no change", resp.ConfigApply, err)
	}
	if got := countJournaled(t, p.Socket, store.KindConfigurationApplied); got != 2 {
		t.Errorf("a no-change apply journaled: %d entries, want 2", got)
	}
}

func TestAddedRoleSkillsAreShownAsPendingAndJournaled(t *testing.T) {
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
	changed := appliedStaffing + fmt.Sprintf("\n[roles.planning]\nleader_added_skills = [%q]\n", t.TempDir())
	if err := os.WriteFile(configFile, []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	preview, err := applyConfig(t, p.Socket, changed, false)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range preview.ConfigApply.Changes {
		if c.Setting == "roles.planning.leader_added_skills" {
			found = c.Skills && len(c.Gained) == 1 && len(c.Agents) == 0 && c.Note == ""
		}
	}
	if !found {
		t.Errorf("the preview omits the new role's pending skill: %+v", preview.ConfigApply.Changes)
	}
	if _, err := applyConfig(t, p.Socket, changed, true); err != nil {
		t.Fatal(err)
	}
	response, err := Call(p.Socket, Request{Command: CommandExport})
	if err != nil {
		t.Fatal(err)
	}
	var recorded store.AppliedConfiguration
	if err := json.Unmarshal(response.Journal[len(response.Journal)-1].Data, &recorded); err != nil {
		t.Fatal(err)
	}
	found = false
	for _, c := range recorded.Changes {
		if c.Setting == "roles.planning.leader_added_skills" {
			found = len(c.Agents) == 0 && c.Note == ""
		}
	}
	if !found {
		t.Errorf("the journal omits the new role's pending skill: %+v", recorded.Changes)
	}
}

func countJournaled(t *testing.T, socket, kind string) int {
	t.Helper()
	resp, err := Call(socket, Request{Command: CommandExport})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range resp.Journal {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

func TestAnInvalidOrMismatchedConfigurationIsRefusedAndNothingIsApplied(t *testing.T) {
	p, configFile := repoDaemon(t)
	if err := os.MkdirAll(filepath.Dir(configFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configFile, []byte(appliedStaffing), 0o600); err != nil {
		t.Fatal(err)
	}
	bad := appliedStaffing + "\n[limits]\nsilence_timeout = \"soon\"\n"
	if _, err := applyConfig(t, p.Socket, bad, true); err == nil || !strings.Contains(err.Error(), `silence_timeout in [limits] is "soon"`) {
		t.Errorf("applying a file with a problem returned %v", err)
	}
	unstaffed := "[roles.coordination]\nleader_provider = \"claude\"\nleader_model = \"opus\"\nworker_provider = \"claude\"\nworker_model = \"opus\"\n"
	if err := os.WriteFile(configFile, []byte(unstaffed), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := applyConfig(t, p.Socket, unstaffed, true); err == nil || !strings.Contains(err.Error(), "[roles.engineering]") {
		t.Errorf("applying a file that leaves a role unstaffed returned %v", err)
	}
	// The file on disk is not the file the user applied.
	if err := os.WriteFile(configFile, []byte(appliedStaffing), 0o600); err != nil {
		t.Fatal(err)
	}
	other := strings.Replace(appliedStaffing, `"opus"`, `"sonnet"`, 1)
	if _, err := applyConfig(t, p.Socket, other, true); err == nil || !strings.Contains(err.Error(), "run `asmai config apply` again") {
		t.Errorf("applying content that is not the daemon's file returned %v", err)
	}
	if got := countJournaled(t, p.Socket, store.KindConfigurationApplied); got != 0 {
		t.Errorf("refused applies journaled %d entries", got)
	}
}

func TestApplyRefusesRemovingARegisteredRepositoryFromTheFile(t *testing.T) {
	p, configFile := repoDaemon(t)
	if err := os.MkdirAll(filepath.Dir(configFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configFile, []byte(appliedStaffing), 0o600); err != nil {
		t.Fatal(err)
	}
	origin := remote(t)
	resp, err := addRepo(t, p, origin, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	clone := resp.Repository.Clone
	with, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	// The repository's entry was written by the daemon: applying it is no
	// change.
	if r, err := applyConfig(t, p.Socket, string(with), false); err != nil || slices.ContainsFunc(r.ConfigApply.Changes, func(c ConfigChange) bool { return strings.HasPrefix(c.Setting, "repositories.") }) {
		t.Fatalf("the daemon's own entry shows as a change: %+v, %v", r.ConfigApply, err)
	}

	db, err := sql.Open("sqlite3", p.Store)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO jobs (repository, state) VALUES ('fixture', 'open')`); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configFile, []byte(appliedStaffing), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := applyConfig(t, p.Socket, appliedStaffing, true); err == nil || !strings.Contains(err.Error(), "[repositories.fixture]") || !strings.Contains(err.Error(), "asmai repo remove fixture") {
		t.Errorf("applying a missing registered repository returned %v", err)
	}
	if _, err := os.Stat(clone); err != nil {
		t.Errorf("the refused apply left the clone %v", err)
	}

	if _, err := db.Exec(`UPDATE jobs SET state = 'ended'`); err != nil {
		t.Fatal(err)
	}
	if _, err := applyConfig(t, p.Socket, appliedStaffing, true); err == nil || !strings.Contains(err.Error(), "asmai repo remove fixture") {
		t.Errorf("applying a missing repository after its job ended returned %v", err)
	}
	if _, err := os.Stat(clone); err != nil {
		t.Errorf("the refused apply left the clone %v", err)
	}
	if listed, _ := Call(p.Socket, Request{Command: CommandRepoList}); len(listed.Repositories) != 1 {
		t.Errorf("the repository registration changed: %+v", listed.Repositories)
	}

	// A table the file has that nothing registered has no clone.
	withTable := string(with) + "\n[repositories.ghost]\norigin = \"o\"\ndefault_branch = \"main\"\n"
	if err := os.WriteFile(configFile, []byte(withTable), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := applyConfig(t, p.Socket, withTable, true); err == nil || !strings.Contains(err.Error(), "asmai repo add") {
		t.Errorf("applying a repository nothing registered returned %v", err)
	}
}

func TestApplyingARegisteredRepositoryIdentityEditIsRefused(t *testing.T) {
	p, configFile := repoDaemon(t)
	if err := os.MkdirAll(filepath.Dir(configFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configFile, []byte(appliedStaffing), 0o600); err != nil {
		t.Fatal(err)
	}
	if resp, err := applyConfig(t, p.Socket, appliedStaffing, true); err != nil || !resp.ConfigApply.Applied {
		t.Fatalf("applying staffing returned %+v, %v", resp.ConfigApply, err)
	}
	origin := remote(t)
	registered, err := addRepo(t, p, origin, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	initial, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []struct{ name, value string }{
		{"location", "/elsewhere"},
		{"origin", "/elsewhere"},
		{"default_branch", "elsewhere"},
	} {
		t.Run(field.name, func(t *testing.T) {
			cfg, problems := config.Parse(initial)
			if len(problems) != 0 {
				t.Fatal(problems)
			}
			entry := cfg.Repositories["fixture"]
			switch field.name {
			case "location":
				entry.Location = field.value
			case "origin":
				entry.Origin = field.value
			case "default_branch":
				entry.DefaultBranch = field.value
			}
			cfg.Repositories["fixture"] = entry
			changed := string(config.Render(cfg))
			if err := os.WriteFile(configFile, []byte(changed), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, confirm := range []bool{false, true} {
				if _, err := applyConfig(t, p.Socket, changed, confirm); err == nil || !strings.Contains(err.Error(), field.name) || !strings.Contains(err.Error(), "does not change the repository record") {
					t.Errorf("apply with confirm=%t returned %v", confirm, err)
				}
			}
		})
	}
	listed, err := Call(p.Socket, Request{Command: CommandRepoShow, Repository: "fixture"})
	if err != nil || listed.Repository == nil || listed.Repository.Location != registered.Repository.Location || listed.Repository.Origin != origin || listed.Repository.DefaultBranch != "trunk" {
		t.Errorf("registered repository changed: %+v, %v", listed.Repository, err)
	}
	if got := countJournaled(t, p.Socket, store.KindConfigurationApplied); got != 1 {
		t.Errorf("refused identity edits journaled %d configurations", got)
	}
}

func TestRepositoryChangesPickOnlyAssignedWorkers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, query := range []string{
		`INSERT INTO jobs(number, repository, state) VALUES (1, 'a', 'open'), (2, 'b', 'open')`,
		`INSERT INTO job_branches(job, repository, name, started_from, tip, view, created_at) VALUES (1, 'a', 'one', 'tip', 'tip', 'view', '2026-01-01T00:00:00Z'), (2, 'b', 'two', 'tip', 'tip', 'view', '2026-01-01T00:00:00Z')`,
		`INSERT INTO assignments(id, job, owner, worker, outcome, criteria, state, created_at) VALUES (1, 1, 'leader@engineering', 'worker1@engineering', 'one', '[]', 'active', '2026-01-01T00:00:00Z'), (2, 2, 'leader@engineering', 'worker2@engineering', 'two', '[]', 'active', '2026-01-01T00:00:00Z'), (3, 1, 'leader@engineering', 'worker4@engineering', 'three', '[]', 'active', '2026-01-01T00:00:00Z')`,
		`INSERT INTO workspaces(assignment, slot, path, tmp, branch, base) VALUES (1, 1, '/one', '/tmp/one', 'one', 'tip'), (2, 2, '/two', '/tmp/two', 'two', 'tip'), (3, 3, '/three', '/tmp/three', 'three', 'tip')`,
	} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	d := &daemon{store: s}
	agents := []store.Agent{
		{Agent: "leader@engineering", Role: "engineering", State: store.AgentRunning},
		{Agent: "leader@quality", Role: "quality", State: store.AgentStopped},
		{Agent: "worker1@engineering", Role: "engineering", State: store.AgentRunning},
		{Agent: "worker2@engineering", Role: "engineering", State: store.AgentRunning},
		{Agent: "worker3@engineering", Role: "engineering", State: store.AgentRunning},
		{Agent: "worker4@engineering", Role: "engineering", State: store.AgentStopped},
	}
	old, _ := config.Parse([]byte("[repositories.a]\norigin = \"a\"\ndefault_branch = \"main\"\n"))
	for _, edited := range []string{"notes = \"new notes\"", `repository_skills = ["engineering"]`, "commit_trailers = false"} {
		newConfig, problems := config.Parse([]byte("[repositories.a]\norigin = \"a\"\ndefault_branch = \"main\"\n" + edited + "\n"))
		if len(problems) != 0 {
			t.Fatal(problems)
		}
		changes := config.Diff(old, newConfig)
		if len(changes) != 1 || changes[0].Repository != "a" {
			t.Fatalf("%s produced changes %+v", edited, changes)
		}
		picked, err := d.pickedUpBy(changes[0], agents)
		want := []string{"worker1@engineering"}
		if strings.HasPrefix(edited, "repository_skills") {
			want = []string{"worker1@engineering", "worker4@engineering"}
		}
		if err != nil || !slices.Equal(picked, want) {
			t.Errorf("%s picked %v, %v", edited, picked, err)
		}
		if edited == "commit_trailers = false" {
			reversed := config.Diff(newConfig, old)
			if len(reversed) != 1 {
				t.Fatalf("reversing %s produced changes %+v", edited, reversed)
			}
			picked, err = d.pickedUpBy(reversed[0], agents)
			if err != nil || !slices.Equal(picked, want) {
				t.Errorf("reversing %s picked %v, %v", edited, picked, err)
			}
		}
	}
	for _, tc := range []struct {
		change config.Change
		want   []string
	}{
		{config.Change{Role: "quality", Side: "leader", Skills: true, After: `["extra"]`}, []string{"leader@quality"}},
		{config.Change{Role: "engineering", Side: "worker", Skills: true, After: `["extra"]`}, []string{"worker1@engineering", "worker2@engineering", "worker3@engineering", "worker4@engineering"}},
		{config.Change{Role: "quality", Side: "leader", Skills: true}, []string{}},
	} {
		picked, err := d.pickedUpBy(tc.change, agents)
		if err != nil || !slices.Equal(picked, tc.want) {
			t.Errorf("role skill change %+v affected %v, %v; want %v", tc.change, picked, err, tc.want)
		}
	}
}

func TestCommitTrailerApplyNamesAffectedWorkerInPreviewAndJournal(t *testing.T) {
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
	if _, err := addRepo(t, p, remote(t), "fixture"); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(p.Store)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	db, err := sql.Open("sqlite3", p.Store)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, query := range []string{
		`INSERT INTO jobs(number, repository, state) VALUES (1, 'fixture', 'open')`,
		`INSERT INTO job_branches(job, repository, name, started_from, tip, view, created_at) VALUES (1, 'fixture', 'one', 'tip', 'tip', 'view', '2026-01-01T00:00:00Z')`,
		`INSERT INTO assignments(id, job, owner, worker, outcome, criteria, state, created_at) VALUES (1, 1, 'leader@engineering', 'worker1@engineering', 'one', '[]', 'active', '2026-01-01T00:00:00Z')`,
		`INSERT INTO workspaces(assignment, slot, path, tmp, branch, base) VALUES (1, 1, '/one', '/tmp/one', 'one', 'tip')`,
	} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.SessionStarted(store.SessionStart{Agent: "worker1@engineering", Role: "engineering", Provider: "claude", Model: "opus", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	cfg, problems := config.Parse(content)
	if len(problems) != 0 {
		t.Fatal(problems)
	}
	options := cfg.Options["fixture"]
	options.CommitTrailers = false
	cfg.Options["fixture"] = options
	changed := string(config.Render(cfg))
	if err := os.WriteFile(configFile, []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	preview, err := applyConfig(t, p.Socket, changed, false)
	if err != nil || preview.ConfigApply == nil || len(preview.ConfigApply.Changes) != 1 {
		t.Fatalf("preview returned %+v, %v", preview.ConfigApply, err)
	}
	change := preview.ConfigApply.Changes[0]
	if change.Setting != "repositories.fixture.commit_trailers" || !slices.Equal(change.Agents, []string{"worker1@engineering"}) || !strings.Contains(change.Note, "next commit") {
		t.Errorf("previewed commit trailer change is %+v", change)
	}
	applied, err := Call(p.Socket, Request{Command: CommandConfigApply, Configuration: changed, Confirm: true, Preview: preview.ConfigApply})
	if err != nil || applied.ConfigApply == nil || !applied.ConfigApply.Applied {
		t.Fatalf("confirmed apply returned %+v, %v", applied.ConfigApply, err)
	}
	journal, err := s.Journal()
	if err != nil {
		t.Fatal(err)
	}
	last := journal[len(journal)-1]
	var recorded store.AppliedConfiguration
	if last.Kind != store.KindConfigurationApplied || json.Unmarshal(last.Data, &recorded) != nil || len(recorded.Changes) != 1 || !slices.Equal(recorded.Changes[0].Agents, []string{"worker1@engineering"}) {
		t.Errorf("journaled commit trailer change is %s %s", last.Kind, last.Data)
	}
}

func TestAgentStaffingUsesTheAppliedConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	before, problems := config.Parse([]byte(appliedStaffing))
	if len(problems) != 0 {
		t.Fatal(problems)
	}
	changed := strings.Replace(appliedStaffing, `leader_model = "opus"`, `leader_model = "sonnet"`, 1)
	if err := os.WriteFile(path, []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	d := &daemon{cfg: Config{ConfigFile: path}, applied: before}
	if cfg, checks := d.staffing(); len(checks) != 0 || cfg.Roles["coordination"].LeaderModel != "opus" {
		t.Errorf("before apply, staffing returned %q and %v", cfg.Roles["coordination"].LeaderModel, checks)
	}
	after, problems := config.Parse([]byte(changed))
	if len(problems) != 0 {
		t.Fatal(problems)
	}
	d.applied = after
	if cfg, checks := d.staffing(); len(checks) != 0 || cfg.Roles["coordination"].LeaderModel != "sonnet" {
		t.Errorf("after apply, staffing returned %q and %v", cfg.Roles["coordination"].LeaderModel, checks)
	}
	d.applied, _ = config.Parse(nil)
	if _, checks := d.staffing(); !slices.ContainsFunc(checks, func(c Check) bool { return strings.Contains(c.Fix, "asmai config apply") }) || len(d.applied.Roles) != 0 {
		t.Errorf("an unapplied first file gave checks %v and applied roles %v", checks, d.applied.Roles)
	}
}

func TestCommitTrailersUseTheAppliedRepositorySetting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, query := range []string{
		`INSERT INTO jobs(number, repository, state) VALUES (1, 'a', 'open')`,
		`INSERT INTO job_branches(job, repository, name, started_from, tip, view, created_at) VALUES (1, 'a', 'one', 'tip', 'tip', 'view', '2026-01-01T00:00:00Z')`,
		`INSERT INTO assignments(id, job, owner, worker, outcome, criteria, state, created_at) VALUES (1, 1, 'leader@engineering', 'worker1@engineering', 'one', '[]', 'active', '2026-01-01T00:00:00Z')`,
		`INSERT INTO workspaces(assignment, slot, path, tmp, branch, base) VALUES (1, 1, '/one', '/tmp/one', 'one', 'tip')`,
		`INSERT INTO messages(id, assignment, job, sender, recipient, kind, body) VALUES (1, 1, 1, 'leader@engineering', 'worker1@engineering', 'assignment', 'one')`,
		`INSERT INTO dispatches(id, message, agent, state, updated_at) VALUES (1, 1, 'worker1@engineering', 'working', '2026-01-01T00:00:00Z')`,
	} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := config.Parse([]byte("[repositories.a]\norigin = \"a\"\ndefault_branch = \"main\"\n"))
	after, _ := config.Parse([]byte("[repositories.a]\norigin = \"a\"\ndefault_branch = \"main\"\ncommit_trailers = false\n"))
	address := roles.WorkerOf(roles.Engineering, 1)
	d := &daemon{
		store: s, applied: before,
		leaders:  map[string]*leader{address.String(): {address: address, session: &session.Session{}, generation: 1, currentDispatch: 1}},
		sessions: map[string]sessionRef{"credential": {address: address, generation: 1}},
	}
	if trailers, err := d.trailers(Request{Session: "credential"}); err != nil || len(trailers) != 3 {
		t.Fatalf("before apply, trailers were %v, %v", trailers, err)
	}
	d.applied = after
	if trailers, err := d.trailers(Request{Session: "credential"}); err != nil || len(trailers) != 0 {
		t.Errorf("after apply, trailers were %v, %v", trailers, err)
	}
}
