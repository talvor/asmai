// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// ClaudeCodeAuthStatus reads only the sign-in state and method reported by
// the pinned CLI. It does not retain the account in the status response.
func ClaudeCodeAuthStatus(ctx context.Context, path string) (bool, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "auth", "status", "--json")
	cmd.Env = append(os.Environ(), "DISABLE_AUTOUPDATER=1")
	out, _ := cmd.Output()
	if ctx.Err() != nil {
		return false, "", fmt.Errorf("Claude Code did not report its sign-in within 30s")
	}
	var status struct {
		LoggedIn   *bool  `json:"loggedIn"`
		AuthMethod string `json:"authMethod"`
	}
	if json.Unmarshal(out, &status) != nil || status.LoggedIn == nil {
		return false, "", fmt.Errorf("Claude Code did not report its sign-in status")
	}
	return *status.LoggedIn, status.AuthMethod, nil
}
