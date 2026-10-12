// SPDX-License-Identifier: Apache-2.0

package githooks

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

var trailers = Trailers(func() ([]string, error) {
	return []string{"AsmAI-Job: 4", "AsmAI-Agent: worker1@engineering", "AsmAI-Dispatch: 9"}, nil
})

// repo makes a repository and enters it, as git does for a hook, with a
// worker's temporary directory beside it.
func repo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("TMPDIR", t.TempDir())
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
	out, err := exec.Command("git", "rev-parse", "--git-path", "COMMIT_EDITMSG").Output()
	if err != nil {
		t.Fatal(err)
	}
	path := strings.TrimSpace(string(out))
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
		if code, stderr := run(t, PrepareCommitMessage, "/own", trailers, file); code != 0 {
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
	code, stderr := run(t, PrepareCommitMessage, "/own", func() ([]string, error) { return nil, errors.New("no current dispatch") }, file)
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

func TestTheRepositorysOwnPrepareHookRunsBeforeTheTrailers(t *testing.T) {
	dir := repo(t)
	record := filepath.Join(dir, "seen")
	executable(t, filepath.Join(dir, ".git", "hooks", PrepareCommitMessage), `cp "$1" `+record+"\nprintf 'Repository detail\\n' >> \"$1\"\n")
	file := message(t, "Add a greeting\n")
	if code, stderr := run(t, PrepareCommitMessage, "/own", trailers, file); code != 0 {
		t.Errorf("the hook exited %d: %s", code, stderr)
	}
	if seen, _ := os.ReadFile(record); string(seen) != "Add a greeting\n" {
		t.Errorf("the repository's hook saw %q, want the original message", seen)
	}
	if got, _ := os.ReadFile(file); !strings.Contains(string(got), "Repository detail") || !strings.Contains(string(got), "AsmAI-Dispatch: 9") {
		t.Errorf("the prepared message is %q, want the repository's edit and AsmAI trailers", got)
	}
}

func TestTheRepositorysHookStillDecidesTheStatus(t *testing.T) {
	dir := repo(t)
	executable(t, filepath.Join(dir, ".git", "hooks", PrepareCommitMessage), "exit 7\n")
	file := message(t, "Add a greeting\n")
	if code, _ := run(t, PrepareCommitMessage, "/own", trailers, file); code != 7 {
		t.Errorf("the hook exited %d, want the repository's own status, 7", code)
	}
	if got, _ := os.ReadFile(file); string(got) != "Add a greeting\n" {
		t.Errorf("the rejected message changed to %q", got)
	}
}

func TestRunRejectsHookNamesOutsideTheInstalledSet(t *testing.T) {
	dir := repo(t)
	record := filepath.Join(dir, "seen")
	executable(t, filepath.Join(dir, ".git", "outside"), "touch "+record+"\n")
	if code, _ := run(t, "../outside", "/own", trailers); code != 2 {
		t.Errorf("unknown hook exited %d, want 2", code)
	}
	if _, err := os.Stat(record); !os.IsNotExist(err) {
		t.Errorf("unknown hook ran the outside executable: %v", err)
	}
}

func TestRunRejectsMessagePathsOtherThanGitsOwn(t *testing.T) {
	dir := repo(t)
	record := filepath.Join(dir, "seen")
	for _, name := range []string{PrepareCommitMessage, CommitMessage} {
		executable(t, filepath.Join(dir, ".git", "hooks", name), "touch "+record+"\n")
	}
	outside := filepath.Join(t.TempDir(), "message")
	if err := os.WriteFile(outside, []byte("Leave alone\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{PrepareCommitMessage, CommitMessage} {
		if code, _ := run(t, name, "/own", trailers, outside); code != 2 {
			t.Errorf("%s with an outside message exited %d, want 2", name, code)
		}
	}
	if got, err := os.ReadFile(outside); err != nil || string(got) != "Leave alone\n" {
		t.Errorf("outside message is %q (%v)", got, err)
	}
	if _, err := os.Stat(record); !os.IsNotExist(err) {
		t.Errorf("repository hook ran on an invalid message: %v", err)
	}
	path := message(t, "Original\n")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if code, _ := run(t, PrepareCommitMessage, "/own", trailers, path); code != 2 {
		t.Errorf("symlinked message exited %d, want 2", code)
	}
}

func TestMergeMessageGetsTrailersFromBothMessageHooks(t *testing.T) {
	repo(t)
	out, err := exec.Command("git", "rev-parse", "--git-path", "MERGE_MSG").Output()
	if err != nil {
		t.Fatal(err)
	}
	file := strings.TrimSpace(string(out))
	if err := os.WriteFile(file, []byte("Merge side\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{PrepareCommitMessage, CommitMessage} {
		if code, stderr := run(t, name, "/own", trailers, file); code != 0 {
			t.Fatalf("%s exited %d: %s", name, code, stderr)
		}
	}
	want := "Merge side\n\nAsmAI-Job: 4\nAsmAI-Agent: worker1@engineering\nAsmAI-Dispatch: 9\n"
	if got, err := os.ReadFile(file); err != nil || string(got) != want {
		t.Errorf("merge message is %q (%v), want %q", got, err, want)
	}
}

func TestTheTrailersAreAddedAgainAfterTheRepositorysCommitMsgRewritesTheMessage(t *testing.T) {
	dir := repo(t)
	executable(t, filepath.Join(dir, ".git", "hooks", CommitMessage), "printf 'Rewritten by the repository\\n' > \"$1\"\n")
	file := message(t, "Merge branch 'side'\n\nAsmAI-Job: 4\n")
	if code, stderr := run(t, CommitMessage, "/own", trailers, file); code != 0 {
		t.Fatalf("commit-msg exited %d: %s", code, stderr)
	}
	want := "Rewritten by the repository\n\nAsmAI-Job: 4\nAsmAI-Agent: worker1@engineering\nAsmAI-Dispatch: 9\n"
	if got, _ := os.ReadFile(file); string(got) != want {
		t.Errorf("the message is %q, want the repository's rewrite with the trailers %q", got, want)
	}
}

func TestPostCommitRestoresTrailersAfterTheRepositoryRewritesTheMessage(t *testing.T) {
	dir := repo(t)
	for _, args := range [][]string{
		{"config", "user.name", "Una User"},
		{"config", "user.email", "una@example.test"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "greeting.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "greeting.txt"}, {"-c", "core.hooksPath=/dev/null", "commit", "-q", "-m", "Add greeting"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	executable(t, filepath.Join(dir, ".git", "hooks", "post-commit"), "git -c core.hooksPath=/dev/null commit --amend --no-verify -q -m 'Stripped by repository hook'\n")
	if code, stderr := run(t, "post-commit", "/own", trailers); code != 0 {
		t.Fatalf("post-commit exited %d: %s", code, stderr)
	}
	got, err := exec.Command("git", "log", "-1", "--format=%(trailers:only,unfold)").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"AsmAI-Job: 4", "AsmAI-Agent: worker1@engineering", "AsmAI-Dispatch: 9"} {
		if !strings.Contains("\n"+string(got), "\n"+line+"\n") {
			t.Errorf("the final commit has trailers %q, missing %q", got, line)
		}
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
	record := filepath.Join(dir, "args")
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

func TestAnInheritedHooksPathSurvivesAsmAIsEnvironmentOverride(t *testing.T) {
	dir := repo(t)
	userHooks := filepath.Join(dir, "user-hooks")
	own := filepath.Join(t.TempDir(), "githooks")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.hooksPath")
	t.Setenv("GIT_CONFIG_VALUE_0", userHooks)
	for key, value := range Environment(os.Environ(), own) {
		t.Setenv(key, value)
	}
	record := filepath.Join(dir, "seen")
	executable(t, filepath.Join(userHooks, "pre-commit"), "touch "+record+"\n")
	if code, stderr := run(t, "pre-commit", own, trailers); code != 0 {
		t.Fatalf("pre-commit exited %d: %s", code, stderr)
	}
	if _, err := os.Stat(record); err != nil {
		t.Errorf("the inherited hook did not run: %v", err)
	}
}

func TestEveryClientHookForwardsToAsmai(t *testing.T) {
	repo(t)
	own := filepath.Join(t.TempDir(), "githooks")
	executablePath := filepath.Join(t.TempDir(), "it's", "asmai")
	record := filepath.Join(t.TempDir(), "invocation")
	executable(t, executablePath, "printf '%s\\n' \"$@\" > \"$ASMAI_HOOK_RECORD\"\nexit 7\n")
	t.Setenv("ASMAI_HOOK_RECORD", record)
	if err := Install(own, executablePath); err != nil {
		t.Fatal(err)
	}
	for _, name := range Names {
		info, err := os.Stat(filepath.Join(own, name))
		if err != nil || info.Mode().Perm() != 0o755 {
			t.Errorf("hook %s is %v (%v), want executable", name, info, err)
			continue
		}
		cmd := exec.Command("git", "-c", "core.hooksPath="+own, "hook", "run", name, "--", "origin", "url")
		var exit *exec.ExitError
		if err := cmd.Run(); !errors.As(err, &exit) || exit.ExitCode() != 7 {
			t.Errorf("git hook run %s returned %v, want status 7", name, err)
		}
		want := "git-hook\n" + name + "\norigin\nurl\n"
		if got, err := os.ReadFile(record); err != nil || string(got) != want {
			t.Errorf("git hook run %s forwarded %q (%v), want %q", name, got, err, want)
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

// workspace makes a clone with a first commit and a worker's workspace, a
// worktree of it, and enters the workspace. It returns the clone's directory,
// a directory outside both, and git run with an identity. The clone and that
// directory are outside every temporary directory, which t.TempDir is not on
// Linux, as a hook may write anywhere beneath those.
func workspace(t *testing.T) (clone, outside string, git func(args ...string)) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	beyondTemp := func(prefix string) string {
		dir, err := os.MkdirTemp(wd, prefix)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(dir) })
		return dir
	}
	outside, clone = beyondTemp(".outside-"), beyondTemp(".clone-")
	repo(t)
	t.Chdir(clone)
	git = func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-c", "user.name=Una User", "-c", "user.email=una@example.test"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "-q")
	if err := os.WriteFile(filepath.Join(clone, "greeting.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "greeting.txt")
	git("commit", "-q", "-m", "Start")
	dir := filepath.Join(t.TempDir(), "repo")
	git("worktree", "add", "-q", "-b", "asmai/job-1/1", dir)
	t.Chdir(dir)
	return clone, outside, git
}

func TestARepositorysHookRunsConfinedToItsWorkspace(t *testing.T) {
	clone, outsideDir, _ := workspace(t)
	workspace, _ := os.Getwd()
	outside := filepath.Join(outsideDir, "outside")
	common := filepath.Join(clone, ".git")
	system := filepath.Join("/tmp", "asmai-hook-"+filepath.Base(outsideDir))
	t.Cleanup(func() { os.Remove(system) })
	refused := []string{outside, filepath.Join(common, "shared"), filepath.Join(common, "hooks", "planted"), filepath.Join(common, "config"), filepath.Join(common, "info", "exclude")}
	executable(t, filepath.Join(common, "hooks", "pre-commit"), `touch inside "$TMPDIR/temporary" `+system+` "$(git rev-parse --absolute-git-dir)/own"
for path in `+strings.Join(refused, " ")+`; do
	if { echo planted >> "$path"; } 2>/dev/null; then echo "$path" >> escaped; else echo "$path" >> blocked; fi
done
`)
	before := map[string][]byte{}
	for _, path := range refused {
		before[path], _ = os.ReadFile(path)
	}
	if code, stderr := run(t, "pre-commit", "/own", trailers); code != 0 {
		t.Fatalf("pre-commit exited %d: %s", code, stderr)
	}
	ownGitDir := filepath.Join(common, "worktrees", "repo")
	for _, path := range []string{filepath.Join(workspace, "inside"), filepath.Join(os.Getenv("TMPDIR"), "temporary"), system, filepath.Join(ownGitDir, "own")} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("the hook could not write %s: %v", path, err)
		}
	}
	for _, path := range refused {
		if got, _ := os.ReadFile(path); !bytes.Equal(got, before[path]) {
			t.Errorf("the hook wrote %s, outside what it may write", path)
		}
	}
	escaped, _ := os.ReadFile(filepath.Join(workspace, "escaped"))
	if blocked, _ := os.ReadFile(filepath.Join(workspace, "blocked")); string(blocked) != strings.Join(refused, "\n")+"\n" {
		t.Errorf("the hook was blocked writing %q and not %q, want it blocked writing every path it may not", blocked, escaped)
	}
}

// On Linux, Landlock cannot let a hook create packed-refs.lock, which git
// takes to delete a ref, so dropping the last stash entry, as lint-staged does
// when it is done, succeeds there but leaves refs/stash behind, as the README
// says.
func TestAHookThatStagesAndStashesLikeLintStaged(t *testing.T) {
	clone, _, git := workspace(t)
	executable(t, filepath.Join(clone, ".git", "hooks", "pre-commit"), `set -e
printf 'hello, formatted\n' > greeting.txt
git add greeting.txt
git stash push -q -m 'lint-staged backup'
git stash apply -q --index
git stash drop -q
`)
	if err := os.WriteFile("greeting.txt", []byte("hello again\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "greeting.txt")
	code, stderr := run(t, "pre-commit", "/own", trailers)
	staged, err := exec.Command("git", "show", ":greeting.txt").Output()
	if err != nil || string(staged) != "hello, formatted\n" {
		t.Errorf("the hook staged %q (%v), want its formatted file", staged, err)
	}
	stashes, err := exec.Command("git", "stash", "list", "--format=%s").Output()
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 || len(stashes) != 0 {
		t.Errorf("pre-commit exited %d (%s) leaving the stashes %q, want its stash made, applied and dropped", code, stderr, stashes)
	}
	left := exec.Command("git", "rev-parse", "--verify", "-q", "refs/stash").Run() == nil
	if runtime.GOOS == "linux" && (!left || !strings.Contains(stderr, "packed-refs.lock")) {
		t.Errorf("refs/stash left behind is %v, with %q, want it left for want of packed-refs.lock", left, stderr)
	} else if runtime.GOOS != "linux" && left {
		t.Errorf("the dropped stash left refs/stash behind")
	}
	git("commit", "-q", "--no-verify", "-m", "Greet again")
}
