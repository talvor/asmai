// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func open(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func kinds(t *testing.T, s *Store) []string {
	t.Helper()
	entries, err := s.Journal()
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, e := range entries {
		kinds = append(kinds, e.Kind)
	}
	return kinds
}

func TestANewStoreIsReadableOnlyByTheUserAndHasAnEmptyJournal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	s := open(t, path)

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("the store's mode is %v, want -rw-------", mode)
	}
	if f, err := s.Factory(); err != nil || f.State != StateNew {
		t.Errorf("a new store's factory is %+v (%v), want state %q", f, err, StateNew)
	}
	if got := kinds(t, s); len(got) != 0 {
		t.Errorf("a new store's journal holds %v, want nothing", got)
	}
}

func TestStartsAndStopsAreJournaledAndTheNextOpenFindsTheStoreAsItWasLeft(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	start := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	s := open(t, path)
	previous, err := s.Started("v1.0.0", 4242, start)
	if err != nil {
		t.Fatal(err)
	}
	if previous.State != StateNew {
		t.Errorf("the first start found the store %q, want %q", previous.State, StateNew)
	}
	if err := s.Stopped("asmai stop", start.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s = open(t, path)
	f, err := s.Factory()
	if err != nil {
		t.Fatal(err)
	}
	want := Factory{State: StateStopped, Version: "v1.0.0", PID: 4242, StartedAt: start, StoppedAt: start.Add(time.Minute)}
	if f != want {
		t.Errorf("after reopening, the factory is %+v, want %+v", f, want)
	}
	previous, err = s.Started("v1.0.1", 4343, start.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if previous != want {
		t.Errorf("the second start found %+v, want %+v", previous, want)
	}

	entries, err := s.Journal()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Kind+" "+e.At.Format(time.RFC3339)+" "+string(e.Data))
	}
	wantEntries := []string{
		`daemon.started 2026-10-08T09:00:00Z {"pid":4242,"previous":"new","version":"v1.0.0"}`,
		`daemon.stopped 2026-10-08T09:01:00Z {"by":"asmai stop","pid":4242,"version":"v1.0.0"}`,
		`daemon.started 2026-10-08T10:00:00Z {"pid":4343,"previous":"stopped","version":"v1.0.1"}`,
	}
	if strings.Join(got, "\n") != strings.Join(wantEntries, "\n") {
		t.Errorf("the journal holds\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(wantEntries, "\n"))
	}
	for i, e := range entries {
		if e.ID != int64(i+1) {
			t.Errorf("entry %d has ID %d, want %d", i, e.ID, i+1)
		}
	}
}

// A start that finds the factory still running means the last daemon did not
// stop cleanly.
func TestAStartAfterAnUncleanStopFindsTheFactoryRunning(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	if _, err := s.Started("dev", 1, time.Now()); err != nil {
		t.Fatal(err)
	}
	previous, err := s.Started("dev", 2, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if previous.State != StateRunning || previous.PID != 1 {
		t.Errorf("the second start found %+v, want the first daemon still running", previous)
	}
}

func TestAStopWithNoDaemonRunningChangesNothing(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	if err := s.Stopped("asmai stop", time.Now()); err == nil {
		t.Error("a stop on a new store succeeded, want an error")
	}
	if got := kinds(t, s); len(got) != 0 {
		t.Errorf("the journal holds %v, want nothing", got)
	}
}

// A change and its journal entry are written in one transaction: when either
// fails, neither is kept.
func TestAChangeAndItsJournalEntryAreKeptTogetherOrNotAtAll(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	if _, err := s.Started("dev", 1, time.Now()); err != nil {
		t.Fatal(err)
	}

	err := s.change(time.Now(), func(tx *sql.Tx) (string, any, error) {
		if _, err := tx.Exec(`UPDATE factory SET state = 'changed'`); err != nil {
			return "", nil, err
		}
		return "", nil, errors.New("the change failed after writing")
	})
	if err == nil {
		t.Fatal("the failed change succeeded")
	}
	err = s.change(time.Now(), func(tx *sql.Tx) (string, any, error) {
		_, err := tx.Exec(`UPDATE factory SET state = 'changed'`)
		return "test.unjournalable", func() {}, err
	})
	if err == nil {
		t.Fatal("the change with an entry that cannot be journaled succeeded")
	}

	if f, err := s.Factory(); err != nil || f.State != StateRunning {
		t.Errorf("the factory is %+v (%v), want it still running", f, err)
	}
	if got := kinds(t, s); strings.Join(got, ",") != KindDaemonStarted {
		t.Errorf("the journal holds %v, want only the start", got)
	}
}

func TestTheJournalIsAppendOnly(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	if _, err := s.Started("dev", 1, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`UPDATE journal SET kind = 'rewritten'`,
		`DELETE FROM journal`,
	} {
		if _, err := s.db.Exec(stmt); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%s: got %v, want the journal to refuse", stmt, err)
		}
	}
	if got := kinds(t, s); strings.Join(got, ",") != KindDaemonStarted {
		t.Errorf("the journal holds %v, want the start unchanged", got)
	}
}

func TestOpenRefusesAStoreItCannotUse(t *testing.T) {
	for name, prepare := range map[string]func(t *testing.T, path string){
		"not a database": func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte(strings.Repeat("not a database\n", 100)), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"another program's database": func(t *testing.T, path string) {
			db, err := sql.Open("sqlite3", "file:"+path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.Exec(`CREATE TABLE notes (text TEXT)`); err != nil {
				t.Fatal(err)
			}
		},
		"written by a later asmai": func(t *testing.T, path string) {
			s := open(t, path)
			if _, err := s.db.Exec(`PRAGMA user_version = 99`); err != nil {
				t.Fatal(err)
			}
			s.Close()
		},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "store.db")
			prepare(t, path)
			s, err := Open(path)
			if err == nil {
				s.Close()
				t.Fatal("Open succeeded, want it to refuse the store")
			}
			if !strings.Contains(err.Error(), "cannot be used") {
				t.Errorf("Open's error %q does not say the store cannot be used", err)
			}
		})
	}
}

func TestJournalEntriesEncodeAsJSON(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	if _, err := s.Started("dev", 7, time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	entries, err := s.Journal()
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"id":1,"at":"2026-10-08T09:00:00Z","kind":"daemon.started","data":{"pid":7,"previous":"new","version":"dev"}}]`
	if string(got) != want {
		t.Errorf("the journal encodes as\n%s\nwant\n%s", got, want)
	}
}

func TestAProviderInstallIsRecordedAndJournaledOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	s := open(t, path)
	if got, err := s.Providers(); err != nil || len(got) != 0 {
		t.Fatalf("a new store records providers %v (%v), want none", got, err)
	}
	at := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	install := ProviderInstall{Name: "claude-code", Version: "2.1.292", Path: "/state/providers/claude-code/2.1.292/claude", SHA256: strings.Repeat("a", 64), InstalledAt: at}

	recorded, err := s.ProviderInstalled(install)
	if err != nil || !recorded {
		t.Fatalf("ProviderInstalled = %v, %v, want it recorded", recorded, err)
	}
	again := install
	again.InstalledAt = at.Add(time.Hour)
	if recorded, err := s.ProviderInstalled(again); err != nil || recorded {
		t.Errorf("recording the same install again = %v, %v, want nothing recorded", recorded, err)
	}
	s.Close()

	s = open(t, path)
	got, err := s.Providers()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != install {
		t.Errorf("the store records %+v, want only %+v", got, install)
	}
	entries, err := s.Journal()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Kind != KindProviderInstalled {
		t.Fatalf("the journal holds %+v, want one provider install", entries)
	}
	want := `{"name":"claude-code","path":"/state/providers/claude-code/2.1.292/claude","sha256":"` + strings.Repeat("a", 64) + `","version":"2.1.292"}`
	if string(entries[0].Data) != want {
		t.Errorf("the journal entry holds %s, want %s", entries[0].Data, want)
	}
}

// A store an earlier asmai wrote, at schema 1, is migrated with everything it
// holds.
func TestOpenMigratesAStoreAnEarlierAsmaiWrote(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	db, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		migrations[0],
		`INSERT INTO journal (at, kind, data) VALUES ('2026-10-08T09:00:00Z', 'daemon.started', '{}')`,
		`PRAGMA user_version = 1`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	s := open(t, path)
	if got := kinds(t, s); strings.Join(got, ",") != KindDaemonStarted {
		t.Errorf("the migrated journal holds %v, want the earlier start", got)
	}
	if _, err := s.ProviderInstalled(ProviderInstall{Name: "claude-code", Version: "2.1.292", Path: "/p", SHA256: "s", InstalledAt: time.Now()}); err != nil {
		t.Errorf("the migrated store cannot record a provider install: %v", err)
	}
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != schemaVersion {
		t.Errorf("the migrated store is at schema %d (%v), want %d", version, err, schemaVersion)
	}
}

func TestEachSessionOfAnAgentIsTheNextGenerationAndIsJournaled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	s := open(t, path)
	at := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	start := SessionStart{
		Agent: "leader@coordination", Role: "coordination", Provider: "claude-code", Version: "2.1.292", Model: "opus",
		Executable: "/state/providers/claude-code/2.1.292/claude", Args: []string{"--model", "opus"}, Dir: "/state/agents/leader@coordination",
		PID: 41, At: at,
	}
	if generation, err := s.SessionStarted(start); err != nil || generation != 1 {
		t.Fatalf("the first session is generation %d (%v), want 1", generation, err)
	}
	payload := json.RawMessage(`{"hook_event_name":"Stop","session_id":"s"}`)
	if err := s.Observed("leader@coordination", "coordination", 1, "Stop", payload, at); err != nil {
		t.Fatal(err)
	}
	if err := s.SessionEnded("leader@coordination", 1, "exit status 0", "asmai stop", at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	start.PID = 42
	if generation, err := s.SessionStarted(start); err != nil || generation != 2 {
		t.Fatalf("the second session is generation %d (%v), want 2", generation, err)
	}
	s.Close()

	s = open(t, path)
	agents, err := s.Agents()
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || agents[0].Agent != "leader@coordination" || agents[0].State != AgentRunning || agents[0].Generation != 2 || agents[0].PID != 42 {
		t.Errorf("the store records %+v, want generation 2 of leader@coordination running", agents)
	}
	// A daemon that did not stop cleanly left the session running; the next
	// start ends it.
	if err := s.EndAbandonedSessions(at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if agents, _ := s.Agents(); agents[0].State != AgentStopped || agents[0].Exit != "unknown" {
		t.Errorf("after the abandoned sessions ended, the store records %+v", agents)
	}
	entries, err := s.Journal()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Kind+" "+string(e.Data))
	}
	want := []string{
		`session.started {"agent":"leader@coordination","role":"coordination","provider":"claude-code","version":"2.1.292","model":"opus","executable":"/state/providers/claude-code/2.1.292/claude","args":["--model","opus"],"dir":"/state/agents/leader@coordination","pid":41,"generation":1}`,
		`observation {"agent":"leader@coordination","event":"Stop","generation":1,"payload":{"hook_event_name":"Stop","session_id":"s"},"role":"coordination"}`,
		`session.ended {"agent":"leader@coordination","by":"asmai stop","exit":"exit status 0","generation":1,"role":"coordination"}`,
		`session.started {"agent":"leader@coordination","role":"coordination","provider":"claude-code","version":"2.1.292","model":"opus","executable":"/state/providers/claude-code/2.1.292/claude","args":["--model","opus"],"dir":"/state/agents/leader@coordination","pid":42,"generation":2}`,
		`session.ended {"agent":"leader@coordination","by":"the previous daemon, which did not stop cleanly","exit":"unknown","generation":2,"role":"coordination"}`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("the journal holds\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestAWitnessedMessageIsJournaledWithTheObservationItCites(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	at := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	if err := s.ConversationEntered("leader@coordination", "coordination", 1, "/dev/pts/3", at); err != nil {
		t.Fatal(err)
	}
	w := Witnessed{Agent: "leader@coordination", Role: "coordination", Generation: 1, Terminal: "/dev/pts/3", Text: "open a job"}
	payload := json.RawMessage(`{"hook_event_name":"UserPromptSubmit","prompt":"open a job"}`)
	observation, message, err := s.ObservedWitnessed(w, "UserPromptSubmit", payload, at.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if observation != 2 || message != 3 {
		t.Errorf("the observation and the witnessed message are entries %d and %d, want 2 and 3", observation, message)
	}
	if err := s.ConversationLeft("leader@coordination", "coordination", 1, "/dev/pts/3", "detached", "automation", at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	entries, err := s.Journal()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Kind+" "+string(e.Data))
	}
	want := []string{
		`conversation.entered {"agent":"leader@coordination","generation":1,"role":"coordination","terminal":"/dev/pts/3"}`,
		`observation {"agent":"leader@coordination","event":"UserPromptSubmit","generation":1,"payload":{"hook_event_name":"UserPromptSubmit","prompt":"open a job"},"role":"coordination"}`,
		`message.witnessed {"agent":"leader@coordination","role":"coordination","generation":1,"terminal":"/dev/pts/3","text":"open a job","observation":2}`,
		`conversation.left {"agent":"leader@coordination","generation":1,"how":"detached","input":"automation","role":"coordination","terminal":"/dev/pts/3"}`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("the journal holds\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if !entries[2].At.Equal(at.Add(time.Second)) {
		t.Errorf("the witnessed message is journaled at %v, want %v", entries[2].At, at.Add(time.Second))
	}
}

func TestARegisteredRepositoryIsRecordedJournaledAndListedByName(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	at := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	otman := Repository{Name: "otman", Location: "/home/me/src/otman", Origin: "git@github.com:me/otman.git", DefaultBranch: "main", Clone: "/state/repositories/otman", AddedAt: at}
	fixture := Repository{Name: "fixture", Origin: "https://example.test/fixture.git", DefaultBranch: "trunk", Clone: "/state/repositories/fixture", AddedAt: at.Add(time.Minute)}
	for _, r := range []Repository{otman, fixture} {
		if err := s.RepositoryAdded(r); err != nil {
			t.Fatal(err)
		}
	}

	if got, err := s.Repository("otman"); err != nil || got != otman {
		t.Errorf("Repository(otman) is %+v (%v), want %+v", got, err, otman)
	}
	list, err := s.Repositories()
	if err != nil || len(list) != 2 || list[0] != fixture || list[1] != otman {
		t.Errorf("Repositories is %+v (%v), want fixture then otman", list, err)
	}
	if _, err := s.Repository("nope"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("Repository of an unregistered name returned %v, want sql.ErrNoRows", err)
	}
	if got := strings.Join(kinds(t, s), ","); got != KindRepositoryAdded+","+KindRepositoryAdded {
		t.Errorf("the journal holds %s, want both registrations", got)
	}
	entries, _ := s.Journal()
	var journaled Repository
	if err := json.Unmarshal(entries[0].Data, &journaled); err != nil || journaled != otman {
		t.Errorf("the journal entry holds %+v (%v), want the repository as recorded", journaled, err)
	}
}

func TestARepositoryCannotBeRegisteredTwice(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	r := Repository{Name: "otman", Origin: "o", DefaultBranch: "main", Clone: "/c", AddedAt: time.Now()}
	if err := s.RepositoryAdded(r); err != nil {
		t.Fatal(err)
	}
	if err := s.RepositoryAdded(r); !errors.Is(err, ErrRepositoryRegistered) {
		t.Errorf("registering the name again returned %v, want ErrRepositoryRegistered", err)
	}
	if got := strings.Join(kinds(t, s), ","); got != KindRepositoryAdded {
		t.Errorf("the journal holds %s, want the one registration", got)
	}
}

func TestARepositoryIsRemovedAndJournaledUnlessItHasAnOpenJob(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "store.db"))
	at := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	for _, name := range []string{"otman", "fixture"} {
		if err := s.RepositoryAdded(Repository{Name: name, Origin: "o", DefaultBranch: "main", Clone: "/c/" + name, AddedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	for _, job := range []struct{ repository, state string }{{"otman", "open"}, {"otman", "paused"}, {"otman", JobEnded}, {"fixture", JobEnded}} {
		if _, err := s.db.Exec(`INSERT INTO jobs (repository, state) VALUES (?, ?)`, job.repository, job.state); err != nil {
			t.Fatal(err)
		}
	}

	var open *OpenJobsError
	if _, err := s.RepositoryRemoved("otman", at); !errors.As(err, &open) || len(open.Jobs) != 2 || open.Jobs[0] != 1 || open.Jobs[1] != 2 {
		t.Errorf("removing a repository with an open and a paused job returned %v, want its jobs 1 and 2", err)
	}
	if _, err := s.Repository("otman"); err != nil {
		t.Errorf("the refused removal left the repository %v", err)
	}

	removed, err := s.RepositoryRemoved("fixture", at.Add(time.Hour))
	if err != nil || removed.Name != "fixture" || removed.Clone != "/c/fixture" {
		t.Errorf("removing a repository whose job ended returned %+v, %v", removed, err)
	}
	if _, err := s.Repository("fixture"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("after its removal the repository is still there (%v)", err)
	}
	if _, err := s.RepositoryRemoved("fixture", at); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("removing it again returned %v, want sql.ErrNoRows", err)
	}
	if got := strings.Join(kinds(t, s), ","); got != strings.Join([]string{KindRepositoryAdded, KindRepositoryAdded, KindRepositoryRemoved}, ",") {
		t.Errorf("the journal holds %s, want two registrations and the one removal", got)
	}
}
