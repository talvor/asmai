// SPDX-License-Identifier: Apache-2.0

// Command qualify is AsmAI's qualification harness. Run on a qualification
// host as that host's own OS user, it builds the commit under test, runs its
// own factory with the real pinned provider CLIs, signed in as that user, and
// reports each qualification case as passed or failed with the platform, the
// pinned provider version and the commit (11 rules 6, 7 and 12 to 16).
//
// It is for development only: it lives in its own directory, is built only by
// `make qualify`, and is never part of the asmai executable or a release. It
// never runs in hosted CI, and asmai has no qualify command.
//
//	qualify [--commit REV] [--repo DIR] [--cases C3,C4] [--json] [--show-screen]
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/talvor/asmai/internal/config"
	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/statedir"
)

// How long a case waits for the provider's session to get going, and how
// often it looks.
const (
	sessionWait = 2 * time.Minute
	sessionPoll = 250 * time.Millisecond
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	cancel()
	os.Exit(code)
}

// run exits 0 when every case passed, 1 when a case failed, which leaves the
// combination unqualified, and 2 when the harness could not run.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("qualify", flag.ContinueOnError)
	flags.SetOutput(stderr)
	commit := flags.String("commit", "", "the commit to qualify; by default the commit this harness was built from, else the repository's HEAD")
	repo := flags.String("repo", ".", "a directory in the repository holding the commit")
	only := flags.String("cases", "", "the cases to run, such as C3,C4; by default all")
	jsonOutput := flags.Bool("json", false, "print the report as JSON")
	showScreen := flags.Bool("show-screen", false, "show the leader's last screen in a failure; it may show the signed-in account")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "qualify: unexpected argument %q\n", flags.Arg(0))
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "qualify: %v\n", err)
		return 2
	}
	ids, err := parseCases(*only)
	if err != nil {
		return fail(err)
	}

	if err := checkAccount(os.Getuid(), os.Getenv("HOME"), os.Getenv); err != nil {
		return fail(err)
	}
	paths, err := statedir.Default()
	if err != nil {
		return fail(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fail(err)
	}
	defaultPaths := statedir.At(filepath.Join(home, ".local", "state", "asmai"))
	if err := checkFactory(defaultPaths); err != nil {
		return fail(err)
	}
	configFile, err := config.ActivePath(home)
	if err != nil {
		return fail(err)
	}
	if paths.Dir != defaultPaths.Dir {
		if err := checkFactory(paths); err != nil {
			return fail(err)
		}
	}

	root, rev, err := resolveCommit(ctx, *repo, commitUnderTest(*commit))
	if err != nil {
		return fail(err)
	}
	if built, modified := builtFrom(); built != "" && (modified || built != rev) {
		return fail(fmt.Errorf("this harness was built from %s (uncommitted changes: %t), but the commit under test is %s: the harness and the code it qualifies share a commit, so check out the commit and run `make qualify` again", built, modified, rev))
	}

	scratch, err := os.MkdirTemp("", "asmai-qualify-")
	if err != nil {
		return fail(err)
	}
	defer os.RemoveAll(scratch)
	fmt.Fprintf(stderr, "qualify: building commit %s\n", rev)
	subject, err := build(ctx, root, rev, scratch)
	if err != nil {
		return fail(err)
	}
	var qualificationPaths statedir.Paths
	// cleanup removes the disposable state once the cases have run, and is
	// left to the deferred call only when the run ends early.
	var cleanup func() error
	defer func() {
		if cleanup != nil {
			if err := cleanup(); err != nil {
				fmt.Fprintf(stderr, "qualify: %v\n", err)
			}
		}
	}()
	if len(ids) == 0 || slices.ContainsFunc(ids, disposableState) {
		qualificationPaths, cleanup, err = createQualificationState(home)
		if err != nil {
			return fail(err)
		}
	}

	host, _ := os.Hostname()
	account := os.Getenv("USER")
	if u, err := user.Current(); err == nil {
		account = u.Username
	}
	h := &Harness{
		Subject:            subject,
		Provider:           ClaudeCode{},
		Paths:              paths,
		QualificationPaths: qualificationPaths,
		ConfigFile:         configFile,
		Env:                os.Environ(),
		Host:               host,
		User:               account,
		Wait:               sessionWait,
		Poll:               sessionPoll,
		ShowScreen:         *showScreen,
		Log:                stderr,
	}
	report := h.Run(ctx, ids)
	if cleanup != nil {
		if err := cleanup(); err != nil {
			report.Cleanup = err.Error()
		}
		cleanup = nil
	}
	if ctx.Err() != nil {
		if report.Cleanup != "" {
			return fail(fmt.Errorf("interrupted; %s", report.Cleanup))
		}
		return fail(errors.New("interrupted"))
	}
	if *jsonOutput {
		if err := report.WriteJSON(stdout); err != nil {
			return fail(err)
		}
	} else {
		report.WriteText(stdout)
	}
	if report.Failed() {
		return 1
	}
	if report.Cleanup != "" {
		return 2
	}
	return 0
}

// createQualificationState makes the disposable state directory, and returns
// it with the cleanup that removes it, which says why when it cannot.
func createQualificationState(home string) (statedir.Paths, func() error, error) {
	dir := filepath.Join(home, ".local", "state", "asmai", "qualification")
	if err := os.MkdirAll(filepath.Dir(dir), 0700); err != nil {
		return statedir.Paths{}, nil, err
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		return statedir.Paths{}, nil, fmt.Errorf("creating disposable qualification state %s: %w; remove a stale qualification directory only after confirming no factory is using it", dir, err)
	}
	return statedir.At(dir), func() error {
		if err := removeTree(dir); err != nil {
			return fmt.Errorf("removing the disposable qualification state %s: %w; remove it by hand once no factory is using it", dir, err)
		}
		return nil
	}, nil
}

// removeTree removes dir and everything in it. It first gives the owner
// write permission on every directory under dir, as repos.RemoveView does
// before it removes a view, because a job's view is made read-only.
func removeTree(dir string) error {
	filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && entry.IsDir() {
			if info, err := entry.Info(); err == nil {
				os.Chmod(path, info.Mode().Perm()|0o700)
			}
		}
		return nil
	})
	return os.RemoveAll(dir)
}

func envValue(env []string, name, value string) []string {
	result := make([]string, 0, len(env)+1)
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if key != name {
			result = append(result, entry)
		}
	}
	return append(result, name+"="+value)
}

// commitUnderTest is the revision to qualify: the one asked for, else the
// commit the harness was built from, else HEAD.
func commitUnderTest(asked string) string {
	if asked != "" {
		return asked
	}
	if built, _ := builtFrom(); built != "" {
		return built
	}
	return "HEAD"
}

// parseCases returns the case IDs a --cases value names, each of which must be
// a case the harness has.
func parseCases(value string) ([]string, error) {
	if value == "" {
		return nil, nil
	}
	var ids []string
	for id := range strings.SplitSeq(value, ",") {
		id = strings.ToUpper(strings.TrimSpace(id))
		known := false
		for _, c := range Cases {
			known = known || c.ID == id
		}
		if !known {
			return nil, fmt.Errorf("there is no case %q: the harness has %s", id, caseIDs())
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func caseIDs() string {
	var ids []string
	for _, c := range Cases {
		ids = append(ids, c.ID)
	}
	return strings.Join(ids, ", ")
}

// checkFactory refuses to run unless the harness user's own factory is ready
// for the harness to run its cases in: configured, and not already running.
// A factory that is running was not started with the harness's environment,
// and the harness user's work in it is not the harness's to stop.
func checkFactory(paths statedir.Paths) error {
	if _, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandStatus}); err == nil {
		return fmt.Errorf("a factory is already running in %s: run `asmai stop` as this user first, because the harness starts its own factory", paths.Dir)
	} else if !errors.Is(err, daemon.ErrNotRunning) {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	path, err := config.ActivePath(home)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("%s is missing: the harness's factory needs the roles staffed, as docs/qualification-harness.md describes", path)
	}
	return nil
}
