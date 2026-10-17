// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"os"
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
	_, problems, err := fileProblems(path)
	if errors.Is(err, config.ErrMissing) {
		return o.fail(fmt.Errorf("%s does not exist; run `asmai init`, which writes it with every role staffed, or print an example to start from with `asmai config example`", path))
	}
	if err != nil {
		return o.fail(err)
	}
	if o.json {
		list := make([]configProblem, 0, len(problems))
		for _, p := range problems {
			list = append(list, configProblem{Line: p.Line, Problem: p.Problem, Fix: p.Fix})
		}
		code := o.printJSON(map[string]any{"file": path, "valid": len(problems) == 0, "problems": list})
		if code == 0 && len(problems) > 0 {
			code = 1
		}
		return code
	}
	if len(problems) == 0 {
		fmt.Fprintf(o.stdout, "%s has no problems.\n", path)
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

func registeredRepositoryProblems(cfg config.Config) ([]config.Problem, error) {
	paths, err := statedir.Default()
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(paths.Store); errors.Is(err, os.ErrNotExist) {
		return nil, nil
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
	return daemon.RepositoryProblems(cfg, registered), nil
}

// configExample is `asmai config example`: it prints the example
// configuration, with every default written out, which passes `asmai config
// check`. It writes no file.
func configExample(o *output) int {
	if o.json {
		return o.printJSON(map[string]string{"example": string(config.ExampleConfiguration())})
	}
	fmt.Fprint(o.stdout, string(config.ExampleConfiguration()))
	return 0
}
