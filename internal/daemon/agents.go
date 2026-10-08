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
	"strings"
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

// The input owners of an agent session.
const (
	InputAutomation = "automation"
	InputUser       = "user"
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
	// userInput is set while the user owns the session's input: from the
	// first key they type in the conversation until the provider confirms
	// their message submitted, or they leave.
	userInput bool
	// submits counts the Enter keys the user typed in the conversation
	// that the provider has not yet confirmed as a prompt submission, and
	// submitTerminal is the user's terminal the last was typed in. Each
	// confirmation the session reports while submits is above zero is the
	// user's own message. Automation types nothing into a session yet; once
	// it does, its typing resets submits, so that its own submissions are
	// never taken for the user's.
	submits        int
	submitTerminal string
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
	l.userInput, l.submits, l.submitTerminal = false, 0, ""
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
			state.Input = InputAutomation
			if l.userInput {
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

// promptSubmitted is the Claude Code hook event that confirms a prompt was
// submitted.
const promptSubmitted = "UserPromptSubmit"

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
		Event  string  `json:"hook_event_name"`
		Prompt *string `json:"prompt"`
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, payload); err != nil || json.Unmarshal(payload, &fields) != nil {
		return errors.New("the hook's payload is not a JSON object")
	}
	if fields.Event == "" {
		return errors.New("the hook's payload names no hook_event_name")
	}
	if fields.Event == promptSubmitted {
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
	return nil
}

// userSubmitted reports whether a prompt submission ref's session reports
// confirms an Enter key the user typed in the conversation, and the user's
// terminal they typed it in. Once the provider has confirmed each, the user
// has no message under way, and automation owns the input again.
func (d *daemon) userSubmitted(ref sessionRef) (terminal string, ok bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	l := d.leaders[ref.address.String()]
	if l == nil || l.session == nil || l.generation != ref.generation || l.submits == 0 {
		return "", false
	}
	l.submits--
	if l.submits == 0 {
		l.userInput = false
	}
	return l.submitTerminal, true
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
	events, stop := s.Follow()
	defer func() { stop() }()
	for {
		select {
		case <-gone:
			if leaving {
				return LeftByDetaching
			}
			return LeftByTerminalClose
		case e, ok := <-events:
			if ok {
				if !send(Frame{State: e.State, Output: e.Output}) {
					return LeftByTerminalClose
				}
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
