// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

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
	return Call(p, Request{Command: CommandConfigApply, Configuration: text, Confirm: confirm})
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

func TestRemovingARepositoryFromTheFileIsRefusedWhileItHasAnOpenJob(t *testing.T) {
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
	if _, err := applyConfig(t, p.Socket, appliedStaffing, true); err == nil || !strings.Contains(err.Error(), "[repositories.fixture]") || !strings.Contains(err.Error(), "open job 1") {
		t.Errorf("removing a repository with an open job returned %v", err)
	}
	if _, err := os.Stat(clone); err != nil {
		t.Errorf("the refused apply left the clone %v", err)
	}

	if _, err := db.Exec(`UPDATE jobs SET state = 'ended'`); err != nil {
		t.Fatal(err)
	}
	r, err := applyConfig(t, p.Socket, appliedStaffing, true)
	if err != nil || !r.ConfigApply.Applied || !slices.Equal(r.ConfigApply.Removed, []string{"fixture"}) {
		t.Fatalf("applying once the job ended returned %+v, %v", r.ConfigApply, err)
	}
	if _, err := os.Stat(clone); !os.IsNotExist(err) {
		t.Errorf("the clone is still there (%v)", err)
	}
	if listed, _ := Call(p.Socket, Request{Command: CommandRepoList}); len(listed.Repositories) != 0 {
		t.Errorf("the repository is still registered: %+v", listed.Repositories)
	}

	// A table the file has that nothing registered has no clone.
	withTable := appliedStaffing + "\n[repositories.ghost]\norigin = \"o\"\ndefault_branch = \"main\"\n"
	if err := os.WriteFile(configFile, []byte(withTable), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := applyConfig(t, p.Socket, withTable, true); err == nil || !strings.Contains(err.Error(), "asmai repo add") {
		t.Errorf("applying a repository nothing registered returned %v", err)
	}
}
