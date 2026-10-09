// SPDX-License-Identifier: Apache-2.0

package githooks

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var trailers = Trailers(func() ([]string, error) {
	return []string{"AsmAI-Job: 4", "AsmAI-Agent: worker1@engineering", "AsmAI-Dispatch: 9"}, nil
})

// repo makes a repository and enters it, as git does for a hook.
func repo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	for name, value := range map[string]string{"GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_SYSTEM": "/dev/null"} {
		t.Setenv(name, value)
	}
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	return dir
}

func message(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func run(t *testing.T, name, own string, tr Trailers, args ...string) (code int, stderr string) {
	t.Helper()
	var errOut bytes.Buffer
	code = Run(context.Background(), name, args, strings.NewReader(""), &bytes.Buffer{}, &errOut, own, tr)
	return code, errOut.String()
}

func TestTheCommitMessageGetsTheThreeTrailersOnceWhateverTheNumberOfTimesItIsHooked(t *testing.T) {
	repo(t)
	file := message(t, "Add a greeting\n\nIt says hello.\n")
	for range 2 {
		if code, stderr := run(t, CommitMessage, "/own", trailers, file); code != 0 {
			t.Fatalf("the hook exited %d: %s", code, stderr)
		}
	}
	got, _ := os.ReadFile(file)
	want := "Add a greeting\n\nIt says hello.\n\nAsmAI-Job: 4\nAsmAI-Agent: worker1@engineering\nAsmAI-Dispatch: 9\n"
	if string(got) != want {
		t.Errorf("the message is\n%s\nwant\n%s", got, want)
	}
}

func TestACommitThatCannotCarryItsTrailersIsRefusedAndLeftAlone(t *testing.T) {
	repo(t)
	file := message(t, "Add a greeting\n")
	code, stderr := run(t, CommitMessage, "/own", func() ([]string, error) { return nil, errors.New("no current dispatch") }, file)
	if code != 1 || !strings.Contains(stderr, "cannot carry the AsmAI trailers: no current dispatch") {
		t.Errorf("the hook exited %d saying %q, want it to refuse the commit and say why", code, stderr)
	}
	if got, _ := os.ReadFile(file); string(got) != "Add a greeting\n" {
		t.Errorf("the message changed to %q", got)
	}
}

func executable(t *testing.T, path, script string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestTheRepositorysOwnHookStillRunsAfterTheTrailersAndDecidesTheStatus(t *testing.T) {
	dir := repo(t)
	record := filepath.Join(t.TempDir(), "seen")
	executable(t, filepath.Join(dir, ".git", "hooks", CommitMessage), `cp "$1" `+record+"\nexit 7\n")
	file := message(t, "Add a greeting\n")
	if code, _ := run(t, CommitMessage, "/own", trailers, file); code != 7 {
		t.Errorf("the hook exited %d, want the repository's own status, 7", code)
	}
	if seen, _ := os.ReadFile(record); !strings.Contains(string(seen), "AsmAI-Dispatch: 9") {
		t.Errorf("the repository's hook saw %q, want the message with the trailers", seen)
	}
}

func TestAHooksDirectoryTheRepositorySetsIsFoundEvenThoughAsmAIsIsInTheEnvironment(t *testing.T) {
	dir := repo(t)
	own := filepath.Join(t.TempDir(), "githooks")
	if err := Install(own, "/state/bin/asmai"); err != nil {
		t.Fatal(err)
	}
	for k, v := range Environment(os.Environ(), own) {
		t.Setenv(k, v)
	}
	if out, err := exec.Command("git", "config", "core.hooksPath", ".husky").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	record := filepath.Join(t.TempDir(), "args")
	executable(t, filepath.Join(dir, ".husky", "pre-push"), `echo "$@" > `+record+"; cat >> "+record+"\n")
	var errOut bytes.Buffer
	code := Run(context.Background(), "pre-push", []string{"origin", "url"}, strings.NewReader("refs\n"), &bytes.Buffer{}, &errOut, own, trailers)
	if seen, _ := os.ReadFile(record); code != 0 || string(seen) != "origin url\nrefs\n" {
		t.Errorf("pre-push exited %d (%s) and its repository hook saw %q, want it run with git's arguments and input", code, errOut.String(), seen)
	}
	// With no hook of the name anywhere, there is nothing to run.
	if code, stderr := run(t, "post-commit", own, trailers); code != 0 {
		t.Errorf("a hook with no repository hook exited %d: %s", code, stderr)
	}
}

func TestEveryClientHookForwardsToAsmai(t *testing.T) {
	own := filepath.Join(t.TempDir(), "githooks")
	if err := Install(own, "/state/it's/bin/asmai"); err != nil {
		t.Fatal(err)
	}
	for _, name := range Names {
		data, err := os.ReadFile(filepath.Join(own, name))
		info, statErr := os.Stat(filepath.Join(own, name))
		want := "#!/bin/sh\nexec '/state/it'\\''s/bin/asmai' git-hook " + name + " \"$@\"\n"
		if err != nil || statErr != nil || string(data) != want || info.Mode().Perm() != 0o755 {
			t.Errorf("hook %s is %q (%v, %v), want %q, executable", name, data, err, statErr, want)
		}
	}
	entries, _ := os.ReadDir(own)
	if len(entries) != len(Names) {
		t.Errorf("the hooks directory holds %d files, want %d", len(entries), len(Names))
	}
}

func TestTheHooksDirectoryIsAddedAfterTheConfigurationAlreadyInTheEnvironment(t *testing.T) {
	env := Environment([]string{"HOME=/h", "GIT_CONFIG_COUNT=2", "GIT_CONFIG_KEY_0=a.b", "GIT_CONFIG_VALUE_0=c"}, "/state/githooks")
	if env["GIT_CONFIG_COUNT"] != "3" || env["GIT_CONFIG_KEY_2"] != "core.hooksPath" || env["GIT_CONFIG_VALUE_2"] != "/state/githooks" || len(env) != 3 {
		t.Errorf("the variables are %v, want a third entry for core.hooksPath", env)
	}
	if env := Environment(nil, "/d"); env["GIT_CONFIG_COUNT"] != "1" || env["GIT_CONFIG_KEY_0"] != "core.hooksPath" {
		t.Errorf("with none before, the variables are %v", env)
	}
}
