// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/lavish"
	"github.com/talvor/asmai/internal/lavish/fakelavish"
	"github.com/talvor/asmai/internal/providers"
	"github.com/talvor/asmai/internal/statedir"
)

func TestMain(m *testing.M) {
	fakelavish.RunIfFake()
	// The workers' temporary directories are made in a directory of the
	// tests' own, not the user's /tmp.
	base, err := os.MkdirTemp("", "asmai-tmp")
	if err != nil {
		panic(err)
	}
	tmpBase = base
	code := m.Run()
	os.RemoveAll(base)
	os.Exit(code)
}

// pinnedLavish pins a Lavish build for this platform whose executable is the
// stand-in server, installs it where `asmai providers install` keeps it, and
// returns the pins and the daemon's Lavish settings for a free port, paced
// quickly.
func pinnedLavish(t *testing.T, p statedir.Paths) (providers.Pins, LavishConfig) {
	t.Helper()
	platform, ok := providers.Platform(runtime.GOOS, runtime.GOARCH)
	if !ok {
		t.Skipf("Lavish is not built for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	pins := testPins("2.1.292")
	pins.Lavish = &providers.LavishPin{Version: "0.1.89", Deno: "2.9.6", Releases: "https://example.com/releases", Platforms: map[string]providers.Download{
		platform: {SHA256: hex.EncodeToString(sum[:]), Size: int64(len(data))},
	}}
	plan, err := pins.LavishPlan(platform, p.Providers)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(plan.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plan.Path, data, 0o755); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return pins, LavishConfig{
		Port:         port,
		Environ:      append(os.Environ(), fakelavish.Variable+"="+fakelavish.Well),
		CheckEvery:   100 * time.Millisecond,
		StartupGrace: 5 * time.Second,
		RestartDelay: 50 * time.Millisecond,
		StopGrace:    time.Second,
		CheckTimeout: time.Second,
	}
}

func lavishStatus(t *testing.T, p statedir.Paths) *LavishStatus {
	t.Helper()
	resp, err := Call(p.Socket, Request{Command: CommandLavish})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Lavish == nil {
		t.Fatal("the daemon answered with no Lavish status")
	}
	return resp.Lavish
}

func waitForLavish(t *testing.T, p statedir.Paths, what string, ok func(*LavishStatus) bool) *LavishStatus {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		s := lavishStatus(t, p)
		if ok(s) {
			return s
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s; the status is %+v", what, s)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestTheDaemonRunsTheFactorysOwnLavishServerAndKeepsItsPagesAcrossRestarts(t *testing.T) {
	p := stateDir(t)
	pins, lavishConfig := pinnedLavish(t, p)
	runDaemonWith(t, Config{Paths: p, Version: "v1.2.3", Pins: pins, Lavish: lavishConfig})

	up := waitForLavish(t, p, "the server to be up", func(s *LavishStatus) bool { return s.Running })

	if up.Port != lavishConfig.Port || up.URL != fmt.Sprintf("http://127.0.0.1:%d", lavishConfig.Port) || up.StateDir != p.Lavish {
		t.Errorf("the server is at %s (port %d) with state %s, want the daemon's own port and the state directory's lavish folder %s", up.URL, up.Port, up.StateDir, p.Lavish)
	}
	if up.Build != "0.1.89+deno2.9.6" || up.Problem != "" {
		t.Errorf("the status is %+v, want the pinned build and no problem", up)
	}
	if len(up.Pages) != 0 {
		t.Errorf("a new server has pages %v, want none", up.Pages)
	}
	for _, file := range []string{"/w/plan.html", "/w/questions.html"} {
		resp, err := http.Post(up.URL+"/api/sessions", "application/json", strings.NewReader(fmt.Sprintf(`{"file": %q}`, file)))
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	waitForLavish(t, p, "both pages to be listed", func(s *LavishStatus) bool { return len(s.Pages) == 2 })

	if err := syscall.Kill(up.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}

	again := waitForLavish(t, p, "the killed server to be restarted", func(s *LavishStatus) bool { return s.Running && s.PID != up.PID })
	if again.Restarts != 1 || len(again.Pages) != 2 || again.Pages[0].File != "/w/plan.html" || again.Pages[1].File != "/w/questions.html" {
		t.Errorf("after the restart the status is %+v, want one restart and the two pages created before", again)
	}
}

func TestTheDaemonStopsTheLavishServerWithItself(t *testing.T) {
	p := stateDir(t)
	pins, lavishConfig := pinnedLavish(t, p)
	runDaemonWith(t, Config{Paths: p, Version: "v1.2.3", Pins: pins, Lavish: lavishConfig})
	up := waitForLavish(t, p, "the server to be up", func(s *LavishStatus) bool { return s.Running })

	if _, err := Call(p.Socket, Request{Command: CommandStop}); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for syscall.Kill(up.PID, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("the Lavish server (pid %d) is still running after the daemon stopped", up.PID)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := http.Get(up.URL + "/health"); err == nil {
		t.Error("the server still answers after the daemon stopped")
	}
}

func TestTheDaemonRunsOnlyThePinnedLavishBuild(t *testing.T) {
	p := stateDir(t)
	pins, lavishConfig := pinnedLavish(t, p)
	platform, _ := providers.Platform(runtime.GOOS, runtime.GOARCH)
	plan, err := pins.LavishPlan(platform, p.Providers)
	if err != nil {
		t.Fatal(err)
	}
	// A Lavish that is not the pinned build, as the user's own would be.
	if err := os.WriteFile(plan.Path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	runDaemonWith(t, Config{Paths: p, Version: "v1.2.3", Pins: pins, Lavish: lavishConfig})

	s := waitForLavish(t, p, "the mismatch to be reported", func(s *LavishStatus) bool { return s.Problem != "" })

	if s.Running || !strings.Contains(s.Problem, "not the pinned build") || !strings.Contains(s.Problem, "asmai providers install") {
		t.Errorf("the status is %+v, want a Lavish that is not the pinned build refused with the fix", s)
	}

	// Putting the pinned build back is picked up by the periodic check.
	exe, _ := os.Executable()
	data, _ := os.ReadFile(exe)
	if err := os.WriteFile(plan.Path, data, 0o755); err != nil {
		t.Fatal(err)
	}
	waitForLavish(t, p, "the pinned build to be run once it is back", func(s *LavishStatus) bool { return s.Running })
}

func TestWithNoLavishBuildPinnedOrInstalledTheDaemonSaysSoAndRunsNoServer(t *testing.T) {
	p := stateDir(t)
	// Claude Code only: this asmai pins no Lavish.
	runDaemonWith(t, Config{Paths: p, Version: "v1.2.3", Pins: testPins("2.1.292"), Lavish: LavishConfig{CheckEvery: 50 * time.Millisecond}})

	s := waitForLavish(t, p, "the problem to be reported", func(s *LavishStatus) bool { return s.Problem != "" })

	if s.Running || !strings.Contains(s.Problem, "pins no Lavish") || s.Build != "" || s.Port != lavish.Port {
		t.Errorf("the status is %+v, want no server, the reason and the factory's own port", s)
	}

	// Pinned for another platform than this: not yet published for it.
	other := testPins("2.1.292")
	other.Lavish = &providers.LavishPin{Version: "0.1.89", Deno: "2.9.6", Releases: "https://example.com/releases"}
	p2 := stateDir(t)
	runDaemonWith(t, Config{Paths: p2, Version: "v1.2.3", Pins: other, Lavish: LavishConfig{CheckEvery: 50 * time.Millisecond}})
	s = waitForLavish(t, p2, "the problem to be reported", func(s *LavishStatus) bool { return s.Problem != "" })
	if s.Running || !strings.Contains(s.Problem, "not published") {
		t.Errorf("the status is %+v, want the build said not to be published yet", s)
	}
}

func TestAPinnedLavishThatIsNotInstalledIsSaidToNeedProvidersInstall(t *testing.T) {
	p := stateDir(t)
	pins, lavishConfig := pinnedLavish(t, p)
	platform, _ := providers.Platform(runtime.GOOS, runtime.GOARCH)
	plan, _ := pins.LavishPlan(platform, p.Providers)
	os.Remove(plan.Path)
	runDaemonWith(t, Config{Paths: p, Version: "v1.2.3", Pins: pins, Lavish: lavishConfig})

	s := waitForLavish(t, p, "the problem to be reported", func(s *LavishStatus) bool { return s.Problem != "" })

	if s.Running || !strings.Contains(s.Problem, "is not installed at "+plan.Path) || !strings.Contains(s.Problem, "asmai providers install") {
		t.Errorf("the status is %+v, want the missing build named with its fix", s)
	}
}
