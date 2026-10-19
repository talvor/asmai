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

	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/lavish"
	"github.com/talvor/asmai/internal/providers"
)

// standInLavish is what the stand-in release serves as the Lavish build: a
// script that prints notices of its own, which asmai notices must not show.
var standInLavish = []byte("#!/bin/sh\n[ \"$1\" = --notices ] && echo 'Lavish build notices: lavish-axi, npm packages, fonts and Deno'\nexit 0\n")

// standInReleases serves lavish as the pinned Lavish build, and Claude Code
// as in standInChannel, on stand-ins for the channels they are fetched from,
// and pins both. pinnedLavish is what the pins file says the build is, which
// is not what is served when a test serves another. It returns how many
// Lavish downloads it served.
func standInReleases(t *testing.T, pinnedLavish, served []byte) (lavishFetches *atomic.Int32) {
	t.Helper()
	platform, ok := providers.Platform(runtime.GOOS, runtime.GOARCH)
	if !ok {
		t.Skipf("Lavish is not built for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	lavishFetches = new(atomic.Int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/claude/2.1.292/" + platform + "/claude":
			w.Write(standInClaude)
		case "/releases/lavish-v0.1.89-deno2.9.6/lavish-" + platform:
			lavishFetches.Add(1)
			w.Write(served)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	claude, lavishSum := sha256.Sum256(standInClaude), sha256.Sum256(pinnedLavish)
	previous := pins
	pins = fmt.Appendf(nil, `{"claude-code": {"version": "2.1.292", "channel": %q, "platforms": {%q: {"sha256": %q, "size": %d}}},
		"lavish": {"version": "0.1.89", "deno": "2.9.6", "releases": %q, "platforms": {%q: {"sha256": %q, "size": %d}}}}`,
		srv.URL+"/claude", platform, hex.EncodeToString(claude[:]), len(standInClaude),
		srv.URL+"/releases", platform, hex.EncodeToString(lavishSum[:]), len(pinnedLavish))
	t.Cleanup(func() { pins = previous })
	return lavishFetches
}

// inProcess runs asmai in this process, which reads the pins file the test
// stands in for, rather than the built executable's embedded one.
func inProcess(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(args, strings.NewReader(""), &out, &errOut)
	return out.String(), errOut.String(), code
}

func inProcessJSON(t *testing.T, v any, args ...string) {
	t.Helper()
	stdout, stderr, code := inProcess(t, append(args, "--json")...)
	if code != 0 {
		t.Fatalf("asmai %s --json exited %d: %s%s", strings.Join(args, " "), code, stdout, stderr)
	}
	if err := json.Unmarshal([]byte(stdout), v); err != nil {
		t.Fatalf("asmai %s --json printed %q, which is not JSON: %v", strings.Join(args, " "), stdout, err)
	}
}

type installedJSON struct {
	Installed bool           `json:"installed"`
	Provider  providerJSON   `json:"provider"`
	Providers []providerJSON `json:"providers"`
}

func TestProvidersInstallFetchesThePinnedLavishBuildBesideClaudeCodeOnceConfirmed(t *testing.T) {
	stateDir := factoryHome(t)
	fetches := standInReleases(t, standInLavish, standInLavish)
	startUnready(t)
	path := filepath.Join(stateDir, "providers", "lavish", "0.1.89+deno2.9.6", "lavish")

	stdout, stderr, code := install(t, "y\n")

	if code != 0 {
		t.Fatalf("asmai providers install exited %d: %s%s", code, stdout, stderr)
	}
	for _, want := range []string{"Claude Code 2.1.292", "Lavish 0.1.89+deno2.9.6", "/releases/lavish-v0.1.89-deno2.9.6/lavish-", "a release of talvor/asmai", "SHA-256", path,
		"your own Lavish and ~/.lavish-axi are not touched, and it needs no Node", "Install them? [y/N]", "Installed Claude Code 2.1.292.", "Installed Lavish 0.1.89+deno2.9.6."} {
		if !strings.Contains(stdout, want) {
			t.Errorf("asmai providers install printed\n%s\nwant it to show %q", stdout, want)
		}
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, standInLavish) {
		t.Errorf("%s holds %q (%v), want the pinned Lavish build", path, got, err)
	}
	if !providers.Executable(path) {
		t.Errorf("%s is not executable", path)
	}
	if n := fetches.Load(); n != 1 {
		t.Errorf("the release served %d Lavish downloads, want 1", n)
	}
	var shown struct {
		Providers []providerJSON `json:"providers"`
	}
	inProcessJSON(t, &shown, "providers", "list")
	list := shown.Providers
	if len(list) != 2 || list[0].Name != "claude-code" || list[1].Name != "lavish" || list[1].Path != path || list[1].Version != "0.1.89+deno2.9.6" {
		t.Errorf("asmai providers list shows %+v, want Claude Code and the Lavish build", list)
	}
	out, _, _ := inProcess(t, "providers", "list")
	if !strings.Contains(out, "lavish") || !strings.Contains(out, "pinned") || strings.Contains(out, "not the pinned download") {
		t.Errorf("asmai providers list printed\n%s\nwant both installs marked pinned", out)
	}

	// The same pins again do nothing.
	stdout, stderr, code = install(t, "")
	if code != 0 || !strings.Contains(stdout, "Lavish 0.1.89+deno2.9.6 is already installed") || strings.Contains(stdout, "Install") && strings.Contains(stdout, "[y/N]") {
		t.Errorf("a repeat install exited %d printing\n%s%s\nwant both said to be installed already", code, stdout, stderr)
	}
	if n := fetches.Load(); n != 1 {
		t.Errorf("the release served %d downloads after a repeat install, want 1", n)
	}

	// --json says what was installed.
	var result installedJSON
	inProcessJSON(t, &result, "providers", "install")
	if result.Installed || len(result.Providers) != 2 || result.Provider.Name != "claude-code" {
		t.Errorf("a repeat install --json printed %+v, want nothing installed and both providers", result)
	}
	checkHomeUntouched(t)
}

func TestProvidersInstallRefusesALavishBuildThatIsNotThePinnedOne(t *testing.T) {
	stateDir := factoryHome(t)
	other := []byte("#!/bin/sh\necho the user's own lavish\n")
	fetches := standInReleases(t, standInLavish, other)
	startUnready(t)

	stdout, stderr, code := install(t, "y\n")

	if code != 1 || !strings.Contains(stderr, "SHA-256") && !strings.Contains(stderr, "bytes") || !strings.Contains(stderr, "Lavish was not installed") {
		t.Errorf("an install of a build that does not match the pin exited %d printing\n%s%s\nwant it refused", code, stdout, stderr)
	}
	if fetches.Load() != 1 {
		t.Errorf("the release served %d downloads, want the one that was refused", fetches.Load())
	}
	path := filepath.Join(stateDir, "providers", "lavish", "0.1.89+deno2.9.6")
	if entries, _ := os.ReadDir(path); len(entries) != 0 {
		t.Errorf("a refused build left %v", entries)
	}
	var shown struct {
		Providers []providerJSON `json:"providers"`
	}
	inProcessJSON(t, &shown, "providers", "list")
	for _, p := range shown.Providers {
		if p.Name == "lavish" {
			t.Errorf("a refused build is listed as installed: %+v", p)
		}
	}
}

func TestProvidersListMarksALavishBuildThatIsNotThePinnedBytes(t *testing.T) {
	stateDir := factoryHome(t)
	standInReleases(t, standInLavish, standInLavish)
	startUnready(t)
	if _, stderr, code := install(t, "y\n"); code != 0 {
		t.Fatalf("the install exited %d: %s", code, stderr)
	}
	path := filepath.Join(stateDir, "providers", "lavish", "0.1.89+deno2.9.6", "lavish")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho changed\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	out, _, code := inProcess(t, "providers", "list")

	if code != 0 || !strings.Contains(out, "mismatch") || !strings.Contains(out, "Lavish 0.1.89+deno2.9.6 is not the pinned download, so AsmAI does not run it") {
		t.Errorf("asmai providers list exited %d printing\n%s\nwant the build marked a mismatch", code, out)
	}
	var list struct {
		Providers []struct {
			Name  string `json:"name"`
			State string `json:"state"`
		} `json:"providers"`
	}
	inProcessJSON(t, &list, "providers", "list")
	states := map[string]string{}
	for _, p := range list.Providers {
		states[p.Name] = p.State
	}
	if states["lavish"] != "mismatch" || states["claude-code"] != "pinned" {
		t.Errorf("the states are %v, want lavish a mismatch and Claude Code pinned", states)
	}

	// Putting the pinned build back is an install.
	if stdout, stderr, code := install(t, "y\n"); code != 0 || strings.Contains(stdout, "Lavish 0.1.89+deno2.9.6 is already installed") {
		t.Errorf("the repair install exited %d printing\n%s%s", code, stdout, stderr)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, standInLavish) {
		t.Errorf("%s holds %q, want the pinned build back", path, got)
	}
}

func TestProvidersListSaysAPinnedLavishBuildIsNotInstalled(t *testing.T) {
	factoryHome(t)
	standInReleases(t, standInLavish, standInLavish)
	startUnready(t)
	// Only Claude Code is installed: a copy of the pinned download put in place and recorded.
	var list struct {
		NotInstalled []struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"not_installed"`
	}
	inProcessJSON(t, &list, "providers", "list")
	if len(list.NotInstalled) != 2 {
		t.Errorf("not_installed is %+v, want both pinned providers", list.NotInstalled)
	}
}

func TestProvidersInstallSaysALavishBuildNotYetPublishedForThePlatformIsNotInstalled(t *testing.T) {
	factoryHome(t)
	standInChannel(t, standInClaude)
	platform, _ := providers.Platform(runtime.GOOS, runtime.GOARCH)
	previous := pins
	pins = append(bytes.TrimRight(pins, "}"), []byte(`}}}, "lavish": {"version": "0.1.89", "deno": "2.9.6", "releases": "https://example.com/releases"}}`)...)
	t.Cleanup(func() { pins = previous })
	startUnready(t)

	stdout, stderr, code := install(t, "y\n")

	if code != 0 || !strings.Contains(stdout, "Lavish 0.1.89+deno2.9.6 is pinned, but its build is not published for "+platform+" yet") || !strings.Contains(stdout, "Installed Claude Code 2.1.292.") {
		t.Errorf("asmai providers install exited %d printing\n%s%s\nwant Claude Code installed and the Lavish build said not to be published", code, stdout, stderr)
	}
}

func TestAnInstalledLavishBuildDoesNotChangeWhatNoticesPrints(t *testing.T) {
	factoryHome(t)
	standInReleases(t, standInLavish, standInLavish)
	startUnready(t)
	before, _, _ := inProcess(t, "notices")
	if _, stderr, code := install(t, "y\n"); code != 0 {
		t.Fatalf("the install exited %d: %s", code, stderr)
	}

	after, stderr, code := inProcess(t, "notices")

	if code != 0 || after != before || strings.Contains(after, "Lavish build notices") {
		t.Errorf("asmai notices exited %d printing\n%s%s\nwant what it printed before the Lavish build was installed", code, after, stderr)
	}
	var j map[string]string
	inProcessJSON(t, &j, "notices")
	if _, ok := j["lavish_notices"]; ok || len(j) != 2 {
		t.Errorf("asmai notices --json has keys %v, want only license and third_party_notices", j)
	}
}

func TestAsmaiLavishListsTheReviewLinksAndTheForwardingCommand(t *testing.T) {
	stateDir := factoryHome(t)
	startUnready(t)
	state := filepath.Join(stateDir, "lavish")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	page := fmt.Sprintf(`{"sessions": {
		"a": {"key": "a", "file": "/w/plan.html", "url": "http://127.0.0.1:%[1]d/session/a", "status": "open", "pending_prompts": 0},
		"b": {"key": "b", "file": "/w/questions.html", "url": "http://127.0.0.1:%[1]d/session/b", "status": "feedback", "pending_prompts": 2},
		"c": {"key": "c", "file": "/w/old.html", "url": "http://127.0.0.1:%[1]d/session/c", "status": "ended"}}}`, lavish.Port)
	if err := os.WriteFile(filepath.Join(state, "state.json"), []byte(page), 0o600); err != nil {
		t.Fatal(err)
	}

	out, stderr, code := runAsmai(t, "lavish")

	if code != 0 {
		t.Fatalf("asmai lavish exited %d: %s%s", code, out, stderr)
	}
	port := fmt.Sprint(lavish.Port)
	for _, want := range []string{
		"Lavish server  not running:",
		"http://127.0.0.1:" + port + "/session/a", "/w/plan.html",
		"http://127.0.0.1:" + port + "/session/b", "/w/questions.html", "2 messages not yet collected",
		"ssh -N -L " + port + ":127.0.0.1:" + port + " ",
		"listens on this host only",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("asmai lavish printed\n%s\nwant it to show %q", out, want)
		}
	}
	if strings.Contains(out, "/w/old.html") {
		t.Errorf("asmai lavish lists an ended page:\n%s", out)
	}

	var j struct {
		Lavish  daemon.LavishStatus `json:"lavish"`
		Forward string              `json:"forward"`
	}
	asmaiJSON(t, &j, "lavish")
	if j.Lavish.Port != lavish.Port || j.Lavish.StateDir != state || len(j.Lavish.Pages) != 2 || !strings.HasPrefix(j.Forward, "ssh -N -L "+port+":127.0.0.1:"+port+" ") {
		t.Errorf("asmai lavish --json printed %+v, want the factory's own port and state directory, the two pending pages and the forwarding command", j)
	}
}

func TestAsmaiLavishWithNoPagesSaysNoneAreWaiting(t *testing.T) {
	factoryHome(t)
	startUnready(t)

	out, _, code := runAsmai(t, "lavish")

	if code != 0 || !strings.Contains(out, "no review page is waiting for you") {
		t.Errorf("asmai lavish exited %d printing\n%s\nwant it to say no page is waiting", code, out)
	}
}

func TestAsmaiLavishStillListsThePagesWhileTheFactoryIsStopped(t *testing.T) {
	stateDir := factoryHome(t)
	state := filepath.Join(stateDir, "lavish")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	page := `{"sessions": {"a": {"key": "a", "file": "/w/plan.html", "url": "http://127.0.0.1:14387/session/a", "status": "open"}}}`
	if err := os.WriteFile(filepath.Join(state, "state.json"), []byte(page), 0o600); err != nil {
		t.Fatal(err)
	}

	out, _, code := runAsmai(t, "lavish")

	if code != 0 || !strings.Contains(out, "the factory is stopped") || !strings.Contains(out, "/session/a") {
		t.Errorf("asmai lavish exited %d printing\n%s\nwant the pages the stopped factory's Lavish kept", code, out)
	}
}

func TestAnAgentCannotRunAsmaiLavish(t *testing.T) {
	t.Setenv(daemon.SessionCredential, "an-agent-session")
	var stdout, stderr bytes.Buffer

	code := run([]string{"lavish"}, strings.NewReader(""), &stdout, &stderr)

	if code != 1 || !strings.Contains(stderr.String(), "an agent cannot run this command; ask the user to run `asmai lavish`") {
		t.Errorf("an agent running asmai lavish exited %d printing %q, want it refused", code, stderr.String())
	}
}
