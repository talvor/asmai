// SPDX-License-Identifier: Apache-2.0

package statedir

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScratchStateDirectoryOverride(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "asmai-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	dir := filepath.Join(root, "state")
	t.Setenv("ASMAI_STATE_DIR", dir)
	p, err := Default()
	if err != nil || p.Dir != dir {
		t.Fatalf("Default() = %q, %v; want %q", p.Dir, err, dir)
	}
	t.Setenv("ASMAI_STATE_DIR", "relative")
	if _, err := Default(); err == nil {
		t.Fatal("relative state directory was accepted")
	}
}

func TestMakeFoldersMakesEachMissingFolderPrivateOnce(t *testing.T) {
	p := At(filepath.Join(t.TempDir(), "state"))
	made, err := p.MakeFolders()
	if err != nil || !made {
		t.Fatalf("MakeFolders in a new state directory = %t, %v, want the folders made", made, err)
	}
	for _, dir := range append([]string{p.Dir, p.Agents, p.Workspaces, p.Repositories, p.Views}, p.Folders()...) {
		if info, err := os.Stat(dir); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Errorf("%s: %v, %v; want a directory only the user can read", dir, info, err)
		}
	}
	if made, err := p.MakeFolders(); err != nil || made {
		t.Errorf("MakeFolders again = %t, %v, want nothing made", made, err)
	}
}
