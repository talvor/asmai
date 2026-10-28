// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/talvor/asmai/internal/providers"
)

// pinStandInLavish adds the pinned Lavish build to the pins file the test
// already stands in for, serving build from a stand-in for the release it is
// fetched from. It returns how many Lavish downloads it served.
func pinStandInLavish(t *testing.T, build []byte) (fetches *atomic.Int32) {
	t.Helper()
	platform, ok := providers.Platform(runtime.GOOS, runtime.GOARCH)
	if !ok {
		t.Skipf("Lavish is not built for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	fetches = new(atomic.Int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/releases/lavish-v0.1.89-deno2.9.6/lavish-"+platform {
			http.NotFound(w, r)
			return
		}
		fetches.Add(1)
		w.Write(build)
	}))
	t.Cleanup(srv.Close)
	var all map[string]json.RawMessage
	if err := json.Unmarshal(pins, &all); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(build)
	all["lavish"] = fmt.Appendf(nil, `{"version": "0.1.89", "deno": "2.9.6", "releases": %q, "platforms": {%q: {"sha256": %q, "size": %d}}}`,
		srv.URL+"/releases", platform, hex.EncodeToString(sum[:]), len(build))
	previous := pins
	pins, _ = json.Marshal(all)
	t.Cleanup(func() { pins = previous })
	return fetches
}

// lavishPath is where the pinned Lavish build is kept in stateDir, which is
// where the daemon looks for it when it starts the Lavish server.
func lavishPath(stateDir string) string {
	return filepath.Join(stateDir, "providers", "lavish", "0.1.89+deno2.9.6", "lavish")
}

// answerLavish waits for setup to ask to install the pinned Lavish, and
// answers.
func (s *setupTerminal) answerLavish(t *testing.T, answer string) {
	t.Helper()
	waitFor(t, "setup to ask to install Lavish", s.shows("asmai will install Lavish 0.1.89+deno2.9.6"))
	s.typeKeys(t, answer+"\r")
}

// Init installs the pinned Lavish beside the pinned Claude Code once the
// user confirms, where the daemon looks for it, and skips it when it is
// there already.
func TestInitInstallsThePinnedLavishOnceConfirmed(t *testing.T) {
	stateDir, _ := setupFactory(t, map[string][]string{"agents": trusting, "workspaces": trusting})
	fetches := pinStandInLavish(t, standInLavish)
	startUnready(t)
	agents, workspaces := filepath.Join(stateDir, "agents"), filepath.Join(stateDir, "workspaces")

	s := setupInTerminal(t)
	s.chooseModel(t, "haiku")
	s.confirmInstall(t)
	s.answerLavish(t, "y")
	s.trust(t, agents)
	s.trust(t, workspaces)
	if code := s.exited(t); code != 0 {
		t.Fatalf("asmai init exited %d, printing\n%s", code, s.printed())
	}

	out := s.printed()
	path := lavishPath(stateDir)
	for _, want := range []string{"Install it? [y/N]", "your own Lavish and ~/.lavish-axi are not touched", "keep at  " + path} {
		if !strings.Contains(out, want) {
			t.Errorf("asmai init printed\n%s\nwant it to say %q", out, want)
		}
	}
	if got := lineSaying(out, "Lavish:"); !strings.HasPrefix(got, "Lavish: done. Installed the pinned Lavish 0.1.89+deno2.9.6 at "+path) {
		t.Errorf("init reported %q, want Lavish installed at %s", got, path)
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, standInLavish) {
		t.Errorf("%s holds %q (%v), want the pinned Lavish build", path, got, err)
	}
	if !providers.Executable(path) {
		t.Errorf("%s is not executable", path)
	}
	var shown struct {
		Providers []providerJSON `json:"providers"`
	}
	inProcessJSON(t, &shown, "providers", "list")
	if len(shown.Providers) != 2 || shown.Providers[1].Name != "lavish" || shown.Providers[1].Path != path {
		t.Errorf("asmai providers list shows %+v after init, want the daemon to record Lavish at %s", shown.Providers, path)
	}

	again := setupInTerminal(t)
	if code := again.exited(t); code != 0 {
		t.Fatalf("asmai init again exited %d, printing\n%s", code, again.printed())
	}
	if got := lineSaying(again.printed(), "Lavish:"); !strings.HasPrefix(got, "Lavish: already done.") {
		t.Errorf("init again reported %q, want Lavish already done", got)
	}
	if n := fetches.Load(); n != 1 {
		t.Errorf("the release served %d Lavish downloads, want 1", n)
	}
}

// A user who declines Lavish still gets the rest of init, and is told
// plainly that Lavish is not installed, what that costs and how to install it.
func TestInitSaysPlainlyWhenTheUserDeclinesLavish(t *testing.T) {
	stateDir, _ := setupFactory(t, map[string][]string{"agents": trusting, "workspaces": trusting})
	fetches := pinStandInLavish(t, standInLavish)
	startUnready(t)
	agents, workspaces := filepath.Join(stateDir, "agents"), filepath.Join(stateDir, "workspaces")

	s := setupInTerminal(t)
	s.chooseModel(t, "haiku")
	s.confirmInstall(t)
	s.answerLavish(t, "n")
	s.trust(t, agents)
	s.trust(t, workspaces)
	if code := s.exited(t); code != 0 {
		t.Fatalf("asmai init exited %d after Lavish was declined, printing\n%s", code, s.printed())
	}

	out := s.printed()
	if got := lineSaying(out, "Lavish:"); !strings.HasPrefix(got, "Lavish: stopped. You declined, so the pinned Lavish 0.1.89+deno2.9.6 is not installed") {
		t.Errorf("init reported %q, want Lavish stopped and not installed", got)
	}
	for _, want := range []string{"no decision request can be shown to you", "asmai providers install", "Claude Code: done.", "trust workspaces/: done."} {
		if !strings.Contains(out, want) {
			t.Errorf("asmai init printed\n%s\nwant it to say %q", out, want)
		}
	}
	if _, err := os.Stat(lavishPath(stateDir)); err == nil {
		t.Error("Lavish was installed although the user declined")
	}
	if n := fetches.Load(); n != 0 {
		t.Errorf("the release served %d Lavish downloads, want none", n)
	}

	// The next run asks again.
	again := setupInTerminal(t)
	again.answerLavish(t, "y")
	if code := again.exited(t); code != 0 {
		t.Fatalf("asmai init again exited %d, printing\n%s", code, again.printed())
	}
	if _, err := os.Stat(lavishPath(stateDir)); err != nil {
		t.Errorf("Lavish is not installed after the user confirmed it: %v", err)
	}
}

// init --yes confirms Lavish too, without asking.
func TestInitYesInstallsLavishWithoutAsking(t *testing.T) {
	stateDir, _ := setupFactory(t, map[string][]string{"agents": trusting, "workspaces": trusting})
	pinStandInLavish(t, standInLavish)
	startUnready(t)

	s := setupInTerminal(t, "--yes")
	for _, dir := range []string{"agents", "workspaces"} {
		waitFor(t, "Claude Code to ask about trusting "+dir, s.shows("❯ No, exit"))
		s.typeKeys(t, "\x1b[B")
		waitFor(t, "Yes to be selected", s.shows("❯ Yes, I trust this folder"))
		s.typeKeys(t, "\r")
		waitFor(t, "Claude Code to come up", s.shows("Claude Code is ready"))
		s.typeKeys(t, "/exit\r")
	}
	if code := s.exited(t); code != 0 {
		t.Fatalf("asmai init --yes exited %d, printing\n%s", code, s.printed())
	}
	out := s.printed()
	if strings.Contains(out, "[y/N]") {
		t.Errorf("asmai init --yes asked a question:\n%s", out)
	}
	if got := lineSaying(out, "Lavish:"); !strings.HasPrefix(got, "Lavish: done.") || !providers.Executable(lavishPath(stateDir)) {
		t.Errorf("init --yes reported %q, want Lavish installed at %s", got, lavishPath(stateDir))
	}
}

// init --help says what init installs, Lavish included.
func TestInitHelpSaysWhatInitInstalls(t *testing.T) {
	var stdout, stderr bytes.Buffer
	run([]string{"init", "--help"}, strings.NewReader(""), &stdout, &stderr)
	out := stdout.String() + stderr.String()
	for _, want := range []string{"installs", "the pinned Claude Code", "the pinned Lavish", "after you confirm", "--yes"} {
		if !strings.Contains(out, want) {
			t.Errorf("asmai init --help printed\n%s\nwant it to say %q", out, want)
		}
	}
	if usageText := usage; !strings.Contains(usageText, "install the pinned Claude Code and Lavish") {
		t.Errorf("the usage text does not say init installs Lavish:\n%s", usageText)
	}
}

// Doctor fails a missing pinned Lavish naming the remedy, fails a build that
// is not the pinned one, and passes the pinned build.
func TestDoctorReportsAMissingPinnedLavish(t *testing.T) {
	stateDir := readyFactory(t, idleLeader)
	pinStandInLavish(t, standInLavish)
	fakeUpstream(t, "v0.0.0", nil)

	stdout, stderr, code := doctorIn(t)
	if code != 1 {
		t.Errorf("asmai doctor exited %d with the pinned Lavish missing, want 1; it printed %q", code, stdout+stderr)
	}
	for _, want := range []string{"failed  Lavish 0.1.89+deno2.9.6", "is not installed at " + lavishPath(stateDir), "fix: run `asmai providers install`"} {
		if !strings.Contains(stdout+stderr, want) {
			t.Errorf("asmai doctor printed\n%s\nwant it to say %q", stdout+stderr, want)
		}
	}

	if err := os.MkdirAll(filepath.Dir(lavishPath(stateDir)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lavishPath(stateDir), []byte("#!/bin/sh\necho the user's own lavish\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code = doctorIn(t)
	if code != 1 || !strings.Contains(stdout, "failed  Lavish 0.1.89+deno2.9.6") || !strings.Contains(stdout, "not the pinned build") {
		t.Errorf("asmai doctor exited %d printing %q with another Lavish in place, want a failure saying it is not the pinned build", code, stdout+stderr)
	}

	if err := os.WriteFile(lavishPath(stateDir), standInLavish, 0o755); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code = doctorIn(t)
	if code != 0 || !strings.Contains(stdout, "ok      Lavish 0.1.89+deno2.9.6") || !strings.Contains(stdout, "Every check passed.") {
		t.Errorf("asmai doctor exited %d printing %q with the pinned Lavish installed, want it ok", code, stdout+stderr)
	}
}
