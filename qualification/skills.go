// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/talvor/asmai/internal/providers"
	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/skills"
)

// c34 qualifies that per-session skill delivery works although Claude Code
// namespaces plugin skills while upstream's skills call each other by bare
// name (08 rules 20 and 22). It writes a worker's plugin as the daemon does
// for an assignment that narrows the list (`skills.WritePlugin`), starts the
// pinned Claude Code with the command line the code under test builds for the
// worker (`providers.ClaudeCodeArgs`, with the plugin and no setting source),
// and has the session run implement, whose text says "Use /tdd", and then
// the skill it names.
func (h *Harness) c34(ctx context.Context, f *Factory, r *Result) error {
	if err := checkGuardDependencies(); err != nil {
		return err
	}
	path, err := h.ready(ctx, f)
	if err != nil {
		return err
	}
	return h.skillsAreDelivered(ctx, f, path, r)
}

// skillsAreDelivered is C34 against the pinned copy at path.
func (h *Harness) skillsAreDelivered(ctx context.Context, f *Factory, path string, r *Result) error {
	g, err := newGuardFixture()
	if err != nil {
		return err
	}
	defer os.RemoveAll(g.root)

	// The worker's list is its role's, narrowed as an assignment narrows it.
	const named, called = "implement", "tdd"
	list, err := skills.WorkerSkills(roles.Engineering, []string{named, called}, named)
	if err != nil {
		return err
	}
	plugin := filepath.Join(g.root, "plugin")
	if err := skills.WritePlugin(plugin, list); err != nil {
		return err
	}
	instructions, err := roles.WorkerInstructions(roles.Engineering)
	if err != nil {
		return err
	}
	perms := g.permissions()
	perms.Readable = []string{plugin}
	args := providers.ClaudeCodeArgs(noopHook, guardModel, instructions, plugin, providers.WriteGuard{Writable: []string{g.tmp}}, perms)

	turn, err := h.Provider.Ask(ctx, f, path, AskSpec{
		Dir:  g.workspace,
		Args: args,
		Prompt: "You are being qualified. Use the Skill tool to run the skill `" + named + "` from your skill list, once. " +
			"Its text tells you to use another skill by name: use the Skill tool to run that skill too, under the name your skill list gives it. " +
			"Do not carry out what the skills tell you, and do not run any other tool: once both skills have started, reply with the single word done.",
	})
	if err != nil {
		return err
	}

	// The session was given the plugin's skills, and only the narrowed list.
	if len(turn.Skills) > 0 {
		// The worker's list is all the bundle's skills it could have been
		// given: what Claude Code lists besides them is not this case's.
		var got []string
		for _, name := range turn.Skills {
			bare := strings.TrimPrefix(name, skills.Namespaced(""))
			if slices.Contains(skills.WorkerList(roles.Engineering), bare) && !slices.Contains(got, bare) {
				got = append(got, bare)
			}
		}
		slices.Sort(got)
		if want := slices.Sorted(slices.Values(list)); !slices.Equal(got, want) {
			return fmt.Errorf("the session listed the %s plugin's skills as %v, want exactly the worker's narrowed list %v", skills.PluginName, got, want)
		}
		r.observe("the session was given the narrowed list as the plugin %s: %s", skills.PluginName, strings.Join(list, ", "))
	} else {
		r.observe("known limitation: the session's start did not list its skills, so the narrowed list is shown by the skills it ran alone")
	}
	for _, other := range skills.WorkerList(roles.Engineering) {
		if slices.Contains(list, other) {
			continue
		}
		if slices.Contains(turn.Skills, skills.Namespaced(other)) || slices.Contains(turn.Skills, other) {
			return fmt.Errorf("the session was given %s, which the assignment narrowed out of its list", other)
		}
	}

	// A skill from the list runs.
	ran := func(name string) (Command, bool) {
		var found Command
		for _, c := range turn.SkillCalls {
			if c.Text != name && c.Text != skills.Namespaced(name) {
				continue
			}
			if !c.Failed {
				return c, true
			}
			found = c
		}
		return found, false
	}
	first, ok := ran(named)
	if !ok {
		if first.Text == "" {
			return fmt.Errorf("inconclusive: the session never ran %s with the Skill tool, so the harness cannot show that a skill of its list runs", named)
		}
		return fmt.Errorf("the skill %s of the session's list did not run as %q: %s", named, first.Text, strings.TrimSpace(first.Output))
	}
	r.observe("the skill %s, named by the assignment, ran from the session's list as %q", named, first.Text)

	// A skill that calls another by bare name finds it.
	var attempts []string
	for _, c := range turn.SkillCalls {
		if c.Text == called || c.Text == skills.Namespaced(called) {
			state := "ran"
			if c.Failed {
				state = "was not found: " + strings.TrimSpace(c.Output)
			}
			attempts = append(attempts, fmt.Sprintf("%q %s", c.Text, state))
		}
	}
	if len(attempts) == 0 {
		return fmt.Errorf("inconclusive: the session never ran %s, which %s's text names by bare name, so the harness cannot show that the call resolves", called, named)
	}
	second, ok := ran(called)
	if !ok {
		return fmt.Errorf("the skill %s, which %s's text names by bare name, did not resolve: %s", called, named, strings.Join(attempts, "; "))
	}
	if second.Text == called {
		r.observe("%s's text names %s by bare name, and Claude Code ran it under that bare name, though the plugin namespaces it as %s", named, called, skills.Namespaced(called))
	} else {
		r.observe("%s's text names %s by bare name, and it ran as %q, the name the operating guide gives; attempts: %s", named, called, second.Text, strings.Join(attempts, "; "))
	}
	if len(turn.Plugins) > 0 && !slices.ContainsFunc(turn.Plugins, func(name string) bool { return strings.HasPrefix(name, skills.PluginName) }) {
		return errors.New("the session did not load the per-session plugin")
	}
	return nil
}
