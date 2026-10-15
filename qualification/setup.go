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
	"github.com/talvor/asmai/internal/repos"
	"github.com/talvor/asmai/internal/session"
	"github.com/talvor/asmai/internal/statedir"
	"github.com/talvor/asmai/internal/store"
	"github.com/talvor/asmai/internal/vt"
)

// setupWaits is how many of the harness's waits `asmai setup` may take: the
// first install fetches about 250 MB.
const setupWaits = 5

// setupAgreeMarker starts the line on which `asmai setup` or `asmai repo
// add`, after saying how to answer Claude Code's question about trusting a
// folder, asks whether to open Claude Code there; the folder follows it.
const setupAgreeMarker = "press enter to open claude code in "

// setupColumns is the width of the terminal `asmai setup` runs in: wide
// enough that none of its lines wraps.
const setupColumns = 400

// setup runs `asmai setup` for the factory's disposable state directory, in
// a terminal of its own, as its user would, and answers its questions as
// answerTrust does. It confirms installing the pinned Claude Code too.
// Setup opens Claude Code in each folder not yet trusted, in order.
func (h *Harness) setup(ctx context.Context, f *Factory) error {
	folders, err := trustedAtSetup(f.paths)
	if err != nil {
		return err
	}
	record := providers.ClaudeCodeRecord(envLookup(f.env, "HOME"), func(name string) string { return envLookup(f.env, name) })
	var asking []string
	for _, d := range folders {
		if ok, err := providers.Trusted(record, d); err != nil {
			return err
		} else if !ok {
			asking = append(asking, d)
		}
	}
	return h.answerTrust(ctx, f, "asmai setup", []string{"setup"}, asking)
}

// repoAdd runs `asmai repo add` of origin, as the qualification repository,
// for the factory's disposable state directory, as setup runs `asmai setup`:
// repo add has the user trust the new clone, a repository no trust given to
// a folder above it covers.
func (h *Harness) repoAdd(ctx context.Context, f *Factory, origin string) error {
	clone := filepath.Join(f.paths.Repositories, qualificationRepository)
	return h.answerTrust(ctx, f, "asmai repo add", []string{"repo", "add", origin, "--name", qualificationRepository}, []string{clone})
}

// answerTrust runs command, asmai with args, for the factory's disposable
// state directory, in a terminal of its own, as its user would. It agrees
// with Enter when the command asks to open the pinned Claude Code in one of
// asking, the folders it may ask about, in order, and in each Claude Code session
// the command opens it answers "Yes, I trust this folder" on the user's
// behalf, then leaves the session with /exit. The sessions `asmai setup` and
// `asmai repo add` open are the only place the harness answers Claude Code's
// question about trusting a folder (ADR 0011), and only in the disposable
// state directory: everywhere else, the question fails the case. It moves to
// "Yes, I trust this folder" when the question starts elsewhere, and confirms
// only while that option is selected, so it never chooses to exit.
func (h *Harness) answerTrust(ctx context.Context, f *Factory, command string, args, asking []string) error {
	if f.paths.Dir != h.QualificationPaths.Dir {
		return fmt.Errorf("the harness runs %s only for its disposable state directory, not %s", command, f.paths.Dir)
	}
	record := providers.ClaudeCodeRecord(envLookup(f.env, "HOME"), func(name string) string { return envLookup(f.env, name) })
	h.logf("running %s for %s", command, f.paths.Dir)
	s, err := session.Start(session.Spec{Path: f.asmai, Args: args, Env: f.env, Dir: "/", Columns: setupColumns, Rows: 40})
	if err != nil {
		return fmt.Errorf("running %s: %w", command, err)
	}
	defer s.Stop(5 * time.Second)
	const maxKeys, keyGap = 8, time.Second
	confirmed := false
	agreed := map[string]bool{}
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
			return h.trustEnded(s, command, record, lines)
		default:
		}
		screen, err := vt.FromState(s.Screen())
		if err != nil {
			return err
		}
		lines = screen.Lines()
		if marker, found := screenHas(lines, loginPromptMarkers...); found {
			return fmt.Errorf("Claude Code asked for a login (its screen shows %q) in a session %s opened", marker, command)
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
		// The command's explanation above its question names Claude Code's
		// answers too, so once the harness has agreed, only the screen below
		// the question is Claude Code's.
		agree := setupAgreeMarker + strings.ToLower(dir)
		claude := lines
		if agreed[dir] {
			claude = linesBelow(lines, agree)
		}
		_, asked := screenHas(claude, trustPromptMarker)
		switch {
		case time.Since(lastKey) < keyGap:
		case !confirmed && screenShows(lines, "install it? [y/n]"):
			h.logf("confirming the install of the pinned Claude Code")
			err = s.Input([]byte("y\r"))
			confirmed, lastKey = true, time.Now()
		case dir != "" && !agreed[dir] && screenShows(lines, agree):
			h.logf("agreeing, as %s asks, to open Claude Code in %s", command, dir)
			err = s.Input([]byte("\r"))
			agreed[dir], lastKey = true, time.Now()
		case agreed[dir] && asked && !trusted:
			if keys[dir]++; keys[dir] > maxKeys {
				return fmt.Errorf("Claude Code's question about trusting %s did not clear after %d keys", dir, maxKeys)
			}
			if strings.Contains(selectedOption(claude), trustPromptMarker) {
				h.logf("answering \"Yes, I trust this folder\" for %s, in the Claude Code %s opened", dir, command)
				err = s.Input([]byte("\r"))
				h.answered[dir] = true
			} else {
				h.logf("moving Claude Code's question about trusting %s to %q", dir, trustPromptMarker)
				err = s.Input([]byte(keyDown))
			}
			lastKey = time.Now()
		// The command itself reports a folder Claude Code did not record
		// trusted.
		case agreed[dir] && !asked && (trusted || h.answered[dir]) && promptSurface(claude):
			h.logf("leaving the Claude Code %s opened in %s", command, dir)
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
			return fmt.Errorf("%s did not finish within %s%s", command, setupWaits*h.Wait, h.screenNote(lines))
		}
		time.Sleep(h.Poll)
	}
}

// trustedAtSetup returns the folders `asmai setup` has the user trust, in
// the order it asks: the agents and workspaces folders, then each registered
// repository's clone, whose worktrees workers start in.
func trustedAtSetup(paths statedir.Paths) ([]string, error) {
	clones, err := paths.Clones()
	if err != nil {
		return nil, err
	}
	return append([]string{paths.Agents, paths.Workspaces}, clones...), nil
}

// trustEnded checks how command, `asmai setup` or `asmai repo add`, ended:
// it exited 0, and Claude Code's record trusts every folder agents start in.
func (h *Harness) trustEnded(s *session.Session, command, record string, lines []string) error {
	if screen, err := vt.FromState(s.Screen()); err == nil {
		lines = screen.Lines()
	}
	if err := s.Exit(); err != nil {
		// asmai's own error is its last line, after "asmai: ", which may
		// follow what Claude Code left on that line; the rest of the screen
		// may show the signed-in account.
		for i := len(lines) - 1; i >= 0; i-- {
			if _, message, found := strings.Cut(lines[i], "asmai: "); found {
				return fmt.Errorf("%s failed (%v): %s", command, err, strings.TrimSpace(message))
			}
		}
		return fmt.Errorf("%s failed (%v)%s", command, err, h.screenNote(lines))
	}
	folders, err := trustedAtSetup(h.QualificationPaths)
	if err != nil {
		return err
	}
	for _, dir := range folders {
		if ok, err := providers.Trusted(record, dir); err != nil {
			return err
		} else if !ok {
			return fmt.Errorf("%s finished, but Claude Code's record does not trust %s", command, dir)
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

// linesBelow returns the lines below the last that shows text, whatever its
// case, or all of them when none does.
func linesBelow(lines []string, text string) []string {
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.Contains(strings.ToLower(lines[i]), text) {
			return lines[i+1:]
		}
	}
	return lines
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
// is past that question. covered says which trust asmai setup or asmai repo
// add had the user give was to cover dir, and why it did not when the question appears.
func (h *Harness) startsWithoutTrustQuestion(ctx context.Context, f *Factory, path, dir, covered string) error {
	marks, err := os.MkdirTemp("", "asmai-qualify-start-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(marks)
	started := filepath.Join(marks, "started")
	args := providers.ClaudeCodeArgs("touch "+providers.ShellQuote(started), "sonnet", "", "", providers.WriteGuard{}, providers.Permissions{})
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
			return fmt.Errorf("Claude Code asked whether to trust %s, though the user had trusted %s", dir, covered)
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
// the factory relies on (ADR 0011): with the agents folder trusted, as `asmai
// setup` has the user trust it, a new leader directory under agents/ starts
// with no question about trusting it; and with a registered repository's
// clone trusted, as `asmai repo add` has the user trust it, a worker's new workspace, a git worktree of the clone made as the
// daemon makes it, starts with no question either. Claude Code does not
// document that trust covers the folders below a trusted one, nor that it
// covers a repository's worktrees, and its changelog shows the behaviour has
// changed before.
func (h *Harness) c48(ctx context.Context, f *Factory, r *Result) error {
	path, err := h.ready(ctx, f)
	if err != nil {
		return err
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

	// Registering the repository has the user trust its clone.
	if err := h.prepareHandoffRepository(ctx, f); err != nil {
		return err
	}
	clone := filepath.Join(f.paths.Repositories, qualificationRepository)
	record := providers.ClaudeCodeRecord(envLookup(f.env, "HOME"), func(name string) string { return envLookup(f.env, name) })
	folders, err := trustedAtSetup(f.paths)
	if err != nil {
		return err
	}
	for _, dir := range folders {
		if ok, err := providers.Trusted(record, dir); err != nil {
			return err
		} else if !ok {
			return fmt.Errorf("Claude Code's record does not trust %s after asmai setup and asmai repo add", dir)
		}
		if h.answered[dir] {
			r.observe("asmai setup or asmai repo add for the disposable state directory had Claude Code record the user's trust of %s, answered in the session it opened there: the only place the harness answers that question", dir)
		} else {
			r.observe("asmai setup and asmai repo add for the disposable state directory found %s trusted already in Claude Code's record (from an earlier run or a trusted folder above it), and opened no session there", dir)
		}
	}

	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	agent := filepath.Join(f.paths.Agents, "qualification-c48-"+suffix)
	if err := os.MkdirAll(agent, 0o700); err != nil {
		return err
	}
	err = h.startsWithoutTrustQuestion(ctx, f, path, agent, "the agents folder above it: on this pin, trusting a folder does not cover the folders below it")
	os.RemoveAll(agent)
	if err != nil {
		return err
	}
	r.observe("the pinned Claude Code started in %s, a new leader directory, with an agent's command line and no trust question, and reported SessionStart", agent)

	commit, err := repos.OriginTip(ctx, clone, "main")
	if err != nil {
		return err
	}
	top := filepath.Join(f.paths.Workspaces, "qualification-c48-"+suffix)
	workspace := filepath.Join(top, "assignment-1", "repo")
	branch := "qualification-c48-" + suffix
	if err := repos.AddWorkspace(ctx, clone, workspace, branch, commit); err != nil {
		return err
	}
	err = h.startsWithoutTrustQuestion(ctx, f, path, workspace, "the clone "+clone+" it is a worktree of: on this pin, trusting a repository does not cover its worktrees")
	if removeErr := repos.RemoveWorkspace(ctx, clone, workspace, branch); removeErr != nil && err == nil {
		err = removeErr
	}
	os.RemoveAll(top)
	if err != nil {
		return err
	}
	r.observe("the pinned Claude Code started in %s, a new worker's workspace made as the daemon makes it, a git worktree of the clone %s, with an agent's command line and no trust question, and reported SessionStart", workspace, clone)
	return nil
}
