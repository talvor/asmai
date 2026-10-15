// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"unicode"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/talvor/asmai/internal/config"
	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/statedir"
)

// configFile returns the configuration file the factory would read, and its
// content.
func configFile() (path string, data []byte, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", nil, fmt.Errorf("finding the configuration file: %w", err)
	}
	if path, err = config.ActivePath(home); err != nil {
		return "", nil, err
	}
	data, err = os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return path, nil, fmt.Errorf("%s does not exist; run `asmai setup`, which writes it with every role staffed, or print an example to start from with `asmai config example`", path)
	}
	if err != nil {
		return path, nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return path, data, nil
}

// printProblems tells the user on w what is wrong with the file at path.
func printProblems(w io.Writer, path string, problems []config.Problem) {
	noun := "problems"
	if len(problems) == 1 {
		noun = "problem"
	}
	fmt.Fprintf(w, "%s has %d %s:\n", path, len(problems), noun)
	for _, p := range problems {
		where := "  "
		if p.Line > 0 {
			where = fmt.Sprintf("  line %d: ", p.Line)
		}
		fmt.Fprintf(w, "%s%s\n", where, p.Problem)
		fmt.Fprintf(w, "      fix: %s\n", strings.ReplaceAll(p.Fix, "\n", "\n           "))
	}
}

// configShow is `asmai config show`: the effective configuration, every
// setting written out with its default.
func configShow(o *output) int {
	path, data, err := configFile()
	if err != nil {
		return o.fail(err)
	}
	cfg, found := config.Parse(data)
	if len(found) > 0 {
		printProblems(o.stderr, path, found)
		fmt.Fprintln(o.stderr, "asmai: fix them, as `asmai config check` shows, to see the effective configuration")
		return 1
	}
	rendered := config.Render(cfg)
	if o.json {
		var settings map[string]any
		if err := toml.Unmarshal(rendered, &settings); err != nil {
			return o.fail(err)
		}
		return o.printJSON(map[string]any{"file": path, "configuration": settings})
	}
	fmt.Fprintf(o.stdout, "# The effective configuration of %s, with every default written out.\n", path)
	o.stdout.Write(rendered)
	return 0
}

// configEdit is `asmai config edit`: it opens a copy of the configuration
// file in the user's editor, and accepts what they save only when it has no
// problem. A file with problems is shown them, and the user edits it again
// or leaves the file as it was.
func configEdit(o *output, stdin io.Reader) int {
	home, err := os.UserHomeDir()
	if err != nil {
		return o.fail(fmt.Errorf("finding the configuration file: %w", err))
	}
	path, err := config.ActivePath(home)
	if err != nil {
		return o.fail(err)
	}
	original, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return o.fail(fmt.Errorf("%s does not exist; run `asmai setup` to create it before editing", path))
	}
	if err != nil {
		return o.fail(fmt.Errorf("reading %s: %w", path, err))
	}
	editor := firstSet("VISUAL", "EDITOR")
	if editor == "" {
		return o.fail(errors.New("no editor is set: set VISUAL or EDITOR to the editor to open, such as EDITOR=vi"))
	}
	command, err := editorCommand(editor)
	if err != nil {
		return o.fail(fmt.Errorf("invalid editor command %q: %w", editor, err))
	}
	// Edit a copy, so that nothing is accepted before it is checked.
	copyFile, err := os.CreateTemp("", "asmai-config-*.toml")
	if err != nil {
		return o.fail(err)
	}
	keepCopy := false
	defer func() {
		if !keepCopy {
			os.Remove(copyFile.Name())
		}
	}()
	if _, err := copyFile.Write(original); err != nil {
		copyFile.Close()
		return o.fail(err)
	}
	if err := copyFile.Close(); err != nil {
		return o.fail(err)
	}
	w := o.stdout
	if o.json {
		w = o.stderr
	}
	failEdit := func(err error) int {
		copyData, readErr := os.ReadFile(copyFile.Name())
		if readErr == nil && bytes.Equal(copyData, original) {
			return o.fail(err)
		}
		if readErr != nil {
			if _, statErr := os.Lstat(copyFile.Name()); statErr != nil {
				return o.fail(err)
			}
		}
		keepCopy = true
		return o.fail(fmt.Errorf("%w; your edited copy remains at %s", err, copyFile.Name()))
	}
	for {
		cmd := exec.Command(command[0], append(command[1:], copyFile.Name())...)
		cmd.Stdout, cmd.Stderr = w, o.stderr
		// A full-screen editor needs the user's own terminal; any other
		// input is the user's answers, which the editor must not consume.
		if f, ok := stdin.(*os.File); ok {
			cmd.Stdin = f
		}
		if err := cmd.Run(); err != nil {
			return failEdit(fmt.Errorf("the editor %q did not finish: %w; %s is as it was", editor, err, path))
		}
		edited, err := os.ReadFile(copyFile.Name())
		if err != nil {
			return failEdit(err)
		}
		if bytes.Equal(edited, original) {
			fmt.Fprintf(w, "No change; %s is as it was.\n", path)
			return o.editResult(path, false)
		}
		cfg, parseProblems := config.Parse(edited)
		problems := parseProblems
		if len(parseProblems) == 0 {
			problems = cfg.MissingStaffing(roles.Staffed)
			repositoryProblems, err := registeredRepositoryProblems(cfg)
			if err != nil {
				return failEdit(err)
			}
			problems = append(problems, repositoryProblems...)
		}
		if len(problems) > 0 {
			printProblems(o.stderr, copyFile.Name()+" (your edit)", problems)
			fmt.Fprint(w, "Edit it again? [Y/n] ")
			line := readLine(stdin)
			if answer := strings.ToLower(strings.TrimSpace(line)); answer == "n" || answer == "no" || !strings.HasSuffix(line, "\n") {
				return o.fail(fmt.Errorf("not accepted: %s is as it was", path))
			}
			continue
		}
		// What is on disk now is what the edit began from, or the edit would
		// replace someone else's change.
		now, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return failEdit(err)
		}
		if !bytes.Equal(now, original) {
			keepCopy = true
			return o.fail(fmt.Errorf("%s changed while you edited it, so your edit was not saved; your edited copy remains at %s", path, copyFile.Name()))
		}
		if err := config.Replace(path, edited); err != nil {
			return failEdit(fmt.Errorf("saving %s: %w", path, err))
		}
		fmt.Fprintf(w, "Saved %s; it has no problems. Run `asmai config apply` to see which running agents pick the change up.\n", path)
		return o.editResult(path, true)
	}
}

func (o *output) editResult(path string, saved bool) int {
	if o.json {
		return o.printJSON(map[string]any{"file": path, "saved": saved})
	}
	return 0
}

// firstSet returns the first of the environment variables names that is
// set and not empty.
func firstSet(names ...string) string {
	for _, name := range names {
		if v := os.Getenv(name); v != "" {
			return v
		}
	}
	return ""
}

func editorCommand(value string) ([]string, error) {
	var args []string
	var word strings.Builder
	var quote rune
	escaped, started := false, false
	for _, r := range value {
		switch {
		case escaped:
			word.WriteRune(r)
			escaped = false
		case r == '\\' && quote != '\'':
			escaped = true
			started = true
		case quote != 0 && r == quote:
			quote = 0
		case quote == 0 && (r == '\'' || r == '"'):
			quote = r
			started = true
		case quote == 0 && unicode.IsSpace(r):
			if started {
				args = append(args, word.String())
				word.Reset()
				started = false
			}
		default:
			word.WriteRune(r)
			started = true
		}
	}
	if quote != 0 || escaped {
		return nil, errors.New("unterminated quote or escape")
	}
	if started {
		args = append(args, word.String())
	}
	if len(args) == 0 || args[0] == "" {
		return nil, errors.New("no editor executable")
	}
	return args, nil
}

// configApply is `asmai config apply`: it hands the configuration file to
// the running daemon, which validates it and answers with what it changes
// and its effect on running agents. Once the user confirms, the daemon
// applies it and journals it.
func configApply(o *output, paths statedir.Paths, stdin io.Reader) int {
	path, data, err := configFile()
	if err != nil {
		return o.fail(err)
	}
	cfg, problems := config.Parse(data)
	if len(problems) == 0 {
		problems = cfg.MissingStaffing(roles.Staffed)
		repositoryProblems, err := registeredRepositoryProblems(cfg)
		if err != nil {
			return o.fail(err)
		}
		problems = append(problems, repositoryProblems...)
	}
	if len(problems) > 0 {
		printProblems(o.stderr, path, problems)
		fmt.Fprintln(o.stderr, "asmai: nothing was applied")
		return 1
	}
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandConfigApply, Configuration: string(data)})
	if errors.Is(err, daemon.ErrNotRunning) {
		if o.json {
			return o.printJSON(map[string]any{"file": path, "applied": false, "running": false})
		}
		fmt.Fprintf(o.stdout, "%s has no problems. The factory is not running, so no agent has anything to pick up: it reads the file when it starts, with `asmai start`.\n", path)
		return 0
	}
	if err != nil {
		return o.fail(err)
	}
	preview := resp.ConfigApply
	if preview == nil {
		return o.fail(errors.New("the daemon did not answer with the change"))
	}
	if len(preview.Changes) == 0 {
		if o.json {
			return o.printJSON(preview)
		}
		fmt.Fprintf(o.stdout, "%s is as applied; there is no change.\n", path)
		return 0
	}
	w := o.stdout
	if o.json {
		w = o.stderr
	}
	showChanges(w, path, preview.Changes)
	fmt.Fprint(w, "Apply it? [y/N] ")
	answer := readLine(stdin)
	if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
		if !strings.HasSuffix(answer, "\n") {
			fmt.Fprintln(w)
		}
		return o.fail(errors.New("not confirmed; nothing was applied"))
	}
	resp, err = daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandConfigApply, Configuration: string(data), Confirm: true, Preview: preview})
	if err != nil {
		return o.fail(fmt.Errorf("%w; nothing more was applied", err))
	}
	applied := resp.ConfigApply
	if applied == nil || !applied.Applied {
		return o.fail(errors.New("the daemon did not apply the configuration"))
	}
	if o.json {
		return o.printJSON(applied)
	}
	fmt.Fprintf(o.stdout, "Applied, and journaled as configuration.applied (SHA-256 %s).\n", applied.SHA256)
	return 0
}

// showChanges prints each change and its effect on running agents.
func showChanges(w io.Writer, path string, changes []daemon.ConfigChange) {
	noun := "changes"
	if len(changes) == 1 {
		noun = "change"
	}
	fmt.Fprintf(w, "%s has %d %s:\n", path, len(changes), noun)
	for _, c := range changes {
		switch {
		case c.Added:
			fmt.Fprintf(w, "  %s: added\n", c.Setting)
		case c.Removed:
			fmt.Fprintf(w, "  %s: removed\n", c.Setting)
		default:
			fmt.Fprintf(w, "  %s: %s -> %s\n", c.Setting, orNone(c.Before), orNone(c.After))
		}
		if c.Skills && len(c.Gained) > 0 {
			fmt.Fprintf(w, "      adds skills: %s\n", strings.Join(c.Gained, ", "))
		}
		if c.Skills && len(c.Lost) > 0 {
			fmt.Fprintf(w, "      removes skills: %s\n", strings.Join(c.Lost, ", "))
		}
		if c.Repository != "" {
			if len(c.Gained) > 0 {
				fmt.Fprintf(w, "      records %s's own skills for: %s\n", c.Repository, strings.Join(c.Gained, ", "))
			}
			if len(c.Lost) > 0 {
				fmt.Fprintf(w, "      removes %s's own skills setting for: %s\n", c.Repository, strings.Join(c.Lost, ", "))
			}
		}
		switch {
		case c.Note != "":
			fmt.Fprintf(w, "      %s\n", c.Note)
		case len(c.Agents) > 0:
			fmt.Fprintf(w, "      picked up at the next start of: %s\n", strings.Join(c.Agents, ", "))
		default:
			fmt.Fprintf(w, "      no running agent is affected, so it applies at once\n")
		}
	}
}

func orNone(text string) string {
	if text == "" {
		return "(none)"
	}
	return text
}
