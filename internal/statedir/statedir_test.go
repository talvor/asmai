// SPDX-License-Identifier: Apache-2.0

package statedir

import (
	"path/filepath"
	"testing"
)

func TestScratchStateDirectoryOverride(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
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
