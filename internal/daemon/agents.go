// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/talvor/asmai/internal/config"
	"github.com/talvor/asmai/internal/providers"
	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/session"
	"github.com/talvor/asmai/internal/store"
	"github.com/talvor/asmai/internal/vt"
)

// SessionCredential is the environment variable that carries an agent
// session's credential, which its hooks pass back to the daemon so that it
// knows which session and generation they report on.
const SessionCredential = "ASMAI_SESSION"

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

// Leader is a role's leader as `asmai status` shows it.
type Leader struct {
	Agent      string `json:"agent"`
	State      string `json:"state"`
	Generation int    `json:"generation,omitempty"`
	PID        int    `json:"pid,omitempty"`
}

// Frame is one line of an attach stream after the daemon's answer: a State
// for the client to start again from, output to feed the terminal it made
// from the last State, or the end of the session.
type Frame struct {
	State  *vt.State `json:"state,omitempty"`
	Output []byte    `json:"output,omitempty"`
	Ended  string    `json:"ended,omitempty"`
}

// Resize is what an attach client sends the daemon when its terminal's size
// changes: the size the agent's terminal is made.
type Resize struct {
	Columns int `json:"columns"`
	Rows    int `json:"rows"`
}

// leader is a role's leader, and its session while it runs.
type leader struct {
	address    roles.Address
	session    *session.Session
	generation int
	startedAt  time.Time
	// restart is the timer that restarts the leader after its session ended
	// on its own, and quickExits counts the sessions in a row that ended
	// soon after they started.
	restart    *time.Timer
	quickExits int
}

// sessionRef is the session an agent session's credential belongs to.
type sessionRef struct {
	address    roles.Address
	generation int
}

func (d *daemon) leader(role string) *leader {
	address := roles.LeaderOf(role)
	l, ok := d.leaders[address.String()]
	if !ok {
		l = &leader{address: address}
		d.leaders[address.String()] = l
	}
	return l
}

// checks runs the checks that exist before agents start: the staffing in the
// configuration file and the pinned Claude Code installed. It returns the
// configuration and the install to start agents with.
func (d *daemon) checks() ([]Check, config.Config, store.ProviderInstall) {
	var checks []Check
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
	}
	return checks, cfg, install
}

// staffing reads the configuration file and checks that the roles M1 runs
// are staffed, returning a failed check for each problem.
func (d *daemon) staffing() (config.Config, []Check) {
	path := d.cfg.ConfigFile
	failed := func(problem, fix string) Check {
		return Check{Name: "staffing", Problem: problem, Fix: fix}
	}
	if path == "" {
		return config.Config{}, []Check{failed("this daemon reads no configuration file", "start the factory with `asmai start`")}
	}
	cfg, problems, err := config.Load(path)
	if errors.Is(err, config.ErrMissing) {
		return cfg, []Check{failed(fmt.Sprintf("%s does not exist, so no role is staffed", path),
			fmt.Sprintf("create it with a table for each of %s, such as:\n%s", tables(), exampleStaffing))}
	}
	if err != nil {
		return cfg, []Check{failed(fmt.Sprintf("reading %s: %v", path, err), "make the file readable")}
	}
	if len(problems) == 0 {
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

func tables() string {
	var t []string
	for _, role := range roles.Staffed {
		t = append(t, "[roles."+role+"]")
	}
	return t[0] + ", " + t[1] + " and " + t[2]
}

const exampleStaffing = `[roles.coordination]
leader_provider = "claude"
leader_model = "opus"
worker_provider = "claude"
worker_model = "opus"`

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
	return store.ProviderInstall{}, fmt.Errorf("the pinned Claude Code %s is not installed", version)
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
	credential, err := newCredential()
	if err != nil {
		return err
	}
	args := providers.ClaudeCodeArgs(providers.HookCommand(d.cfg.Paths.Executable), staffing.LeaderModel, instructions)
	env := providers.SessionEnv(os.Environ(), d.cfg.Paths.Bin, map[string]string{SessionCredential: credential})
	s, err := session.Start(session.Spec{Path: install.Path, Args: args, Env: env, Dir: dir, Columns: terminalColumns, Rows: terminalRows})
	if err != nil {
		return err
	}
	at := time.Now()
	generation, err := d.store.SessionStarted(store.SessionStart{
		Agent:      l.address.String(),
		Role:       l.address.Role,
		Provider:   install.Name,
		Version:    install.Version,
		Model:      staffing.LeaderModel,
		Executable: install.Path,
		Args:       args,
		Dir:        dir,
		PID:        s.PID(),
		At:         at,
	})
	if err != nil {
		s.Stop(stopGrace)
		return fmt.Errorf("journaling the session's start: %w", err)
	}
	l.session, l.generation, l.startedAt = s, generation, at
	d.sessions[credential] = sessionRef{address: l.address, generation: generation}
	d.log.Info("agent started", "agent", l.address.String(), "generation", generation, "pid", s.PID(), "provider", install.Name, "version", install.Version, "model", staffing.LeaderModel)
	d.agents.Add(1)
	go d.watch(l, s, generation)
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
// always runs while the factory runs.
func (d *daemon) watch(l *leader, s *session.Session, generation int) {
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
	if d.stopping || l.address.Role != roles.Coordination {
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
		}
		states = append(states, state)
	}
	return states
}

// observe journals the hook payload a session's hook reported, as an
// observation of that session.
func (d *daemon) observe(credential string, payload json.RawMessage) error {
	d.mu.Lock()
	ref, ok := d.sessions[credential]
	d.mu.Unlock()
	if !ok {
		return errors.New("the hook is not from an agent session this daemon started")
	}
	var fields struct {
		Event string `json:"hook_event_name"`
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, payload); err != nil || json.Unmarshal(payload, &fields) != nil {
		return errors.New("the hook's payload is not a JSON object")
	}
	if fields.Event == "" {
		return errors.New("the hook's payload names no hook_event_name")
	}
	if err := d.store.Observed(ref.address.String(), ref.address.Role, ref.generation, fields.Event, compact.Bytes(), time.Now()); err != nil {
		d.log.Error("journaling an observation", "agent", ref.address.String(), "error", err.Error())
		return fmt.Errorf("journaling the observation: %w", err)
	}
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
	if address.Name == roles.Leader && roles.Known(address.Role) {
		if l := d.leaders[address.String()]; l != nil && l.session != nil {
			return address, l.session, l.generation, nil
		}
	}
	return address, nil, 0, fmt.Errorf("%s is not running; `asmai status` shows the leaders", address)
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
	if req.Columns > 0 && req.Rows > 0 {
		s.Resize(req.Columns, req.Rows)
	}
	conn.SetDeadline(time.Time{})
	reply(conn, Response{Attached: attached})
	d.log.Info("attached", "agent", address.String(), "generation", generation)
	defer d.log.Info("detached", "agent", address.String(), "generation", generation)

	gone := make(chan struct{})
	go func() {
		defer close(gone)
		for {
			line, err := r.ReadBytes('\n')
			if err != nil {
				return
			}
			var size Resize
			if json.Unmarshal(line, &size) == nil && size.Columns > 0 && size.Rows > 0 {
				s.Resize(size.Columns, size.Rows)
			}
		}
	}()
	enc := json.NewEncoder(conn)
	send := func(f Frame) bool {
		conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		return enc.Encode(f) == nil
	}
	events, stop := s.Follow()
	defer func() { stop() }()
	for {
		select {
		case <-gone:
			return
		case e, ok := <-events:
			if ok {
				if !send(Frame{State: e.State, Output: e.Output}) {
					return
				}
				continue
			}
			select {
			case <-s.Done():
				send(Frame{Ended: describeExit(s.Exit())})
				return
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
