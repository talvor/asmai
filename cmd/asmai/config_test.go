// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigCheckReportsEveryProblemWithItsLineAndExitsNonZero(t *testing.T) {
	factoryHome(t)
	writeConfig(t, `[roles.coordination]
leader_provider = "codex"
leader_model = "opus"
worker_provider = "claude"
worker_model = "opus"

[providers.codex]
worker_cap = 3

[limits]
silence_timeout = "soon"
`)
	stdout, stderr, code := runAsmai(t, "config", "check")
	if code != 1 || stdout != "" {
		t.Fatalf("asmai config check exited %d, printing %q, want 1 and nothing on stdout", code, stdout)
	}
	for _, want := range []string{
		"has 3 problems",
		`line 2: leader_provider in [roles.coordination] is "codex"`, "Codex arrives in a later asmai",
		"line 7: [providers.codex] names Codex, which arrives in a later asmai",
		`line 11: silence_timeout in [limits] is "soon", which is not a duration`,
		"fix:",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("asmai config check printed\n%s\nwant %q", stderr, want)
		}
	}

	var report struct {
		File     string `json:"file"`
		Valid    bool   `json:"valid"`
		Problems []struct {
			Line    int    `json:"line"`
			Problem string `json:"problem"`
			Fix     string `json:"fix"`
		} `json:"problems"`
	}
	out, _, code := runAsmai(t, "config", "check", "--json")
	if code != 1 {
		t.Errorf("asmai config check --json exited %d, want 1", code)
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil || report.Valid || len(report.Problems) != 3 || report.Problems[0].Line != 2 || report.Problems[0].Fix == "" {
		t.Errorf("asmai config check --json printed %q (%v)", out, err)
	}
}

func TestConfigCheckPassesTheExampleConfiguration(t *testing.T) {
	factoryHome(t)
	example, stderr, code := runAsmai(t, "config", "example")
	if code != 0 || !strings.Contains(example, "[repositories.otman]") {
		t.Fatalf("asmai config example exited %d, printing %q (%s)", code, example, stderr)
	}
	writeConfig(t, example)
	stdout, stderr, code := runAsmai(t, "config", "check")
	if code != 0 || !strings.Contains(stdout, "has no problems") {
		t.Errorf("asmai config check exited %d on the example: %s%s", code, stdout, stderr)
	}
}

func TestConfigCheckOfAMissingFileSaysSo(t *testing.T) {
	factoryHome(t)
	_, stderr, code := runAsmai(t, "config", "check")
	if code != 1 || !strings.Contains(stderr, "does not exist") || !strings.Contains(stderr, "asmai init") {
		t.Errorf("asmai config check of no file exited %d: %s", code, stderr)
	}
	// Nothing was made.
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".config", "asmai", "config.toml")); err == nil {
		t.Error("asmai config check wrote the configuration file")
	}
}

func TestAnAgentCannotCheckTheConfiguration(t *testing.T) {
	factoryHome(t)
	t.Setenv("ASMAI_SESSION", "credential")
	for _, sub := range []string{"check", "example"} {
		_, stderr, code := runAsmai(t, "config", sub)
		if code != 1 || !strings.Contains(stderr, "an agent cannot run this command") {
			t.Errorf("asmai config %s by an agent exited %d: %s", sub, code, stderr)
		}
	}
}
