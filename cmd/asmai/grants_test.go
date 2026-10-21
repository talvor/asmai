// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTheUserGivesAGrantInTheConversationAndListsAndRevokesIt(t *testing.T) {
	testDir := t.TempDir()
	t.Setenv("ASMAI_TEST_DIR", testDir)
	script := []string{
		`{"screen": "> "}`,
		`{"expect": "Please fix the fixture\r"}`,
		`{"hook": "UserPromptSubmit", "payload": {"hook_event_name": "UserPromptSubmit", "prompt": "Please fix the fixture"}}`,
		fakeRun(`asmai job open --message latest --repository fixture --reading 'Fix the fixture' --criterion 'Tests pass'`, 0, "Opened job 1"),
		// Nothing is granted yet: spending is zero, and a grant needs the
		// user's own words to cite.
		fakeRun(`asmai authorize --job 1 --action spend --amount 5 --currency USD --what 'a staging database'`, 1, "spending budget is zero"),
		fakeRun(`asmai decision record --grant --scope job --job 1 --action deploy --approved 'deploy the build to staging' --condition none`, 1, "cites the witnessed message"),
		fakeRun(`asmai decision record --grant --scope job --job 1 --action deploy --approved 'deploy the build to staging' --condition none --cites 1`, 1, "not a witnessed message"),
		fakeRun(`asmai decision record --grant --scope job --job 1 --action merge --approved 'merge the pull request' --condition none --cites latest`, 1, "no grant can authorize a merge"),
		fakeRun(`touch "$ASMAI_TEST_DIR/asked"`, 0, ""),
		`{"expect": "You may deploy the build to staging for this job while the tests pass\r"}`,
		`{"hook": "UserPromptSubmit", "payload": {"hook_event_name": "UserPromptSubmit", "prompt": "You may deploy the build to staging for this job while the tests pass"}}`,
		fakeRun(`asmai decision record --grant --scope job --job 1 --action deploy --approved 'deploy the build to staging' --condition 'the tests pass' --cites latest`, 0, "(?s)recorded grant 1 \\| job 1 \\| deploy \\| active \\| recorded by leader@coordination.*Approved: deploy the build to staging.*Conditions: the tests pass.*The user's words: You may deploy the build to staging for this job"),
		fakeRun(`asmai authorize --job 1 --action deploy --what 'build 7 to staging'`, 0, "authorized deploy in job 1 by grant 1 \\(authorization 1\\): build 7 to staging"),
		fakeRun(`asmai authorize --job 1 --action destructive --what 'drop the table'`, 1, "no grant covers destructive in job 1"),
		fakeRun(`asmai authorize --job 1 --action merge --what 'the pull request'`, 1, "AsmAI never merges"),
		fakeRun(`asmai brief 1`, 0, "(?s)Grants \\(.*- grant 1 \\| job 1 \\| deploy \\| active"),
		// Listing and revoking grants are the user's.
		fakeRun(`asmai grants`, 1, "ask the user to run `asmai grants`"),
		fakeRun(`asmai grant revoke 1`, 1, "ask the user to run `asmai grant revoke 1`"),
		fakeRun(`touch "$ASMAI_TEST_DIR/granted"`, 0, ""),
		fakeRun(`while [ ! -e "$ASMAI_TEST_DIR/revoked" ]; do sleep 0.02; done; asmai authorize --job 1 --action deploy --what 'build 8 to staging'`, 1, "grant 1 was revoked by the user"),
		fakeRun(`touch "$ASMAI_TEST_DIR/refused"`, 0, ""),
		`{"expect": "only typed by a test that means to"}`,
	}
	stateDir := readyFactory(t, script)
	_ = stateDir
	origin := filepath.Join(t.TempDir(), "fixture.git")
	gitIn(t, t.TempDir(), "init", "--bare", "-b", "main", origin)
	work := t.TempDir()
	gitIn(t, work, "clone", "--quiet", origin, "fixture")
	checkout := filepath.Join(work, "fixture")
	gitIn(t, checkout, "commit", "--allow-empty", "-m", "initial")
	gitIn(t, checkout, "push", "--quiet", "origin", "HEAD:main")
	if _, stderr, code := runAsmai(t, "repo", "add", origin); code != 0 {
		t.Fatalf("repo add: %s", stderr)
	}
	a := inTerminal(t, 100, 25)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("Coordination's terminal:\n%s", strings.Join(a.lines(), "\n"))
		}
	})
	waitFor(t, "Coordination's prompt", a.shows(">"))
	a.ptmx.WriteString("Please fix the fixture\r")
	waitFor(t, "Coordination to ask for the grant", touched(testDir, "asked"))
	a.ptmx.WriteString("You may deploy the build to staging for this job while the tests pass\r")
	waitFor(t, "the grant to be recorded and used", touched(testDir, "granted"))

	// The user lists the grant, with their own words beside it.
	stdout, stderr, code := runAsmai(t, "grants")
	if code != 0 {
		t.Fatalf("asmai grants: %d, %s", code, stderr)
	}
	for _, want := range []string{"ID", "job 1", "deploy", "deploy the build to staging", "active", "You may deploy the build to staging for this job while the tests pass", "asmai grant revoke <id>"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("asmai grants did not show %q:\n%s", want, stdout)
		}
	}
	var listed struct {
		Grants []struct {
			ID    int64  `json:"id"`
			Scope string `json:"scope"`
			State string `json:"state"`
			Words string `json:"words"`
		} `json:"grants"`
	}
	asmaiJSON(t, &listed, "grants")
	if len(listed.Grants) != 1 || listed.Grants[0].Scope != "job" || listed.Grants[0].State != "active" || !strings.HasPrefix(listed.Grants[0].Words, "You may deploy") {
		t.Fatalf("grants = %+v", listed.Grants)
	}

	// Revoking one stops later actions and reports the earlier one as it stands.
	if _, stderr, code := runAsmai(t, "grant", "revoke", "9"); code != 1 || !strings.Contains(stderr, "does not exist") {
		t.Fatalf("revoking a grant that does not exist: %d, %s", code, stderr)
	}
	stdout, stderr, code = runAsmai(t, "grant", "revoke", "1")
	if code != 0 {
		t.Fatalf("asmai grant revoke 1: %d, %s", code, stderr)
	}
	for _, want := range []string{"grant 1 revoked: every later action it covers is refused", "stands as it is; nothing is undone", "authorization 1 | deploy | job 1 | build 7 to staging | by leader@coordination"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("asmai grant revoke did not show %q:\n%s", want, stdout)
		}
	}
	if err := os.WriteFile(filepath.Join(testDir, "revoked"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the later action to be refused", touched(testDir, "refused"))
	if _, stderr, code := runAsmai(t, "grant", "revoke", "1"); code != 1 || !strings.Contains(stderr, "already revoked") {
		t.Fatalf("revoking twice: %d, %s", code, stderr)
	}
	stdout, _, _ = runAsmai(t, "grants")
	if !strings.Contains(stdout, "revoked") {
		t.Errorf("asmai grants after the revocation:\n%s", stdout)
	}
	if got := journaled(t, "grant.recorded"); len(got) != 1 {
		t.Errorf("%d grants journaled, want 1", len(got))
	}
	if got := journaled(t, "grant.revoked"); len(got) != 1 {
		t.Errorf("%d revocations journaled, want 1", len(got))
	}
	if got := journaled(t, "action.authorized"); len(got) != 1 {
		t.Errorf("%d authorizations journaled, want 1", len(got))
	}
	// The user does not run the agents' commands.
	if _, stderr, code := runAsmai(t, "authorize", "--job", "1", "--action", "deploy", "--what", "x"); code != 1 || !strings.Contains(stderr, "agent command") {
		t.Fatalf("the user authorizing an action: %d, %s", code, stderr)
	}
}
