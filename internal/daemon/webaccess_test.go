// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"encoding/json"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/talvor/asmai/internal/providers"
	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/store"
)

// allowedBy returns the permission rules, the permission mode and the sandbox
// of the settings a stand-in Claude Code was started with, from the command
// line it recorded.
func allowedBy(t *testing.T, args string) (allow []string, mode string, sandbox json.RawMessage) {
	t.Helper()
	first, _, _ := strings.Cut(args, "\n--setting-sources")
	var settings struct {
		Permissions struct {
			Allow       []string `json:"allow"`
			DefaultMode string   `json:"defaultMode"`
		} `json:"permissions"`
		Sandbox json.RawMessage `json:"sandbox"`
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(first, "--settings\n")), &settings); err != nil {
		t.Fatalf("the command line %q does not start with the settings: %v", args, err)
	}
	return settings.Permissions.Allow, settings.Permissions.DefaultMode, settings.Sandbox
}

// A Research worker reads the web with auto_mode off or on, and no other
// worker is given the rules that let it: the permission rules built for each
// role's worker differ by those two, read-only rules and nothing else.
func TestOnlyAResearchWorkersSessionIsGivenTheWebRulesWhateverAutoModeSays(t *testing.T) {
	const switchedOn = "[providers.claude]\nauto_mode = true\n"
	type outcome struct {
		allow   []string
		mode    string
		sandbox string
	}
	start := func(t *testing.T, mandate string, repository bool, options factoryOptions, session, skill string, readOnly bool) outcome {
		t.Helper()
		p := newJobFactoryWith(t, mandate, repository, "", options)
		if !repository {
			trustWorkerFolder(t, p.d.cfg.ClaudeRecord, p.d.cfg.Paths, p.d.cfg.Paths.Workspaces)
		} else {
			trustWorkerFolder(t, p.d.cfg.ClaudeRecord, p.d.cfg.Paths, p.clone)
		}
		req := Request{Session: session, Command: CommandAssign, Job: p.job.Number, Outcome: "Do the work", Criteria: []string{"done"}, Skill: skill, ReadOnly: readOnly}
		if readOnly {
			branch, err := p.s.JobBranch(p.job.Number)
			if err != nil {
				t.Fatal(err)
			}
			req.Commit = branch.Tip
		}
		if _, _, err := p.d.assign(req); err != nil {
			t.Fatal(err)
		}
		allow, mode, sandbox := allowedBy(t, waitForFile(t, filepath.Join(p.out, "args")))
		return outcome{allow, mode, string(sandbox)}
	}
	webRules := providers.WebRules()
	hasWeb := func(allow []string) bool {
		for _, rule := range allow {
			if tool, _, _ := strings.Cut(rule, "("); tool == "WebFetch" || tool == "WebSearch" {
				return true
			}
		}
		return false
	}

	for name, tc := range map[string]struct {
		options factoryOptions
		mode    string
	}{
		"auto_mode off":                         {factoryOptions{}, "dontAsk"},
		"auto_mode on and available":            {factoryOptions{config: switchedOn, answers: []string{autoModeAnswer(t, "")}}, "auto"},
		"auto_mode on and not available":        {factoryOptions{config: switchedOn}, "dontAsk"},
		"auto_mode off and auto mode available": {factoryOptions{answers: []string{autoModeAnswer(t, "")}}, "dontAsk"},
	} {
		t.Run(name, func(t *testing.T) {
			research := start(t, store.MandateResearchOnly, false, tc.options, roles.Research, "research", false)
			if research.mode != tc.mode {
				t.Errorf("the Research worker runs in the %q mode, want %q", research.mode, tc.mode)
			}
			if !slices.Contains(research.allow, "WebFetch") || !slices.Contains(research.allow, "WebSearch") {
				t.Errorf("the Research worker is allowed %q, want WebFetch and WebSearch", research.allow)
			}
			planning := start(t, store.MandatePlanningOnly, true, tc.options, roles.Planning, "grilling", false)
			if hasWeb(planning.allow) {
				t.Errorf("a Planning worker is allowed %q, want no web access", planning.allow)
			}
			for _, worker := range []struct {
				role, mandate, skill string
				readOnly             bool
			}{
				{roles.Coordination, store.MandateSetup, "setup-matt-pocock-skills", false},
				{roles.Engineering, store.MandateTestedPR, "", false},
				{roles.Quality, store.MandateTestedPR, "", true},
			} {
				got := start(t, worker.mandate, true, tc.options, worker.role, worker.skill, worker.readOnly)
				if hasWeb(got.allow) {
					t.Errorf("a %s worker is allowed %q, want no web access", worker.role, got.allow)
				}
			}
			// Reading only: what separates a Research worker from another
			// worker is the web rules, and the sandbox, which guards a
			// shell, is built the same.
			var extra []string
			for _, rule := range research.allow {
				if !slices.Contains(planning.allow, rule) && !strings.HasPrefix(rule, "Read(") && !strings.HasPrefix(rule, "Edit(") {
					extra = append(extra, rule)
				}
			}
			if !slices.Equal(extra, webRules) {
				t.Errorf("the Research worker has the rules %q the Planning worker lacks, want only %q", extra, webRules)
			}
			// Each factory has its own daemon socket, so the network
			// settings are compared by what they set, not their values.
			networkKeys := func(sandbox string) []string {
				var parsed struct {
					Network map[string]json.RawMessage `json:"network"`
				}
				if err := json.Unmarshal([]byte(sandbox), &parsed); err != nil {
					t.Fatal(err)
				}
				return slices.Sorted(maps.Keys(parsed.Network))
			}
			if r, p := networkKeys(research.sandbox), networkKeys(planning.sandbox); !slices.Equal(r, p) || !slices.Equal(r, []string{"allowUnixSockets"}) {
				t.Errorf("the Research worker's sandbox network settings are %q, want the Planning worker's %q: the daemon's socket only", r, p)
			}
			if strings.Contains(research.sandbox, "allowedDomains") {
				t.Errorf("the Research worker's sandbox is %s, want no network domains for a shell", research.sandbox)
			}
		})
	}
}

func TestNoRoleLeaderIsGivenWebRules(t *testing.T) {
	for _, role := range []string{roles.Coordination, roles.Planning, roles.Research, roles.Engineering, roles.Quality} {
		t.Run(role, func(t *testing.T) {
			p := newJobFactoryWith(t, store.MandateTestedPR, true, "", factoryOptions{})
			trustWorkerFolder(t, p.d.cfg.ClaudeRecord, p.d.cfg.Paths, p.clone)
			address := roles.LeaderOf(role)
			delete(p.d.leaders, address.String())
			p.d.mu.Lock()
			err := p.d.ensureAgent(address)
			p.d.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			allow, _, _ := allowedBy(t, waitForFile(t, filepath.Join(p.out, "args")))
			for _, rule := range allow {
				if tool, _, _ := strings.Cut(rule, "("); tool == "WebFetch" || tool == "WebSearch" {
					t.Errorf("%s's leader is allowed %q, want no web access", role, rule)
				}
			}
		})
	}
}
