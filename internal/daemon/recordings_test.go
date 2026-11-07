// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bufio"
	"compress/gzip"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/config"
	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/session"
	"github.com/talvor/asmai/internal/statedir"
	"github.com/talvor/asmai/internal/store"
	"github.com/talvor/asmai/internal/vt"
)

func castLines(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	s := bufio.NewScanner(gz)
	for s.Scan() {
		b.WriteString(s.Text() + "\n")
	}
	return b.String()
}

// A dispatch typed into a session is recorded from the moment it is typed
// until its turn ends, in a compressed file whose place the journal holds.
func TestADispatchTypedToASessionIsRecordedUntilItsTurnEnds(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	const agent = "leader@engineering"
	if _, err := s.SessionStarted(store.SessionStart{Agent: agent, Role: roles.Engineering, Provider: "claude-code", Version: "v", Model: "m", Executable: "/bin/sh", Dir: t.TempDir(), PID: 1, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	// The program echoes the nudge back, as a terminal does.
	sess, err := session.Start(session.Spec{Path: "/bin/sh", Args: []string{"-c", `IFS= read -r line; printf 'GOT:%s\r\n' "$line"; sleep 60`}, Env: os.Environ(), Dir: t.TempDir(), Columns: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Stop(100 * time.Millisecond) })
	dispatch := addPendingDispatch(t, s, agent, store.DispatchCreated, 1)
	paths := statedir.At(t.TempDir())
	l := &leader{address: roles.LeaderOf(roles.Engineering), session: sess, generation: 1}
	d := &daemon{leaders: map[string]*leader{agent: l}, store: s, log: slog.New(slog.NewTextHandler(io.Discard, nil)), cfg: Config{Paths: paths}}

	d.mu.Lock()
	l.pendingDispatch = dispatch
	d.syncRecording(l)
	d.mu.Unlock()
	sess.Input([]byte("hello\r"))
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(screenOf(sess), "GOT:hello"); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the program never answered")
		}
	}
	d.mu.Lock()
	l.pendingDispatch = 0
	d.syncRecording(l)
	d.mu.Unlock()

	r, ok, err := s.Recording(dispatch)
	if err != nil || !ok {
		t.Fatalf("the dispatch has no recording (%v)", err)
	}
	if want := filepath.Join(paths.Recordings, agent, "dispatch-1.cast.gz"); r.Path != want {
		t.Errorf("the recording is at %s, want %s", r.Path, want)
	}
	info, err := os.Stat(r.Path)
	if err != nil || info.Size() == 0 || r.Size != info.Size() {
		t.Errorf("the journaled size %d, the file %v (%v)", r.Size, info, err)
	}
	if got := castLines(t, r.Path); !strings.Contains(got, "GOT:hello") {
		t.Errorf("the recording holds %q, want the terminal's output", got)
	}
	if l.recording != nil {
		t.Error("the leader is still recording after its dispatch ended")
	}
}

func screenOf(s *session.Session) string {
	t, _ := vt.FromState(s.Screen())
	return t.String()
}

// Recordings are deleted once their job ended for the retention period, a
// setting that defaults to 14 days, and no sooner.
func TestARecordingIsDeletedOnceItsJobEndedForTheRetention(t *testing.T) {
	custom := config.DefaultLogs()
	custom.RecordingRetention = 2 * time.Hour
	for name, tc := range map[string]struct {
		logs      config.Logs
		retention time.Duration
	}{
		"the default": {config.DefaultLogs(), 14 * 24 * time.Hour},
		"a setting":   {custom, 2 * time.Hour},
	} {
		t.Run(name, func(t *testing.T) {
			s, err := store.Open(filepath.Join(t.TempDir(), "store.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { s.Close() })
			const agent = "leader@engineering"
			dispatch := addPendingDispatch(t, s, agent, store.DispatchCreated, 1)
			jobNumber, err := s.DispatchJob(dispatch)
			if err != nil {
				t.Fatal(err)
			}
			job, err := s.Job(jobNumber)
			if err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(t.TempDir(), agent, "dispatch-1.cast.gz")
			if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil || os.WriteFile(file, []byte("x"), 0o600) != nil {
				t.Fatal("making the recording file")
			}
			ended := time.Now()
			if _, err := s.RecordingStarted(dispatch, file, ended); err != nil {
				t.Fatal(err)
			}
			d := &daemon{leaders: map[string]*leader{}, store: s, log: slog.New(slog.NewTextHandler(io.Discard, nil)), applied: config.Config{Logs: tc.logs}}

			d.sweepRecordings(ended.Add(100 * 24 * time.Hour))
			if _, err := os.Stat(file); err != nil {
				t.Fatalf("a recording of an open job was deleted: %v", err)
			}
			if _, err := s.JobCancelled(store.JobCancel{Job: job.Number, Witness: job.Witness}, ended); err != nil {
				t.Fatal(err)
			}
			d.sweepRecordings(ended.Add(tc.retention - time.Minute))
			if _, err := os.Stat(file); err != nil {
				t.Fatalf("a recording was deleted before %s after its job ended: %v", tc.retention, err)
			}
			d.sweepRecordings(ended.Add(tc.retention))
			if _, err := os.Stat(file); !os.IsNotExist(err) {
				t.Fatalf("the recording is still there %s after its job ended: %v", tc.retention, err)
			}
			if r, _, _ := s.Recording(dispatch); r.Deleted.IsZero() {
				t.Error("the deletion was not recorded")
			}
		})
	}
}
