// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/talvor/asmai"
)

// The pins file asmai embeds pins Claude Code on its official channel for
// every certified platform.
func TestTheEmbeddedPinsFilePinsClaudeCodeForEveryCertifiedPlatform(t *testing.T) {
	p, err := ParsePins(asmai.Pins)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := p.ClaudeCode.Channel, "https://downloads.claude.ai/claude-code-releases"; got != want {
		t.Errorf("Claude Code's channel is %q, want its official channel %q", got, want)
	}
	for _, platform := range [][2]string{{"linux", "amd64"}, {"darwin", "arm64"}} {
		name, ok := Platform(platform[0], platform[1])
		if !ok {
			t.Fatalf("%s/%s has no channel name", platform[0], platform[1])
		}
		if _, err := p.ClaudeCodePlan(name, t.TempDir()); err != nil {
			t.Errorf("%s/%s: %v", platform[0], platform[1], err)
		}
	}
}

func TestParsePinsRefusesAPinItCannotInstallFrom(t *testing.T) {
	good := `{"version": "1.2.3", "channel": "https://example.com/releases", "platforms": {"linux-x64": {"sha256": "` + strings.Repeat("a", 64) + `", "size": 10}}}`
	if _, err := ParsePins([]byte(`{"claude-code": ` + good + `}`)); err != nil {
		t.Fatalf("a good pins file is refused: %v", err)
	}
	for name, pins := range map[string]string{
		"not JSON":         `pins`,
		"an unknown field": `{"claude-code": ` + good + `, "kodex": {}}`,
		"no Claude Code":   `{}`,
		"a bad version":    `{"claude-code": ` + strings.Replace(good, "1.2.3", "latest", 1) + `}`,
		"a bad channel":    `{"claude-code": ` + strings.Replace(good, "https://example.com/releases", "ftp://example.com", 1) + `}`,
		"no platforms":     `{"claude-code": {"version": "1.2.3", "channel": "https://example.com", "platforms": {}}}`,
		"a bad digest":     `{"claude-code": ` + strings.Replace(good, strings.Repeat("a", 64), "abc", 1) + `}`,
		"a bad size":       `{"claude-code": ` + strings.Replace(good, `"size": 10`, `"size": 0`, 1) + `}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParsePins([]byte(pins)); err == nil {
				t.Errorf("ParsePins accepted %s", pins)
			}
		})
	}
}

// channel is a stand-in for Claude Code's channel, serving download at
// /1.2.3/linux-x64/claude. It returns a plan pinning want.
func channel(t *testing.T, download, want []byte) Plan {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/releases/1.2.3/linux-x64/claude" {
			http.NotFound(w, r)
			return
		}
		w.Write(download)
	}))
	t.Cleanup(srv.Close)
	sum := sha256.Sum256(want)
	pins := fmt.Sprintf(`{"claude-code": {"version": "1.2.3", "channel": %q, "platforms": {"linux-x64": {"sha256": %q, "size": %d}}}}`,
		srv.URL+"/releases", hex.EncodeToString(sum[:]), len(want))
	p, err := ParsePins([]byte(pins))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := p.ClaudeCodePlan("linux-x64", filepath.Join(t.TempDir(), "providers"))
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestFetchKeepsTheDownloadOnlyWhenItMatchesThePin(t *testing.T) {
	pinned := []byte("#!/bin/sh\necho 1.2.3\n")
	plan := channel(t, pinned, pinned)
	if want := "/releases/1.2.3/linux-x64/claude"; !strings.HasSuffix(plan.URL, want) {
		t.Errorf("the plan fetches %s, want the channel's %s", plan.URL, want)
	}
	if present, err := Present(plan); err != nil || present {
		t.Errorf("Present before fetching is %v (%v), want false", present, err)
	}

	if err := Fetch(context.Background(), http.DefaultClient, plan); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(plan.Path)
	if err != nil || string(got) != string(pinned) {
		t.Errorf("%s holds %q (%v), want the download", plan.Path, got, err)
	}
	if info, err := os.Stat(plan.Path); err != nil || info.Mode().Perm() != 0o755 {
		t.Errorf("%s is %v (%v), want it executable", plan.Path, info.Mode(), err)
	}
	if present, err := Present(plan); err != nil || !present {
		t.Errorf("Present after fetching is %v (%v), want true", present, err)
	}
	if left := files(t, filepath.Dir(plan.Path)); strings.Join(left, ",") != "claude" {
		t.Errorf("the provider's directory holds %v, want only claude", left)
	}
}

func TestAFailedOrMismatchedDownloadLeavesNothing(t *testing.T) {
	pinned := []byte("#!/bin/sh\necho 1.2.3\n")
	for name, tc := range map[string]struct {
		plan func(t *testing.T) Plan
		want string
	}{
		"missing from the channel": {func(t *testing.T) Plan {
			plan := channel(t, pinned, pinned)
			plan.URL = strings.Replace(plan.URL, "1.2.3", "9.9.9", 1)
			return plan
		}, "404"},
		"another SHA-256": {func(t *testing.T) Plan {
			return channel(t, []byte("#!/bin/sh\necho 6.6.6\n"), pinned)
		}, "SHA-256"},
		"shorter": {func(t *testing.T) Plan {
			return channel(t, pinned[:5], pinned)
		}, "bytes"},
		"longer": {func(t *testing.T) Plan {
			return channel(t, append(pinned, "more"...), pinned)
		}, "bytes"},
		"unreachable": {func(t *testing.T) Plan {
			plan := channel(t, pinned, pinned)
			srv := httptest.NewServer(http.NotFoundHandler())
			plan.URL = srv.URL + "/claude"
			srv.Close()
			return plan
		}, "fetching"},
	} {
		t.Run(name, func(t *testing.T) {
			plan := tc.plan(t)
			err := Fetch(context.Background(), http.DefaultClient, plan)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Fetch returned %v, want an error mentioning %q", err, tc.want)
			}
			if _, err := os.Stat(plan.Path); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s exists after a failed fetch (%v)", plan.Path, err)
			}
			if left := files(t, filepath.Dir(plan.Path)); len(left) != 0 {
				t.Errorf("a failed fetch left %v", left)
			}
		})
	}
}

func TestPlatformNamesTheChannelsPlatforms(t *testing.T) {
	for _, tc := range []struct{ goos, goarch, want string }{
		{"linux", "amd64", "linux-x64"},
		{"linux", "arm64", "linux-arm64"},
		{"darwin", "arm64", "darwin-arm64"},
		{"darwin", "amd64", "darwin-x64"},
		{"windows", "amd64", ""},
		{"linux", "386", ""},
	} {
		got, ok := Platform(tc.goos, tc.goarch)
		if got != tc.want || ok != (tc.want != "") {
			t.Errorf("Platform(%s, %s) = %q, %v, want %q", tc.goos, tc.goarch, got, ok, tc.want)
		}
	}
}

func files(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}
