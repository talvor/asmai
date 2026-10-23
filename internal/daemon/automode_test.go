// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/talvor/asmai/internal/store"
)

// A worker's terminal is nobody's: its session runs in dontAsk mode, or in
// auto mode where the user switched auto_mode on in the configuration and
// Claude Code reports it available. Claude Code starts a session that asks
// for auto mode in the default mode, silently, where it is not available, so
// the switch alone never selects it (ADR 0018).
func TestAWorkersSessionRunsInAutoModeOnlyWhereTheUserSwitchedItOnAndClaudeCodeReportsItAvailable(t *testing.T) {
	const switchedOn = "[providers.claude]\nauto_mode = true\n"
	for name, tc := range map[string]struct {
		options factoryOptions
		mode    string
	}{
		"the switch is off and auto mode is available":        {factoryOptions{answers: []string{autoModeAnswer(t, "")}}, "dontAsk"},
		"the switch is off":                                   {factoryOptions{}, "dontAsk"},
		"the switch is on and auto mode is available":         {factoryOptions{config: switchedOn, answers: []string{autoModeAnswer(t, "")}}, "auto"},
		"the switch is on and Claude Code refuses it":         {factoryOptions{config: switchedOn, answers: []string{autoModeAnswer(t, "Cannot set permission mode to auto: auto mode unavailable")}}, "dontAsk"},
		"the switch is on and Claude Code says nothing of it": {factoryOptions{config: switchedOn}, "dontAsk"},
		"the switch is on and the administrator disabled it":  {factoryOptions{config: switchedOn, answers: []string{disabledAutoModeAnswer(t), autoModeAnswer(t, "")}}, "dontAsk"},
	} {
		t.Run(name, func(t *testing.T) {
			p := newJobFactoryWith(t, store.MandateSetup, true, "", tc.options)
			if _, _, err := p.d.assign(Request{Session: "coordination", Command: CommandAssign, Job: p.job.Number, Outcome: "Set fixture up for the skills", Criteria: []string{"the setup files are on the branch"}, Skill: "setup-matt-pocock-skills"}); err != nil {
				t.Fatal(err)
			}
			args := waitForFile(t, filepath.Join(p.out, "args"))
			if want := `"defaultMode":"` + tc.mode + `"`; !strings.Contains(args, want) {
				t.Errorf("the worker's command line lacks %s: %s", want, args)
			}
		})
	}
}
