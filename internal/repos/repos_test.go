// SPDX-License-Identifier: Apache-2.0

package repos

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// run runs git in dir with an identity and no configuration of the host's,
// and returns what it printed.
func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	// Tests may themselves run in a git hook, whose variables point at the
	// repository being hooked.
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env,
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@example.test", "GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@example.test")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// realPath resolves symbolic links, as git reports a checkout's root.
func realPath(t *testing.T, path string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return real
}

// origin makes a bare repository with one commit on branch, standing in for
// a remote, and returns its path.
func origin(t *testing.T, branch string) string {
	t.Helper()
	dir := realPath(t, t.TempDir())
	bare := filepath.Join(dir, "remote.git")
	run(t, dir, "init", "--bare", "-b", branch, bare)
	seed := filepath.Join(dir, "seed")
	run(t, dir, "init", "-b", branch, seed)
	run(t, seed, "commit", "--allow-empty", "-m", "first")
	run(t, seed, "push", bare, branch)
	return bare
}

func TestACheckoutIsResolvedToItsRootAndItsOrigin(t *testing.T) {
	remote := origin(t, "trunk")
	checkout := filepath.Join(realPath(t, t.TempDir()), "work")
	run(t, filepath.Dir(checkout), "clone", "--quiet", remote, checkout)
	if err := os.Mkdir(filepath.Join(checkout, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	for _, arg := range []string{checkout, filepath.Join(checkout, "sub")} {
		src, err := Resolve(context.Background(), arg)
		if err != nil || src.Location != checkout || src.Origin != remote {
			t.Errorf("Resolve(%s) = %+v, %v; want the checkout's root %s and origin %s", arg, src, err, checkout, remote)
		}
	}
}

func TestARelativeOriginIsReadFromTheCheckoutsRoot(t *testing.T) {
	dir := realPath(t, t.TempDir())
	run(t, dir, "init", "--bare", "-b", "main", "remote.git")
	run(t, dir, "init", "-b", "main", "work")
	run(t, filepath.Join(dir, "work"), "remote", "add", "origin", "../remote.git")

	src, err := Resolve(context.Background(), filepath.Join(dir, "work"))
	if err != nil || src.Origin != filepath.Join(dir, "remote.git") {
		t.Errorf("Resolve = %+v, %v; want the origin as a path from the checkout", src, err)
	}
}

func TestABareRepositoryOrAUrlIsItsOwnOriginWithNoLocation(t *testing.T) {
	remote := origin(t, "main")
	for _, arg := range []string{remote, "https://example.test/me/repo.git", "ssh://git@example.test/me/repo.git", "git@example.test:me/repo.git", "file://" + remote} {
		src, err := Resolve(context.Background(), arg)
		if err != nil || src.Location != "" || (src.Origin != arg) {
			t.Errorf("Resolve(%s) = %+v, %v; want it as the origin, with no location", arg, src, err)
		}
	}
}

func TestWhatCannotBeRegisteredIsRefusedWithWhy(t *testing.T) {
	dir := realPath(t, t.TempDir())
	notRepo := filepath.Join(dir, "plain")
	if err := os.Mkdir(notRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	noOrigin := filepath.Join(dir, "noorigin")
	run(t, dir, "init", "-b", "main", noOrigin)
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	withPassword := filepath.Join(dir, "password")
	run(t, dir, "init", "-b", "main", withPassword)
	run(t, withPassword, "remote", "add", "origin", "https://me:hunter2@example.test/r.git")

	for name, tc := range map[string]struct{ arg, want string }{
		"empty":                    {"", "name a local checkout or a URL"},
		"a missing directory":      {filepath.Join(dir, "missing"), "does not look like a URL"},
		"a file":                   {file, "is a file"},
		"not a repository":         {notRepo, "is not a git repository"},
		"no origin":                {noOrigin, "has no remote named origin"},
		"a password in a URL":      {"https://me:hunter2@example.test/r.git", "carries a password"},
		"a password in the origin": {withPassword, "carries a password"},
		"an option":                {"--upload-pack=touch /tmp/x", "does not look like a URL"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Resolve(context.Background(), tc.arg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Resolve(%q) returned %v, want an error saying %q", tc.arg, err, tc.want)
			}
			if err != nil && strings.Contains(err.Error(), "hunter2") {
				t.Errorf("the error repeats the password: %v", err)
			}
		})
	}
}

func TestANameIsMadeFromTheOrigin(t *testing.T) {
	for origin, want := range map[string]string{
		"git@github.com:me/otman.git":      "otman",
		"https://github.com/me/otman":      "otman",
		"https://github.com/me/otman.git/": "otman",
		"/tmp/x/fixture.git":               "fixture",
		"ssh://git@host:2222/srv/my.repo":  "my-repo",
		"/tmp/x/.hidden":                   "hidden",
		"":                                 "",
	} {
		if got := DeriveName(origin); got != want {
			t.Errorf("DeriveName(%q) = %q, want %q", origin, got, want)
		}
	}
}

func TestACloneIsFetchedFromOriginAndNamesOriginsDefaultBranch(t *testing.T) {
	remote := origin(t, "trunk")
	dest := filepath.Join(realPath(t, t.TempDir()), "repositories", "fixture")

	branch, err := Clone(context.Background(), remote, dest)
	if err != nil || branch != "trunk" {
		t.Fatalf("Clone = %q, %v; want trunk", branch, err)
	}
	if got, want := run(t, dest, "rev-parse", "HEAD"), run(t, remote, "rev-parse", "trunk"); got != want {
		t.Errorf("the clone is at %s, want origin's %s", got, want)
	}
	if got := run(t, dest, "config", "--get", "remote.origin.url"); got != remote {
		t.Errorf("the clone's origin is %s, want %s", got, remote)
	}
}

func TestAFailedCloneLeavesNothing(t *testing.T) {
	empty := filepath.Join(realPath(t, t.TempDir()), "empty.git")
	run(t, filepath.Dir(empty), "init", "--bare", "-b", "main", empty)
	for name, tc := range map[string]struct{ origin, want string }{
		"an origin with no commits": {empty, "no default branch"},
		"no origin at all":          {filepath.Join(t.TempDir(), "missing.git"), "cloning"},
	} {
		t.Run(name, func(t *testing.T) {
			dest := filepath.Join(realPath(t, t.TempDir()), "clone")
			_, err := Clone(context.Background(), tc.origin, dest)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Clone returned %v, want an error saying %q", err, tc.want)
			}
			if _, statErr := os.Stat(dest); statErr == nil {
				t.Error("the failed clone left a directory")
			}
		})
	}
}
