// SPDX-License-Identifier: Apache-2.0

package certify

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/talvor/asmai/internal/providers"
)

func pins() providers.Pins {
	return providers.Pins{ClaudeCode: providers.Pin{Version: "2.1.292", Platforms: map[string]providers.Download{
		"linux-x64":    {},
		"darwin-arm64": {},
	}}}
}

func TestLinuxAndMacOSAreCertified(t *testing.T) {
	for _, p := range [][2]string{{"linux", "amd64"}, {"darwin", "arm64"}} {
		if c := Platform(p[0], p[1], pins()); !c.OK || c.Name != PlatformName {
			t.Errorf("%s/%s: %+v, want certified", p[0], p[1], c)
		}
	}
}

func TestAnUncertifiedPlatformSaysWhatToDo(t *testing.T) {
	for _, p := range [][2]string{{"linux", "arm64"}, {"darwin", "amd64"}, {"windows", "amd64"}, {"freebsd", "amd64"}, {"", ""}} {
		c := Platform(p[0], p[1], pins())
		if c.OK {
			t.Errorf("%s/%s was certified", p[0], p[1])
			continue
		}
		for _, want := range []string{p[0] + "/" + p[1], "Linux x86_64 (linux-x64)", "macOS arm64 (darwin-arm64)", "starts no agents"} {
			if !strings.Contains(c.Problem, want) {
				t.Errorf("%s/%s problem %q does not say %q", p[0], p[1], c.Problem, want)
			}
		}
		if !strings.Contains(c.Fix, "certified platform") {
			t.Errorf("%s/%s fix %q does not say what to do", p[0], p[1], c.Fix)
		}
	}
}

func TestAPlatformThePinsFileDoesNotQualifyIsNotCertified(t *testing.T) {
	p := providers.Pins{ClaudeCode: providers.Pin{Version: "2.1.292", Platforms: map[string]providers.Download{"linux-x64": {}}}}
	if c := Platform("darwin", "arm64", p); c.OK {
		t.Errorf("macOS was certified by a pins file that qualifies only Linux: %+v", c)
	}
	if c := Platform("linux", "amd64", providers.Pins{}); c.OK || !strings.Contains(c.Problem, "none") {
		t.Errorf("a pins file with no platform certified Linux: %+v", c)
	}
}

func reports(version string, err error) func(context.Context, string) (string, error) {
	return func(context.Context, string) (string, error) { return version, err }
}

func TestOnlyThePinnedVersionPasses(t *testing.T) {
	ctx := context.Background()
	if c := Version(ctx, "/p/claude", "2.1.292", reports("2.1.292", nil)); !c.OK || c.Name != VersionName {
		t.Errorf("the pinned version: %+v", c)
	}
	c := Version(ctx, "/p/claude", "2.1.292", reports("2.1.300", nil))
	if c.OK {
		t.Fatal("an unpinned version passed")
	}
	for _, want := range []string{"2.1.300", "2.1.292", "/p/claude", "before any agent runs"} {
		if !strings.Contains(c.Problem, want) {
			t.Errorf("problem %q does not say %q", c.Problem, want)
		}
	}
	if !strings.Contains(c.Fix, "asmai providers install") {
		t.Errorf("fix %q does not say what to do", c.Fix)
	}
}

func TestAVersionThatCannotBeReadIsRefused(t *testing.T) {
	c := Version(context.Background(), "/p/claude", "2.1.292", reports("", errors.New("exec format error")))
	if c.OK || !strings.Contains(c.Problem, "exec format error") {
		t.Errorf("%+v", c)
	}
}

func TestNoPinMeansNoCopyRuns(t *testing.T) {
	if c := Version(context.Background(), "/p/claude", "", reports("2.1.292", nil)); c.OK {
		t.Errorf("%+v", c)
	}
}
