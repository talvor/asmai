// SPDX-License-Identifier: Apache-2.0

// Package repos is what registering a repository needs from git: finding out
// what the user gave `asmai repo add`, and making AsmAI's own clone of it.
// It runs the user's git, with the user's configuration and credentials, and
// never one that asks for a password: the daemon has no terminal.
package repos

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// Source is a repository as the user gave it to `asmai repo add`.
type Source struct {
	// Location is the root of the user's own checkout, or empty when the
	// user gave a URL, or a bare repository standing in for one.
	Location string
	// Origin is the origin remote AsmAI's clone is fetched from.
	Origin string
}

// scpLike matches git's scp-like syntax for an SSH remote, user@host:path.
var scpLike = regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9._-]+:`)

func looksLikeURL(arg string) bool {
	return strings.Contains(arg, "://") || scpLike.MatchString(arg)
}

// Resolve finds what arg names: a local checkout, whose origin remote it
// reads; a bare repository on this host, which is itself the origin; or a
// URL. It refuses what has no origin for AsmAI's clone to be fetched from.
func Resolve(ctx context.Context, arg string) (Source, error) {
	if arg == "" {
		return Source{}, errors.New("name a local checkout or a URL")
	}
	info, err := os.Stat(arg)
	switch {
	case err == nil && info.IsDir():
		return fromDirectory(ctx, arg)
	case err == nil:
		return Source{}, fmt.Errorf("%s is a file: name a local checkout or a URL", arg)
	case looksLikeURL(arg):
		if err := checkCredentials(arg); err != nil {
			return Source{}, err
		}
		return Source{Origin: arg}, nil
	}
	return Source{}, fmt.Errorf("%s is not a directory, and does not look like a URL such as https://github.com/me/repo.git or git@github.com:me/repo.git", arg)
}

func fromDirectory(ctx context.Context, dir string) (Source, error) {
	bare, err := git(ctx, dir, "rev-parse", "--is-bare-repository")
	if err != nil {
		return Source{}, fmt.Errorf("%s is not a git repository: %w", dir, err)
	}
	if bare == "true" {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return Source{}, err
		}
		return Source{Origin: abs}, nil
	}
	top, err := git(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return Source{}, fmt.Errorf("finding the root of %s: %w", dir, err)
	}
	origin, err := git(ctx, top, "config", "--get", "remote.origin.url")
	if err != nil || origin == "" {
		return Source{}, fmt.Errorf("%s has no remote named origin, so AsmAI's clone would have nothing to be fetched from; add it with `git remote add origin <url>` and register it again", top)
	}
	if !looksLikeURL(origin) && !filepath.IsAbs(origin) {
		// Git reads a relative remote from the checkout's root.
		origin = filepath.Join(top, origin)
	}
	if err := checkCredentials(origin); err != nil {
		return Source{}, fmt.Errorf("the origin of %s: %w", top, err)
	}
	return Source{Location: top, Origin: origin}, nil
}

// checkCredentials refuses a URL that carries a password, which would be
// written to the configuration file and the journal. AsmAI handles no
// tokens: git's own credential helpers and SSH keys do.
func checkCredentials(origin string) error {
	if !strings.Contains(origin, "://") {
		return nil
	}
	u, err := url.Parse(origin)
	if err != nil {
		return nil
	}
	if _, has := u.User.Password(); has {
		return errors.New("the URL carries a password; AsmAI handles no tokens, so use a URL without one and let git's credential helper or an SSH key supply it")
	}
	return nil
}

// DeriveName names a repository from its origin: the last part of its path
// without ".git", such as otman for git@github.com:me/otman.git. It returns
// "" when nothing usable is left.
func DeriveName(origin string) string {
	name := strings.TrimRight(origin, "/")
	if i := strings.LastIndexAny(name, "/:"); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimSuffix(name, ".git")
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return strings.TrimLeft(b.String(), "-_")
}

// Clone makes AsmAI's clone of origin at dest, which must not exist, and
// returns the repository's default branch, as origin names it. A failed
// clone leaves nothing at dest.
func Clone(ctx context.Context, origin, dest string) (defaultBranch string, err error) {
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			os.RemoveAll(dest)
		}
	}()
	if _, err := git(ctx, "", "clone", "--quiet", "--", origin, dest); err != nil {
		return "", fmt.Errorf("cloning %s: %w", origin, err)
	}
	head, err := git(ctx, dest, "symbolic-ref", "--short", "refs/remotes/origin/HEAD")
	if err != nil {
		return "", fmt.Errorf("%s has no default branch: it probably has no commits yet; push one to it and register it again", origin)
	}
	return strings.TrimPrefix(head, "origin/"), nil
}

// inherited are the variables that would point git at some other repository
// than the one it is asked about.
var inherited = []string{"GIT_DIR=", "GIT_WORK_TREE=", "GIT_INDEX_FILE=", "GIT_COMMON_DIR=", "GIT_PREFIX="}

// git runs git in dir, or where it is when dir is empty, and returns what it
// printed on stdout without its final newline.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	out, _, err := gitStatus(ctx, dir, args...)
	return out, err
}

// gitStatus is git, which also returns the exit status of a git that ran and
// failed, so that a caller can tell what a command answered with its status,
// such as merge-base --is-ancestor, from a git that could not run.
func gitStatus(ctx context.Context, dir string, args ...string) (stdout string, exit int, err error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	for _, kv := range os.Environ() {
		keep := true
		for _, prefix := range inherited {
			keep = keep && !strings.HasPrefix(kv, prefix)
		}
		if keep {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0")
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return "", -1, errors.New("git is not installed, or not on the daemon's PATH")
		}
		if ctx.Err() != nil {
			return "", -1, ctx.Err()
		}
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		exit = -1
		var failed *exec.ExitError
		if errors.As(err, &failed) {
			exit = failed.ExitCode()
		}
		return "", exit, errors.New(strings.ReplaceAll(message, "\n", "; "))
	}
	return strings.TrimSuffix(out.String(), "\n"), 0, nil
}
