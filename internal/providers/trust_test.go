// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// withRereads has Trusted reread a record that does not parse tries times in
// all, pause apart, for the rest of the test.
func withRereads(t *testing.T, tries int, pause time.Duration) {
	t.Helper()
	oldTries, oldPause := recordTries, recordPause
	recordTries, recordPause = tries, pause
	t.Cleanup(func() { recordTries, recordPause = oldTries, oldPause })
}

// A record read while Claude Code, or anything else, writes it in place can
// be empty: Trusted reads it again before it counts it as broken.
func TestARecordReadWhileItIsWrittenIsReadAgain(t *testing.T) {
	root := t.TempDir()
	record := filepath.Join(root, ".claude.json")
	if err := os.WriteFile(record, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	written := make(chan error, 1)
	go func() {
		time.Sleep(30 * time.Millisecond)
		written <- os.WriteFile(record, []byte(`{"projects": {"`+root+`": {"hasTrustDialogAccepted": true}}}`), 0o600)
	}()
	if ok, err := Trusted(record, root); err != nil || !ok {
		t.Errorf("with the record written 30 ms after an empty read, Trusted = %t, %v, want trusted", ok, err)
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
}

// A record that is still cut short after Trusted has read it again is broken.
func TestARecordThatStaysCutShortIsAnError(t *testing.T) {
	withRereads(t, 5, time.Millisecond)
	root := t.TempDir()
	record := filepath.Join(root, ".claude.json")
	if err := os.WriteFile(record, []byte(`{"projects": {"`+root+`": {"hasTr`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Trusted(record, root); err == nil || !strings.Contains(err.Error(), "is not Claude Code's JSON record") {
		t.Errorf("with the record cut short, Trusted's error is %v, want that it is not Claude Code's JSON record", err)
	}
}

// While the record is truncated and written again in pieces, over and over,
// every read of it still finds the folder trusted.
func TestARecordRewrittenInPlaceIsNeverReadBroken(t *testing.T) {
	withRereads(t, 5, 5*time.Millisecond)
	root := t.TempDir()
	record := filepath.Join(root, ".claude.json")
	data := []byte(`{"projects": {"` + root + `": {"hasTrustDialogAccepted": true}}}`)
	if err := os.WriteFile(record, data, 0o600); err != nil {
		t.Fatal(err)
	}
	stop, stopped := make(chan struct{}), make(chan error, 1)
	go func() {
		for {
			select {
			case <-stop:
				stopped <- nil
				return
			default:
			}
			f, err := os.OpenFile(record, os.O_WRONLY|os.O_TRUNC, 0o600)
			if err != nil {
				stopped <- err
				return
			}
			_, err = f.Write(data[:len(data)/2])
			time.Sleep(100 * time.Microsecond)
			if err == nil {
				_, err = f.Write(data[len(data)/2:])
			}
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				stopped <- err
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	for i := 0; i < 1000; i++ {
		if ok, err := Trusted(record, root); err != nil || !ok {
			t.Errorf("read %d: Trusted = %t, %v, want trusted", i, ok, err)
			break
		}
	}
	close(stop)
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
}
