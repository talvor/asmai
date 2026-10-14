// SPDX-License-Identifier: Apache-2.0

package statedir

import (
	"os"
	"path/filepath"
	"slices"
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

// Each registered repository's clone is a directory in repositories/; a
// registration's unfinished clone, named with a leading dot, is not one.
func TestTheClonesAreTheDirectoriesInRepositories(t *testing.T) {
	p := At(filepath.Join(t.TempDir(), "state"))
	if clones, err := p.Clones(); err != nil || len(clones) != 0 {
		t.Errorf("with no repositories folder, Clones() = %q, %v, want none", clones, err)
	}
	for _, name := range []string{"beta", "alpha", ".gamma.partial"} {
		if err := os.MkdirAll(filepath.Join(p.Repositories, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(p.Repositories, "stray"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	clones, err := p.Clones()
	want := []string{filepath.Join(p.Repositories, "alpha"), filepath.Join(p.Repositories, "beta")}
	if err != nil || !slices.Equal(clones, want) {
		t.Errorf("Clones() = %q, %v, want %q", clones, err, want)
	}
}
