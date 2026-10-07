// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestVersionPrintsTheBuildVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"version"}, &stdout, &stderr)
	if code != 0 {
		t.Errorf("asmai version exited %d, want 0", code)
	}
	if got, want := stdout.String(), "asmai dev\n"; got != want {
		t.Errorf("asmai version printed %q, want %q", got, want)
	}
}

func TestUnknownCommandIsRefusedWithUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"frobnicate"}, &stdout, &stderr)
	if code != 2 {
		t.Errorf("exited %d, want 2", code)
	}
	if stdout.Len() != 0 {
		t.Errorf("printed %q to stdout, want nothing", stdout.String())
	}
	for _, want := range []string{`unknown command "frobnicate"`, "asmai version"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr %q does not mention %q", stderr.String(), want)
		}
	}
}

func TestNoCommandPrintsUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(nil, &stdout, &stderr)
	if code != 2 {
		t.Errorf("exited %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "asmai version") {
		t.Errorf("stderr %q does not show usage", stderr.String())
	}
}
