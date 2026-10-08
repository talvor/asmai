// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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

// standInClaude is what the stand-in channel serves as Claude Code.
var standInClaude = []byte("#!/bin/sh\necho '2.1.292 (Claude Code)'\n")

// standInChannel serves download as the pinned Claude Code on a stand-in for
// Claude Code's channel, and makes it the channel this asmai installs from,
// pinning standInClaude. It returns how many downloads it served.
func standInChannel(t *testing.T, download []byte) (fetches *atomic.Int32) {
	t.Helper()
	platform, ok := providers.Platform(runtime.GOOS, runtime.GOARCH)
	if !ok {
		t.Skipf("Claude Code is not published for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	fetches = new(atomic.Int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if download == nil || r.URL.Path != "/releases/2.1.292/"+platform+"/claude" {
			http.NotFound(w, r)
			return
		}
		fetches.Add(1)
		w.Write(download)
	}))
	t.Cleanup(srv.Close)
	sum := sha256.Sum256(standInClaude)
	previous := pins
	pins = fmt.Appendf(nil, `{"claude-code": {"version": "2.1.292", "channel": %q, "platforms": {%q: {"sha256": %q, "size": %d}}}}`,
		srv.URL+"/releases", platform, hex.EncodeToString(sum[:]), len(standInClaude))
	t.Cleanup(func() { pins = previous })
	return fetches
}

// install runs `asmai providers install` in this process, answering its
// question with answer.
func install(t *testing.T, answer string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(append([]string{"providers", "install"}, args...), strings.NewReader(answer), &out, &errOut)
	return out.String(), errOut.String(), code
}

type providerJSON struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Path    string `json:"path"`
	SHA256  string `json:"sha256"`
}

func listed(t *testing.T) []providerJSON {
	t.Helper()
	var list struct {
		Providers []providerJSON `json:"providers"`
	}
	asmaiJSON(t, &list, "providers", "list")
	return list.Providers
}

func journalKinds(t *testing.T) []string {
	t.Helper()
	var j journalJSON
	asmaiJSON(t, &j, "export")
	var kinds []string
	for _, e := range j.Journal {
		kinds = append(kinds, e.Kind)
	}
	return kinds
}

// The home holds nothing of Claude Code's own, such as ~/.claude or a copy
// installed for the user: only AsmAI's state directory.
func checkHomeUntouched(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(os.Getenv("HOME"), ".local"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "state" {
			t.Errorf("~/.local holds %s, want only AsmAI's state directory", e.Name())
		}
	}
	entries, err = os.ReadDir(os.Getenv("HOME"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != ".local" {
			t.Errorf("the home holds %s, want only .local", e.Name())
		}
	}
}

func TestProvidersInstallFetchesThePinnedClaudeCodeOnceConfirmed(t *testing.T) {
	stateDir := factoryHome(t)
	fetches := standInChannel(t, standInClaude)
	if _, stderr, code := runAsmai(t, "start"); code != 0 {
		t.Fatalf("asmai start exited %d: %s", code, stderr)
	}
	path := filepath.Join(stateDir, "providers", "claude-code", "2.1.292", "claude")

	stdout, stderr, code := install(t, "y\n")

	if code != 0 {
		t.Fatalf("asmai providers install exited %d: %s%s", code, stdout, stderr)
	}
	for _, want := range []string{"Claude Code 2.1.292", "/releases/2.1.292/", "official channel", path, "Install it? [y/N]", "Installed Claude Code 2.1.292."} {
		if !strings.Contains(stdout, want) {
			t.Errorf("asmai providers install printed\n%s\nwant it to show %q", stdout, want)
		}
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, standInClaude) {
		t.Errorf("%s holds %q (%v), want the pinned download", path, got, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm()&0o100 == 0 {
		t.Errorf("%s is not executable (%v)", path, err)
	}
	if n := fetches.Load(); n != 1 {
		t.Errorf("the channel served %d downloads, want 1", n)
	}
	sum := sha256.Sum256(standInClaude)
	want := providerJSON{Name: "claude-code", Version: "2.1.292", Path: path, SHA256: hex.EncodeToString(sum[:])}
	if got := listed(t); len(got) != 1 || got[0] != want {
		t.Errorf("asmai providers list shows %+v, want %+v", got, want)
	}
	if out, _, _ := runAsmai(t, "providers", "list"); !strings.Contains(out, "claude-code") || !strings.Contains(out, path) {
		t.Errorf("asmai providers list printed\n%s\nwant the install", out)
	}
	if got := journalKinds(t); strings.Join(got, ",") != "daemon.started,provider.installed" {
		t.Errorf("the journal holds %v, want the start and the install", got)
	}

	// The same pin again does nothing: it asks nothing, fetches nothing and
	// records nothing.
	stdout, stderr, code = install(t, "")
	if code != 0 || !strings.Contains(stdout, "already installed") || strings.Contains(stdout, "Install it?") {
		t.Errorf("a repeat install exited %d printing\n%s%s\nwant it to say the pin is already installed", code, stdout, stderr)
	}
	if n := fetches.Load(); n != 1 {
		t.Errorf("the channel served %d downloads after a repeat install, want 1", n)
	}
	if got := journalKinds(t); strings.Join(got, ",") != "daemon.started,provider.installed" {
		t.Errorf("after a repeat install the journal holds %v, want only the first install", got)
	}
	checkHomeUntouched(t)
}

func TestProvidersInstallFetchesNothingWithoutConfirmation(t *testing.T) {
	stateDir := factoryHome(t)
	fetches := standInChannel(t, standInClaude)
	if _, stderr, code := runAsmai(t, "start"); code != 0 {
		t.Fatalf("asmai start exited %d: %s", code, stderr)
	}
	for name, answer := range map[string]string{"no": "n\n", "anything but yes": "sure\n", "no answer": ""} {
		t.Run(name, func(t *testing.T) {
			stdout, stderr, code := install(t, answer)
			if code != 1 || !strings.Contains(stderr, "not confirmed; nothing was fetched or installed") {
				t.Errorf("answering %q exited %d printing\n%s%s\nwant 1 and that nothing was installed", answer, code, stdout, stderr)
			}
		})
	}

	if n := fetches.Load(); n != 0 {
		t.Errorf("the channel served %d downloads, want none", n)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "providers")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the providers directory exists (%v), want nothing installed", err)
	}
	if got := listed(t); len(got) != 0 {
		t.Errorf("asmai providers list shows %+v, want nothing", got)
	}
	if got := journalKinds(t); strings.Join(got, ",") != "daemon.started" {
		t.Errorf("the journal holds %v, want only the start", got)
	}
	checkHomeUntouched(t)
}

func TestAFailedDownloadInstallsNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		download []byte
		want     string
	}{
		"missing from the channel": {nil, "404"},
		"not the pinned bytes":     {bytes.Replace(standInClaude, []byte("Claude"), []byte("Kl4ude"), 1), "SHA-256"},
		"cut short":                {standInClaude[:10], "bytes"},
	} {
		t.Run(name, func(t *testing.T) {
			stateDir := factoryHome(t)
			standInChannel(t, tc.download)
			if _, stderr, code := runAsmai(t, "start"); code != 0 {
				t.Fatalf("asmai start exited %d: %s", code, stderr)
			}

			stdout, stderr, code := install(t, "y\n", "--json")

			var failure struct {
				Error string `json:"error"`
			}
			if code != 1 || json.Unmarshal([]byte(stdout), &failure) != nil || !strings.Contains(failure.Error, tc.want) || !strings.Contains(failure.Error, "nothing was installed") {
				t.Errorf("the install exited %d printing\n%s%s\nwant 1 and a JSON error mentioning %q", code, stdout, stderr, tc.want)
			}
			dir := filepath.Join(stateDir, "providers", "claude-code", "2.1.292")
			if entries, _ := os.ReadDir(dir); len(entries) != 0 {
				t.Errorf("a failed download left %v in %s", entries, dir)
			}
			if got := listed(t); len(got) != 0 {
				t.Errorf("asmai providers list shows %+v, want nothing", got)
			}
			if got := journalKinds(t); strings.Join(got, ",") != "daemon.started" {
				t.Errorf("the journal holds %v, want only the start", got)
			}
		})
	}
}

func TestProvidersNeedTheFactoryRunning(t *testing.T) {
	factoryHome(t)
	fetches := standInChannel(t, standInClaude)
	for _, args := range [][]string{{"providers", "install"}, {"providers", "list"}} {
		var stdout, stderr bytes.Buffer
		code := run(args, strings.NewReader("y\n"), &stdout, &stderr)
		if code != 1 || !strings.Contains(stderr.String(), "asmai start") {
			t.Errorf("asmai %s exited %d printing %q, want 1 and to start the factory", strings.Join(args, " "), code, stderr.String())
		}
	}
	if n := fetches.Load(); n != 0 {
		t.Errorf("the channel served %d downloads, want none", n)
	}
}

func TestProvidersWantInstallOrList(t *testing.T) {
	for _, args := range [][]string{{"providers"}, {"providers", "remove"}} {
		var stdout, stderr bytes.Buffer
		if code := run(args, strings.NewReader(""), &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "install or list") {
			t.Errorf("asmai %s exited %d printing %q, want 2 and usage", strings.Join(args, " "), code, stderr.String())
		}
	}
}
