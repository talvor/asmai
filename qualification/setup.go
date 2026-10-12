// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/talvor/asmai/internal/providers"
	"github.com/talvor/asmai/internal/session"
	"github.com/talvor/asmai/internal/store"
	"github.com/talvor/asmai/internal/vt"
)

// setupWaits is how many of the harness's waits `asmai setup` may take: the
// first install fetches about 250 MB.
const setupWaits = 5

// setupColumns is the width of the terminal `asmai setup` runs in: wide
// enough that none of its lines wraps.
const setupColumns = 400

// setup runs `asmai setup` for the factory's disposable state directory, in
// a terminal of its own, as its user would. It confirms installing the
// pinned Claude Code, and in each Claude Code session setup opens it answers
// "Yes, I trust this folder" on the user's behalf, then leaves the session
// with /exit. This is the only place the harness answers Claude Code's
// question about trusting a folder (ADR 0011), and only in the disposable
// state directory: everywhere else, the question fails the case. It moves to
// "Yes, I trust this folder" when the question starts elsewhere, and confirms
// only while that option is selected, so it never chooses to exit.
func (h *Harness) setup(ctx context.Context, f *Factory) error {
	if f.paths.Dir != h.QualificationPaths.Dir {
		return fmt.Errorf("the harness runs asmai setup only for its disposable state directory, not %s", f.paths.Dir)
	}
	record := providers.ClaudeCodeRecord(envLookup(f.env, "HOME"), func(name string) string { return envLookup(f.env, name) })
	h.logf("running asmai setup for %s", f.paths.Dir)
	s, err := session.Start(session.Spec{Path: f.asmai, Args: []string{"setup"}, Env: f.env, Dir: "/", Columns: setupColumns, Rows: 40})
	if err != nil {
		return fmt.Errorf("running asmai setup: %w", err)
	}
	defer s.Stop(5 * time.Second)
	const maxKeys, keyGap = 8, time.Second
	// Setup opens Claude Code in each folder not yet trusted, in order.
	var asking []string
	for _, d := range []string{f.paths.Agents, f.paths.Workspaces} {
		if ok, err := providers.Trusted(record, d); err != nil {
			return err
		} else if !ok {
			asking = append(asking, d)
		}
	}
	confirmed := false
	keys := map[string]int{}
	left := map[string]bool{}
	if h.answered == nil {
		h.answered = map[string]bool{}
	}
	var lastKey time.Time
	var lines []string
	deadline := time.Now().Add(setupWaits * h.Wait)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		select {
		case <-s.Done():
			return h.setupEnded(s, record, lines)
		default:
		}
		screen, err := vt.FromState(s.Screen())
		if err != nil {
			return err
		}
		lines = screen.Lines()
		if marker, found := screenHas(lines, loginPromptMarkers...); found {
			return fmt.Errorf("Claude Code asked for a login (its screen shows %q) in a session asmai setup opened", marker)
		}
		// The folder Claude Code is open in is the first whose session the
		// harness has not left.
		dir := ""
		for _, d := range asking {
			if !left[d] {
				dir = d
				break
			}
		}
		trusted := false
		if dir != "" {
			if trusted, err = providers.Trusted(record, dir); err != nil {
				return err
			}
		}
		_, asked := screenHas(lines, trustPromptMarker)
		switch {
		case time.Since(lastKey) < keyGap:
		case !confirmed && screenShows(lines, "install it? [y/n]"):
			h.logf("confirming the install of the pinned Claude Code")
			err = s.Input([]byte("y\r"))
			confirmed, lastKey = true, time.Now()
		case asked && !trusted && dir != "":
			if keys[dir]++; keys[dir] > maxKeys {
				return fmt.Errorf("Claude Code's question about trusting %s did not clear after %d keys", dir, maxKeys)
			}
			if strings.Contains(selectedOption(lines), trustPromptMarker) {
				h.logf("answering \"Yes, I trust this folder\" for %s, in the Claude Code asmai setup opened", dir)
				err = s.Input([]byte("\r"))
				h.answered[dir] = true
			} else {
				h.logf("moving Claude Code's question about trusting %s to %q", dir, trustPromptMarker)
				err = s.Input([]byte(keyDown))
			}
			lastKey = time.Now()
		// Setup itself reports a folder Claude Code did not record trusted.
		case !asked && (trusted || h.answered[dir]) && dir != "" && promptSurface(lines):
			h.logf("leaving the Claude Code asmai setup opened in %s", dir)
			// Typed at once, Claude Code may take the Enter as part of a paste.
			if err = s.Input([]byte("/exit")); err == nil {
				time.Sleep(750 * time.Millisecond)
				err = s.Input([]byte("\r"))
			}
			left[dir], lastKey = true, time.Now()
		}
		if err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("asmai setup did not finish within %s%s", setupWaits*h.Wait, h.screenNote(lines))
		}
		time.Sleep(h.Poll)
	}
}

// setupEnded checks how `asmai setup` ended: it exited 0, and Claude Code's
// record trusts both folders agents start in.
func (h *Harness) setupEnded(s *session.Session, record string, lines []string) error {
	if screen, err := vt.FromState(s.Screen()); err == nil {
		lines = screen.Lines()
	}
	if err := s.Exit(); err != nil {
		// asmai's own error is its last line, after "asmai: ", which may
		// follow what Claude Code left on that line; the rest of the screen
		// may show the signed-in account.
		for i := len(lines) - 1; i >= 0; i-- {
			if _, message, found := strings.Cut(lines[i], "asmai: "); found {
				return fmt.Errorf("asmai setup failed (%v): %s", err, strings.TrimSpace(message))
			}
		}
		return fmt.Errorf("asmai setup failed (%v)%s", err, h.screenNote(lines))
	}
	for _, dir := range []string{h.QualificationPaths.Agents, h.QualificationPaths.Workspaces} {
		if ok, err := providers.Trusted(record, dir); err != nil {
			return err
		} else if !ok {
			return fmt.Errorf("asmai setup finished, but Claude Code's record does not trust %s", dir)
		}
	}
	return nil
}

func (h *Harness) screenNote(lines []string) string {
	if h.ShowScreen {
		return "\nthe last screen:\n" + strings.Join(lines, "\n")
	}
	return " (run with --show-screen to see the screen; it may show the signed-in account)"
}

// screenShows reports whether the screen shows text, whatever its case.
func screenShows(lines []string, text string) bool {
	_, found := screenHas(lines, text)
	return found
}

// envLookup returns name's value in env.
func envLookup(env []string, name string) string {
	value := ""
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == name {
			value = v
		}
	}
	return value
}

// startsWithoutTrustQuestion starts the pinned Claude Code at path in dir,
// a directory new to it, with an agent's command line, whose hooks only
// note the session's start, and requires the session to start with no
// question about trusting dir: Claude Code reports SessionStart only once it
// is past that question.
func (h *Harness) startsWithoutTrustQuestion(ctx context.Context, f *Factory, path, dir string) error {
	marks, err := os.MkdirTemp("", "asmai-qualify-start-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(marks)
	started := filepath.Join(marks, "started")
	args := providers.ClaudeCodeArgs("touch "+providers.ShellQuote(started), "sonnet", "", providers.WriteGuard{})
	s, err := session.Start(session.Spec{Path: path, Args: args, Env: providers.SessionEnv(f.env, f.paths.Bin, nil), Dir: dir, Columns: 80, Rows: 24})
	if err != nil {
		return err
	}
	defer s.Stop(5 * time.Second)
	var lines []string
	deadline := time.Now().Add(h.Wait)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if screen, err := vt.FromState(s.Screen()); err == nil {
			lines = screen.Lines()
		}
		if _, found := screenHas(lines, trustPromptMarker); found {
			return fmt.Errorf("Claude Code asked whether to trust %s, though asmai setup had the user trust the folder above it: on this pin, trusting a folder does not cover the folders below it", dir)
		}
		if marker, found := screenHas(lines, loginPromptMarkers...); found {
			return fmt.Errorf("Claude Code asked for a login (its screen shows %q) in %s", marker, dir)
		}
		if _, err := os.Stat(started); err == nil {
			return nil
		}
		select {
		case <-s.Done():
			return fmt.Errorf("Claude Code ended in %s before it reported SessionStart%s", dir, h.screenNote(lines))
		default:
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("Claude Code in %s reported no SessionStart within %s%s", dir, h.Wait, h.screenNote(lines))
		}
		time.Sleep(h.Poll)
	}
}

// promptSurface reports whether Claude Code's screen shows its input prompt.
func promptSurface(lines []string) bool {
	for _, line := range lines {
		if strings.Contains(line, "❯") || strings.TrimSpace(line) == ">" {
			return true
		}
	}
	return false
}

// c48 qualifies, for the pin, the behaviour of Claude Code's directory trust
// the factory relies on (ADR 0011): with the agents and workspaces folders
// trusted, as `asmai setup` has the user trust them, a new leader directory
// under agents/ and a new assignment workspace under workspaces/ start with
// no question about trusting them. Claude Code does not document that trust
// covers the folders below a trusted one, and its changelog shows the
// behaviour has changed before.
func (h *Harness) c48(ctx context.Context, f *Factory, r *Result) error {
	path, err := h.ready(ctx, f)
	if err != nil {
		return err
	}
	record := providers.ClaudeCodeRecord(envLookup(f.env, "HOME"), func(name string) string { return envLookup(f.env, name) })
	for _, dir := range []string{f.paths.Agents, f.paths.Workspaces} {
		if ok, err := providers.Trusted(record, dir); err != nil {
			return err
		} else if !ok {
			return fmt.Errorf("Claude Code's record does not trust %s after asmai setup", dir)
		}
	}
	for _, dir := range []string{f.paths.Agents, f.paths.Workspaces} {
		if h.answered[dir] {
			r.observe("asmai setup for the disposable state directory had Claude Code record the user's trust of %s, answered in the session setup opened there: the only place the harness answers that question", dir)
		} else {
			r.observe("asmai setup for the disposable state directory found %s trusted already in Claude Code's record, from an earlier run, and opened no session there", dir)
		}
	}

	leader, ok, err := f.Leader()
	if err != nil {
		return err
	}
	if !ok || leader.State != store.AgentRunning {
		return fmt.Errorf("Coordination's leader is not running after asmai start%s", exitNote(leader.Exit))
	}
	if err := h.awaitSessionStart(ctx, f, leader); err != nil {
		return err
	}
	r.observe("%s (generation %d) started in %s with no trust question, and reported SessionStart through its hook", leader.Agent, leader.Generation, filepath.Join(f.paths.Agents, leader.Agent))

	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	for _, dir := range []struct{ top, dir, what string }{
		{filepath.Join(f.paths.Agents, "qualification-c48-"+suffix), "", "a new leader directory"},
		{filepath.Join(f.paths.Workspaces, "qualification-c48-"+suffix), filepath.Join("assignment-1", "repo"), "a new assignment workspace"},
	} {
		start := filepath.Join(dir.top, dir.dir)
		if err := os.MkdirAll(start, 0o700); err != nil {
			return err
		}
		err := h.startsWithoutTrustQuestion(ctx, f, path, start)
		os.RemoveAll(dir.top)
		if err != nil {
			return err
		}
		r.observe("the pinned Claude Code started in %s, %s, with an agent's command line and no trust question, and reported SessionStart", start, dir.what)
	}
	return nil
}
