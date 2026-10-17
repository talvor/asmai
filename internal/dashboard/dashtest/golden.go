// SPDX-License-Identifier: Apache-2.0

package dashtest

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden screens from what the tests draw")

// Golden compares got with the golden screen testdata/golden/<name>.txt in
// the test's package, or rewrites it with -update.
func Golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "golden", name+".txt")
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the golden screen %s: %v (run the tests with -update to write it)", path, err)
	}
	if string(want) != got {
		t.Errorf("the screen differs from %s (run the tests with -update to accept it)\n--- want\n%s\n--- got\n%s", path, want, got)
	}
}
