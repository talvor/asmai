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
