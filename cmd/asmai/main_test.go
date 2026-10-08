// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/statedir"
	"github.com/talvor/asmai/internal/store"
)

func TestVersionPrintsTheBuildVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"version"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Errorf("asmai version exited %d, want 0", code)
	}
	if got, want := stdout.String(), "asmai dev\n"; got != want {
		t.Errorf("asmai version printed %q, want %q", got, want)
	}
}

// asmai notices prints AsmAI's LICENSE and the third-party notices file from
// inside the executable: it reads no file, so needs no daemon or configuration.
func TestNoticesPrintsTheLicenseAndTheThirdPartyNotices(t *testing.T) {
	license, err := os.ReadFile("../../LICENSE")
	if err != nil {
		t.Fatal(err)
	}
	notices, err := os.ReadFile("../../THIRD_PARTY_NOTICES")
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	var stdout, stderr bytes.Buffer

	code := run([]string{"notices"}, strings.NewReader(""), &stdout, &stderr)

	if code != 0 {
		t.Errorf("asmai notices exited %d, want 0; stderr %q", code, stderr.String())
	}
	if got, want := stdout.String(), string(license)+"\n"+string(notices); got != want {
		t.Errorf("asmai notices printed\n%s\nwant\n%s", got, want)
	}
}

func TestUnknownCommandIsRefusedWithUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"frobnicate"}, strings.NewReader(""), &stdout, &stderr)
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

// A bare asmai is the conversation, which needs a terminal: without one it
// says so and starts nothing.
func TestNoCommandIsTheConversation(t *testing.T) {
	// os.MkdirTemp keeps the socket's path short enough on macOS.
	home, err := os.MkdirTemp("", "home")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("HOME", home)
	var stdout, stderr bytes.Buffer
	code := run(nil, strings.NewReader(""), &stdout, &stderr)
	if code != 1 {
		t.Errorf("exited %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "the conversation with Coordination needs a terminal") {
		t.Errorf("stderr %q does not say the conversation needs a terminal", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".local")); err == nil {
		t.Error("without a terminal, a bare asmai made the state directory")
	}
}

func TestHelpPrintsUsage(t *testing.T) {
	for _, arg := range []string{"help", "-h", "--help"} {
		var stdout, stderr bytes.Buffer
		if code := run([]string{arg}, strings.NewReader(""), &stdout, &stderr); code != 0 {
			t.Errorf("asmai %s exited %d, want 0", arg, code)
		}
		if !strings.Contains(stdout.String(), "asmai, asmai chat") {
			t.Errorf("asmai %s printed %q, want the usage", arg, stdout.String())
		}
	}
}

// The scripted fake provider is for development tests only: the asmai
// executable must never contain it.
func TestTheFakeProviderIsNotBuiltIn(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", ".").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	for pkg := range strings.Lines(string(out)) {
		pkg = strings.TrimSpace(pkg)
		if pkg == "github.com/talvor/asmai/internal/fakeprovider" || strings.HasPrefix(pkg, "github.com/talvor/asmai/internal/fakeprovider/") {
			t.Errorf("asmai is built with %s", pkg)
		}
	}
}

func TestAFailedCheckDoesNotSayARunningLeaderIsNotStarted(t *testing.T) {
	var stdout, stderr bytes.Buffer
	o := &output{stdout: &stdout, stderr: &stderr}
	resp := daemon.Response{
		Checks:  []daemon.Check{{Name: "staffing", Problem: "the configuration file is not valid", Fix: "fix it"}},
		Leaders: []daemon.Leader{{Agent: "leader@coordination", State: store.AgentRunning, Generation: 1}},
	}
	if code := o.started(daemon.Status{}, true, statedir.Paths{}, resp); code != 1 {
		t.Errorf("asmai start exited %d, want 1 for a failed check", code)
	}
	got := stderr.String()
	if strings.Contains(got, "but not Coordination's leader") || !strings.Contains(got, "so is Coordination's leader, but a check failed") || !strings.Contains(got, "failed  staffing") {
		t.Errorf("asmai start said %q, want the failed check named and the leader not called stopped", got)
	}
}
