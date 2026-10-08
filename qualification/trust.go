// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/talvor/asmai/internal/providers"
	"github.com/talvor/asmai/internal/session"
	"github.com/talvor/asmai/internal/vt"
)

// prepareEngineeringTrust answers Claude Code's first-use directory trust
// prompt before the daemon starts Engineering. This is provider setup in the
// harness user's own scratch agent directory, not a dispatch submission.
func (h *Harness) prepareEngineeringTrust(ctx context.Context, f *Factory, path string) error {
	dir := filepath.Join(f.paths.Agents, "leader@engineering")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	env := providers.SessionEnv(f.env, f.paths.Bin, nil)
	s, err := session.Start(session.Spec{Path: path, Args: []string{"--setting-sources", "user"}, Env: env, Dir: dir, Columns: 80, Rows: 24})
	if err != nil {
		return err
	}
	defer s.Stop(5 * time.Second)
	deadline := time.Now().Add(h.Wait)
	keys := 0
	var lastKey time.Time
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		select {
		case <-s.Done():
			return fmt.Errorf("Claude Code ended while preparing Engineering's directory: %v", s.Exit())
		default:
		}
		screen, err := vt.FromState(s.Screen())
		if err != nil {
			return err
		}
		lines := screen.Lines()
		if marker, found := screenHas(lines, loginPromptMarkers...); found {
			return fmt.Errorf("Claude Code asked for a login while preparing Engineering's directory (%s)", marker)
		}
		if _, found := screenHas(lines, trustPromptMarker); found {
			if !h.canAutoAcceptTrust(f, "leader@engineering") {
				return errors.New("Claude Code needs Engineering's qualification directory trusted; automatic trust acceptance is limited to the authorized asmai-vm qualification directory")
			}
			if keys >= 8 {
				return errors.New("Claude Code's Engineering directory trust prompt did not clear after eight keys")
			}
			if time.Since(lastKey) >= time.Second {
				if strings.Contains(selectedOption(lines), trustPromptMarker) {
					err = s.Input([]byte("\r"))
					h.logf("confirming Engineering's Claude Code directory trust prompt")
				} else {
					err = s.Input([]byte(keyDown))
					h.logf("moving Engineering's Claude Code directory trust prompt to yes")
				}
				if err != nil {
					return err
				}
				keys++
				lastKey = time.Now()
			}
		} else if promptSurface(lines) {
			return nil
		}
		time.Sleep(h.Poll)
	}
	return errors.New("Claude Code showed no ready prompt while preparing Engineering's directory")
}

func (h *Harness) canAutoAcceptTrust(f *Factory, address string) bool {
	if h.Host != "asmai-vm" || (address != "leader@coordination" && address != "leader@engineering") {
		return false
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	root := filepath.Join(home, ".local", "state", "asmai", "qualification")
	return filepath.Clean(f.paths.Dir) == filepath.Clean(root) &&
		filepath.Clean(filepath.Join(f.paths.Agents, address)) == filepath.Clean(filepath.Join(root, "agents", address))
}

func promptSurface(lines []string) bool {
	for _, line := range lines {
		if strings.Contains(line, "❯") || strings.TrimSpace(line) == ">" {
			return true
		}
	}
	return false
}
