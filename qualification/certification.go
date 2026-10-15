// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/talvor/asmai/internal/certify"
	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/providers"
	"github.com/talvor/asmai/internal/store"
)

// uncertifiedPlatforms are platforms the pins file does not qualify, which
// C1 requires the certification check to refuse. Linux and macOS are the
// certified ones, each on the architecture the pins file names.
var uncertifiedPlatforms = [][2]string{{"linux", "arm64"}, {"darwin", "amd64"}, {"windows", "amd64"}, {"freebsd", "amd64"}}

// c1 qualifies platform certification (02 rule 6): this host is a certified
// platform, so the factory's start passes the platform check and runs
// Coordination's leader, and the check that start runs, which `asmai init` and
// `asmai doctor` run too, certifies Linux and macOS and refuses any other
// platform with what to do about it.
//
// A real uncertified host is not available to the harness, so the refusal is
// shown at the check's boundary, with the platform's name, as C42 shows
// GitHub's refusal of a draft: the daemon's refusal of an uncertified
// platform, before any agent, is exercised by the development tests with the
// fake provider.
func (h *Harness) c1(ctx context.Context, f *Factory, r *Result) error {
	if _, err := h.ready(ctx, f); err != nil {
		return err
	}
	res, code, err := f.Start(ctx)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("asmai start on this certified platform failed its checks: %s", failedChecks(res.Checks))
	}
	var platform *daemon.Check
	for i, c := range res.Checks {
		if c.Name == certify.PlatformName {
			platform = &res.Checks[i]
		}
	}
	switch {
	case platform == nil:
		return errors.New("asmai start ran no platform certification check")
	case !platform.OK:
		return fmt.Errorf("asmai start refused this platform, %s/%s: %s (fix: %s)", runtime.GOOS, runtime.GOARCH, platform.Problem, platform.Fix)
	}
	leader, ok, err := f.Leader()
	if err != nil {
		return err
	}
	if !ok || leader.State != store.AgentRunning {
		return fmt.Errorf("Coordination's leader is not running on %s/%s, which the platform check certified", runtime.GOOS, runtime.GOARCH)
	}
	r.observe("%s/%s passed asmai start's platform check, and Coordination's leader runs", runtime.GOOS, runtime.GOARCH)

	for _, p := range [][2]string{{"linux", "amd64"}, {"darwin", "arm64"}} {
		if c := certify.Platform(p[0], p[1], h.Subject.Pins); !c.OK {
			return fmt.Errorf("the platform check refuses %s/%s, a platform AsmAI is certified on: %s", p[0], p[1], c.Problem)
		}
	}
	r.observe("the platform check certifies Linux on x86_64 and macOS on arm64, the platforms this commit's pins file qualifies")

	for _, p := range uncertifiedPlatforms {
		c := certify.Platform(p[0], p[1], h.Subject.Pins)
		switch {
		case c.OK:
			return fmt.Errorf("the platform check certifies %s/%s, which this commit's pins file does not qualify", p[0], p[1])
		case !strings.Contains(c.Problem, p[0]+"/"+p[1]) || c.Fix == "":
			return fmt.Errorf("the platform check's refusal of %s/%s does not name the platform and what to do: %+v", p[0], p[1], c)
		}
	}
	r.observe("the platform check refuses %d platforms the pins file does not qualify, each time naming the platform and what to do; the refusal is shown at the check's boundary, as no uncertified host is available", len(uncertifiedPlatforms))
	return nil
}

// stoodIn is a stand-in for the Claude Code at the pinned copy's path: it
// reports a version other than the pin, as a copy that updated itself does,
// and writes a marker if anything else ever runs it, as a session would.
func stoodIn(version, marker string) string {
	return "#!/bin/sh\n" +
		"if [ \"$1\" = --version ]; then echo " + providers.ShellQuote(version+" (Claude Code)") + "; exit 0; fi\n" +
		": > " + providers.ShellQuote(marker) + "\n" +
		"exit 1\n"
}

// otherVersion is a version that is not the pin.
const otherVersion = "999.0.0"

// c2 qualifies the refusal of an unqualified provider version (03 rules 4
// and 5). In the disposable state directory it puts, at the pinned copy's
// path, an executable that reports a version other than the pin, and
// requires `asmai start` to refuse it before any dispatch: it exits 1 with
// the failed check naming both versions and the fix, no session starts, and
// the executable is run for nothing but its version. It then puts the pinned
// copy back, starts the leader, and puts the other version in place under the
// running leader, as a provider that updated itself does. The leader keeps
// running; once its session ends, the daemon does not restart it, and
// the agent is held until the pinned copy is back, when it starts again.
func (h *Harness) c2(ctx context.Context, f *Factory, r *Result) (err error) {
	path, err := h.ready(ctx, f)
	if err != nil {
		return err
	}
	pinned := h.Subject.Pins.ClaudeCode.Version
	real := path + ".qualification-real"
	marker := filepath.Join(f.paths.Dir, "unpinned-ran")
	// The pinned copy is kept: the run's later cases and the next run use it.
	swapped := false
	putBack := func() error {
		if !swapped {
			return nil
		}
		if err := os.Rename(real, path); err != nil {
			return fmt.Errorf("putting the pinned copy back at %s from %s: %w", path, real, err)
		}
		swapped = false
		return nil
	}
	defer func() {
		if restoreErr := putBack(); restoreErr != nil {
			err = errors.Join(err, restoreErr)
		}
	}()
	swapIn := func() error {
		if err := os.Rename(path, real); err != nil {
			return err
		}
		swapped = true
		// A new file, not a write into the old one: a running executable
		// cannot be written to, and an update replaces it.
		if err := os.WriteFile(path, []byte(stoodIn(otherVersion, marker)), 0o755); err != nil {
			return err
		}
		return nil
	}

	// 1. A version that is not the pin is refused before any dispatch.
	before, _, err := f.Leader()
	if err != nil {
		return err
	}
	if err := f.Stop(ctx); err != nil {
		return err
	}
	if err := swapIn(); err != nil {
		return err
	}
	res, code, err := f.Start(ctx)
	if err != nil {
		return err
	}
	check, err := versionCheck(res)
	if err != nil {
		return err
	}
	switch {
	case code != 1 || check.OK:
		return fmt.Errorf("asmai start with Claude Code %s at the pinned path exited %d with the version check ok=%t, want it refused (exit 1)", otherVersion, code, check.OK)
	case !strings.Contains(check.Problem, otherVersion) || !strings.Contains(check.Problem, pinned) || check.Fix == "":
		return fmt.Errorf("the refusal does not name the version found, the pinned version and what to do: %+v", check)
	}
	leader, ok, err := f.Leader()
	if err != nil {
		return err
	}
	if ok && leader.State == store.AgentRunning {
		return fmt.Errorf("Coordination's leader runs on Claude Code %s, which is not the pinned %s", otherVersion, pinned)
	}
	if ok && leader.Generation != before.Generation {
		return fmt.Errorf("a session started on Claude Code %s (generation %d, was %d) before the refusal", otherVersion, leader.Generation, before.Generation)
	}
	if exists(marker) {
		return fmt.Errorf("Claude Code %s was run for more than its version before it was refused", otherVersion)
	}
	r.observe("with Claude Code %s at the pinned path, asmai start was refused before any dispatch: %s (fix: %s)", otherVersion, check.Problem, check.Fix)
	if err := f.Stop(ctx); err != nil {
		return err
	}
	if err := putBack(); err != nil {
		return err
	}

	// 2. A provider that updated itself under a running agent holds it at
	// its next restart.
	res, code, err = f.Start(ctx)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("asmai start with the pinned copy back failed its checks: %s", failedChecks(res.Checks))
	}
	running, ok, err := f.Leader()
	if err != nil {
		return err
	}
	if !ok || running.State != store.AgentRunning {
		return errors.New("Coordination's leader did not start on the pinned copy")
	}
	if err := swapIn(); err != nil {
		return err
	}
	if current, ok, err := f.Leader(); err != nil || !ok || current.State != store.AgentRunning || current.Generation != running.Generation {
		return fmt.Errorf("Coordination's leader changed when its provider's executable was replaced under it (%+v, %v)", current, err)
	}
	r.observe("with the provider replaced by Claude Code %s under the running leader (generation %d, pid %d), the session kept running", otherVersion, running.Generation, running.PID)

	// The session ends as it does for a restart; the daemon restarts
	// Coordination's leader after a session that ended on its own.
	if err := endSession(running.PID); err != nil {
		return err
	}
	if err := h.awaitHeld(ctx, f, running.Generation); err != nil {
		return err
	}
	if exists(marker) {
		return fmt.Errorf("Claude Code %s was run for a session after it updated itself", otherVersion)
	}
	held, err := statusCheck(f)
	if err != nil {
		return err
	}
	if held.OK || !strings.Contains(held.Problem, otherVersion) {
		return fmt.Errorf("asmai status does not show the leader held for Claude Code %s: %+v", otherVersion, held)
	}
	r.observe("when its session ended, the leader was not restarted on Claude Code %s: it is held, and asmai status shows %q", otherVersion, held.Problem)

	// 3. The hold lifts when the qualified copy is back.
	if err := f.Stop(ctx); err != nil {
		return err
	}
	if err := putBack(); err != nil {
		return err
	}
	if res, code, err = f.Start(ctx); err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("asmai start after the pinned copy was put back failed its checks: %s", failedChecks(res.Checks))
	}
	r.observe("with the pinned copy back, asmai start ran the leader again")
	return nil
}

// versionCheck returns the version check of a start's result.
func versionCheck(res StartResult) (daemon.Check, error) {
	for _, c := range res.Checks {
		if c.Name == certify.VersionName {
			return c, nil
		}
	}
	return daemon.Check{}, fmt.Errorf("asmai start ran no check of Claude Code's version (%s)", failedChecks(res.Checks))
}

// statusCheck returns the version check as asmai status shows it.
func statusCheck(f *Factory) (daemon.Check, error) {
	resp, err := daemon.Call(f.paths.Socket, daemon.Request{Command: daemon.CommandStatus})
	if err != nil {
		return daemon.Check{}, err
	}
	return versionCheck(StartResult{Checks: resp.Checks})
}

// endSession ends the agent session of pid as a restart does: with
// SIGTERM, and with SIGKILL if it does not leave in time.
func endSession(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("ending the leader's session (pid %d): %w", pid, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if proc.Signal(syscall.Signal(0)) != nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return proc.Signal(syscall.SIGKILL)
}

// awaitHeld waits until the leader of generation has ended and the daemon has
// not started it again, which it does within seconds if it can: Coordination's
// leader restarts a second after a session that ended on its own.
func (h *Harness) awaitHeld(ctx context.Context, f *Factory, generation int) error {
	deadline := time.Now().Add(h.Wait)
	var seenEnded time.Time
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		leader, ok, err := f.Leader()
		if err != nil {
			return err
		}
		switch {
		case ok && leader.State == store.AgentRunning && leader.Generation != generation:
			return fmt.Errorf("the daemon restarted the leader (generation %d) on a provider that is not the pinned version", leader.Generation)
		case ok && leader.State != store.AgentRunning && leader.Generation == generation:
			if seenEnded.IsZero() {
				seenEnded = time.Now()
			}
			// The restart wait is a second, and a start checks at once.
			if time.Since(seenEnded) > 5*time.Second {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return errors.New("the leader's session did not end")
		}
		time.Sleep(h.Poll)
	}
}
