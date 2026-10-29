// SPDX-License-Identifier: Apache-2.0

package statedir

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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

func TestTheBranchIDIsMadeOnceAndKept(t *testing.T) {
	p := At(filepath.Join(t.TempDir(), "state"))
	if id, err := p.ReadBranchID(); err != nil || id != "" {
		t.Fatalf("before any ID is made, reading gave %q (%v)", id, err)
	}
	id, err := p.BranchIDOrMake()
	if err != nil || len(id) != 6 {
		t.Fatalf("making the ID gave %q (%v), want 6 characters", id, err)
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			t.Errorf("the ID %q has %q, want lower-case letters and digits", id, r)
		}
	}
	if again, err := p.BranchIDOrMake(); err != nil || again != id {
		t.Errorf("asking again gave %q (%v), want %q", again, err, id)
	}
	if read, err := p.ReadBranchID(); err != nil || read != id {
		t.Errorf("reading gave %q (%v), want %q", read, err, id)
	}
	info, err := os.Stat(p.BranchID)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("the ID file is %v (%v), want private", info, err)
	}
}

func TestConcurrentFirstUsePublishesOneCompleteBranchID(t *testing.T) {
	p := At(filepath.Join(t.TempDir(), "state"))
	if err := p.Prepare(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.Dir, ".branch-id-interrupted"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	const callers = 32
	start := make(chan struct{})
	ids := make([]string, callers)
	errs := make([]error, callers)
	var ready, done sync.WaitGroup
	ready.Add(callers)
	done.Add(callers)
	for i := range ids {
		go func(i int) {
			defer done.Done()
			ready.Done()
			<-start
			ids[i], errs[i] = p.BranchIDOrMake()
		}(i)
	}
	ready.Wait()
	close(start)
	done.Wait()
	for i := range ids {
		if errs[i] != nil || ids[i] == "" || ids[i] != ids[0] {
			t.Fatalf("first-use caller %d got %q (%v), want the same complete ID %q", i, ids[i], errs[i], ids[0])
		}
	}
	if got, err := p.ReadBranchID(); err != nil || got != ids[0] {
		t.Errorf("the published branch ID is %q (%v), want %q", got, err, ids[0])
	}
	entries, err := os.ReadDir(p.Dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != ".branch-id-interrupted" && strings.HasPrefix(entry.Name(), ".branch-id-") {
			t.Errorf("first use left an unpublished ID file %s", entry.Name())
		}
	}
}
