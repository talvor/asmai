// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/statedir"
	"github.com/talvor/asmai/internal/store"
)

// stateDir returns the paths in a new state directory, short enough for its
// socket on every platform.
func stateDir(t *testing.T) statedir.Paths {
	t.Helper()
	dir, err := os.MkdirTemp("", "asmai")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return statedir.At(filepath.Join(dir, "state"))
}

// runDaemon runs a daemon in the background until the test ends, and waits
// for it to answer. stopped waits for it to stop and returns what Run did.
func runDaemon(t *testing.T, p statedir.Paths) (stopped func() error, cancel func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var err error
	go func() {
		err = Run(ctx, Config{Paths: p, Version: "v1.2.3"})
		close(done)
	}()
	stopped = func() error {
		<-done
		return err
	}
	t.Cleanup(func() {
		cancel()
		stopped()
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := Call(p.Socket, Request{Command: CommandStatus}); err == nil {
			return stopped, cancel
		}
		select {
		case <-done:
			t.Fatalf("the daemon exited before it answered: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the daemon did not answer")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func journal(t *testing.T, p statedir.Paths) []string {
	t.Helper()
	s, err := store.Open(p.Store)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	entries, err := s.Journal()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Kind+" "+string(e.Data))
	}
	return got
}

func TestTheDaemonAnswersOnlyOnAPrivateSocketAndStopsWhenAsked(t *testing.T) {
	p := stateDir(t)
	stopped, _ := runDaemon(t, p)

	resp, err := Call(p.Socket, Request{Command: CommandStatus})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status.Version != "v1.2.3" || resp.Status.PID != os.Getpid() {
		t.Errorf("status is %+v, want version v1.2.3 and this process", resp.Status)
	}
	for path, want := range map[string]os.FileMode{p.Dir: 0o700, p.Socket: 0o600, p.Store: 0o600, p.Log: 0o600} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s has mode %v, want %v", filepath.Base(path), got, want)
		}
	}

	if _, err := Call(p.Socket, Request{Command: CommandStop}); err != nil {
		t.Fatal(err)
	}
	if err := stopped(); err != nil {
		t.Fatalf("the daemon stopped with %v", err)
	}
	if _, err := os.Stat(p.Socket); !os.IsNotExist(err) {
		t.Errorf("the socket is still there after the stop (%v)", err)
	}
	if _, err := Call(p.Socket, Request{Command: CommandStatus}); !errors.Is(err, ErrNotRunning) {
		t.Errorf("status after the stop got %v, want ErrNotRunning", err)
	}
	pid := os.Getpid()
	want := []string{
		`daemon.started {"pid":` + strconv.Itoa(pid) + `,"previous":"new","version":"v1.2.3"}`,
		`daemon.stopped {"by":"asmai stop","pid":` + strconv.Itoa(pid) + `,"version":"v1.2.3"}`,
	}
	if got := journal(t, p); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("the journal holds\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestASecondDaemonForTheSameUserDoesNotStart(t *testing.T) {
	p := stateDir(t)
	runDaemon(t, p)
	err := Run(context.Background(), Config{Paths: p, Version: "v1.2.3"})
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Errorf("a second daemon got %v, want ErrAlreadyRunning", err)
	}
	if _, err := Call(p.Socket, Request{Command: CommandStatus}); err != nil {
		t.Errorf("the first daemon no longer answers: %v", err)
	}
}

func TestASignalStopsTheDaemonAndIsJournaled(t *testing.T) {
	p := stateDir(t)
	stopped, cancel := runDaemon(t, p)
	cancel()
	if err := stopped(); err != nil {
		t.Fatalf("the daemon stopped with %v", err)
	}
	got := journal(t, p)
	if len(got) != 2 || !strings.HasPrefix(got[1], `daemon.stopped {"by":"signal"`) {
		t.Errorf("the journal holds %q, want the start then a stop by signal", got)
	}
}

func TestTheDaemonRefusesWhatItDoesNotServe(t *testing.T) {
	p := stateDir(t)
	runDaemon(t, p)
	_, err := Call(p.Socket, Request{Command: "frobnicate"})
	if err == nil || !strings.Contains(err.Error(), `does not serve "frobnicate"`) {
		t.Errorf("an unknown command got %v, want it refused", err)
	}
}
