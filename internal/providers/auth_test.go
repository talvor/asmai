// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestClaudeCodeAuthStatusReadsOnlySignInStateAndMethod(t *testing.T) {
	for _, tc := range []struct {
		name, answer, method string
		loggedIn             bool
		errorExpected        bool
	}{
		{"subscription", `{"loggedIn":true,"authMethod":"claude.ai","account":"private@example.com"}`, "claude.ai", true, false},
		{"api key", `{"loggedIn":true,"authMethod":"apiKey"}`, "apiKey", true, false},
		{"signed out", `{"loggedIn":false,"authMethod":""}`, "", false, false},
		{"missing signal", `{"authMethod":"claude.ai"}`, "", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "claude")
			program := "#!/bin/sh\nprintf '%s\\n' '" + tc.answer + "'\n"
			if err := os.WriteFile(path, []byte(program), 0o700); err != nil {
				t.Fatal(err)
			}
			loggedIn, method, err := ClaudeCodeAuthStatus(context.Background(), path)
			if loggedIn != tc.loggedIn || method != tc.method || (err != nil) != tc.errorExpected {
				t.Errorf("auth status = %t, %q, %v", loggedIn, method, err)
			}
		})
	}
}

func TestClaudeCodeAuthStatusUsesAgentCredentialEnvironment(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "key")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "token")
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "1")
	t.Setenv("ANTHROPIC_EXTRA_API_KEY", "another-key")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "subscription")
	path := filepath.Join(t.TempDir(), "claude")
	program := `#!/bin/sh
if [ -n "${ANTHROPIC_API_KEY-}" ] || [ -n "${ANTHROPIC_AUTH_TOKEN-}" ] || [ -n "${CLAUDE_CODE_USE_BEDROCK-}" ] || [ -n "${ANTHROPIC_EXTRA_API_KEY-}" ]; then
  printf '%s\n' '{"loggedIn":true,"authMethod":"apiKey"}'
elif [ "$DISABLE_AUTOUPDATER" = 1 ] && [ "$CLAUDE_CODE_OAUTH_TOKEN" = subscription ]; then
  printf '%s\n' '{"loggedIn":true,"authMethod":"claude.ai"}'
fi
`
	if err := os.WriteFile(path, []byte(program), 0o700); err != nil {
		t.Fatal(err)
	}
	loggedIn, method, err := ClaudeCodeAuthStatus(context.Background(), path)
	if err != nil || !loggedIn || method != "claude.ai" {
		t.Errorf("auth status with exported API credentials = %t, %q, %v, want subscription", loggedIn, method, err)
	}
}
