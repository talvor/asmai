// SPDX-License-Identifier: Apache-2.0

// Package githooks puts AsmAI's trailers on the commits its workers make. A
// worker's git is given a hooks directory of AsmAI's through its environment,
// not the repository's configuration: each hook in it runs `asmai git-hook`,
// which adds the trailers to a commit's message and runs the hook the
// repository would have run, so the repository's own hooks still apply.
package githooks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/talvor/asmai/internal/providers"
)

// Names are git's client-side hooks. Each is forwarded, so that giving a
// worker's git AsmAI's hooks directory takes none of the repository's away.
var Names = []string{
	"applypatch-msg", "pre-applypatch", "post-applypatch",
	"pre-commit", "pre-merge-commit", "prepare-commit-msg", "commit-msg", "post-commit",
	"pre-rebase", "post-checkout", "post-merge", "pre-push", "pre-auto-gc", "post-rewrite",
	"reference-transaction", "push-to-checkout", "post-index-change", "fsmonitor-watchman",
	"sendemail-validate",
}

const PrepareCommitMessage = "prepare-commit-msg"

// Trailers returns the trailers of a worker's commit, each as "Key: value".
type Trailers func() ([]string, error)

// Install writes the forwarding hooks into dir, replacing those a previous
// daemon wrote. Each runs executable's `git-hook` command with its own name
// and the arguments git gave it.
func Install(dir, executable string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	for _, name := range Names {
		script := "#!/bin/sh\nexec " + providers.ShellQuote(executable) + " git-hook " + name + " \"$@\"\n"
		part, err := os.CreateTemp(dir, ".hook-*")
		if err != nil {
			return fmt.Errorf("writing the git hooks: %w", err)
		}
		_, err = part.WriteString(script)
		if err == nil {
			err = part.Chmod(0o755)
		}
		if closeErr := part.Close(); err == nil {
			err = closeErr
		}
		if err == nil {
			err = os.Rename(part.Name(), filepath.Join(dir, name))
		}
		if err != nil {
			os.Remove(part.Name())
			return fmt.Errorf("writing the git hooks: %w", err)
		}
	}
	return nil
}

// Environment returns the variables that give git the hooks directory dir
// through its configuration, after any configuration environ already gives
// it, as a map of the variables to set.
func Environment(environ []string, dir string) map[string]string {
	count := 0
	for _, kv := range environ {
		if value, ok := strings.CutPrefix(kv, "GIT_CONFIG_COUNT="); ok {
			if n, err := strconv.Atoi(value); err == nil && n > 0 {
				count = n
			}
		}
	}
	index := strconv.Itoa(count)
	return map[string]string{
		"GIT_CONFIG_COUNT":          strconv.Itoa(count + 1),
		"GIT_CONFIG_KEY_" + index:   "core.hooksPath",
		"GIT_CONFIG_VALUE_" + index: dir,
	}
}

// Run runs the hook name that git called with args, with its standard input,
// and returns the exit status git should see. For every hook, the
// repository's own hook of that name runs if it has one, and decides the
// status. The prepared message then gets the trailers. ownDir is AsmAI's
// hooks directory, which is never run as the repository's.
func Run(ctx context.Context, name string, args []string, stdin io.Reader, stdout, stderr io.Writer, ownDir string, trailers Trailers) int {
	hook := repositoryHook(ctx, name, ownDir)
	if hook != "" {
		cmd := exec.CommandContext(ctx, hook, args...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
		if err := cmd.Run(); err != nil {
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				return exit.ExitCode()
			}
			fmt.Fprintf(stderr, "asmai git-hook: running the repository's %s: %v\n", name, err)
			return 1
		}
	}
	if name == PrepareCommitMessage {
		if len(args) < 1 {
			fmt.Fprintln(stderr, "asmai git-hook: prepare-commit-msg needs the message file git passes it")
			return 2
		}
		if err := addTrailers(ctx, args[0], trailers); err != nil {
			fmt.Fprintf(stderr, "asmai: this commit was refused because it cannot carry the AsmAI trailers: %v\n", err)
			return 1
		}
	}
	return 0
}

// addTrailers adds the trailers to the commit message in file, replacing any
// of the same keys, so that amending a commit never doubles them.
func addTrailers(ctx context.Context, file string, trailers Trailers) error {
	lines, err := trailers()
	if err != nil {
		return err
	}
	args := []string{"interpret-trailers", "--in-place", "--if-exists", "replace", "--if-missing", "add"}
	for _, line := range lines {
		args = append(args, "--trailer", line)
	}
	cmd := exec.CommandContext(ctx, "git", append(args, file)...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git interpret-trailers: %w: %s", err, strings.TrimSpace(out.String()))
	}
	return nil
}

// repositoryHook returns the path of the hook name that git would have run
// without AsmAI's hooks directory, or "" when there is none. The repository
// is asked with AsmAI's configuration taken out of the environment, so that
// a hooks directory the repository's own tooling set is found too.
func repositoryHook(ctx context.Context, name, ownDir string) string {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--git-path", "hooks")
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_CONFIG_COUNT=") && !strings.HasPrefix(kv, "GIT_CONFIG_KEY_") && !strings.HasPrefix(kv, "GIT_CONFIG_VALUE_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	dir := strings.TrimSpace(string(out))
	if !filepath.IsAbs(dir) {
		cwd, err := os.Getwd()
		if err != nil {
			return ""
		}
		dir = filepath.Join(cwd, dir)
	}
	if same(dir, ownDir) {
		return ""
	}
	path := filepath.Join(dir, name)
	if info, err := os.Stat(path); err != nil || info.IsDir() || info.Mode().Perm()&0o111 == 0 {
		return ""
	}
	return path
}

func same(a, b string) bool {
	ia, err := os.Stat(a)
	if err != nil {
		return false
	}
	ib, err := os.Stat(b)
	return err == nil && os.SameFile(ia, ib)
}
