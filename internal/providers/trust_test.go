// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"bytes"
	"os"
	"path/filepath"
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
