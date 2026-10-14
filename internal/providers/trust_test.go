// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestClaudeCodesRecordIsInTheHomeUnlessItsConfigDirectoryIsSet(t *testing.T) {
	none := func(string) string { return "" }
	if got, want := ClaudeCodeRecord("/home/u", none), "/home/u/.claude.json"; got != want {
		t.Errorf("the record is %s, want %s", got, want)
	}
	set := func(name string) string {
		if name == "CLAUDE_CONFIG_DIR" {
			return "/elsewhere"
		}
		return ""
	}
	if got, want := ClaudeCodeRecord("/home/u", set), "/elsewhere/.claude.json"; got != want {
		t.Errorf("with CLAUDE_CONFIG_DIR set, the record is %s, want %s", got, want)
	}
}

// Trust given to a directory covers every directory below it, as on the
// pinned Claude Code, and nothing beside or above it.
func TestATrustedDirectoryCoversTheDirectoriesBelowIt(t *testing.T) {
	root := t.TempDir()
	agents := filepath.Join(root, "agents")
	record := filepath.Join(root, ".claude.json")
	data := []byte(`{"numStartups": 3, "projects": {"` + agents + `": {"allowedTools": [], "hasTrustDialogAccepted": true}, "` + filepath.Join(root, "views") + `": {"hasTrustDialogAccepted": false}}}`)
	if err := os.WriteFile(record, data, 0o600); err != nil {
		t.Fatal(err)
	}
	for dir, want := range map[string]bool{
		agents: true,
		filepath.Join(agents, "leader@engineering"): true,
		filepath.Join(agents, "a", "b"):             true,
		filepath.Join(root, "workspaces"):           false,
		filepath.Join(root, "views"):                false,
		filepath.Join(root, "agents-other"):         false,
		root:                                        false,
	} {
		got, err := Trusted(record, dir)
		if err != nil || got != want {
			t.Errorf("Trusted(%s) = %t, %v, want %t", dir, got, err, want)
		}
	}
	if after, _ := os.ReadFile(record); !bytes.Equal(after, data) {
		t.Error("reading the record changed it")
	}
}

// Claude Code records the directory it runs in with its symbolic links
// resolved, so trust recorded there covers the directory as given.
func TestTrustRecordedAtTheResolvedPathCoversTheDirectory(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.MkdirAll(filepath.Join(real, "agents"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(real, "agents"))
	if err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(root, ".claude.json")
	if err := os.WriteFile(record, []byte(`{"projects": {"`+resolved+`": {"hasTrustDialogAccepted": true}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, err := Trusted(record, filepath.Join(link, "agents")); err != nil || !ok {
		t.Errorf("Trusted through the link = %t, %v, want trusted", ok, err)
	}
}

func TestAMissingRecordTrustsNothingAndABrokenOneIsAnError(t *testing.T) {
	root := t.TempDir()
	if ok, err := Trusted(filepath.Join(root, ".claude.json"), root); ok || err != nil {
		t.Errorf("with no record, Trusted = %t, %v, want untrusted and no error", ok, err)
	}
	record := filepath.Join(root, ".claude.json")
	if err := os.WriteFile(record, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Trusted(record, root); err == nil {
		t.Error("a record that is not JSON was read without an error")
	}
}

// git runs git in dir with none of the user's configuration.
func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@example.test", "GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@example.test")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// writeRecord writes a record of Claude Code's trusting each of dirs, and
// returns its path.
func writeRecord(t *testing.T, dirs ...string) string {
	t.Helper()
	var entries []string
	for _, dir := range dirs {
		entries = append(entries, `"`+dir+`": {"hasTrustDialogAccepted": true}`)
	}
	record := filepath.Join(t.TempDir(), ".claude.json")
	if err := os.WriteFile(record, []byte(`{"projects": {`+strings.Join(entries, ", ")+`}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return record
}

// A worker's workspace is a worktree of AsmAI's clone, made as the daemon
// makes it: Claude Code looks for trust at the clone, the main repository's
// root, and at a plain folder's own path otherwise.
func TestTheFolderClaudeCodeLooksForTrustAtIsAWorktreesMainRepository(t *testing.T) {
	root := t.TempDir()
	clone := filepath.Join(root, "repositories", "app")
	git(t, root, "init", "--quiet", "-b", "main", clone)
	git(t, clone, "commit", "--quiet", "--allow-empty", "-m", "initial")
	worktree := filepath.Join(root, "workspaces", "job-1", "assignment-1", "repo")
	git(t, clone, "worktree", "add", "--quiet", "-b", "job-1", worktree)
	plain := filepath.Join(root, "agents", "leader@engineering")
	if err := os.MkdirAll(plain, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(worktree, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		dir, folder string
		repository  bool
	}{
		{worktree, clone, true},
		{filepath.Join(worktree, "src"), clone, true},
		{clone, clone, true},
		{plain, plain, false},
	} {
		folder, repository := TrustFolder(tc.dir)
		resolved, _ := filepath.EvalSymlinks(folder)
		want, _ := filepath.EvalSymlinks(tc.folder)
		if resolved != want || repository != tc.repository {
			t.Errorf("TrustFolder(%s) = %s, %t, want %s, %t", tc.dir, folder, repository, tc.folder, tc.repository)
		}
	}
}

// On the pinned Claude Code, trust of a folder above a repository does not
// cover it or its worktrees, and trust of the repository covers every
// worktree of it, wherever it is.
func TestOnlyTheClonesOwnTrustCoversItsWorktrees(t *testing.T) {
	root := t.TempDir()
	clone := filepath.Join(root, "repositories", "app")
	git(t, root, "init", "--quiet", "-b", "main", clone)
	git(t, clone, "commit", "--quiet", "--allow-empty", "-m", "initial")
	workspaces := filepath.Join(root, "workspaces")
	worktree := filepath.Join(workspaces, "job-1", "assignment-1", "repo")
	git(t, clone, "worktree", "add", "--quiet", "-b", "job-1", worktree)
	plain := filepath.Join(workspaces, "job-1", "assignment-1", "tmp")
	if err := os.MkdirAll(plain, 0o700); err != nil {
		t.Fatal(err)
	}

	parents := writeRecord(t, workspaces, filepath.Join(root, "repositories"), root)
	for dir, want := range map[string]bool{worktree: false, clone: false, plain: true} {
		if got, err := Trusted(parents, dir); err != nil || got != want {
			t.Errorf("with only folders above trusted, Trusted(%s) = %t, %v, want %t", dir, got, err, want)
		}
	}
	// Claude Code records the clone by its resolved path.
	resolved, err := filepath.EvalSymlinks(clone)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range []string{writeRecord(t, clone), writeRecord(t, resolved)} {
		for _, dir := range []string{worktree, clone} {
			if got, err := Trusted(record, dir); err != nil || !got {
				t.Errorf("with the clone trusted, Trusted(%s) = %t, %v, want trusted", dir, got, err)
			}
		}
	}
	// A worktree's own entry is not where Claude Code records its trust.
	if got, err := Trusted(writeRecord(t, worktree), worktree); err != nil || got {
		t.Errorf("with only the worktree's own path trusted, Trusted(%s) = %t, %v, want untrusted", worktree, got, err)
	}
}
