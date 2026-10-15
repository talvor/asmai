// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/talvor/asmai/internal/certify"
	"github.com/talvor/asmai/internal/config"
	"github.com/talvor/asmai/internal/githooks"
	"github.com/talvor/asmai/internal/providers"
	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/session"
	"github.com/talvor/asmai/internal/store"
	"github.com/talvor/asmai/internal/vt"
)

// SessionCredential is the environment variable that carries an agent
// session's credential, which its CLI calls and hooks pass back to the daemon
// so that it knows which session and generation made each call.
const SessionCredential = "ASMAI_SESSION"

// SlotVariable is the environment variable that carries a worker's slot: the
// number of its workspace, which it can use to keep its checks from colliding
// with those of workers in other workspaces.
const SlotVariable = "ASMAI_SLOT"

// The size of a new agent terminal, until an attach client gives its own.
const (
	terminalColumns = 80
	terminalRows    = 24
)

// stopGrace is how long a stopping agent session has to exit before it is
// killed.
const stopGrace = 5 * time.Second

// The longest and shortest waits before Coordination's leader is restarted
// after its session ended on its own; the wait doubles with each quick exit.
const (
	restartWait    = time.Second
	restartWaitMax = time.Minute
)

// Check is one of the checks `asmai start` runs before starting agents.
type Check struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Problem string `json:"problem,omitempty"`
	Fix     string `json:"fix,omitempty"`
}

// The input owners of an agent session.
const (
	InputAutomation = "automation"
	InputUser       = "user"
	InputUnknown    = "unknown"
)

// Leader is a role's leader as `asmai status` shows it.
type Leader struct {
	Agent      string `json:"agent"`
	State      string `json:"state"`
	Generation int    `json:"generation,omitempty"`
	PID        int    `json:"pid,omitempty"`
	// Input is who owns a running leader's input: InputAutomation, or
	// InputUser while the user has a message under way in the
	// conversation.
	Input string `json:"input,omitempty"`
	// Conversation is the user's terminal the conversation is open in,
	// when it is open with this leader.
	Conversation string `json:"conversation,omitempty"`
}

// Frame is one line of an attach stream after the daemon's answer: a State
// for the client to start again from, output to feed the terminal it made
// from the last State, the end of the session, or, once the user asked to
// leave the conversation, that they have left it.
type Frame struct {
	State  *vt.State `json:"state,omitempty"`
	Output []byte    `json:"output,omitempty"`
	Ended  string    `json:"ended,omitempty"`
	Left   bool      `json:"left,omitempty"`
	// Prompts, when set, are the native prompts agents now wait at that
	// nobody can answer, as the status line shows them: the stream sends
	// them first and whenever they change.
	Prompts *[]string `json:"prompts,omitempty"`
}

// ClientMessage is what an attach client sends the daemon after its
// request, one JSON line each: the size to make the agent's terminal when
// the client's changes, and, in the conversation, the keys the user typed or
// that they leave.
type ClientMessage struct {
	Columns int    `json:"columns,omitempty"`
	Rows    int    `json:"rows,omitempty"`
	Keys    []byte `json:"keys,omitempty"`
	Leave   bool   `json:"leave,omitempty"`
}

// How the user stops following an agent's terminal, as the journal records
// leaving the conversation.
const (
	LeftByDetaching     = "detached"
	LeftByTerminalClose = "terminal closed"
	LeftBySessionEnd    = "session ended"
)

// leader is an agent's session while it runs: a role's leader, or a worker
// that carries an assignment. It is kept under the agent's address, and the
// daemon treats the two alike when it nudges them and takes their hooks.
type leader struct {
	address    roles.Address
	session    *session.Session
	generation int
	startedAt  time.Time
	// assignment is the assignment a worker's session carries, and zero for
	// a leader's.
	assignment int64
	// restart is the timer that restarts the leader after its session ended
	// on its own, and quickExits counts the sessions in a row that ended
	// soon after they started.
	restart    *time.Timer
	quickExits int
	// userInput is set while the user owns the session's input: from the
	// first key they type in the conversation until the provider confirms
	// their message submitted, a finished turn leaves none of their Enter
	// keys that may yet submit, or they leave.
	userInput bool
	// submits and earlier count the Enter keys the user typed in the
	// conversation that the provider has not yet confirmed as a prompt
	// submission: submits those typed since the provider last finished a
	// turn, earlier those typed before, which a message queued in that turn
	// may still be submitting. Each confirmation the session reports while
	// either is above zero is the user's own message, and submitTerminal is
	// the user's terminal the last was typed in. A confirmation takes from
	// submits first, and from earlier only when submits is zero, so an Enter
	// that submitted nothing, on a menu or an empty prompt, is never
	// swapped for a newer one: it is forgotten at the next finished turn,
	// and no surplus survives two finished turns. An automated submission is
	// matched to its pending dispatch before these user Enter counts, so a
	// nudge is never recorded as a witnessed message.
	submits        int
	earlier        int
	submitTerminal string
	// ready is set only by a provider lifecycle boundary. A submission
	// attempt consumes it until its matching acknowledgment and Stop. started
	// is set once the session's own SessionStart reports it came up, which a
	// session that resumed a provider session that is gone never does.
	ready           bool
	started         bool
	inputUnknown    bool
	pendingDispatch int64
	currentDispatch int64
	currentPromptID string
	pendingPrompt   string
	transcript      string
	// prompt is the native permission prompt the session waits at that
	// nobody can answer, if it waits at one.
	prompt *WaitingPrompt
}

// submitted reports whether a prompt submission the leader's session
// reports confirms an Enter key the user typed in the conversation, and the
// user's terminal they typed it in: one typed since the last finished turn,
// or else one typed before it. Each confirmation of the user's message
// returns the input to automation.
func (l *leader) submitted() (terminal string, ok bool) {
	switch {
	case l.submits > 0:
		l.submits--
	case l.earlier > 0:
		l.earlier--
	default:
		return "", false
	}
	l.userInput = false
	return l.submitTerminal, true
}

// finished ages the user's unconfirmed Enter keys once the leader's session
// reports its turn finished: those still unconfirmed from before its previous
// turn finished submitted nothing, as a message queued then has been
// submitted since, and are forgotten. With no Enter left that may yet
// submit, the input returns to automation.
func (l *leader) finished() {
	if l.earlier > 0 && l.submits == 0 {
		l.userInput = false
	}
	l.earlier, l.submits = l.submits, 0
}

// conversation is the user's conversation with Coordination's leader: the
// session it was opened with, and the user's terminal it is open in.
type conversation struct {
	leader     *leader
	session    *session.Session
	generation int
	terminal   string
	// pasting is set inside a bracketed paste, and carry holds the start
	// of a paste's bracket that the next keys may finish.
	pasting bool
	carry   []byte
}

// sessionRef is the session an agent session's credential belongs to.
type sessionRef struct {
	address    roles.Address
	generation int
}

func (d *daemon) leader(role string) *leader {
	return d.agent(roles.LeaderOf(role))
}

// agent returns the session slot of the agent at address, making it if the
// agent has none yet. d.mu is held.
func (d *daemon) agent(address roles.Address) *leader {
	l, ok := d.leaders[address.String()]
	if !ok {
		l = &leader{address: address}
		d.leaders[address.String()] = l
	}
	return l
}

// checks runs the checks that exist before agents start: the staffing in the
// configuration file, the pinned Claude Code installed, and the user's trust
// of the directories agents start in. It returns the configuration and the
// install to start agents with.
func (d *daemon) checks() ([]Check, config.Config, store.ProviderInstall) {
	var checks []Check
	checks = append(checks, d.platformCheck())
	cfg, staffing := d.staffing()
	if len(staffing) == 0 {
		checks = append(checks, Check{Name: "staffing", OK: true})
	}
	checks = append(checks, staffing...)

	pinned := d.cfg.Pins.ClaudeCode.Version
	name := "Claude Code " + pinned
	install, err := d.claudeCode(pinned)
	switch {
	case pinned == "":
		checks = append(checks, Check{Name: "Claude Code", Problem: "this asmai pins no Claude Code", Fix: "run an asmai built with its pins file"})
	case err != nil:
		checks = append(checks, Check{Name: name, Problem: err.Error(), Fix: "run `asmai providers install`"})
	default:
		checks = append(checks, Check{Name: name, OK: true})
		// A copy that is not the pinned version is refused before it runs
		// for anything, the permission rules probe included.
		version := d.versionCheck(install.Path, pinned)
		checks = append(checks, version)
		if version.OK {
			checks = append(checks, d.permissionRulesCheck(install.Path))
		}
	}
	checks = append(checks, d.trustCheck())
	return checks, cfg, install
}

// platformCheck checks that this platform is certified, so that an
// uncertified one starts no agent (02 rule 6).
func (d *daemon) platformCheck() Check {
	goos, goarch := d.cfg.GOOS, d.cfg.GOARCH
	if goos == "" || goarch == "" {
		goos, goarch = runtime.GOOS, runtime.GOARCH
	}
	return Check(certify.Platform(goos, goarch, d.cfg.Pins))
}

// versionRead is the version read from an executable, with the file it was
// read from.
type versionRead struct {
	size    int64
	modTime time.Time
	version string
}

// versionCheck checks that the Claude Code at path reports the pinned
// version. A provider CLI that updated itself fails it at the agent's next
// start, which holds the agent until the combination is qualified (03 rule 5).
// The version is read again only when the file changed.
func (d *daemon) versionCheck(path, pinned string) Check {
	report := d.cfg.ClaudeVersion
	if report == nil {
		report = providers.ClaudeCodeVersion
	}
	info, statErr := os.Stat(path)
	read := func(ctx context.Context, path string) (string, error) {
		if statErr == nil {
			if seen, ok := d.versionSeen[path]; ok && seen.size == info.Size() && seen.modTime.Equal(info.ModTime()) {
				return seen.version, nil
			}
		}
		version, err := report(ctx, path)
		if err == nil && statErr == nil {
			if d.versionSeen == nil {
				d.versionSeen = map[string]versionRead{}
			}
			d.versionSeen[path] = versionRead{size: info.Size(), modTime: info.ModTime(), version: version}
		}
		return version, err
	}
	return Check(certify.Version(context.Background(), path, pinned, read))
}

// permissionRulesCheck runs PermissionRulesCheck once per daemon run for the
// install at path; a failed check is run again at the next start.
func (d *daemon) permissionRulesCheck(path string) Check {
	d.rulesMu.Lock()
	defer d.rulesMu.Unlock()
	if d.rulesOK == path {
		return Check{Name: PermissionRulesName, OK: true}
	}
	c := PermissionRulesCheck(path, d.cfg.Paths.Dir)
	if c.OK {
		d.rulesOK = path
	}
	return c
}

// PermissionRulesName is the name of the check that Claude Code applies the
// permission rules AsmAI gives its agents.
const PermissionRulesName = "permission rules"

// PermissionRulesCheck checks, with the Claude Code at path, started in dir,
// that this account lets AsmAI's own permission rules take effect. Under a
// managed policy that applies only the managed settings' permission rules,
// an agent would stop at the first command that needs approval, which
// nobody can give. The check asks Claude Code itself, and never reads or
// writes any of its settings (ProbePermissionRules).
func PermissionRulesCheck(path, dir string) Check {
	verdict, err := providers.ProbePermissionRules(context.Background(), path, dir, os.Environ())
	if err != nil {
		return Check{Name: PermissionRulesName, Problem: err.Error(),
			Fix: "run `asmai start` again; if it persists, run `asmai providers install` to reinstall the pinned Claude Code"}
	}
	if verdict.Honoured() {
		return Check{Name: PermissionRulesName, OK: true}
	}
	return Check{Name: PermissionRulesName,
		Problem: fmt.Sprintf("Claude Code ignores the permission rules AsmAI gives its agents: this account's managed settings set %s, so an agent would stop at its first command that needs approval, which nobody can give. Ignored: %s", providers.ManagedPolicySetting, strings.Join(verdict.Ignored, ", ")),
		Fix: fmt.Sprintf("run AsmAI under an account without that policy, or ask an administrator to add AsmAI's permission rules to the managed settings' permissions.allow: %s, and the Read and Edit rules for each workspace (see Under a Claude Code managed policy in the README). AsmAI never changes managed settings",
			strings.Join(providers.CommandRules(), ", "))}
}

// trustName is the name of the check that the user trusts the directories
// agents start in.
const trustName = "directory trust"

// trustCheck checks that Claude Code's record holds the user's trust of the
// directories every agent starts in: the agents directory, which holds the
// leaders' directories, the workspaces directory, and AsmAI's clone of each
// registered repository, whose trust covers every workspace checked out from
// it, as the workspaces directory's does not. An agent started in a
// directory Claude Code does not trust waits at its question about trusting
// the folder, which only the user can answer (ADR 0011).
func (d *daemon) trustCheck() Check {
	dirs := []string{d.cfg.Paths.Agents, d.cfg.Paths.Workspaces}
	if d.store != nil {
		registered, err := d.store.Repositories()
		if err != nil {
			return Check{Name: trustName, Problem: fmt.Sprintf("reading the registered repositories: %v", err), Fix: "run `asmai start` again"}
		}
		// A clone that is missing has no workspace to start a worker in, and
		// `asmai setup` has none to trust.
		for _, r := range registered {
			if _, err := os.Stat(r.Clone); err == nil {
				dirs = append(dirs, r.Clone)
			}
		}
	}
	var untrusted []string
	for _, dir := range dirs {
		if err := d.trusted(dir); err != nil {
			var notTrusted *untrustedError
			if !errors.As(err, &notTrusted) {
				fix := "make " + d.cfg.ClaudeRecord + " readable, then run `asmai start` again"
				if d.cfg.ClaudeRecord == "" {
					fix = "start the factory with `asmai start`"
				}
				return Check{Name: trustName, Problem: err.Error(), Fix: fix}
			}
			untrusted = append(untrusted, notTrusted.folder)
		}
	}
	if len(untrusted) == 0 {
		return Check{Name: trustName, OK: true}
	}
	return Check{Name: trustName,
		Problem: fmt.Sprintf("Claude Code does not trust %s, so an agent started there would wait at its question about trusting the folder", strings.Join(untrusted, " or ")),
		Fix:     "run `asmai setup`, which opens Claude Code there for you to trust it"}
}

// untrustedError is a directory Claude Code's record does not trust, and
// the folder whose trust Claude Code looks for, when that is another: the
// main repository a workspace is checked out from.
type untrustedError struct {
	dir, folder string
}

func (e *untrustedError) Error() string {
	if e.folder != e.dir {
		return fmt.Sprintf("Claude Code does not trust %s, the repository %s is checked out from, so the agent would wait at its question about trusting the folder; run `asmai setup`, which opens Claude Code there for you to trust it", e.folder, e.dir)
	}
	return fmt.Sprintf("Claude Code does not trust %s, so the agent would wait at its question about trusting the folder; run `asmai setup`", e.dir)
}

// trusted returns nil when Claude Code's record trusts dir, an
// untrustedError when it does not, and why the record cannot be read
// otherwise. It never changes the record.
func (d *daemon) trusted(dir string) error {
	if d.cfg.ClaudeRecord == "" {
		return errors.New("this daemon reads no record of Claude Code's directory trust")
	}
	ok, err := providers.Trusted(d.cfg.ClaudeRecord, dir)
	if err != nil {
		return fmt.Errorf("reading Claude Code's record of directory trust: %w", err)
	}
	if !ok {
		folder, _ := providers.TrustFolder(dir)
		return &untrustedError{dir: filepath.Clean(dir), folder: folder}
	}
	return nil
}

// staffing checks that the applied configuration staffs the roles M1 runs.
func (d *daemon) staffing() (config.Config, []Check) {
	path := d.cfg.ConfigFile
	failed := func(problem, fix string) Check {
		return Check{Name: "staffing", Problem: problem, Fix: fix}
	}
	if path == "" {
		return config.Config{}, []Check{failed("this daemon reads no configuration file", "start the factory with `asmai start`")}
	}
	d.configMu.Lock()
	cfg := d.applied
	d.configMu.Unlock()
	var problems []config.Problem
	if len(cfg.Roles) == 0 {
		var err error
		cfg, problems, err = config.Load(path)
		if errors.Is(err, config.ErrMissing) {
			return cfg, []Check{failed(fmt.Sprintf("%s does not exist, so no role is staffed", path), "run `asmai setup` to write the configuration file")}
		}
		if err != nil {
			return cfg, []Check{failed(fmt.Sprintf("reading %s: %v", path, err), "make the file readable")}
		}
		if len(problems) == 0 {
			problems = cfg.MissingStaffing(roles.Staffed)
		}
		if len(problems) == 0 {
			return config.Config{}, []Check{failed(fmt.Sprintf("%s has staffing that has not been applied", path), "run `asmai config apply` and confirm it before starting agents")}
		}
	} else {
		problems = cfg.MissingStaffing(roles.Staffed)
	}
	var checks []Check
	for _, p := range problems {
		where := path
		if p.Line > 0 {
			where = fmt.Sprintf("%s, line %d", path, p.Line)
		}
		checks = append(checks, failed(where+": "+p.Problem, p.Fix))
	}
	return cfg, checks
}

// claudeCode returns the install of the pinned Claude Code, or why there is
// none to run.
func (d *daemon) claudeCode(version string) (store.ProviderInstall, error) {
	installs, err := d.store.Providers()
	if err != nil {
		return store.ProviderInstall{}, fmt.Errorf("reading the provider installs: %w", err)
	}
	for _, p := range installs {
		if p.Name != providers.ClaudeCode || p.Version != version {
			continue
		}
		info, err := os.Stat(p.Path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o100 == 0 {
			return p, fmt.Errorf("the pinned Claude Code %s is not at %s, where it was installed", version, p.Path)
		}
		return p, nil
	}
	if p, ok := d.pinnedCopy(); ok && p.Version == version {
		return p, nil
	}
	return store.ProviderInstall{}, fmt.Errorf("the pinned Claude Code %s is not installed", version)
}

// pinnedCopy records the pinned Claude Code as installed when it is at the
// path the pins file's plan puts it, matching the pin, though no install
// records it: as when `asmai setup` fetched it while the daemon was stopped.
func (d *daemon) pinnedCopy() (store.ProviderInstall, bool) {
	platform, ok := providers.Platform(runtime.GOOS, runtime.GOARCH)
	if !ok {
		return store.ProviderInstall{}, false
	}
	plan, err := d.cfg.Pins.ClaudeCodePlan(platform, d.cfg.Paths.Providers)
	if err != nil {
		return store.ProviderInstall{}, false
	}
	if present, err := providers.Present(plan); err != nil || !present {
		return store.ProviderInstall{}, false
	}
	install := store.ProviderInstall{Name: plan.Name, Version: plan.Version, Path: plan.Path, SHA256: plan.Download.SHA256, InstalledAt: time.Now()}
	if _, err := d.store.ProviderInstalled(install); err != nil {
		d.log.Error("recording a provider install", "error", err.Error())
		return store.ProviderInstall{}, false
	}
	d.log.Info("provider installed", "name", plan.Name, "version", plan.Version, "path", plan.Path, "found", "at its path")
	return install, true
}

// held is the error of an agent that is not started because a check failed,
// such as a provider CLI that updated itself since the agent last ran: the
// agent is held where it is, not started on a combination outside the
// qualification record (03 rule 5). It names each failed check, with its fix,
// and keeps them for `asmai status`. d.mu is held.
func (d *daemon) held(agent string, checks []Check) error {
	d.lastChecks = checks
	var why []string
	for _, c := range checks {
		if !c.OK {
			d.log.Warn("an agent is held: a check failed", "agent", agent, "check", c.Name, "problem", c.Problem, "fix", c.Fix)
			why = append(why, fmt.Sprintf("%s: %s (fix: %s)", c.Name, c.Problem, c.Fix))
		}
	}
	return fmt.Errorf("%s is held, not started: %s", agent, strings.Join(why, "; "))
}

func failed(checks []Check) bool {
	for _, c := range checks {
		if !c.OK {
			return true
		}
	}
	return false
}

// ensureCoordination runs the checks and, if they pass, starts
// Coordination's leader unless it is running, as every start of the factory
// does. It returns the checks, with a failed one for a leader that could not
// start.
func (d *daemon) ensureCoordination() []Check {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopping {
		return d.lastChecks
	}
	checks, cfg, install := d.checks()
	d.lastChecks = checks
	if failed(checks) {
		for _, c := range checks {
			if !c.OK {
				d.log.Warn("a check failed, so Coordination's leader is not started", "check", c.Name, "problem", c.Problem, "fix", c.Fix)
			}
		}
		return checks
	}
	l := d.leader(roles.Coordination)
	if l.session != nil {
		return checks
	}
	if l.restart != nil {
		l.restart.Stop()
		l.restart = nil
	}
	if err := d.startLeader(l, cfg.Roles[roles.Coordination], install); err != nil {
		d.log.Error("starting an agent", "agent", l.address.String(), "error", err.Error())
		checks = append(checks, Check{Name: l.address.String(), Problem: "it did not start: " + err.Error(), Fix: "see `asmai log`, then run `asmai start` again"})
		d.lastChecks = checks
	}
	return checks
}

// launch is what distinguishes one agent's session from another's: its
// instructions, model, working directory and the variables it is given.
type launch struct {
	instructions string
	model        string
	dir          string
	env          map[string]string
	// writable are the directories besides dir that the session's write
	// guard lets its commands write, and editable those its Edit and Write
	// tools change files in without a prompt.
	writable []string
	editable []string
	// assignment is the assignment a worker's session carries, and resume
	// the provider's own session it continues, when it continues one.
	assignment int64
	resume     string
}

// startLeader starts l's session on the installed Claude Code, with
// everything it is given on its command line, and journals it. d.mu is held.
func (d *daemon) startLeader(l *leader, staffing config.Staffing, install store.ProviderInstall) error {
	instructions, err := roles.LeaderInstructions(l.address.Role)
	if err != nil {
		return err
	}
	dir := filepath.Join(d.cfg.Paths.Agents, l.address.String())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating the agent's directory: %w", err)
	}
	return d.startSession(l, install, launch{instructions: instructions, model: staffing.LeaderModel, dir: dir})
}

// startWorker starts the session of the worker l on the installed Claude
// Code in the workspace of the assignment it carries, with that workspace's
// slot and temporary directory, and journals it. options are the settings of
// the job's repository in the configuration file, whose notes the worker is
// given with the repository's instruction files. A worker that resumes the
// provider's session resume starts in it. d.mu is held.
func (d *daemon) startWorker(l *leader, staffing config.Staffing, options config.RepositoryOptions, install store.ProviderInstall, a store.Assignment, resume string) error {
	instructions, err := roles.WorkerInstructions(l.address.Role)
	if err != nil {
		return err
	}
	if _, err := os.Stat(a.Workspace.Path); err != nil {
		return fmt.Errorf("the workspace of assignment %d is missing: %w", a.ID, err)
	}
	if err := os.MkdirAll(a.Workspace.Tmp, 0o700); err != nil {
		return fmt.Errorf("creating the workspace's temporary directory: %w", err)
	}
	repository, err := roles.RepositoryInstructions(a.Workspace.Path, options.Notes)
	if err != nil {
		return fmt.Errorf("reading the repository's instruction files in the workspace of assignment %d: %w", a.ID, err)
	}
	if repository != "" {
		instructions += "\n" + repository
	}
	l.assignment = a.ID
	env := map[string]string{
		SlotVariable: strconv.Itoa(a.Workspace.Slot),
		"TMPDIR":     a.Workspace.Tmp,
	}
	// The worker's git adds the trailers to each commit, as the user.
	maps.Copy(env, githooks.Environment(os.Environ(), d.cfg.Paths.GitHooks))
	return d.startSession(l, install, launch{instructions: instructions, model: staffing.WorkerModel, dir: a.Workspace.Path, env: env, writable: []string{a.Workspace.Tmp}, editable: []string{a.Workspace.Path, a.Workspace.Tmp}, assignment: a.ID, resume: resume})
}

// startSession starts l's session on the installed Claude Code as launch
// says, and journals it. d.mu is held.
func (d *daemon) startSession(l *leader, install store.ProviderInstall, launch launch) error {
	// Claude Code asks whether to trust a directory it is not trusted in
	// before anything else, and only the user can answer it.
	if err := d.trusted(launch.dir); err != nil {
		return err
	}
	credential, err := newCredential()
	if err != nil {
		return err
	}
	hook := providers.HookCommand(d.cfg.Paths.Executable)
	guard := providers.WriteGuard{Writable: launch.writable, Socket: d.cfg.Paths.Socket}
	// Only Coordination's terminal takes the user's keys, in the
	// conversation: every other session would wait at a native prompt
	// nobody can answer, so it is denied what its rules do not allow.
	perms := providers.Permissions{Unattended: l.address.Role != roles.Coordination, Editable: launch.editable}
	args := providers.ClaudeCodeArgs(hook, launch.model, launch.instructions, guard, perms)
	if launch.resume != "" {
		args = providers.ClaudeCodeResumeArgs(hook, launch.model, launch.instructions, guard, perms, launch.resume)
	}
	set := map[string]string{SessionCredential: credential}
	maps.Copy(set, launch.env)
	env := providers.SessionEnv(os.Environ(), d.cfg.Paths.Bin, set)
	s, err := session.Start(session.Spec{Path: install.Path, Args: args, Env: env, Dir: launch.dir, Columns: terminalColumns, Rows: terminalRows})
	if err != nil {
		return err
	}
	at := time.Now()
	generation, err := d.store.SessionStarted(store.SessionStart{
		Agent:      l.address.String(),
		Role:       l.address.Role,
		Provider:   install.Name,
		Version:    install.Version,
		Model:      launch.model,
		Executable: install.Path,
		Args:       args,
		Dir:        launch.dir,
		PID:        s.PID(),
		At:         at,
		Assignment: launch.assignment,
		Resumes:    launch.resume,
	})
	if err != nil {
		s.Stop(stopGrace)
		return fmt.Errorf("journaling the session's start: %w", err)
	}
	l.session, l.generation, l.startedAt = s, generation, at
	l.userInput, l.submits, l.earlier, l.submitTerminal = false, 0, 0, ""
	l.ready, l.started, l.inputUnknown, l.pendingDispatch, l.currentDispatch, l.currentPromptID, l.pendingPrompt, l.transcript = false, false, false, 0, 0, "", "", ""
	l.prompt = nil
	d.sessions[credential] = sessionRef{address: l.address, generation: generation}
	d.log.Info("agent started", "agent", l.address.String(), "generation", generation, "pid", s.PID(), "provider", install.Name, "version", install.Version, "model", launch.model)
	d.agents.Add(1)
	go d.watch(l, s, generation, launch.resume != "")
	return nil
}

func newCredential() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("making the session's credential: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// watch waits for l's session s to end and journals how. Coordination's
// leader is restarted after a session that ended on its own, as Coordination
// always runs while the factory runs. A worker's session that resumed the
// provider's session and ended without coming up could not resume it: its
// assignment needs reconciliation.
func (d *daemon) watch(l *leader, s *session.Session, generation int, resumed bool) {
	defer d.agents.Done()
	<-s.Done()
	exit := describeExit(s.Exit())
	d.mu.Lock()
	defer d.mu.Unlock()
	by := "its own exit"
	if d.stopping {
		by = d.stoppedBy
	}
	if err := d.store.SessionEnded(l.address.String(), generation, exit, by, time.Now()); err != nil {
		d.log.Error("journaling an agent's end", "agent", l.address.String(), "error", err.Error())
	}
	d.log.Info("agent ended", "agent", l.address.String(), "generation", generation, "exit", exit, "by", by)
	if l.session != s {
		return
	}
	l.session = nil
	if d.stopping {
		return
	}
	if resumed && !l.started {
		d.cannotResume(l, exit)
	}
	if l.address.Role != roles.Coordination {
		pending, err := d.store.PendingLeaders()
		if err != nil {
			d.log.Error("reading pending leaders after agent exit", "agent", l.address.String(), "error", err)
			return
		}
		for _, agent := range pending {
			if agent == l.address.String() {
				if err := d.ensureAgent(l.address); err != nil {
					d.log.Error("restarting agent with pending dispatches", "agent", agent, "error", err)
				}
				return
			}
		}
		return
	}
	if time.Since(l.startedAt) > restartWaitMax {
		l.quickExits = 0
	}
	wait := min(restartWait<<min(l.quickExits, 6), restartWaitMax)
	l.quickExits++
	d.log.Warn("Coordination's leader ended on its own; restarting it", "agent", l.address.String(), "in", wait.String())
	l.restart = time.AfterFunc(wait, func() { d.ensureCoordination() })
}

func describeExit(err error) string {
	var exit *exec.ExitError
	switch {
	case err == nil:
		return "exit status 0"
	case errors.As(err, &exit):
		return exit.Error()
	default:
		return err.Error()
	}
}

// stopAgents ends every agent session and waits until each end is journaled.
func (d *daemon) stopAgents(by string) {
	d.mu.Lock()
	d.stopping, d.stoppedBy = true, by
	var running []*session.Session
	for _, l := range d.leaders {
		if l.restart != nil {
			l.restart.Stop()
		}
		if l.session != nil {
			running = append(running, l.session)
		}
	}
	d.mu.Unlock()
	for _, s := range running {
		go s.Stop(stopGrace)
	}
	d.agents.Wait()
}

// leaderStates returns each staffed role's leader, running or stopped.
func (d *daemon) leaderStates() []Leader {
	d.mu.Lock()
	defer d.mu.Unlock()
	var states []Leader
	for _, role := range roles.Staffed {
		l := d.leader(role)
		state := Leader{Agent: l.address.String(), State: store.AgentStopped}
		if l.session != nil {
			state.State, state.Generation, state.PID = store.AgentRunning, l.generation, l.session.PID()
			state.Input = InputAutomation
			if l.inputUnknown {
				state.Input = InputUnknown
			} else if l.userInput {
				state.Input = InputUser
			}
			if c := d.conversation; c != nil && c.leader == l {
				state.Conversation = c.terminal
			}
		}
		states = append(states, state)
	}
	return states
}

// The Claude Code hook events that confirm a prompt was submitted, and that
// a turn finished.
const (
	promptSubmitted = "UserPromptSubmit"
	turnStopped     = "Stop"
)

// observe journals the hook payload a session's hook reported, as an
// observation of that session. A prompt submission confirming an Enter key
// the user typed in the conversation is also journaled as a witnessed
// message, with the user's words as the provider reports them submitted.
func (d *daemon) observe(credential string, payload json.RawMessage) error {
	d.mu.Lock()
	ref, ok := d.sessions[credential]
	d.mu.Unlock()
	if !ok {
		return errors.New("the hook is not from an agent session this daemon started")
	}
	var fields struct {
		Event      string  `json:"hook_event_name"`
		Prompt     *string `json:"prompt"`
		PromptID   string  `json:"prompt_id"`
		Transcript string  `json:"transcript_path"`
		Session    string  `json:"session_id"`
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, payload); err != nil || json.Unmarshal(payload, &fields) != nil {
		return errors.New("the hook's payload is not a JSON object")
	}
	if fields.Event == "" {
		return errors.New("the hook's payload names no hook_event_name")
	}
	d.promptObserved(ref, fields.Event, compact.Bytes())
	if fields.Session != "" {
		// Every hook of a session carries the provider's own identifier of
		// it, which resuming the session after a restart needs.
		if err := d.store.SessionIdentified(ref.address.String(), ref.generation, fields.Session, fields.Transcript, time.Now()); err != nil {
			d.log.Error("recording the provider's session", "agent", ref.address.String(), "generation", ref.generation, "error", err.Error())
		}
	}
	if fields.Event == promptSubmitted {
		if fields.Prompt != nil {
			if handled, err := d.automatedSubmitted(ref, *fields.Prompt, fields.PromptID, fields.Transcript, compact.Bytes()); handled || err != nil {
				if err != nil {
					return err
				}
				return nil
			}
		}
		if terminal, ok := d.userSubmitted(ref); ok {
			if fields.Prompt == nil {
				d.log.Warn("the user's prompt submission does not say what was submitted, so no witnessed message is journaled", "agent", ref.address.String(), "generation", ref.generation)
			} else {
				return d.witness(ref, terminal, *fields.Prompt, compact.Bytes())
			}
		}
	}
	if err := d.store.Observed(ref.address.String(), ref.address.Role, ref.generation, fields.Event, compact.Bytes(), time.Now()); err != nil {
		d.log.Error("journaling an observation", "agent", ref.address.String(), "error", err.Error())
		return fmt.Errorf("journaling the observation: %w", err)
	}
	if fields.Event == turnStopped {
		d.turnFinished(ref)
		stopped, err := d.endCurrentDispatch(ref, fields.PromptID, fields.Transcript)
		if err != nil {
			return err
		}
		if stopped != 0 {
			d.dispatchStopped(ref, stopped)
		}
	}
	if fields.Event == "SessionStart" {
		d.initialBoundary(ref, fields.Transcript)
	} else if fields.Event == turnStopped {
		d.boundary(ref, fields.Transcript)
	}
	return nil
}

// userSubmitted reports whether a prompt submission ref's session reports
// confirms an Enter key the user typed in the conversation, and the user's
// terminal they typed it in.
func (d *daemon) userSubmitted(ref sessionRef) (terminal string, ok bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	l := d.leaders[ref.address.String()]
	if l == nil || l.session == nil || l.generation != ref.generation {
		return "", false
	}
	return l.submitted()
}

// turnFinished ages the user's unconfirmed Enter keys in ref's session,
// which reports its turn finished.
func (d *daemon) turnFinished(ref sessionRef) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if l := d.leaders[ref.address.String()]; l != nil && l.session != nil && l.generation == ref.generation {
		l.finished()
	}
}

// witness journals the prompt submission ref's session reported with
// payload as an observation, and text, what it submitted, as the user's
// witnessed message typed in terminal.
func (d *daemon) witness(ref sessionRef, terminal, text string, payload json.RawMessage) error {
	w := store.Witnessed{Agent: ref.address.String(), Role: ref.address.Role, Generation: ref.generation, Terminal: terminal, Text: text}
	observation, message, err := d.store.ObservedWitnessed(w, promptSubmitted, payload, time.Now())
	if err != nil {
		d.log.Error("journaling a witnessed message", "agent", w.Agent, "error", err.Error())
		return fmt.Errorf("journaling the witnessed message: %w", err)
	}
	d.log.Info("message witnessed", "agent", w.Agent, "generation", w.Generation, "terminal", terminal, "message", message, "observation", observation)
	return nil
}

// running returns the session of the agent addressed, or why there is none.
func (d *daemon) running(agent string) (roles.Address, *session.Session, int, error) {
	address, err := roles.ParseAddress(agent)
	if err != nil {
		return address, nil, 0, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if l := d.leaders[address.String()]; l != nil && l.session != nil {
		return address, l.session, l.generation, nil
	}
	return address, nil, 0, fmt.Errorf("%s is not running; `asmai status` shows the leaders and `asmai agents` the agents", address)
}

// attach streams the session of the agent the request addresses to conn,
// which r reads, until the session ends or the client goes: the session's
// State, then its output as it comes. The client may resize the agent's
// terminal; nothing it sends reaches the agent.
func (d *daemon) attach(conn *net.UnixConn, r *bufio.Reader, req Request) {
	address, s, generation, err := d.running(req.Agent)
	if err != nil {
		reply(conn, Response{Error: err.Error()})
		return
	}
	attached := &Leader{Agent: address.String(), State: store.AgentRunning, Generation: generation, PID: s.PID()}
	if req.Snapshot {
		screen := s.Screen()
		reply(conn, Response{Attached: attached, Screen: &screen})
		return
	}
	d.log.Info("attached", "agent", address.String(), "generation", generation)
	defer d.log.Info("detached", "agent", address.String(), "generation", generation)
	d.stream(conn, r, req, s, attached, nil)
}

// converse opens the conversation: it attaches the user's terminal, named
// in the request, to Coordination's leader with input, and streams the
// leader's session to conn as attach does, while the keys the client sends
// are typed into it. The conversation is not an intervention: leaving it,
// however the user leaves, returns the leader's input to automation at once
// and pauses nothing. One terminal at a time holds the conversation.
func (d *daemon) converse(conn *net.UnixConn, r *bufio.Reader, req Request) {
	address := roles.LeaderOf(roles.Coordination)
	d.mu.Lock()
	if c := d.conversation; c != nil {
		d.mu.Unlock()
		reply(conn, Response{Error: fmt.Sprintf("the conversation is open in another terminal (%s); leave it there with Ctrl-] first", c.terminal)})
		return
	}
	l := d.leaders[address.String()]
	if l == nil || l.session == nil {
		d.mu.Unlock()
		reply(conn, Response{Error: fmt.Sprintf("%s is not running; `asmai start` shows why", address)})
		return
	}
	terminal := req.Terminal
	if terminal == "" {
		terminal = "a terminal asmai could not name"
	}
	c := &conversation{leader: l, session: l.session, generation: l.generation, terminal: terminal}
	d.conversation = c
	d.mu.Unlock()

	if err := d.store.ConversationEntered(address.String(), address.Role, c.generation, c.terminal, time.Now()); err != nil {
		d.log.Error("journaling the conversation's start", "error", err.Error())
	}
	d.log.Info("the user entered the conversation", "agent", address.String(), "generation", c.generation, "terminal", c.terminal)
	attached := &Leader{Agent: address.String(), State: store.AgentRunning, Generation: c.generation, PID: c.session.PID(), Conversation: c.terminal}
	how := d.stream(conn, r, req, c.session, attached, func(keys []byte) { d.typed(c, keys) })
	d.leave(c, how)
	if how == LeftByDetaching {
		json.NewEncoder(conn).Encode(Frame{Left: true})
	}
}

// typed types the keys the user typed in the conversation c into its
// session. From the first key the user owns its input, and each Enter that
// can submit their message is counted, so that the provider's confirmation
// of it can be told apart from automation's submissions.
func (d *daemon) typed(c *conversation, keys []byte) {
	d.mu.Lock()
	l := c.leader
	if l.session != c.session {
		d.mu.Unlock()
		return
	}
	l.userInput = true
	if n := c.enters(keys); n > 0 {
		l.submits += n
		l.submitTerminal = c.terminal
	}
	d.mu.Unlock()
	if err := c.session.Input(keys); err != nil {
		d.log.Warn("typing the user's keys into the conversation", "agent", l.address.String(), "error", err.Error())
	}
}

// The brackets a terminal puts around pasted text.
const (
	pasteStart = "\x1b[200~"
	pasteEnd   = "\x1b[201~"
)

// enters counts the Enter keys in keys, the next the user typed in c, that
// can submit a message: a carriage return that is neither pasted text nor
// part of Alt-Enter (ESC CR, which a terminal sends in one write), which
// starts a new line instead.
func (c *conversation) enters(keys []byte) int {
	b := append(c.carry, keys...)
	c.carry = nil
	n := 0
	for i := 0; i < len(b); i++ {
		rest := string(b[i:])
		switch {
		case strings.HasPrefix(rest, pasteStart):
			c.pasting = true
			i += len(pasteStart) - 1
		case strings.HasPrefix(rest, pasteEnd):
			c.pasting = false
			i += len(pasteEnd) - 1
		case len(rest) >= 2 && len(rest) < len(pasteStart) && (strings.HasPrefix(pasteStart, rest) || strings.HasPrefix(pasteEnd, rest)):
			// The bracket may end in the next keys.
			c.carry = []byte(rest)
			return n
		case b[i] == '\r' && !c.pasting && (i == 0 || b[i-1] != 0x1b):
			n++
		}
	}
	return n
}

// leave closes the conversation c, which the user left as how says: the
// leader's input returns to automation at once, and nothing is paused.
func (d *daemon) leave(c *conversation, how string) {
	d.mu.Lock()
	if d.conversation == c {
		d.conversation = nil
	}
	l := c.leader
	if l.session == c.session {
		l.userInput = false
	}
	d.mu.Unlock()
	if err := d.store.ConversationLeft(l.address.String(), l.address.Role, c.generation, c.terminal, how, InputAutomation, time.Now()); err != nil {
		d.log.Error("journaling leaving the conversation", "error", err.Error())
	}
	d.log.Info("the user left the conversation", "agent", l.address.String(), "generation", c.generation, "terminal", c.terminal, "how", how)
	d.nudge(l.address.String())
}

// stream answers the client on conn, which r reads, with attached, then
// sends it session s's State and its output as it comes until the session
// ends or the client goes, and returns which it was. The client may resize
// the agent's terminal. Only given keys may it also type: each key it sends
// is passed to keys, and it may ask to leave, which ends the stream with
// LeftByDetaching for the caller to answer.
func (d *daemon) stream(conn *net.UnixConn, r *bufio.Reader, req Request, s *session.Session, attached *Leader, keys func([]byte)) (how string) {
	if req.Columns > 0 && req.Rows > 0 {
		s.Resize(req.Columns, req.Rows)
	}
	conn.SetDeadline(time.Time{})
	reply(conn, Response{Attached: attached})

	gone := make(chan struct{})
	leaving := false
	go func() {
		defer close(gone)
		for {
			line, err := r.ReadBytes('\n')
			if err != nil {
				return
			}
			var m ClientMessage
			if json.Unmarshal(line, &m) != nil {
				continue
			}
			if m.Columns > 0 && m.Rows > 0 {
				s.Resize(m.Columns, m.Rows)
			}
			if keys == nil {
				continue
			}
			if len(m.Keys) > 0 {
				keys(m.Keys)
			}
			if m.Leave {
				leaving = true
				return
			}
		}
	}()
	enc := json.NewEncoder(conn)
	send := func(f Frame) bool {
		conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		return enc.Encode(f) == nil
	}
	// The prompts go with the first frame, and on their own whenever they
	// change.
	prompts := d.promptLines()
	pending := &prompts
	check := time.NewTicker(promptCheck)
	defer check.Stop()
	events, stop := s.Follow()
	defer func() { stop() }()
	for {
		select {
		case <-check.C:
			if now := d.promptLines(); !slices.Equal(now, prompts) {
				prompts = now
				if pending == nil && !send(Frame{Prompts: &prompts}) {
					return LeftByTerminalClose
				}
			}
		case <-gone:
			if leaving {
				return LeftByDetaching
			}
			return LeftByTerminalClose
		case e, ok := <-events:
			if ok {
				if !send(Frame{State: e.State, Output: e.Output, Prompts: pending}) {
					return LeftByTerminalClose
				}
				pending = nil
				continue
			}
			select {
			case <-s.Done():
				send(Frame{Ended: describeExit(s.Exit())})
				return LeftBySessionEnd
			default:
				// The client fell behind; it starts again from a new State.
				events, stop = s.Follow()
			}
		}
	}
}

// installExecutable copies the asmai executable at from to the daemon's
// fixed path, to, replacing the copy a previous daemon left, so that agent
// sessions and their hooks run this daemon's version.
func installExecutable(from, to string) error {
	if same(from, to) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(to), err)
	}
	src, err := os.Open(from)
	if err != nil {
		return fmt.Errorf("copying asmai for the agents: %w", err)
	}
	defer src.Close()
	part, err := os.CreateTemp(filepath.Dir(to), ".asmai-*")
	if err != nil {
		return fmt.Errorf("copying asmai for the agents: %w", err)
	}
	defer os.Remove(part.Name())
	if _, err := io.Copy(part, src); err != nil {
		part.Close()
		return fmt.Errorf("copying asmai for the agents: %w", err)
	}
	if err := part.Chmod(0o755); err != nil {
		part.Close()
		return err
	}
	if err := part.Close(); err != nil {
		return err
	}
	if err := os.Rename(part.Name(), to); err != nil {
		return fmt.Errorf("copying asmai for the agents: %w", err)
	}
	return nil
}

func same(a, b string) bool {
	ia, err := os.Stat(a)
	if err != nil {
		return false
	}
	ib, err := os.Stat(b)
	return err == nil && os.SameFile(ia, ib)
}
