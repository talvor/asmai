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
