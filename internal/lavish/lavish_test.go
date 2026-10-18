// SPDX-License-Identifier: Apache-2.0

package lavish_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/lavish"
	"github.com/talvor/asmai/internal/lavish/fakelavish"
)

func TestMain(m *testing.M) {
	fakelavish.RunIfFake()
	os.Exit(m.Run())
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// syncBuffer is a log buffer that is safe to write from the supervisor and
// read from the test.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// supervised runs a Server of the stand-in in the given mode, quickly paced,
// on a free port in a fresh state directory, until the test ends.
func supervised(t *testing.T, mode string, resolve func() (string, error)) (*lavish.Server, *syncBuffer) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if resolve == nil {
		resolve = func() (string, error) { return exe, nil }
	}
	logs := &syncBuffer{}
	s := &lavish.Server{
		Resolve:  resolve,
		StateDir: filepath.Join(t.TempDir(), "lavish"),
		Port:     freePort(t),
		Log:      slog.New(slog.NewTextHandler(logs, nil)),
		// The user's own Lavish settings are in the daemon's environment.
		Environ:      append(os.Environ(), fakelavish.Variable+"="+mode, "LAVISH_AXI_HOST=0.0.0.0", "LAVISH_AXI_PORT=4387", "LAVISH_AXI_STATE_DIR=/home/user/.lavish-axi", "LAVISH_AXI_TELEMETRY=on"),
		CheckEvery:   100 * time.Millisecond,
		StartupGrace: 3 * time.Second,
		Unhealthy:    2,
		RestartDelay: 50 * time.Millisecond,
		StopGrace:    200 * time.Millisecond,
		CheckTimeout: 300 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return s, logs
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func TestTheEnvironmentGivesTheServerItsOwnStateDirectoryAndPortOnLocalhostWithTelemetryOff(t *testing.T) {
	base := []string{"PATH=/usr/bin", "HOME=/home/user", "LAVISH_AXI_HOST=0.0.0.0", "LAVISH_AXI_PORT=4387", "LAVISH_AXI_STATE_DIR=/home/user/.lavish-axi",
		"LAVISH_AXI_TELEMETRY=on", "LAVISH_AXI_LINK_HOST=laptop.example", "LAVISH_AXI_ALLOWED_HOSTS=laptop.example", "LAVISH_AXI_UMAMI_HOST=https://stats.example"}

	env := lavish.Environment(base, "/state/lavish", 14387)

	got := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if _, dup := got[k]; dup {
			t.Errorf("%s is set twice in %v", k, env)
		}
		got[k] = v
	}
	for k, want := range map[string]string{
		"LAVISH_AXI_STATE_DIR": "/state/lavish",
		"LAVISH_AXI_PORT":      "14387",
		"LAVISH_AXI_HOST":      "127.0.0.1",
		"LAVISH_AXI_TELEMETRY": "off",
		"LAVISH_AXI_NO_OPEN":   "1",
		"PATH":                 "/usr/bin",
		"HOME":                 "/home/user",
	} {
		if got[k] != want {
			t.Errorf("%s is %q, want %q", k, got[k], want)
		}
	}
	for _, k := range []string{"LAVISH_AXI_LINK_HOST", "LAVISH_AXI_ALLOWED_HOSTS", "LAVISH_AXI_UMAMI_HOST"} {
		if v, ok := got[k]; ok {
			t.Errorf("the user's %s=%s reaches the factory's server", k, v)
		}
	}
	if got["LAVISH_AXI_PORT"] == "4387" || lavish.Port == 4387 {
		t.Error("the factory's Lavish uses the default port, which the user's personal Lavish uses")
	}
	if !slices.Equal(lavish.Args(14387), []string{"server", "--port", "14387"}) {
		t.Errorf("the server runs with %v, want the foreground server on its port", lavish.Args(14387))
	}
}

func TestPagesAreTheSessionsTheUserHasNotEnded(t *testing.T) {
	dir := t.TempDir()
	if pages, err := lavish.Pages(dir); err != nil || len(pages) != 0 {
		t.Errorf("a state directory with no state has pages %v (%v), want none", pages, err)
	}
	state := `{"sessions": {
		"b": {"key": "b", "file": "/w/b.html", "url": "http://127.0.0.1:14387/session/b", "status": "feedback", "pending_prompts": 2, "updated_at": "2026-10-18T10:15:33.792Z"},
		"a": {"key": "a", "file": "/w/a.html", "url": "http://127.0.0.1:14387/session/a", "status": "open", "pending_prompts": 0},
		"c": {"key": "c", "file": "/w/c.html", "url": "http://127.0.0.1:14387/session/c", "status": "ended"}
	}}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}

	pages, err := lavish.Pages(dir)

	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 2 || pages[0].File != "/w/a.html" || pages[1].File != "/w/b.html" {
		t.Fatalf("the pages are %+v, want a and b by file, and not the ended c", pages)
	}
	if pages[1].PendingPrompts != 2 || pages[1].Status != "feedback" || pages[1].URL != "http://127.0.0.1:14387/session/b" || pages[1].UpdatedAt.IsZero() {
		t.Errorf("page b is %+v, want its status, link and pending prompts", pages[1])
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := lavish.Pages(dir); err == nil || !strings.Contains(err.Error(), "state.json") {
		t.Errorf("a state file that is not JSON gave %v, want an error naming it", err)
	}
}

func TestTheForwardingCommandForwardsTheServersPortOverSSH(t *testing.T) {
	if got, want := lavish.ForwardCommand("me@build-host", 14387), "ssh -N -L 14387:127.0.0.1:14387 me@build-host"; got != want {
		t.Errorf("ForwardCommand = %q, want %q", got, want)
	}
}

func TestTheSupervisorRunsTheServerOnItsOwnPortAndStateDirectoryOnLocalhostOnly(t *testing.T) {
	s, _ := supervised(t, fakelavish.Well, nil)

	waitFor(t, "the server to be up", func() bool { return s.State().Running })

	resp, err := http.Get(lavish.URL(s.Port) + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var h lavish.Health
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		t.Fatal(err)
	}
	if h.StateDir != s.StateDir || !slices.Equal(h.Hosts, []string{"127.0.0.1"}) {
		t.Errorf("the server serves %s on %v, want %s on 127.0.0.1 only, whatever the daemon's environment says", h.StateDir, h.Hosts, s.StateDir)
	}
	if st := s.State(); st.PID == 0 || st.Problem != "" || st.Restarts != 0 {
		t.Errorf("the state is %+v, want a running server with no problem", st)
	}
}

func TestKillingTheServerRestartsItAndThePagesCreatedBeforeSurvive(t *testing.T) {
	s, logs := supervised(t, fakelavish.Well, nil)
	waitFor(t, "the server to be up", func() bool { return s.State().Running })
	open := func(file string) {
		t.Helper()
		resp, err := http.Post(lavish.URL(s.Port)+"/api/sessions", "application/json", strings.NewReader(fmt.Sprintf(`{"file": %q}`, file)))
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	open("/w/plan.html")
	open("/w/questions.html")
	first := s.State().PID

	if err := syscall.Kill(first, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "the server to be restarted", func() bool { st := s.State(); return st.Running && st.PID != first })
	if st := s.State(); st.Restarts != 1 {
		t.Errorf("the supervisor counts %d restarts, want 1", st.Restarts)
	}
	pages, err := lavish.Pages(s.StateDir)
	if err != nil || len(pages) != 2 || pages[0].File != "/w/plan.html" || pages[1].File != "/w/questions.html" {
		t.Errorf("after the restart the pages are %+v (%v), want the two created before", pages, err)
	}
	if !strings.Contains(logs.String(), "the Lavish server exited") {
		t.Errorf("the log is\n%s\nwant the exit logged", logs.String())
	}
	// The restarted server serves the same state directory.
	resp, err := http.Get(lavish.URL(s.Port) + "/health")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestAServerThatIsAskedToShutDownIsStartedAgain(t *testing.T) {
	s, _ := supervised(t, fakelavish.Well, nil)
	waitFor(t, "the server to be up", func() bool { return s.State().Running })
	first := s.State().PID

	resp, err := http.Post(lavish.URL(s.Port)+"/shutdown", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	waitFor(t, "the server to be started again", func() bool { st := s.State(); return st.Running && st.PID != first })
}

func TestAServerThatStaysUpButStopsAnsweringIsRestartedByThePeriodicCheck(t *testing.T) {
	s, logs := supervised(t, fakelavish.Well, nil)
	waitFor(t, "the server to be up", func() bool { return s.State().Running })
	first := s.State().PID

	// Stopped, not killed: the process is there, and answers nothing.
	if err := syscall.Kill(first, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "the stopped server to be replaced", func() bool { st := s.State(); return st.Running && st.PID != first })
	if !strings.Contains(logs.String(), "failed its check") {
		t.Errorf("the log is\n%s\nwant the failed checks logged", logs.String())
	}
	waitFor(t, "the stopped server's process to be gone", func() bool { return !alive(first) })
}

func TestAServerThatNeverAnswersIsRestartedUntilItDoes(t *testing.T) {
	var mu sync.Mutex
	starts := 0
	exe, _ := os.Executable()
	s, _ := supervised(t, fakelavish.Hangs, func() (string, error) {
		mu.Lock()
		defer mu.Unlock()
		starts++
		return exe, nil
	})
	s.StartupGrace = 300 * time.Millisecond

	waitFor(t, "the server to be started three times", func() bool { mu.Lock(); defer mu.Unlock(); return starts >= 3 })

	if st := s.State(); st.Running {
		t.Errorf("the state is %+v, want a server that never answered not to be running", st)
	}
}

func TestAServerThatAlsoListensBeyondLocalhostIsNotAccepted(t *testing.T) {
	s, logs := supervised(t, fakelavish.AlsoListens, nil)

	waitFor(t, "the problem to be reported", func() bool { return strings.Contains(s.State().Problem, "localhost only") })

	if s.State().Running {
		t.Error("a server that also listens on another address is reported running")
	}
	if !strings.Contains(logs.String(), "100.64.0.7") {
		t.Errorf("the log is\n%s\nwant the extra address named", logs.String())
	}
}

func TestAServerThatKeepsExitingIsStartedAgainWithAGrowingDelay(t *testing.T) {
	var mu sync.Mutex
	var at []time.Time
	exe, _ := os.Executable()
	s, _ := supervised(t, fakelavish.Exits, func() (string, error) {
		mu.Lock()
		defer mu.Unlock()
		at = append(at, time.Now())
		return exe, nil
	})
	s.RestartDelay = 40 * time.Millisecond

	waitFor(t, "four starts", func() bool { mu.Lock(); defer mu.Unlock(); return len(at) >= 4 })

	mu.Lock()
	defer mu.Unlock()
	if gap1, gap3 := at[1].Sub(at[0]), at[3].Sub(at[2]); gap3 < gap1*2 {
		t.Errorf("the gaps between starts are %v then %v, want the delay to grow while the server keeps exiting", gap1, gap3)
	}
	if st := s.State(); st.Running || !strings.Contains(st.Problem, "exited") {
		t.Errorf("the state is %+v, want a server that exited", st)
	}
}

func TestWithNoPinnedBuildToRunTheSupervisorSaysWhyAndStartsItOnceThereIs(t *testing.T) {
	var mu sync.Mutex
	installed := false
	exe, _ := os.Executable()
	s, _ := supervised(t, fakelavish.Well, func() (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if !installed {
			return "", fmt.Errorf("the pinned Lavish build is not installed; run `asmai providers install`")
		}
		return exe, nil
	})

	waitFor(t, "the problem to be reported", func() bool { return strings.Contains(s.State().Problem, "providers install") })
	if s.State().Running {
		t.Fatal("a server is running without a build")
	}

	mu.Lock()
	installed = true
	mu.Unlock()

	waitFor(t, "the server to be started by the periodic check", func() bool { return s.State().Running })
	if p := s.State().Problem; p != "" {
		t.Errorf("the problem is still %q", p)
	}
}

func TestThePortIsReclaimedFromAServerAPreviousDaemonLeftRunning(t *testing.T) {
	exe, _ := os.Executable()
	state := filepath.Join(t.TempDir(), "lavish")
	port := freePort(t)
	orphan := exec.Command(exe, lavish.Args(port)...)
	orphan.Env = append(lavish.Environment(os.Environ(), state, port), fakelavish.Variable+"="+fakelavish.Well)
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := orphan.Start(); err != nil {
		t.Fatal(err)
	}
	orphanExited := make(chan struct{})
	go func() { orphan.Wait(); close(orphanExited) }()
	t.Cleanup(func() { orphan.Process.Kill() })
	waitFor(t, "the orphan to listen", func() bool {
		c, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprint(port)))
		if err == nil {
			c.Close()
		}
		return err == nil
	})
	logs := &syncBuffer{}
	s := &lavish.Server{
		Resolve:  func() (string, error) { return exe, nil },
		StateDir: state, Port: port, Log: slog.New(slog.NewTextHandler(logs, nil)),
		Environ:    append(os.Environ(), fakelavish.Variable+"="+fakelavish.Well),
		CheckEvery: 100 * time.Millisecond, RestartDelay: 50 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	waitFor(t, "the supervisor's own server to be up", func() bool { return s.State().Running })

	select {
	case <-orphanExited:
	case <-time.After(5 * time.Second):
		t.Error("the server the previous daemon left running was not shut down")
	}
	if s.State().PID == orphan.Process.Pid {
		t.Error("the supervisor reports the orphan as its own server")
	}
}

func TestAnotherLavishOnThePortIsReportedAndLeftAlone(t *testing.T) {
	exe, _ := os.Executable()
	theirs := filepath.Join(t.TempDir(), ".lavish-axi")
	port := freePort(t)
	other := exec.Command(exe, lavish.Args(port)...)
	other.Env = append(lavish.Environment(os.Environ(), theirs, port), fakelavish.Variable+"="+fakelavish.Well)
	if err := os.MkdirAll(theirs, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { other.Process.Kill(); other.Wait() })
	waitFor(t, "their Lavish to listen", func() bool {
		c, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprint(port)))
		if err == nil {
			c.Close()
		}
		return err == nil
	})
	s := &lavish.Server{
		Resolve:  func() (string, error) { return exe, nil },
		StateDir: filepath.Join(t.TempDir(), "lavish"), Port: port, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Environ:    append(os.Environ(), fakelavish.Variable+"="+fakelavish.Well),
		CheckEvery: 100 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	waitFor(t, "the problem to be reported", func() bool { return strings.Contains(s.State().Problem, "in use by another Lavish") })

	if s.State().Running || !alive(other.Process.Pid) {
		t.Errorf("the state is %+v and their Lavish alive is %v, want theirs left running and ours not started", s.State(), alive(other.Process.Pid))
	}
}

func TestTheServerIsStoppedWhenTheSupervisorIs(t *testing.T) {
	exe, _ := os.Executable()
	logs := &syncBuffer{}
	s := &lavish.Server{
		Resolve:  func() (string, error) { return exe, nil },
		StateDir: filepath.Join(t.TempDir(), "lavish"), Port: freePort(t), Log: slog.New(slog.NewTextHandler(logs, nil)),
		Environ:    append(os.Environ(), fakelavish.Variable+"="+fakelavish.Well),
		CheckEvery: 100 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	waitFor(t, "the server to be up", func() bool { return s.State().Running })
	pid := s.State().PID

	cancel()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the supervisor did not return")
	}
	if alive(pid) {
		// A zombie still answers signal 0; it is the supervisor's to have waited for.
		t.Errorf("the server (pid %d) is still running after the supervisor returned", pid)
	}
	if st := s.State(); st.Running {
		t.Errorf("the state is %+v, want not running", st)
	}
}

// Run against a real Lavish build, which only a maintainer with one can do:
//
//	ASMAI_REAL_LAVISH=/path/to/lavish go test ./internal/lavish -run Real -v
func TestARealLavishBuildRunsUnderTheSupervisorAndKeepsItsPagesAcrossAKill(t *testing.T) {
	exe := os.Getenv("ASMAI_REAL_LAVISH")
	if exe == "" {
		t.Skip("ASMAI_REAL_LAVISH names no Lavish build")
	}
	s := &lavish.Server{
		Resolve:  func() (string, error) { return exe, nil },
		StateDir: filepath.Join(t.TempDir(), "lavish"), Port: freePort(t), Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Environ:    append(os.Environ(), "LAVISH_AXI_HOST=0.0.0.0", "LAVISH_AXI_TELEMETRY=on"),
		CheckEvery: 200 * time.Millisecond, RestartDelay: 100 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	waitFor(t, "the server to be up", func() bool { return s.State().Running })

	page := filepath.Join(t.TempDir(), "review.html")
	if err := os.WriteFile(page, []byte("<html><body style=\"background:#fff\"><h1>review</h1></body></html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	open := exec.Command(exe, page)
	open.Env = lavish.Environment(os.Environ(), s.StateDir, s.Port)
	if out, err := open.CombinedOutput(); err != nil {
		t.Fatalf("opening a page failed: %v\n%s", err, out)
	}
	first := s.State().PID
	if pages, err := lavish.Pages(s.StateDir); err != nil || len(pages) != 1 || !strings.HasPrefix(pages[0].URL, lavish.URL(s.Port)+"/session/") {
		t.Fatalf("the pages are %+v (%v), want the one opened, linked on the factory's port", pages, err)
	}

	if err := syscall.Kill(first, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "the server to be restarted", func() bool { st := s.State(); return st.Running && st.PID != first })
	if pages, err := lavish.Pages(s.StateDir); err != nil || len(pages) != 1 || pages[0].File != page {
		t.Errorf("after the kill the pages are %+v (%v), want the page created before", pages, err)
	}
	resp, err := http.Get(lavish.URL(s.Port) + "/session/" + strings.TrimPrefix(func() string { p, _ := lavish.Pages(s.StateDir); return p[0].URL }(), lavish.URL(s.Port)+"/session/"))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Errorf("the page does not load after the restart: %v %v", resp, err)
	}
}
