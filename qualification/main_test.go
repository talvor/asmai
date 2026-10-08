// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/statedir"
)

func TestOnlyCasesTheHarnessHasCanBeAskedFor(t *testing.T) {
	for value, want := range map[string][]string{"": nil, "c3": {"C3"}, "C3, c4": {"C3", "C4"}} {
		got, err := parseCases(value)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("parseCases(%q) = %q, %v, want %q", value, got, err, want)
		}
	}
	if _, err := parseCases("C3,C99"); err == nil || !strings.Contains(err.Error(), `no case "C99"`) || !strings.Contains(err.Error(), "C3, C4") {
		t.Errorf("parseCases(C3,C99) = %v, want an error naming the cases there are", err)
	}
}

func TestTheHarnessFirstCasesAreC3AndC4(t *testing.T) {
	var ids []string
	for _, c := range Cases {
		ids = append(ids, c.ID)
	}
	if want := []string{"C3", "C4"}; !slices.Equal(ids, want) {
		t.Errorf("the harness runs %q, want %q: later cases belong to later tickets", ids, want)
	}
}

func TestRunRefusesInHostedCIBeforeDoingAnything(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "true")
	var stdout, stderr bytes.Buffer

	code := run(context.Background(), nil, &stdout, &stderr)

	if code != 2 || !strings.Contains(stderr.String(), "never runs in hosted CI") || stdout.Len() != 0 {
		t.Errorf("run exited %d printing %q and %q, want 2 and the refusal", code, stdout.String(), stderr.String())
	}
}

func TestRunRefusesAnUnknownCaseAndStrayArguments(t *testing.T) {
	for _, args := range [][]string{{"--cases", "C99"}, {"extra"}, {"--nonsense"}} {
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), args, &stdout, &stderr); code != 2 {
			t.Errorf("run %q exited %d, want 2", args, code)
		}
	}
}

// The harness never runs in a factory that is already running, which was not
// started with the harness's environment and is not the harness's to stop.
func TestTheFactoryMustBeConfiguredAndNotRunning(t *testing.T) {
	home, err := os.MkdirTemp("", "home")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("HOME", home)
	paths := statedir.At(filepath.Join(home, ".local", "state", "asmai"))

	if err := checkFactory(paths); err == nil || !strings.Contains(err.Error(), "config.toml is missing") {
		t.Errorf("a factory with no configuration was accepted: %v", err)
	}

	h := harness(t, signedIn, startsDirectly)
	if err := checkFactory(h.Paths); err != nil {
		t.Fatalf("a configured factory that is not running was refused: %v", err)
	}
	f := NewFactory(h.Subject.Asmai, h.Paths, h.Env)
	if _, _, err := f.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := checkFactory(h.Paths); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Errorf("a running factory was accepted: %v", err)
	}
	if _, err := daemon.Call(h.Paths.Socket, daemon.Request{Command: daemon.CommandStatus}); err != nil {
		t.Errorf("checking the factory stopped it: %v", err)
	}
}

func TestACommitIsBuiltFromExactlyItsOwnFiles(t *testing.T) {
	repo := t.TempDir()
	gitIn := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	gitIn("init", "-q")
	if err := os.MkdirAll(filepath.Join(repo, "cmd", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{"cmd/x/main.go": "package main\n", "pins.json": "{}"} {
		if err := os.WriteFile(filepath.Join(repo, path), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(repo, "cmd", "x", "main.go"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn("add", ".")
	gitIn("commit", "-q", "-m", "one")
	committed := gitIn("rev-parse", "HEAD")
	// What the working tree holds beyond the commit is not under test.
	if err := os.WriteFile(filepath.Join(repo, "cmd", "x", "main.go"), []byte("package changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "untracked.go"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	root, commit, err := resolveCommit(context.Background(), filepath.Join(repo, "cmd"), "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if resolved, _ := filepath.EvalSymlinks(root); resolved != mustEval(t, repo) || commit != committed {
		t.Errorf("resolveCommit = %s, %s, want %s, %s", root, commit, repo, committed)
	}
	if _, _, err := resolveCommit(context.Background(), repo, "no-such-revision"); err == nil {
		t.Error("a revision that does not exist was resolved")
	}
	if _, _, err := resolveCommit(context.Background(), t.TempDir(), "HEAD"); err == nil {
		t.Error("a directory outside any repository was resolved")
	}

	dir := filepath.Join(t.TempDir(), "src")
	if err := export(context.Background(), root, commit, dir); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "cmd", "x", "main.go")); string(got) != "package main\n" {
		t.Errorf("the exported main.go holds %q, want the committed file", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "untracked.go")); err == nil {
		t.Error("the export holds a file the commit does not")
	}
	if info, err := os.Stat(filepath.Join(dir, "cmd", "x", "main.go")); err != nil || info.Mode().Perm()&0o100 == 0 {
		t.Errorf("the exported main.go lost its executable bit: %v", err)
	}
}

func mustEval(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func TestBuildRefusesACommitWithoutAValidPinsFile(t *testing.T) {
	repo := t.TempDir()
	gitIn := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	gitIn("init", "-q")
	gitIn("commit", "-q", "--allow-empty", "-m", "empty")

	_, commit, err := resolveCommit(context.Background(), repo, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := build(context.Background(), repo, commit, t.TempDir()); err == nil || !strings.Contains(err.Error(), "no pins file") {
		t.Errorf("building a commit with no pins file = %v, want a refusal naming it", err)
	}
}

// The harness never runs in hosted CI: no workflow builds or runs it.
func TestNoWorkflowRunsTheHarness(t *testing.T) {
	files, err := filepath.Glob("../.github/workflows/*")
	if err != nil || len(files) == 0 {
		t.Fatalf("finding the workflows: %v, %v", files, err)
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"qualification", "qualify"} {
			// A workflow may mention qualification in prose, such as a release
			// workflow checking a qualification record, but never the harness's
			// directory, binary or make target.
			for line := range strings.Lines(string(data)) {
				if strings.Contains(line, "./"+forbidden) || strings.Contains(line, "bin/"+forbidden) || strings.Contains(line, "make "+forbidden) {
					t.Errorf("%s runs the qualification harness: %q", file, strings.TrimSpace(line))
				}
			}
		}
	}
}
