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

func TestClaudeCodeVersionIsWhatTheExecutableReports(t *testing.T) {
	dir := t.TempDir()
	write := func(name, out string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+out+"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		return path
	}
	got, err := ClaudeCodeVersion(context.Background(), write("claude", `echo "2.1.292 (Claude Code)"`))
	if err != nil || got != "2.1.292" {
		t.Errorf("version = %q, %v; want 2.1.292", got, err)
	}
	if _, err := ClaudeCodeVersion(context.Background(), write("other", `echo "bash 5.2"`)); err == nil {
		t.Error("a program that prints no version was read as one")
	}
	if _, err := ClaudeCodeVersion(context.Background(), write("broken", `exit 3`)); err == nil {
		t.Error("a program that fails was read as a version")
	}
	if _, err := ClaudeCodeVersion(context.Background(), filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing program was read as a version")
	}
}

const lavishPins = `{"claude-code": {"version": "1.2.3", "channel": "https://example.com/releases", "platforms": {"linux-x64": {"sha256": "` + "%[1]s" + `", "size": 10}}},
	"lavish": {"version": "0.1.89", "deno": "2.9.6", "releases": "%[2]s", "platforms": {"linux-x64": {"sha256": "%[3]s", "size": %[4]d}}}}`

// lavishPlan is the plan to install a Lavish build serving download from a
// stand-in for the release, pinning want.
func lavishPlan(t *testing.T, download, want []byte) Plan {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/releases/lavish-v0.1.89-deno2.9.6/lavish-linux-x64" {
			http.NotFound(w, r)
			return
		}
		w.Write(download)
	}))
	t.Cleanup(srv.Close)
	sum := sha256.Sum256(want)
	p, err := ParsePins(fmt.Appendf(nil, lavishPins, strings.Repeat("a", 64), srv.URL+"/releases", hex.EncodeToString(sum[:]), len(want)))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := p.LavishPlan("linux-x64", filepath.Join(t.TempDir(), "providers"))
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestTheEmbeddedPinsFilePinsALavishBuildPublishedAsAReleaseOfThisRepository(t *testing.T) {
	p, err := ParsePins(asmai.Pins)
	if err != nil {
		t.Fatal(err)
	}
	if p.Lavish == nil {
		t.Fatal("the pins file pins no Lavish")
	}
	if got, want := p.Lavish.Releases, "https://github.com/talvor/asmai/releases/download"; got != want {
		t.Errorf("Lavish builds are fetched from %q, want %q", got, want)
	}
	if got, want := p.Lavish.Tag(), "lavish-v"+p.Lavish.Version+"-deno"+p.Lavish.Deno; got != want {
		t.Errorf("the Lavish build's tag is %q, want %q", got, want)
	}
	// A platform with a digest is one the Lavish build was published for,
	// and only the platforms the pins file certifies are published.
	for platform, d := range p.Lavish.Platforms {
		if _, certified := p.ClaudeCode.Platforms[platform]; !certified {
			t.Errorf("the Lavish build is pinned for %s, which is not certified", platform)
		}
		plan, err := p.LavishPlan(platform, t.TempDir())
		if err != nil || plan.Download != d || !strings.HasSuffix(plan.URL, "/"+p.Lavish.Tag()+"/lavish-"+platform) {
			t.Errorf("%s: plan %+v (%v), want its digest and its executable on the build's release", platform, plan, err)
		}
	}
	// The pins file is kept as MarshalPins writes it.
	if out, err := MarshalPins(p); err != nil || string(out) != string(asmai.Pins) {
		t.Errorf("pins.json is not as MarshalPins writes it (%v):\n%s", err, out)
	}
}

func TestParsePinsRefusesALavishPinItCannotInstallFrom(t *testing.T) {
	claude := `"claude-code": {"version": "1.2.3", "channel": "https://example.com/releases", "platforms": {"linux-x64": {"sha256": "` + strings.Repeat("a", 64) + `", "size": 10}}}`
	good := `{"version": "0.1.89", "deno": "2.9.6", "releases": "https://example.com/releases"}`
	if _, err := ParsePins([]byte(`{` + claude + `, "lavish": ` + good + `}`)); err != nil {
		t.Fatalf("a Lavish pin with no platform published yet is refused: %v", err)
	}
	for name, lavish := range map[string]string{
		"a bad version":         strings.Replace(good, "0.1.89", "latest", 1),
		"a bad deno version":    strings.Replace(good, "2.9.6", "2", 1),
		"no releases":           `{"version": "0.1.89", "deno": "2.9.6", "releases": ""}`,
		"an unknown field":      strings.Replace(good, "}", `, "node": "22"}`, 1),
		"a bad digest":          strings.Replace(good, "}", `, "platforms": {"linux-x64": {"sha256": "abc", "size": 10}}}`, 1),
		"a bad size":            strings.Replace(good, "}", `, "platforms": {"linux-x64": {"sha256": "`+strings.Repeat("a", 64)+`", "size": 0}}}`, 1),
		"a releases url of ftp": strings.Replace(good, "https://example.com/releases", "ftp://example.com", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParsePins([]byte(`{` + claude + `, "lavish": ` + lavish + `}`)); err == nil {
				t.Errorf("ParsePins accepted a Lavish pin with %s", name)
			}
		})
	}
}

func TestALavishBuildIsFetchedFromItsReleaseAndKeptBesideTheOtherProviders(t *testing.T) {
	pinned := []byte("#!/bin/sh\necho lavish\n")
	plan := lavishPlan(t, pinned, pinned)
	if !strings.HasSuffix(plan.URL, "/releases/lavish-v0.1.89-deno2.9.6/lavish-linux-x64") {
		t.Errorf("the plan fetches %s, want the executable on the Lavish build's release", plan.URL)
	}
	if plan.Name != "lavish" || plan.Version != "0.1.89+deno2.9.6" || !strings.HasSuffix(plan.Path, filepath.Join("providers", "lavish", "0.1.89+deno2.9.6", "lavish")) {
		t.Errorf("the plan is %+v, want lavish 0.1.89+deno2.9.6 kept in the providers directory", plan)
	}

	if err := Fetch(context.Background(), http.DefaultClient, plan); err != nil {
		t.Fatal(err)
	}

	if present, err := Present(plan); err != nil || !present {
		t.Errorf("Present after fetching is %v (%v), want true", present, err)
	}
	if state, err := plan.Standing("0.1.89+deno2.9.6"); err != nil || state != StatePinned {
		t.Errorf("the fetched build stands as %q (%v), want pinned", state, err)
	}
}

func TestAMismatchedLavishBuildIsRefusedAndLeavesNothing(t *testing.T) {
	pinned := []byte("#!/bin/sh\necho lavish\n")
	plan := lavishPlan(t, []byte("#!/bin/sh\necho the user's own lavish\n"), pinned)

	err := Fetch(context.Background(), http.DefaultClient, plan)

	if err == nil || !strings.Contains(err.Error(), "SHA-256") && !strings.Contains(err.Error(), "bytes") {
		t.Errorf("fetching a build that is not the pinned one gave %v, want it refused for not matching the pin", err)
	}
	if left := files(t, filepath.Dir(plan.Path)); len(left) != 0 {
		t.Errorf("a refused build left %v", left)
	}
}

func TestLavishPlanSaysWhenNoBuildIsPinnedForThePlatform(t *testing.T) {
	good := `"claude-code": {"version": "1.2.3", "channel": "https://example.com/releases", "platforms": {"linux-x64": {"sha256": "` + strings.Repeat("a", 64) + `", "size": 10}}}`
	for name, pins := range map[string]string{
		"no Lavish":         `{` + good + `}`,
		"not yet published": `{` + good + `, "lavish": {"version": "0.1.89", "deno": "2.9.6", "releases": "https://example.com/releases"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			p, err := ParsePins([]byte(pins))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := p.LavishPlan("linux-x64", t.TempDir()); !errors.Is(err, ErrNoLavishBuild) {
				t.Errorf("LavishPlan gave %v, want ErrNoLavishBuild", err)
			}
		})
	}
}

func TestAnInstallStandsAgainstThePinAsPinnedSupersededMissingOrMismatched(t *testing.T) {
	pinned := []byte("#!/bin/sh\necho lavish\n")
	plan := lavishPlan(t, pinned, pinned)
	if state, err := plan.Standing(plan.Version); err != nil || state != StateMissing {
		t.Errorf("a build that is not there stands as %q (%v), want missing", state, err)
	}
	if err := os.MkdirAll(filepath.Dir(plan.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plan.Path, []byte("#!/bin/sh\necho changed\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if state, err := plan.Standing(plan.Version); err != nil || state != StateMismatch {
		t.Errorf("a build that is not the pinned bytes stands as %q (%v), want mismatch", state, err)
	}
	if state, err := plan.Standing("0.1.80+deno2.9.0"); err != nil || state != StateSuperseded {
		t.Errorf("an older build stands as %q (%v), want superseded", state, err)
	}
	if err := os.WriteFile(plan.Path, pinned, 0o755); err != nil {
		t.Fatal(err)
	}
	if state, err := plan.Standing(plan.Version); err != nil || state != StatePinned {
		t.Errorf("the pinned build stands as %q (%v), want pinned", state, err)
	}
}
