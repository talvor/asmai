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

// c33 qualifies that a session is given only its own list: the user's
// personal skills, Claude Code's built-in skills and a repository's skills
// are hidden, Claude Code's built-in code-review among them next to the
// shipped one (08 rule 19). It plants a personal skill in the harness user's
// ~/.claude/skills and a repository skill in the workspace's .claude/skills,
// writes a Quality worker's plugin, whose list holds the shipped code-review,
// as the daemon does (`skills.WritePlugin`), and starts the pinned Claude
// Code with the command line the code under test builds for the worker
// (`providers.ClaudeCodeArgs`). The session must list exactly the plugin's
// skills, and list what `providers.ClaudeCodeUnhidden` records as not hidden
// only as slash commands, never as skills.
func (h *Harness) c33(ctx context.Context, f *Factory, r *Result) error {
	if err := checkGuardDependencies(); err != nil {
		return err
	}
	path, err := h.ready(ctx, f)
	if err != nil {
		return err
	}
	return h.skillsAreHidden(ctx, f, path, r)
}

// skillsAreHidden is C33 against the pinned copy at path.
func (h *Harness) skillsAreHidden(ctx context.Context, f *Factory, path string, r *Result) error {
	g, err := newGuardFixture()
	if err != nil {
		return err
	}
	defer os.RemoveAll(g.root)

	home := envLookup(f.env, "HOME")
	if home == "" {
		return errors.New("the case's environment has no HOME to plant a personal skill in")
	}
	personal, err := randomToken("asmai-c33-personal")
	if err != nil {
		return err
	}
	personalSkills := filepath.Join(home, ".claude", "skills")
	personalDir := filepath.Join(personalSkills, personal)
	if exists(personalDir) {
		return fmt.Errorf("%s already exists, and the case plants its personal skill only where nothing is", personalDir)
	}
	// The planted skill is removed after the case, and so is the user's
	// skills directory when the case made it.
	if !exists(personalSkills) {
		defer os.Remove(personalSkills)
	}
	defer os.RemoveAll(personalDir)
	if err := writeSkill(personalDir, personal, "A personal skill of the harness user's, which no agent session may list."); err != nil {
		return err
	}
	repository := "asmai-c33-repository"
	if err := writeSkill(filepath.Join(g.workspace, ".claude", "skills", repository), repository, "A skill of the repository's, which no agent session may list unless it is switched on."); err != nil {
		return err
	}

	// Quality's workers ship code-review, so the session is given the
	// shipped one, as asmai:code-review, beside which Claude Code's built-in
	// code-review must not be listed.
	const shipped = "code-review"
	list, err := skills.WorkerSkills(roles.Quality, nil, shipped)
	if err != nil {
		return err
	}
	plugin := filepath.Join(g.root, "plugin")
	if err := skills.WritePlugin(plugin, list); err != nil {
		return err
	}
	instructions, err := roles.WorkerInstructions(roles.Quality)
	if err != nil {
		return err
	}
	perms := g.permissions()
	perms.Readable = []string{plugin}
	args := providers.ClaudeCodeArgs(noopHook, guardModel, instructions, plugin, providers.WriteGuard{Writable: []string{g.tmp}}, perms)
	turn, err := h.Provider.Ask(ctx, f, path, AskSpec{Dir: g.workspace, Args: args, Prompt: "Reply with the single word done."})
	if err != nil {
		return err
	}
	if !slices.Contains(turn.Plugins, skills.PluginName) {
		return errors.New("the session did not load the per-session plugin")
	}
	var want []string
	for _, name := range list {
		want = append(want, skills.Namespaced(name))
	}
	var extra []string
	for _, name := range turn.Skills {
		if !slices.Contains(want, name) {
			extra = append(extra, name)
		}
	}
	for _, name := range extra {
		switch name {
		case personal:
			return fmt.Errorf("the session listed %s, the personal skill planted in %s: the user's personal skills are not hidden", name, filepath.Dir(personalDir))
		case repository:
			return fmt.Errorf("the session listed %s, the repository skill planted in the workspace's .claude/skills: repository skills are not hidden", name)
		case shipped:
			return fmt.Errorf("the session listed Claude Code's built-in %s next to the shipped %s: provider built-in skills are not hidden", name, skills.Namespaced(shipped))
		}
	}
	if len(extra) > 0 {
		return fmt.Errorf("the session listed the skills %v, which are not on its list %v: they are not hidden", extra, want)
	}
	for _, name := range want {
		if !slices.Contains(turn.Skills, name) {
			return fmt.Errorf("the session listed the skills %v, which lack %s from its list", turn.Skills, name)
		}
	}
	for _, u := range providers.ClaudeCodeUnhidden() {
		for _, command := range u.Commands {
			if slices.Contains(turn.Skills, command) || !slices.Contains(turn.SlashCommands, command) {
				return fmt.Errorf("the record of what Claude Code cannot hide is out of date: it says /%s stays a slash command but is hidden from the agent, and the session listed the skills %v and the slash commands %v", command, turn.Skills, turn.SlashCommands)
			}
		}
		r.observe("not hidden, as recorded for asmai doctor: %s, listed as slash commands only: %s", u.What, strings.Join(u.Commands, ", "))
	}
	r.observe("the session listed exactly its list as the plugin %s: %s", skills.PluginName, strings.Join(want, ", "))
	r.observe("hidden: the personal skill %s, the repository skill %s and Claude Code's built-in skills, its %s next to the shipped %s", personal, repository, shipped, skills.Namespaced(shipped))
	return nil
}

// writeSkill writes a skill called name, with description, in dir.
func writeSkill(dir, name, description string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	text := "---\nname: " + name + "\ndescription: " + description + "\n---\n\nReply with the word " + name + ".\n"
	return os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(text), 0o644)
}
