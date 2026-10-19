// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/talvor/asmai/internal/config"
	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/statedir"
	"github.com/talvor/asmai/internal/store"
)

// configProblem is a problem of the configuration file as `asmai config
// check --json` prints it.
type configProblem struct {
	Line    int    `json:"line,omitempty"`
	Problem string `json:"problem"`
	Fix     string `json:"fix"`
}

// configCheck is `asmai config check`: it reads the configuration file the
// factory would read and reports every problem in it, each with its line and
// the fix, exiting 1 if there is any. It changes nothing: only the user
// changes the file.
func configCheck(o *output) int {
	home, err := os.UserHomeDir()
	if err != nil {
		return o.fail(fmt.Errorf("finding the configuration file: %w", err))
	}
	path, err := config.ActivePath(home)
	if err != nil {
		return o.fail(err)
	}
	cfg, problems, err := fileProblems(path)
	if errors.Is(err, config.ErrMissing) {
		return o.fail(fmt.Errorf("%s does not exist; run `asmai init`, which writes it with every role staffed, or print an example to start from with `asmai config example`", path))
	}
	if err != nil {
		return o.fail(err)
	}
	if o.structured() {
		list := make([]configProblem, 0, len(problems))
		for _, p := range problems {
			list = append(list, configProblem{Line: p.Line, Problem: p.Problem, Fix: p.Fix})
		}
		writable := cfg.OpenFolders(home)
		if writable == nil {
			writable = []config.RepositoryFolders{}
		}
		code := o.printData(map[string]any{"file": path, "valid": len(problems) == 0, "problems": list, "writable": writable})
		if code == 0 && len(problems) > 0 {
			code = 1
		}
		return code
	}
	if len(problems) == 0 {
		fmt.Fprintf(o.stdout, "%s has no problems.\n", path)
		printOpenFolders(o.stdout, cfg.OpenFolders(home))
		return 0
	}
	noun := "problems"
	if len(problems) == 1 {
		noun = "problem"
	}
	fmt.Fprintf(o.stderr, "%s has %d %s:\n", path, len(problems), noun)
	for _, p := range problems {
		where := "  "
		if p.Line > 0 {
			where = fmt.Sprintf("  line %d: ", p.Line)
		}
		fmt.Fprintf(o.stderr, "%s%s\n", where, p.Problem)
		fmt.Fprintf(o.stderr, "      fix: %s\n", strings.ReplaceAll(p.Fix, "\n", "\n           "))
	}
	return 1
}

// fileProblems reads the configuration file at path and returns it with
// every problem `asmai config check`, `asmai doctor` and the start of the
// factory count: those in the file, a role the factory runs that is not
// staffed, and those of the registered repositories. An error wraps
// config.ErrMissing for a file that does not exist.
func fileProblems(path string) (config.Config, []config.Problem, error) {
	cfg, found, err := config.Load(path)
	if errors.Is(err, config.ErrMissing) {
		return cfg, nil, err
	}
	if err != nil {
		return cfg, nil, fmt.Errorf("reading %s: %w", path, err)
	}
	problems := found
	if len(found) == 0 {
		problems = cfg.MissingStaffing(roles.Staffed)
		repositoryProblems, err := registeredRepositoryProblems(cfg)
		if err != nil {
			return cfg, nil, err
		}
		problems = append(problems, repositoryProblems...)
	}
	return cfg, problems, nil
}

// printOpenFolders tells the user on w which folders each repository opens
// outside its workspaces for its workers to write.
func printOpenFolders(w io.Writer, open []config.RepositoryFolders) {
	if len(open) == 0 {
		return
	}
	fmt.Fprintln(w, "Folders outside their workspaces that a repository's workers may write:")
	for _, r := range open {
		for _, f := range r.Folders {
			text := f.Resolved
			if f.Env != "" {
				text += " (exported as " + f.Env + ")"
			}
			fmt.Fprintf(w, "  %s: %s\n", r.Repository, text)
		}
	}
}

// registeredRepositoryProblems returns the problems of the file's
// repositories on this host: those the registered repositories have with
// their entries, and the places the entries' writable folders open that are
// closed to them.
func registeredRepositoryProblems(cfg config.Config) ([]config.Problem, error) {
	paths, err := statedir.Default()
	if err != nil {
		return nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("finding the home directory: %w", err)
	}
	writable := cfg.WritableProblems(config.NewBounds(home, paths.Dir, paths.Repositories, slices.Collect(maps.Keys(cfg.Repositories))))
	if _, err := os.Stat(paths.Store); errors.Is(err, os.ErrNotExist) {
		return writable, nil
	} else if err != nil {
		return nil, fmt.Errorf("reading the registered repositories: %w", err)
	}
	st, err := store.OpenReadOnly(paths.Store)
	if err != nil {
		return nil, fmt.Errorf("reading the registered repositories: %w", err)
	}
	defer st.Close()
	registered, err := st.Repositories()
	if err != nil {
		return nil, fmt.Errorf("reading the registered repositories: %w", err)
	}
	return append(daemon.RepositoryProblems(cfg, registered), writable...), nil
}

// configExample is `asmai config example`: it prints the example
// configuration, with every default written out, which passes `asmai config
// check`. It writes no file.
func configExample(o *output) int {
	if o.structured() {
		return o.printData(map[string]string{"example": string(config.ExampleConfiguration())})
	}
	fmt.Fprint(o.stdout, string(config.ExampleConfiguration()))
	return 0
}
