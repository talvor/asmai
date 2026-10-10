// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/providers"
	"github.com/talvor/asmai/internal/statedir"
	"github.com/talvor/asmai/internal/store"
)

// Harness runs the qualification cases against the real pinned provider, with
// the harness user's own factory.
type Harness struct {
	Subject  Subject
	Provider Provider
	// Paths is the configured state directory used by C3 and C4.
	Paths statedir.Paths
	// QualificationPaths is the disposable state directory used by C7, C11
	// and C19.
	QualificationPaths statedir.Paths
	// Env is the harness's own environment, which each case's factory starts
	// from.
	Env []string
	// Host and User name the machine and the OS user the harness runs as.
	Host, User string
	// Wait is how long a case waits for the provider's session to get going,
	// and Poll how often it looks.
	Wait, Poll time.Duration
	// ShowScreen includes the leader's last screen in a failure. It may show
	// the signed-in account, so it is the operator's choice.
	ShowScreen bool
	// Log receives a line for each step.
	Log io.Writer
	// Number, when set, numbers each case's exercise in place of the time,
	// so that a scripted provider can expect the request it is given.
	Number func() int64

	reported string
}

func (h *Harness) logf(format string, args ...any) {
	if h.Log != nil {
		fmt.Fprintf(h.Log, format+"\n", args...)
	}
}

// A Case is one qualification case, numbered as in the specification's list
// (11 rule 24).
type Case struct {
	ID    string
	Title string
	// Env returns the environment of the case's factory, and so of its
	// daemon, from the harness's own.
	Env func(env []string, r *Result) []string
	// Run runs the case in f, the case's own factory. It returns why the
	// case failed, or nil when it passed.
	Run func(h *Harness, ctx context.Context, f *Factory, r *Result) error
}

// Cases are the qualification cases implemented so far.
var Cases = []Case{
	{
		ID:    "C3",
		Title: "The pinned provider copy reuses the user's existing sign-in without a new login",
		Env:   signedInOnly,
		Run:   (*Harness).c3,
	},
	{
		ID:    "C4",
		Title: "Every agent session starts without provider API-key variables",
		Env:   withAPIKeyCanaries,
		Run:   (*Harness).c4,
	},
	{ID: "C7", Title: "Every automated submission has a correlated positive acknowledgment", Env: signedInOnly, Run: (*Harness).c7},
	{ID: "C11", Title: "Witnessed user messages are distinct from daemon nudges", Env: signedInOnly, Run: (*Harness).c11},
	{ID: "C19", Title: "Dispatches and results stay correlated and reconcilable across restarts", Env: signedInOnly, Run: (*Harness).c19},
	{ID: "C36", Title: "Instruction files load without the repository's provider configuration", Env: signedInOnly, Run: (*Harness).c36},
	{ID: "C37", Title: "Provider write guards are switched on", Env: signedInOnly, Run: (*Harness).c37},
}

// Run runs the cases named in ids, or all of them when ids is empty, each in
// a factory of its own that is stopped afterwards, and reports them.
func (h *Harness) Run(ctx context.Context, ids []string) Report {
	report := Report{
		Commit:   h.Subject.Commit,
		Platform: runtime.GOOS + "/" + runtime.GOARCH,
		Host:     h.Host,
		User:     h.User,
		Provider: ProviderReport{Name: h.Provider.Name(), Pinned: h.Subject.Pins.ClaudeCode.Version},
	}
	for _, c := range Cases {
		if len(ids) > 0 && !slices.Contains(ids, c.ID) {
			continue
		}
		h.logf("running %s", c.ID)
		report.Results = append(report.Results, h.runCase(ctx, c))
	}
	report.Provider.Reported = h.reported
	return report
}

func (h *Harness) runCase(ctx context.Context, c Case) Result {
	r := Result{ID: c.ID, Title: c.Title, Outcome: Passed}
	f := h.factoryForCase(c, &r)
	err := c.Run(h, ctx, f, &r)
	// The factory is the harness user's own, so each case leaves it stopped.
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	if stopErr := f.Stop(stopCtx); stopErr != nil {
		err = errors.Join(err, fmt.Errorf("stopping the factory afterwards: %w", stopErr))
	}
	if err != nil {
		r.Outcome, r.Failure = Failed, err.Error()
	}
	return r
}

func (h *Harness) factoryForCase(c Case, r *Result) *Factory {
	paths := h.Paths
	env := c.Env(h.Env, r)
	if c.ID == "C7" || c.ID == "C11" || c.ID == "C19" {
		paths = h.QualificationPaths
		env = envValue(env, "ASMAI_STATE_DIR", paths.Dir)
	}
	return NewFactory(h.Subject.Asmai, paths, env)
}

// withoutAPIKeys returns environ less any provider API-key variable, and the
// names it removed.
func withoutAPIKeys(environ []string) (env, removed []string) {
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if providers.IsAPIKeyVariable(name) {
			removed = append(removed, name)
			continue
		}
		env = append(env, kv)
	}
	return env, removed
}

// oauthTokenVariable is the variable that signs Claude Code in with a token
// instead of the sign-in the user made. AsmAI leaves it alone (03 rule 6), so
// C3 removes it to show that the existing sign-in alone serves.
const oauthTokenVariable = "CLAUDE_CODE_OAUTH_TOKEN"

// signedInOnly is C3's environment: the harness's own, without provider
// API-key variables or a sign-in token, so that nothing but the user's
// existing sign-in can serve the session.
func signedInOnly(environ []string, r *Result) []string {
	env, removed := withoutAPIKeys(environ)
	env = slices.DeleteFunc(env, func(kv string) bool {
		if name, _, _ := strings.Cut(kv, "="); name == oauthTokenVariable {
			removed = append(removed, name)
			return true
		}
		return false
	})
	if len(removed) > 0 {
		r.observe("the harness's own environment held %s, which it removed so that only the existing sign-in could serve", strings.Join(removed, ", "))
	}
	return env
}

// canaryValue is what each canary variable holds. It is not a key, so a
// session that used one would fail to sign in rather than spend anything.
const canaryValue = "asmai-qualification-canary-not-a-key"

// canaryVariables are the provider API-key variables C4 sets in the daemon's
// environment: every name AsmAI lists, and one for each prefix whose
// *_API_KEY names it covers by pattern.
func canaryVariables() []string {
	names := providers.APIKeyVariables()
	for _, prefix := range []string{"ANTHROPIC_", "OPENAI_", "CODEX_", "CLAUDE_"} {
		names = append(names, prefix+"QUALIFICATION_API_KEY")
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// withAPIKeyCanaries is C4's environment: the harness's own without provider
// API-key variables, then a canary in every API-key variable.
func withAPIKeyCanaries(environ []string, r *Result) []string {
	env, _ := withoutAPIKeys(environ)
	for _, name := range canaryVariables() {
		env = append(env, name+"="+canaryValue)
	}
	return env
}

// ready starts the factory with Coordination's leader running the pinned
// Claude Code, installing it first if it is not installed, and returns where
// it is. The leader's start is what the case then watches.
func (h *Harness) ready(ctx context.Context, f *Factory) (path string, err error) {
	platform, ok := providers.Platform(runtime.GOOS, runtime.GOARCH)
	if !ok {
		return "", fmt.Errorf("%s/%s is not a platform Claude Code is published for", runtime.GOOS, runtime.GOARCH)
	}
	plan, err := h.Subject.Pins.ClaudeCodePlan(platform, f.paths.Providers)
	if err != nil {
		return "", err
	}
	// The first start runs the daemon, which installing needs. It does not
	// start the leader if the pinned Claude Code is not installed yet.
	res, code, err := f.Start(ctx)
	if err != nil {
		return "", err
	}
	if _, running := f.Running(); !running {
		return "", fmt.Errorf("the factory's daemon did not start: %s", res.Error)
	}
	if code != 0 {
		h.logf("installing the pinned Claude Code %s", plan.Version)
		if err := h.Provider.Install(ctx, f, h.Subject.Pins); err != nil {
			return "", err
		}
		if res, code, err = f.Start(ctx); err != nil {
			return "", err
		}
		if code != 0 {
			return "", fmt.Errorf("asmai start failed its checks: %s", failedChecks(res.Checks))
		}
	}
	reported, err := h.Provider.Version(ctx, f, plan.Path)
	if err != nil {
		return "", err
	}
	h.reported = reported
	if !strings.HasPrefix(reported, plan.Version) {
		return "", fmt.Errorf("the installed copy at %s reports %q, not the pinned %s", plan.Path, reported, plan.Version)
	}
	return plan.Path, nil
}

// c3 qualifies that the pinned copy reuses the user's existing sign-in
// without a new login (03 rule 6): it starts Coordination's leader on the
// pinned Claude Code in the harness user's own factory, which neither starts
// a sign-in nor passes a token, and requires the session to come up signed
// in, with no login prompt.
func (h *Harness) c3(ctx context.Context, f *Factory, r *Result) error {
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
	r.observe("%s %s is installed as the pinned copy; the installed copy reports %s", h.Provider.Name(), h.Subject.Pins.ClaudeCode.Version, h.reported)

	switch auth, err := h.Provider.AuthStatus(ctx, f, path); {
	case errors.Is(err, ErrSignalMissing):
		r.limit("the pinned copy's own sign-in status",
			"AsmAI reads the session itself: a session that is not signed in reports no SessionStart through its hook, and AsmAI types into an agent only what the user types")
	case err != nil:
		return err
	case !auth.SignedIn:
		return errors.New("the pinned copy reports it is not signed in: sign it in as this OS user with its own login, then run the harness again")
	case !subscriptionMethods[auth.Method]:
		return fmt.Errorf("the pinned copy is signed in with %q, which is not the user's subscription", auth.Method)
	default:
		r.observe("the pinned copy's own status check reports it signed in with %s, in an environment without API-key variables", auth.Method)
	}

	pressed, err := h.awaitSessionStart(ctx, f, leader)
	if err != nil {
		return err
	}
	if pressed > 0 {
		r.observe("the session first waited at Claude Code's trust prompt for its new directory, which comes after sign-in; the harness answered it as a user does")
	}
	r.observe("%s (generation %d, pid %d) reported SessionStart through its hook, with no login prompt on its screen", leader.Agent, leader.Generation, leader.PID)
	return nil
}

// awaitSessionStart waits until leader's session reports SessionStart: the
// session started, which Claude Code does only once it is signed in. A new
// directory first asks the user to trust it, which the harness answers as a
// user would: it moves to "Yes, I trust this folder" if the prompt starts
// elsewhere, and confirms only while that option is the selected one, so it
// never chooses to exit. A login prompt, or a session that ends, fails the
// case. It returns how many times it confirmed the trust prompt.
func (h *Harness) awaitSessionStart(ctx context.Context, f *Factory, leader store.Agent) (pressed int, err error) {
	const maxKeys, keyGap = 8, time.Second
	keys := 0
	var lastKey time.Time
	var lines []string
	screenNote := func() string {
		if h.ShowScreen {
			return "\nthe leader's last screen:\n" + strings.Join(lines, "\n")
		}
		return " (run with --show-screen to see the screen; it may show the signed-in account)"
	}
	deadline := time.Now().Add(h.Wait)
	for {
		if err := ctx.Err(); err != nil {
			return pressed, err
		}
		current, ok, err := f.Leader()
		if err != nil {
			return pressed, err
		}
		running := ok && current.State == store.AgentRunning && current.Generation == leader.Generation
		if running {
			// The last screen seen is what a session that then ends leaves to
			// explain itself.
			// The session may end between reading the leader and its screen.
			if screen, err := f.Screen(); err == nil {
				lines = screen
			} else if current, ok, lerr := f.Leader(); lerr != nil || (ok && current.State == store.AgentRunning && current.Generation == leader.Generation) {
				return pressed, err
			} else {
				running = false
			}
		}
		if !running {
			return pressed, fmt.Errorf("the leader's session ended before it reported SessionStart%s%s", exitNote(current.Exit), screenNote())
		}
		events, err := f.Observed(leader.Agent, leader.Generation)
		if err != nil {
			return pressed, err
		}
		if marker, found := screenHas(lines, loginPromptMarkers...); found {
			return pressed, fmt.Errorf("Claude Code asked for a login (its screen shows %q) instead of starting signed in%s", marker, screenNote())
		}
		if slices.Contains(events, "SessionStart") {
			return pressed, nil
		}
		if _, found := screenHas(lines, trustPromptMarker); found && keys < maxKeys && time.Since(lastKey) >= keyGap {
			if !h.canAutoAcceptTrust(f, leader.Agent) {
				return pressed, errors.New("Claude Code needs the Coordination qualification directory trusted; automatic trust acceptance is limited to the authorized asmai-vm qualification directory")
			}
			switch selected := selectedOption(lines); {
			case strings.Contains(selected, trustPromptMarker):
				h.logf("confirming Claude Code's trust prompt")
				err = f.Press("\r")
				pressed++
			case selected != "":
				h.logf("moving Claude Code's trust prompt to %q", trustPromptMarker)
				err = f.Press(keyDown)
			}
			if err != nil {
				return pressed, err
			}
			keys++
			lastKey = time.Now()
		}
		if time.Now().After(deadline) {
			return pressed, fmt.Errorf("the leader reported no SessionStart within %s and showed no login prompt%s", h.Wait, screenNote())
		}
		time.Sleep(h.Poll)
	}
}

// c4 qualifies that every agent session starts without provider API-key
// variables (03 rule 8): with canaries set in the daemon's own environment,
// it reads the names of the variables the running session holds from the
// operating system.
func (h *Harness) c4(ctx context.Context, f *Factory, r *Result) error {
	canaries := canaryVariables()
	if _, err := h.ready(ctx, f); err != nil {
		return err
	}

	daemonPID, _ := f.Running()
	daemonEnv, err := processEnvNames(daemonPID)
	if err != nil {
		return err
	}
	if missing := difference(canaries, daemonEnv); len(missing) > 0 {
		return fmt.Errorf("the harness could not set %s in the daemon's environment, so it cannot show that the session lacks them", strings.Join(missing, ", "))
	}
	r.observe("the daemon (pid %d) started with %d provider API-key variables in its environment, each a canary that is not a key", daemonPID, len(canaries))

	leader, ok, err := f.Leader()
	if err != nil {
		return err
	}
	if !ok || leader.State != store.AgentRunning || leader.PID == 0 {
		return fmt.Errorf("Coordination's leader is not running after asmai start%s", exitNote(leader.Exit))
	}
	sessionEnv, err := processEnvNames(leader.PID)
	if err != nil {
		return err
	}
	if !slices.Contains(sessionEnv, daemon.SessionCredential) {
		return fmt.Errorf("the process read (pid %d) has no %s, so it is not the agent session", leader.PID, daemon.SessionCredential)
	}
	if leaked := leakedVariables(sessionEnv, canaries); len(leaked) > 0 {
		return fmt.Errorf("the agent session started with provider API-key variables in its environment: %s", strings.Join(leaked, ", "))
	}
	r.observe("%s (generation %d, pid %d), the only agent session M1 runs, started with %d environment variables, none of them a provider API-key variable", leader.Agent, leader.Generation, leader.PID, len(sessionEnv))
	return nil
}

// exitNote says how a session ended, if it is known.
func exitNote(exit string) string {
	if exit == "" {
		return ""
	}
	return " (" + exit + ")"
}

// leakedVariables returns the names in a session's environment that are
// provider API-key variables, or the canaries the harness set.
func leakedVariables(sessionEnv, canaries []string) []string {
	var leaked []string
	for _, name := range sessionEnv {
		if providers.IsAPIKeyVariable(name) || slices.Contains(canaries, name) {
			leaked = append(leaked, name)
		}
	}
	return leaked
}

// difference returns the names in want that are not in have.
func difference(want, have []string) []string {
	var missing []string
	for _, name := range want {
		if !slices.Contains(have, name) {
			missing = append(missing, name)
		}
	}
	return missing
}
