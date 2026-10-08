// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/talvor/asmai/internal/providers"
)

// ErrSignalMissing is returned by a provider's AuthStatus when the pinned
// copy gives no sign-in status signal at all. It is a known limitation, not a
// failure.
var ErrSignalMissing = errors.New("the provider gives no sign-in status")

// AuthStatus is what a provider CLI's own status check reports of its
// sign-in: the sign-in method only, never a secret (03 rule 7).
type AuthStatus struct {
	SignedIn bool
	// Method is how the CLI is signed in, such as claude.ai for a
	// subscription and api_key for an API key.
	Method string
}

// Provider is the real provider CLI the harness qualifies AsmAI against. The
// harness's own tests stand in a scripted fake for it, which never counts
// toward qualification: the harness run on a qualification host uses
// ClaudeCode.
type Provider interface {
	// Name is the provider's name for people.
	Name() string
	// Install makes the pinned copy present in the factory, whose daemon is
	// running, where the pins file's plan for the platform puts it.
	Install(ctx context.Context, f *Factory, pins providers.Pins) error
	// Version runs the installed copy at path and returns the version it
	// reports.
	Version(ctx context.Context, f *Factory, path string) (string, error)
	// AuthStatus asks the copy at path, in the factory's environment, for its
	// sign-in status.
	AuthStatus(ctx context.Context, f *Factory, path string) (AuthStatus, error)
}

// ClaudeCode is the real pinned Claude Code.
type ClaudeCode struct{}

// Name implements Provider.
func (ClaudeCode) Name() string { return "Claude Code" }

// Install implements Provider with `asmai providers install`, which fetches
// the pinned copy from Claude Code's official channel and checks it against
// the pins file. The harness answers its question, as the maintainer running
// the harness has already decided to qualify this pin.
func (ClaudeCode) Install(ctx context.Context, f *Factory, _ providers.Pins) error {
	stdout, stderr, code, err := f.Run(ctx, "y\n", "providers", "install")
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("asmai providers install exited %d: %s", code, strings.TrimSpace(stdout+stderr))
	}
	return nil
}

// Version implements Provider with `claude --version`.
func (ClaudeCode) Version(ctx context.Context, f *Factory, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	cmd.Env = f.env
	cmd.Dir = "/"
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("running %s --version: %w", path, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// AuthStatus implements Provider with `claude auth status --json`. It reads
// only whether Claude Code is signed in and its sign-in method; the rest of
// what it prints, such as the account, is never read or kept.
func (ClaudeCode) AuthStatus(ctx context.Context, f *Factory, path string) (AuthStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "auth", "status", "--json")
	cmd.Env = f.env
	cmd.Dir = "/"
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	// Its stderr may name the account, so it is not kept either. A signed-out
	// CLI may exit non-zero with its status, so only what it printed counts.
	cmd.Run()
	var status struct {
		LoggedIn   *bool  `json:"loggedIn"`
		AuthMethod string `json:"authMethod"`
	}
	if json.Unmarshal(stdout.Bytes(), &status) != nil || status.LoggedIn == nil {
		// A CLI that does not know the command, or does not print its status
		// as JSON, gives no signal to read.
		return AuthStatus{}, ErrSignalMissing
	}
	return AuthStatus{SignedIn: *status.LoggedIn, Method: status.AuthMethod}, nil
}

// subscriptionMethods are the sign-in methods that are the user's
// subscription. An API key or an unknown method is not (03 rule 7).
var subscriptionMethods = map[string]bool{"claude.ai": true}

// loginPromptMarkers are what Claude Code's screen shows while it asks the
// user to sign in. They are matched whatever their case.
var loginPromptMarkers = []string{
	"select login method",
	"paste code here",
	"browser didn't open",
	"run /login",
	"not logged in",
}

// trustPromptMarker is what Claude Code's screen shows when it asks the user
// to trust a directory it has not run in. It comes after sign-in, and is the
// first thing a session in a new directory waits for.
const trustPromptMarker = "yes, i trust this folder"

// keyDown is the Down arrow key, as a terminal sends it.
const keyDown = "\x1b[B"

// selectedOption returns, in lower case, the option a Claude Code menu shows
// as selected: the line the ❯ marker is on.
func selectedOption(lines []string) string {
	for _, line := range lines {
		if _, option, found := strings.Cut(line, "❯"); found {
			return strings.ToLower(strings.TrimSpace(option))
		}
	}
	return ""
}

func screenHas(lines []string, markers ...string) (marker string, found bool) {
	text := strings.ToLower(strings.Join(lines, "\n"))
	for _, m := range markers {
		if strings.Contains(text, m) {
			return m, true
		}
	}
	return "", false
}
