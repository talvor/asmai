// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ClaudeCodeRecord returns the file Claude Code keeps its record of the
// user's directory trust in: .claude.json in CLAUDE_CONFIG_DIR when getenv
// sets it, and ~/.claude.json, in home, otherwise. AsmAI only ever reads it.
func ClaudeCodeRecord(home string, getenv func(string) string) string {
	if dir := getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, ".claude.json")
	}
	return filepath.Join(home, ".claude.json")
}

// Trusted reports whether the Claude Code record at path holds the user's
// trust of dir: their "Yes, I trust this folder", given in dir or in a
// directory above it, which on the pinned Claude Code covers every directory
// below it. dir counts as given and with its symbolic links resolved, as
// Claude Code records the directory it runs in. A record that does not exist
// holds no trust. Trusted never changes the record.
func Trusted(path, dir string) (bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var record struct {
		Projects map[string]struct {
			HasTrustDialogAccepted bool `json:"hasTrustDialogAccepted"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(data, &record); err != nil {
		return false, fmt.Errorf("%s is not Claude Code's JSON record: %w", path, err)
	}
	candidates := []string{filepath.Clean(dir)}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil && resolved != candidates[0] {
		candidates = append(candidates, resolved)
	}
	for _, d := range candidates {
		for {
			if record.Projects[d].HasTrustDialogAccepted {
				return true, nil
			}
			parent := filepath.Dir(d)
			if parent == d {
				break
			}
			d = parent
		}
	}
	return false, nil
}
