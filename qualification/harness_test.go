// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/talvor/asmai"
	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/fakeprovider"
	"github.com/talvor/asmai/internal/providers"
	"github.com/talvor/asmai/internal/statedir"
	"github.com/talvor/asmai/internal/store"
)

// asmaiBin is the asmai executable the tests' harness qualifies, and
// fakeProvider the scripted fake it installs as the pinned Claude Code. The
// fake stands in only for the harness's own tests: it never qualifies
// anything.
var asmaiBin, fakeProvider string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "qualify-test-bin")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	asmaiBin = filepath.Join(dir, "asmai")
	fakeProvider = filepath.Join(dir, "fake-provider")
	for out, pkg := range map[string]string{asmaiBin: "../cmd/asmai", fakeProvider: "../internal/fakeprovider/cmd/fake-provider"} {
		build := exec.Command("go", "build", "-o", out, pkg)
		build.Stdout, build.Stderr = os.Stderr, os.Stderr
		if err := build.Run(); err != nil {
			fmt.Fprintln(os.Stderr, "building", pkg+":", err)
			os.Exit(1)
		}
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// staffing staffs every role M1 runs on Claude.
const staffing = `[roles.coordination]
leader_provider = "claude"
leader_model = "opus"
worker_provider = "claude"
worker_model = "opus"

[roles.engineering]
leader_provider = "claude"
leader_model = "opus"
worker_provider = "claude"
worker_model = "opus"

[roles.quality]
leader_provider = "claude"
leader_model = "opus"
worker_provider = "claude"
worker_model = "opus"
`

// fakeClaude stands the scripted fake in for the pinned Claude Code, with the
// sign-in status the test gives it.
type fakeClaude struct {
	auth    AuthStatus
	authErr error
}

func (fakeClaude) Name() string { return "Claude Code" }

// Install puts the fake where the pins file puts the pinned copy and has the
// daemon record it, as `asmai providers install` does for the real one.
func (fakeClaude) Install(_ context.Context, f *Factory, pins providers.Pins) error {
	platform, _ := providers.Platform(runtime.GOOS, runtime.GOARCH)
	plan, err := pins.ClaudeCodePlan(platform, f.paths.Providers)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(fakeProvider)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(plan.Path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(plan.Path, data, 0o755); err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	_, err = daemon.Call(f.paths.Socket, daemon.Request{Command: daemon.CommandProviderInstalled, Provider: &store.ProviderInstall{
		Name: plan.Name, Version: plan.Version, Path: plan.Path, SHA256: hex.EncodeToString(sum[:]),
	}})
	return err
}

func (fakeClaude) Version(context.Context, *Factory, string) (string, error) {
	return pinnedVersion() + " (scripted fake)", nil
}

func (c fakeClaude) AuthStatus(context.Context, *Factory, string) (AuthStatus, error) {
	return c.auth, c.authErr
}

func pinnedVersion() string {
	pins, err := providers.ParsePins(asmai.Pins)
	if err != nil {
		panic(err)
	}
	return pins.ClaudeCode.Version
}

var signedIn = fakeClaude{auth: AuthStatus{SignedIn: true, Method: "claude.ai"}}

// harness returns a harness for the test's own home directory and factory,
// whose leader plays script.
func harness(t *testing.T, provider Provider, script []string) *Harness {
	t.Helper()
	// os.MkdirTemp keeps the socket's path short enough on macOS.
	home, err := os.MkdirTemp("", "home")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Cleanup(func() { os.RemoveAll(home) })
	config := filepath.Join(home, ".config", "asmai", "config.toml")
	if err := os.MkdirAll(filepath.Dir(config), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, []byte(staffing), 0o600); err != nil {
		t.Fatal(err)
	}
	scriptFile := filepath.Join(t.TempDir(), "leader.jsonl")
	if err := os.WriteFile(scriptFile, []byte(strings.Join(script, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pins, err := providers.ParsePins(asmai.Pins)
	if err != nil {
		t.Fatal(err)
	}
	h := &Harness{
		Subject:            Subject{Commit: "0123456789abcdef0123456789abcdef01234567", Asmai: asmaiBin, Pins: pins},
		Provider:           provider,
		Paths:              statedir.At(filepath.Join(home, ".local", "state", "asmai")),
		QualificationPaths: statedir.At(filepath.Join(home, ".local", "state", "asmai", "qualification")),
		Env:                append(os.Environ(), "HOME="+home, fakeprovider.ScriptEnv+"="+scriptFile),
		Host:               "asmai-vm", User: "tester",
		Wait: 30 * time.Second, Poll: 50 * time.Millisecond,
	}
	// A failing test must not leave its daemon running.
	t.Cleanup(func() {
		for _, paths := range []statedir.Paths{h.Paths, h.QualificationPaths} {
			env := h.Env
			if paths.Dir == h.QualificationPaths.Dir {
				env = envValue(env, "ASMAI_STATE_DIR", paths.Dir)
			}
			f := NewFactory(h.Subject.Asmai, paths, env)
			f.Stop(context.Background())
		}
	})
	return h
}

// result runs the case id and returns its result.
func result(t *testing.T, h *Harness, id string) Result {
	t.Helper()
	report := h.Run(context.Background(), []string{id})
	if len(report.Results) != 1 || report.Results[0].ID != id {
		t.Fatalf("running %s reported %+v", id, report.Results)
	}
	return report.Results[0]
}

// startsAfterTrust is a leader whose directory is new to Claude Code: it
// asks the user to trust it, with "No, exit" selected first as Claude Code
// 2.1.292 does, and starts once the user moves to Yes and confirms. A key
// other than the Down arrow, such as Enter on No, ends the session.
var startsAfterTrust = []string{
	`{"screen": "\u001b[2J\u001b[HAccessing workspace:\r\n\r\n \u001b[1m\u276f No, exit\u001b[22m\r\n   Yes, I trust this folder\r\n"}`,
	`{"expect": "\u001b[B"}`,
	`{"screen": "\u001b[2J\u001b[HAccessing workspace:\r\n\r\n   No, exit\r\n \u001b[1m\u276f Yes, I trust this folder\u001b[22m\r\n"}`,
	`{"expect": "\r"}`,
	`{"hook": "SessionStart", "payload": {"session_id": "fake-session", "hook_event_name": "SessionStart", "source": "startup"}}`,
	`{"screen": "\u001b[2J\u001b[H> "}`,
	`{"expect": "only typed by a test that means to"}`,
}

// startsAfterTrustSelectedFirst is the same with Yes selected from the start,
// so the user need only confirm.
var startsAfterTrustSelectedFirst = []string{
	`{"screen": "\u001b[2J\u001b[HAccessing workspace:\r\n\r\n   No, exit\r\n \u001b[1m\u276f Yes, I trust this folder\u001b[22m\r\n"}`,
	`{"expect": "\r"}`,
	`{"hook": "SessionStart", "payload": {"session_id": "fake-session", "hook_event_name": "SessionStart", "source": "startup"}}`,
	`{"screen": "\u001b[2J\u001b[H> "}`,
	`{"expect": "only typed by a test that means to"}`,
}

// startsDirectly is a leader whose directory is already trusted.
var startsDirectly = []string{
	`{"hook": "SessionStart", "payload": {"session_id": "fake-session", "hook_event_name": "SessionStart", "source": "startup"}}`,
	`{"screen": "\u001b[2J\u001b[H> "}`,
	`{"expect": "only typed by a test that means to"}`,
}

func TestC3RefusesToAutomaticallyTrustItsDefaultDirectory(t *testing.T) {
	h := harness(t, signedIn, startsAfterTrust)

	r := result(t, h, "C3")

	if r.Outcome != Failed || !strings.Contains(r.Failure, "automatic trust acceptance is limited") {
		t.Fatalf("C3 %s: %s, want trust prompt refusal", r.Outcome, r.Failure)
	}
}

func TestAutomaticTrustAcceptanceIsLimitedToQualificationDirectoriesOnTheVM(t *testing.T) {
	h := harness(t, signedIn, startsDirectly)
	f := NewFactory(h.Subject.Asmai, h.QualificationPaths, h.Env)
	for _, address := range []string{"leader@coordination", "leader@engineering"} {
		if !h.canAutoAcceptTrust(f, address) {
			t.Errorf("authorized qualification directory %s was not allowed", address)
		}
	}
	h.Host = "Phillips-MacBook-Pro"
	if h.canAutoAcceptTrust(f, "leader@engineering") {
		t.Error("automatic trust acceptance was allowed outside asmai-vm")
	}
	h.Host = "asmai-vm"
	if h.canAutoAcceptTrust(f, "leader@quality") {
		t.Error("automatic trust acceptance was allowed for an unsupported role")
	}
	f.paths.Dir = filepath.Join(h.Paths.Dir, "other")
	f.paths.Agents = filepath.Join(f.paths.Dir, "agents")
	if h.canAutoAcceptTrust(f, "leader@engineering") {
		t.Error("automatic trust acceptance was allowed outside the qualification state")
	}
}

func TestHandoffCasesUseTheDisposableFactoryState(t *testing.T) {
	h := harness(t, signedIn, startsDirectly)
	callerState := filepath.Join(t.TempDir(), "caller-state")
	h.Env = envValue(h.Env, "ASMAI_STATE_DIR", callerState)
	for _, id := range []string{"C7", "C11"} {
		f := h.factoryForCase(Case{ID: id, Env: signedInOnly}, &Result{})
		if f.paths.Dir != h.QualificationPaths.Dir {
			t.Errorf("%s factory state = %s, want disposable state %s", id, f.paths.Dir, h.QualificationPaths.Dir)
		}
		got := ""
		for _, entry := range f.env {
			if strings.HasPrefix(entry, "ASMAI_STATE_DIR=") {
				got = strings.TrimPrefix(entry, "ASMAI_STATE_DIR=")
			}
		}
		if got != h.QualificationPaths.Dir {
			t.Errorf("%s process state environment = %q, want %s", id, got, h.QualificationPaths.Dir)
		}
	}
	f := h.factoryForCase(Case{ID: "C3", Env: signedInOnly}, &Result{})
	if f.paths.Dir != h.Paths.Dir {
		t.Errorf("C3 state = %s, want configured state %s", f.paths.Dir, h.Paths.Dir)
	}
}

func TestC3ConfirmsTheTrustPromptWithoutMovingWhenYesIsAlreadySelected(t *testing.T) {
	h := harness(t, signedIn, startsAfterTrustSelectedFirst)

	r := result(t, h, "C3")

	if r.Outcome != Failed || !strings.Contains(r.Failure, "automatic trust acceptance is limited") {
		t.Fatalf("C3 %s: %s, want trust prompt refusal", r.Outcome, r.Failure)
	}
}

func TestC3PassesWithoutATrustPromptWhenTheDirectoryIsTrusted(t *testing.T) {
	h := harness(t, signedIn, startsDirectly)

	r := result(t, h, "C3")

	if r.Outcome != Passed {
		t.Fatalf("C3 %s: %s", r.Outcome, r.Failure)
	}
	if strings.Contains(strings.Join(r.Evidence, "\n"), "trust prompt") {
		t.Errorf("C3's evidence %q mentions a trust prompt that was never shown", r.Evidence)
	}
}

func TestC3FailsWhenClaudeCodeAsksForALogin(t *testing.T) {
	h := harness(t, signedIn, []string{
		`{"screen": "\u001b[2J\u001b[HSelect login method:\r\n\u001b[1m> Claude account with subscription\u001b[22m\r\n  Anthropic Console account\r\n"}`,
		`{"expect": "typed by nothing, so a harness that presses a key at a login prompt fails the case"}`,
	})
	h.Wait = 10 * time.Second

	r := result(t, h, "C3")

	if r.Outcome != Failed || !strings.Contains(r.Failure, `asked for a login (its screen shows "select login method")`) {
		t.Errorf("C3 %s: %q, want a failure naming the login prompt", r.Outcome, r.Failure)
	}
	if strings.Contains(r.Failure, "Claude account with subscription") {
		t.Errorf("C3's failure %q shows the screen without --show-screen", r.Failure)
	}
}

func TestC3ShowsTheScreenOnlyWhenAsked(t *testing.T) {
	h := harness(t, signedIn, []string{
		`{"screen": "\u001b[2J\u001b[HSelect login method:\r\n  Claude account with subscription\r\n"}`,
		`{"expect": "never"}`,
	})
	h.ShowScreen = true

	r := result(t, h, "C3")

	if r.Outcome != Failed || !strings.Contains(r.Failure, "Claude account with subscription") {
		t.Errorf("C3 %s: %q, want the screen in the failure", r.Outcome, r.Failure)
	}
}

func TestC3FailsWhenTheSessionNeverReportsSessionStart(t *testing.T) {
	h := harness(t, signedIn, []string{`{"screen": "\u001b[2J\u001b[Hstarting..."}`, `{"expect": "never"}`})
	h.Wait = time.Second

	r := result(t, h, "C3")

	if r.Outcome != Failed || !strings.Contains(r.Failure, "reported no SessionStart within 1s") {
		t.Errorf("C3 %s: %q, want a failure for the missing SessionStart", r.Outcome, r.Failure)
	}
}

func TestC3FailsWhenTheLeaderEndsBeforeItStarts(t *testing.T) {
	h := harness(t, signedIn, []string{`{"screen": "bye"}`, `{"run": "sleep 1"}`})

	r := result(t, h, "C3")

	if r.Outcome != Failed || !strings.Contains(r.Failure, "ended before it reported SessionStart") {
		t.Errorf("C3 %s: %q, want a failure for a session that ended", r.Outcome, r.Failure)
	}
}

func TestC3FailsWhenThePinnedCopyIsNotSignedInOrNotOnTheSubscription(t *testing.T) {
	for name, tc := range map[string]struct {
		auth AuthStatus
		want string
	}{
		"signed out": {AuthStatus{SignedIn: false}, "not signed in"},
		"api key":    {AuthStatus{SignedIn: true, Method: "api_key"}, `signed in with "api_key", which is not the user's subscription`},
	} {
		t.Run(name, func(t *testing.T) {
			h := harness(t, fakeClaude{auth: tc.auth}, startsDirectly)

			r := result(t, h, "C3")

			if r.Outcome != Failed || !strings.Contains(r.Failure, tc.want) {
				t.Errorf("C3 %s: %q, want a failure saying %q", r.Outcome, r.Failure, tc.want)
			}
		})
	}
}

// A provider that gives no sign-in status is a missing signal; AsmAI's safe
// default covers it when the session itself starts signed in, so the case
// passes and lists the limitation.
func TestC3ListsAMissingSignInStatusAsAKnownLimitationNotAFailure(t *testing.T) {
	h := harness(t, fakeClaude{authErr: ErrSignalMissing}, startsDirectly)

	r := result(t, h, "C3")

	if r.Outcome != Passed {
		t.Fatalf("C3 %s: %s", r.Outcome, r.Failure)
	}
	if len(r.Limitations) != 1 || !strings.Contains(r.Limitations[0].Signal, "sign-in status") || r.Limitations[0].SafeDefault == "" {
		t.Errorf("C3's known limitations are %+v, want the missing sign-in status with its safe default", r.Limitations)
	}
}

// ... but only when the session itself does start signed in.
func TestAMissingSignalDoesNotPassACaseWhoseSessionNeverStarts(t *testing.T) {
	h := harness(t, fakeClaude{authErr: ErrSignalMissing}, []string{`{"screen": "\u001b[2J\u001b[HSelect login method:\r\n"}`, `{"expect": "never"}`})

	r := result(t, h, "C3")

	if r.Outcome != Failed {
		t.Errorf("C3 %s with a missing signal and a login prompt, want failed", r.Outcome)
	}
}

func TestC4PassesWhenTheSessionHasNoAPIKeyVariableThoughTheDaemonDoes(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reading the agent session's environment with ps is verified on the Mac qualification host")
	}
	h := harness(t, signedIn, startsDirectly)

	r := result(t, h, "C4")

	if r.Outcome != Passed {
		t.Fatalf("C4 %s: %s", r.Outcome, r.Failure)
	}
	evidence := strings.Join(r.Evidence, "\n")
	for _, want := range []string{"the daemon (pid ", "provider API-key variables in its environment, each a canary", "none of them a provider API-key variable"} {
		if !strings.Contains(evidence, want) {
			t.Errorf("C4's evidence\n%s\ndoes not say %q", evidence, want)
		}
	}
	if strings.Contains(evidence, canaryValue) {
		t.Errorf("C4's evidence %q holds a variable's value", evidence)
	}
}

func TestC4FailsNamingTheVariablesASessionLeaks(t *testing.T) {
	leaked := leakedVariables([]string{"HOME", "PATH", "ASMAI_SESSION", "ANTHROPIC_API_KEY", "OPENAI_ORG_API_KEY", "CODEX_QUALIFICATION_API_KEY", "OPENAI_BASE_URL"}, canaryVariables())

	if want := []string{"ANTHROPIC_API_KEY", "OPENAI_ORG_API_KEY", "CODEX_QUALIFICATION_API_KEY"}; !slices.Equal(leaked, want) {
		t.Errorf("leaked variables are %q, want %q", leaked, want)
	}
	if got := leakedVariables([]string{"HOME", "PATH", "ASMAI_SESSION", "OPENAI_BASE_URL"}, canaryVariables()); len(got) != 0 {
		t.Errorf("a session with no API-key variable leaks %q", got)
	}
}

func TestEveryCanaryIsAProviderAPIKeyVariable(t *testing.T) {
	for _, name := range canaryVariables() {
		if !providers.IsAPIKeyVariable(name) {
			t.Errorf("the canary %s is not a provider API-key variable, so a session without it proves nothing", name)
		}
	}
}

func TestACaseStartsFromAnEnvironmentWithoutTheHarnessUsersOwnAPIKeysOrToken(t *testing.T) {
	own := []string{"HOME=/home/u", "ANTHROPIC_API_KEY=real", "OPENAI_API_KEY=real", "CLAUDE_CODE_OAUTH_TOKEN=real", "PATH=/usr/bin"}
	var r Result

	env := signedInOnly(own, &r)

	if want := []string{"HOME=/home/u", "PATH=/usr/bin"}; !slices.Equal(env, want) {
		t.Errorf("C3's environment is %q, want %q", env, want)
	}
	if len(r.Evidence) != 1 || !strings.Contains(r.Evidence[0], "ANTHROPIC_API_KEY, OPENAI_API_KEY, CLAUDE_CODE_OAUTH_TOKEN") || strings.Contains(r.Evidence[0], "real") {
		t.Errorf("C3 recorded %q, want the removed names and no value", r.Evidence)
	}

	canaried := withAPIKeyCanaries(own, &r)
	for _, kv := range canaried {
		if name, value, _ := strings.Cut(kv, "="); providers.IsAPIKeyVariable(name) && value != canaryValue {
			t.Errorf("C4's environment keeps the user's own value in %s", name)
		}
	}
	for _, name := range canaryVariables() {
		if !slices.Contains(canaried, name+"="+canaryValue) {
			t.Errorf("C4's environment lacks the canary %s", name)
		}
	}
}

func TestOnlyTheCasesAskedForRun(t *testing.T) {
	h := harness(t, signedIn, startsDirectly)

	report := h.Run(context.Background(), []string{"C3"})

	if len(report.Results) != 1 || report.Results[0].ID != "C3" {
		t.Errorf("running C3 reported %+v", report.Results)
	}
	if report.Commit != h.Subject.Commit || report.Platform != runtime.GOOS+"/"+runtime.GOARCH || report.Provider.Pinned != h.Subject.Pins.ClaudeCode.Version || report.Provider.Reported == "" {
		t.Errorf("the report is for %+v, want the commit, platform and pinned and reported provider versions", report)
	}
}

func TestACasesOwnFailureIsReported(t *testing.T) {
	h := harness(t, signedIn, startsDirectly)
	broken := Case{ID: "X", Title: "x", Env: func(env []string, _ *Result) []string { return env }, Run: func(*Harness, context.Context, *Factory, *Result) error {
		return errors.New("it went wrong")
	}}

	r := h.runCase(context.Background(), broken)

	if r.Outcome != Failed || r.Failure != "it went wrong" {
		t.Errorf("the case %s: %q, want its own failure", r.Outcome, r.Failure)
	}
}

func TestSelectedOptionIsTheLineTheMarkerIsOn(t *testing.T) {
	lines := []string{"Accessing workspace:", " ❯ No, exit", "   Yes, I trust this folder"}
	if got := selectedOption(lines); got != "no, exit" {
		t.Errorf("selectedOption = %q, want no, exit", got)
	}
	if got := selectedOption([]string{"no marker here"}); got != "" {
		t.Errorf("selectedOption of a screen with no marker = %q, want none", got)
	}
}
