// SPDX-License-Identifier: Apache-2.0

// Package certify holds the checks that decide whether AsmAI may run agents
// at all: that this platform is certified, and that the provider CLI an agent
// would run is the pinned, qualified version (02 rule 6, 03 rules 4 and 5).
//
// The checks are plain functions of what they are given, so that the daemon's
// start, `asmai init` and `asmai doctor` run the same ones and say the same
// thing.
package certify

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/talvor/asmai/internal/providers"
)

// Check names, as `asmai start` shows them.
const (
	PlatformName = "platform"
	VersionName  = "Claude Code version"
)

// Check is the verdict of one certification check. It has the shape of the
// checks `asmai start` reports.
type Check struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Problem string `json:"problem,omitempty"`
	Fix     string `json:"fix,omitempty"`
	Warning string `json:"warning,omitempty"`
}

// Platform checks that the platform named by goos and goarch is certified:
// the pins file holds a qualified download of the pinned Claude Code for it.
// AsmAI is certified on Linux and on macOS, each on the architecture its
// pins file names; anywhere else it starts no agent.
func Platform(goos, goarch string, pins providers.Pins) Check {
	here := goos + "/" + goarch
	certified := certifiedPlatforms(pins)
	if name, ok := providers.Platform(goos, goarch); ok {
		if _, pinned := pins.ClaudeCode.Platforms[name]; pinned {
			return Check{Name: PlatformName, OK: true}
		}
	}
	list := "none"
	if len(certified) > 0 {
		list = strings.Join(certified, ", ")
	}
	return Check{
		Name:    PlatformName,
		Problem: fmt.Sprintf("%s is not a platform this asmai is certified on, so it starts no agents here. The platforms it is certified on are %s", here, list),
		Fix:     "run AsmAI on a certified platform: Linux on x86_64 or macOS on Apple silicon, as listed above. AsmAI never runs agents on a platform its qualification did not cover",
	}
}

// certifiedPlatforms returns the platforms the pins file qualifies, as people
// name them.
func certifiedPlatforms(pins providers.Pins) []string {
	var names []string
	for platform := range pins.ClaudeCode.Platforms {
		names = append(names, describe(platform))
	}
	slices.Sort(names)
	return names
}

// describe names a platform of the pins file, such as linux-x64, as
// "Linux x86_64 (linux-x64)".
func describe(platform string) string {
	os, arch, _ := strings.Cut(platform, "-")
	switch os {
	case "linux":
		os = "Linux"
	case "darwin":
		os = "macOS"
	}
	switch arch {
	case "x64":
		arch = "x86_64"
	case "arm64":
		arch = "arm64"
	}
	return fmt.Sprintf("%s %s (%s)", os, arch, platform)
}

// Version checks that the Claude Code the executable at path reports itself
// to be is the pinned version, the one qualified for this release. Anything
// else, such as a copy that updated itself, is refused, before any agent runs
// on it: the combination is not in the qualification record (03 rules 4
// and 5). report is how the version is read, providers.ClaudeCodeVersion in
// use.
func Version(ctx context.Context, path, pinned string, report func(ctx context.Context, path string) (string, error)) Check {
	if pinned == "" {
		return Check{Name: VersionName, Problem: "this asmai pins no Claude Code", Fix: "run an asmai built with its pins file"}
	}
	reinstall := "run `asmai providers install` to put the pinned copy back"
	got, err := report(ctx, path)
	if err != nil {
		return Check{Name: VersionName, Problem: fmt.Sprintf("cannot tell which Claude Code version is at %s: %v", path, err), Fix: reinstall}
	}
	if got != pinned {
		return Check{
			Name:    VersionName,
			Problem: fmt.Sprintf("the Claude Code at %s is %s, but this asmai runs only the pinned, qualified %s: a version outside its qualification record is refused before any agent runs", path, got, pinned),
			Fix:     reinstall + ", or run the asmai release that pins " + got + " once it is qualified",
		}
	}
	return Check{Name: VersionName, OK: true}
}
