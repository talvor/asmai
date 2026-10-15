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
// and gives the session an assignment, test-first work, that it is told to do
// by implement's instructions exactly: implement, whose text says "Use /tdd", and then
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

	// implement is started by the explicit slash invocation of its
	// namespaced name, so its load does not depend on the model choosing the
	// Skill tool; whether the skill it names by bare name resolves is shown
	// by the session's own Skill call, which it is told to make. A model may
	// still leave that call out, so the session is asked up to
	// skillAttempts times in fresh sessions: the first attempt that shows
	// the call decides the case, passing when it ran and failing when it
	// did not resolve, and attempts that show no call at all leave the case
	// inconclusive.
	assignment := "/" + skills.Namespaced(named) + " This is your assignment: add a function `slugify(text)` to `slug.js` that lowercases text, " +
		"replaces each run of non-alphanumeric characters with one hyphen and trims hyphens from both ends, " +
		"with its tests in `slug.test.js`, built test-first. " +
		"Follow " + named + "'s instructions exactly, including running every skill it tells you to use with the Skill tool, " +
		"under the name your skill list gives it, before you write any code or test. That skill is not optional here: do not start the work until it has started. " +
		"Once it has started, you may stop: reply with the single word done."
	var quiet int
	for attempt := 1; attempt <= skillAttempts; attempt++ {
		turn, err := h.Provider.Ask(ctx, f, path, AskSpec{Dir: g.workspace, Args: args, Prompt: assignment})
		if err != nil {
			return err
		}
		// The session was given the plugin's skills, and only the narrowed list.
		if !slices.Contains(turn.Plugins, skills.PluginName) {
			return errors.New("the session did not load the per-session plugin")
		}
		var got []string
		for _, name := range turn.Skills {
			if bare, ok := strings.CutPrefix(name, skills.Namespaced("")); ok {
				got = append(got, bare)
			}
		}
		slices.Sort(got)
		if want := slices.Sorted(slices.Values(list)); !slices.Equal(got, want) {
			return fmt.Errorf("the session listed the %s plugin's skills as %v, want exactly the worker's narrowed list %v", skills.PluginName, got, want)
		}

		var tries []string
		var ran *Command
		for i, c := range turn.SkillCalls {
			if c.Text != called && c.Text != skills.Namespaced(called) {
				continue
			}
			state := "ran"
			switch {
			case c.ResultEvent == 0:
				state = "had no tool result"
			case c.Failed || c.Denied:
				state = "was not found: " + strings.TrimSpace(c.Output)
			case ran == nil:
				ran = &turn.SkillCalls[i]
			}
			tries = append(tries, fmt.Sprintf("%q %s", c.Text, state))
		}
		if len(tries) == 0 {
			quiet++
			r.observe("attempt %d of %d: the session did not run %s with the Skill tool", attempt, skillAttempts, called)
			continue
		}
		if ran == nil {
			return fmt.Errorf("the skill %s, which %s's text names by bare name, did not resolve (attempt %d): %s", called, named, attempt, strings.Join(tries, "; "))
		}
		r.observe("the session was given the narrowed list as the plugin %s: %s", skills.PluginName, strings.Join(list, ", "))
		r.observe("%s was started by /%s from the session's list", named, skills.Namespaced(named))
		if ran.Text == called {
			r.observe("%s's text names %s by bare name, and Claude Code ran it under that bare name, though the plugin namespaces it as %s (attempt %d)", named, called, skills.Namespaced(called), attempt)
		} else {
			r.observe("%s's text names %s by bare name, and it ran as %q, the name the operating guide gives; attempts: %s (attempt %d)", named, called, ran.Text, strings.Join(tries, "; "), attempt)
		}
		return nil
	}
	return fmt.Errorf("inconclusive: in %d fresh sessions the model never ran %s with the Skill tool after %s, so the harness cannot show that the bare-name call resolves", quiet, called, named)
}

// skillAttempts is how many fresh sessions C34 asks for the call.
const skillAttempts = 3
