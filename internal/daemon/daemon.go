// SPDX-License-Identifier: Apache-2.0

// Package daemon is the per-user AsmAI daemon and the thin client every other
// command uses to reach it. The daemon owns the factory: it is the only
// writer of the store, and it opens no network listener. Commands reach it
// only over a Unix socket in the state directory, which only the user can
// read.
package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/talvor/asmai/internal/githooks"
	"github.com/talvor/asmai/internal/logfile"
	"github.com/talvor/asmai/internal/providers"
	"github.com/talvor/asmai/internal/statedir"
	"github.com/talvor/asmai/internal/store"
	"github.com/talvor/asmai/internal/vt"
)

// ErrAlreadyRunning is returned by Run when another daemon holds the state
// directory.
var ErrAlreadyRunning = errors.New("another asmai daemon is running for this user")

// The commands the daemon serves.
const (
	CommandStatus = "status"
	CommandStop   = "stop"
	CommandExport = "export"
	// CommandProviders lists the provider installs the store records.
	CommandProviders = "providers"
	// CommandProviderInstalled records the provider install in the request,
	// which `asmai providers install` has fetched and checked.
	CommandProviderInstalled = "provider.installed"
	// CommandStart runs the checks and, if they pass, starts Coordination's
	// leader unless it is running.
	CommandStart = "start"
	// CommandAgents lists the agents the store records.
	CommandAgents = "agents"
	// CommandAttach streams an agent's terminal to an attach client, or
	// with Snapshot answers with its screen.
	CommandAttach = "attach"
	// CommandHook journals what an agent session's hook reported.
	CommandHook = "hook"
	// CommandConversation attaches the user's terminal to Coordination's
	// leader with input: the conversation.
	CommandConversation = "conversation"
	// CommandRepoAdd registers the repository the request names, with
	// AsmAI's clone of it; CommandRepoList, CommandRepoShow and
	// CommandRepoRemove list, show and remove registered repositories.
	CommandRepoAdd        = "repo.add"
	CommandRepoList       = "repo.list"
	CommandRepoShow       = "repo.show"
	CommandRepoRemove     = "repo.remove"
	CommandJobOpen        = "job.open"
	CommandJobs           = "jobs"
	CommandJob            = "job"
	CommandBrief          = "brief"
	CommandInbox          = "inbox"
	CommandHandoffSend    = "handoff.send"
	CommandHandoffAccept  = "handoff.accept"
	CommandHandoffClarify = "handoff.clarify"
	CommandHandoffDecline = "handoff.decline"
	// CommandAssign gives a writing assignment to a worker of Engineering, or
	// with ReadOnly a validation to a worker of Quality. CommandEffect
	// records an effect, CommandResult submits an assignment's result and
	// CommandBlocked reports that its worker cannot go on.
	CommandAssign  = "assign"
	CommandEffect  = "effect"
	CommandResult  = "result"
	CommandBlocked = "blocked"
	// CommandAccept, CommandReject and CommandCancel are the owning
	// leader's decisions on an assignment: accepting a result fast-forwards
	// the job branch to it, or for a validation records its findings and
	// delivers the report to Engineering's leader; rejecting returns the
	// assignment to its worker, and cancelling ends it once the worker has
	// pushed its branch, if it has one, and stopped.
	CommandAccept = "accept"
	CommandReject = "reject"
	CommandCancel = "cancel"
	// CommandTrailers answers a worker's git hook with the trailers its
	// commit carries.
	CommandTrailers = "trailers"
)

// Request is a command sent to the daemon: one JSON line per connection.
type Request struct {
	Command  string                 `json:"command"`
	Provider *store.ProviderInstall `json:"provider,omitempty"`
	// Agent addresses an agent, as name@role or a bare role name.
	Agent string `json:"agent,omitempty"`
	// Columns and Rows are the size an attach client gives the agent's
	// terminal.
	Columns int `json:"columns,omitempty"`
	Rows    int `json:"rows,omitempty"`
	// Snapshot asks attach for the agent's screen as it is, without
	// following it.
	Snapshot bool `json:"snapshot,omitempty"`
	// Terminal names the user's terminal the conversation is opened in,
	// such as /dev/pts/3.
	Terminal string `json:"terminal,omitempty"`
	// Session is the credential of the agent session making the call, and
	// Payload is what the provider passed a hook.
	Session string          `json:"session,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
	// Source is what repo.add registers: a local checkout's absolute path
	// or a URL. Repository names the registered repository for the other
	// repo commands, and for repo.add the name to register it as, when the
	// user chose one.
	Source        string   `json:"source,omitempty"`
	Repository    string   `json:"repository,omitempty"`
	Job           int64    `json:"job,omitempty"`
	Witness       int64    `json:"witness,omitempty"`
	LatestWitness bool     `json:"latest_witness,omitempty"`
	Reading       string   `json:"reading,omitempty"`
	Mandate       string   `json:"mandate,omitempty"`
	Criteria      []string `json:"criteria,omitempty"`
	Handoff       int64    `json:"handoff,omitempty"`
	Dispatch      int64    `json:"dispatch,omitempty"`
	Outcome       string   `json:"outcome,omitempty"`
	Decisions     string   `json:"decisions,omitempty"`
	Evidence      string   `json:"evidence,omitempty"`
	Constraints   string   `json:"constraints,omitempty"`
	Permissions   string   `json:"permissions,omitempty"`
	Answer        string   `json:"answer,omitempty"`
	// Kind and Ref say what effect an effect request records.
	Kind string `json:"kind,omitempty"`
	Ref  string `json:"ref,omitempty"`
	// Report is what a result or a blocked report says.
	Report *store.ReportInput `json:"report,omitempty"`
	// Assignment and Reasons say which assignment a decision is about and
	// why the owning leader made it.
	Assignment int64    `json:"assignment,omitempty"`
	Reasons    []string `json:"reasons,omitempty"`
	// ReadOnly and Commit make an assign a read-only validation fixed at the
	// commit, which is the job branch's tip.
	ReadOnly bool   `json:"read_only,omitempty"`
	Commit   string `json:"commit,omitempty"`
}

// Response is the daemon's answer to a Request: one JSON line.
type Response struct {
	Error     string                  `json:"error,omitempty"`
	Status    *Status                 `json:"status,omitempty"`
	Journal   []store.Entry           `json:"journal,omitempty"`
	Providers []store.ProviderInstall `json:"providers,omitempty"`
	// Checks are the checks the last start ran.
	Checks  []Check       `json:"checks,omitempty"`
	Leaders []Leader      `json:"leaders,omitempty"`
	Agents  []store.Agent `json:"agents,omitempty"`
	// Attached is the agent an attach follows, and Screen its screen when
	// a snapshot was asked for.
	Attached *Leader   `json:"attached,omitempty"`
	Screen   *vt.State `json:"screen,omitempty"`
	// Repositories are the registered repositories, and Repository the one
	// a repo command added, showed or removed.
	Repositories []store.Repository `json:"repositories,omitempty"`
	Repository   *store.Repository  `json:"repository,omitempty"`
	Jobs         []store.Job        `json:"jobs,omitempty"`
	Job          *store.Job         `json:"job,omitempty"`
	Brief        string             `json:"brief,omitempty"`
	Handoff      *store.Handoff     `json:"handoff,omitempty"`
	Dispatch     *store.Dispatch    `json:"dispatch,omitempty"`
	Messages     []store.Message    `json:"messages,omitempty"`
	Assignment   *store.Assignment  `json:"assignment,omitempty"`
	Effect       *store.Effect      `json:"effect,omitempty"`
	Report       *store.Report      `json:"report,omitempty"`
	Decision     *store.Decision    `json:"decision,omitempty"`
	JobBranch    *store.JobBranch   `json:"job_branch,omitempty"`
	// Trailers are the trailers a worker's commit carries, each as
	// "Key: value".
	Trailers []string `json:"trailers,omitempty"`
}

// Status describes the running daemon.
type Status struct {
	Version   string    `json:"version"`
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
}

// Config is what Run needs to run the daemon.
type Config struct {
	Paths   statedir.Paths
	Version string
	// Mirror, when set, also receives each line of the log, as a foreground
	// daemon shows its log on its terminal.
	Mirror io.Writer
	// ConfigFile is the configuration file the staffing is read from; with
	// none, no agent starts.
	ConfigFile string
	// Executable, when set, is this asmai, which the daemon copies to its
	// fixed path in the state directory for agent sessions to run.
	Executable string
	// Pins are the provider versions agents run on.
	Pins providers.Pins
}

// Run runs the daemon until `asmai stop` asks it to stop or ctx is done. It
// takes the state directory's lock, so that a user runs at most one daemon,
// opens the store, journals the start, and serves commands on the socket.
// Stopping journals the stop and closes the store, so the next start finds
// it as it was left. A daemon that cannot start returns why, for its caller
// to report: started in the background, its output goes to its log.
func Run(ctx context.Context, cfg Config) error {
	p := cfg.Paths
	if err := p.Prepare(); err != nil {
		return err
	}
	logs, err := logfile.Open(p.Log)
	if err != nil {
		return err
	}
	defer logs.Close()
	var out io.Writer = logs
	if cfg.Mirror != nil {
		out = io.MultiWriter(logs, cfg.Mirror)
	}
	log := slog.New(slog.NewJSONHandler(out, nil))

	lock, err := os.OpenFile(p.Lock, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("opening the daemon's lock: %w", err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return ErrAlreadyRunning
		}
		return fmt.Errorf("taking the daemon's lock: %w", err)
	}

	st, err := store.Open(p.Store)
	if err != nil {
		return err
	}
	defer st.Close()

	// Holding the lock, this is the only daemon, so a socket left in the
	// state directory is a previous daemon's, which did not stop cleanly.
	if err := os.Remove(p.Socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing the previous daemon's socket: %w", err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: p.Socket, Net: "unix"})
	if err != nil {
		return fmt.Errorf("listening on the daemon's socket: %w", err)
	}
	listener.SetUnlinkOnClose(true)
	defer listener.Close()
	if err := os.Chmod(p.Socket, 0o600); err != nil {
		return fmt.Errorf("making the daemon's socket private: %w", err)
	}

	if cfg.Executable != "" {
		if err := installExecutable(cfg.Executable, p.Executable); err != nil {
			return err
		}
		if err := githooks.Install(p.GitHooks, p.Executable); err != nil {
			return err
		}
	}

	// work is what the daemon's long work, such as a clone, runs under: it
	// ends when the daemon is asked to stop.
	work, stopWork := context.WithCancel(context.Background())
	defer stopWork()
	d := &daemon{
		work:     work,
		cfg:      cfg,
		leaders:  map[string]*leader{},
		sessions: map[string]sessionRef{},
		store:    st,
		log:      log,
		status: Status{
			Version:   cfg.Version,
			PID:       os.Getpid(),
			StartedAt: time.Now(),
		},
		stop: make(chan *net.UnixConn),
	}
	previous, err := st.Started(d.status.Version, d.status.PID, d.status.StartedAt)
	if err != nil {
		return fmt.Errorf("journaling the start: %w", err)
	}
	log.Info("daemon started", "version", d.status.Version, "pid", d.status.PID, "previous", previous.State)
	if previous.State == store.StateRunning {
		log.Warn("the previous daemon did not stop cleanly", "version", previous.Version, "pid", previous.PID)
	}
	if err := st.EndAbandonedSessions(time.Now()); err != nil {
		return fmt.Errorf("journaling the previous daemon's agents as ended: %w", err)
	}
	// Every start is a recovery: what the previous daemon left is continued
	// or held for reconciliation before any leader is restored.
	d.recoverWork(previous.State != store.StateRunning)

	served := make(chan struct{})
	go func() {
		d.serve(listener)
		close(served)
	}()
	// Every start restores Coordination's leader, which runs as long as the
	// factory does, and every leader whose role has open work.
	d.ensureCoordination()
	d.restoreLeaders()

	var by string
	var asker *net.UnixConn
	select {
	case <-ctx.Done():
		by = "signal"
	case asker = <-d.stop:
		by = "asmai stop"
		defer asker.Close()
	}

	// End the agents, then stop serving, wait for the commands being
	// served, and persist.
	stopWork()
	d.stopAgents(by)
	listener.Close()
	<-served
	d.wg.Wait()
	err = st.Stopped(by, time.Now())
	if err == nil {
		err = st.Close()
	}
	if err != nil {
		log.Error("the daemon did not persist its state", "error", err.Error())
	} else {
		log.Info("daemon stopped", "by", by)
	}
	// Release the lock before answering, so that a start right after the
	// stop finds the state directory free.
	lock.Close()
	if asker != nil {
		if err != nil {
			reply(asker, Response{Error: "stopping: " + err.Error()})
		} else {
			reply(asker, Response{Status: &d.status})
		}
	}
	return err
}

type daemon struct {
	work   context.Context
	cfg    Config
	store  *store.Store
	log    *slog.Logger
	status Status
	// stop receives the connection that asked the daemon to stop, which is
	// answered once it has.
	stop chan *net.UnixConn
	wg   sync.WaitGroup

	// assignMu makes giving assignments one at a time: each makes a job
	// branch and a workspace of AsmAI's clone, and takes the next worker
	// number and slot.
	assignMu sync.Mutex

	// repoMu makes registering and removing repositories one at a time: each
	// changes the configuration file, the store and the state directory.
	repoMu sync.Mutex

	// mu guards the agents and the checks.
	mu      sync.Mutex
	leaders map[string]*leader
	// conversation is the user's conversation with Coordination, while it
	// is open.
	conversation *conversation
	sessions     map[string]sessionRef
	lastChecks   []Check
	stopping     bool
	stoppedBy    string
	// agents counts the agent sessions whose end is not yet journaled.
	agents sync.WaitGroup
}

func (d *daemon) serve(l *net.UnixListener) {
	for {
		conn, err := l.AcceptUnix()
		if err != nil {
			return
		}
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			if !d.handle(conn) {
				conn.Close()
			}
		}()
	}
}

// handle serves the request on conn. It returns true when it handed conn on
// to be answered later.
func (d *daemon) handle(conn *net.UnixConn) (handedOn bool) {
	conn.SetDeadline(time.Now().Add(time.Minute))
	var req Request
	r := bufio.NewReader(conn)
	line, err := r.ReadBytes('\n')
	if err == nil {
		err = json.Unmarshal(line, &req)
	}
	if err != nil {
		reply(conn, Response{Error: "the request is not a JSON line"})
		return false
	}
	if err := d.authorize(req); err != nil {
		reply(conn, Response{Error: err.Error()})
		return false
	}
	switch req.Command {
	case CommandStatus:
		d.mu.Lock()
		checks := d.lastChecks
		d.mu.Unlock()
		reply(conn, Response{Status: &d.status, Checks: checks, Leaders: d.leaderStates()})
	case CommandStart:
		checks := d.ensureCoordination()
		if !failed(checks) {
			// A factory started before it could run agents restores the
			// leaders with open work once it can.
			d.restoreLeaders()
		}
		reply(conn, Response{Status: &d.status, Checks: checks, Leaders: d.leaderStates()})
	case CommandAgents:
		agents, err := d.store.Agents()
		if err != nil {
			d.log.Error("reading the agents", "error", err.Error())
			reply(conn, Response{Error: "reading the agents: " + err.Error()})
			return false
		}
		reply(conn, Response{Agents: agents})
	case CommandAttach:
		d.attach(conn, r, req)
	case CommandConversation:
		d.converse(conn, r, req)
	case CommandHook:
		if err := d.observe(req.Session, req.Payload); err != nil {
			reply(conn, Response{Error: err.Error()})
			return false
		}
		reply(conn, Response{})
	case CommandExport:
		entries, err := d.store.Journal()
		if err != nil {
			d.log.Error("reading the journal", "error", err.Error())
			reply(conn, Response{Error: "reading the journal: " + err.Error()})
			return false
		}
		reply(conn, Response{Journal: entries})
	case CommandProviders:
		installs, err := d.store.Providers()
		if err != nil {
			d.log.Error("reading the provider installs", "error", err.Error())
			reply(conn, Response{Error: "reading the provider installs: " + err.Error()})
			return false
		}
		reply(conn, Response{Providers: installs})
	case CommandProviderInstalled:
		p := req.Provider
		if p == nil || p.Name == "" || p.Version == "" || p.SHA256 == "" || !filepath.IsAbs(p.Path) {
			reply(conn, Response{Error: "a provider install needs a name, a version, a SHA-256 and an absolute path"})
			return false
		}
		p.InstalledAt = time.Now()
		recorded, err := d.store.ProviderInstalled(*p)
		if err != nil {
			d.log.Error("recording a provider install", "error", err.Error())
			reply(conn, Response{Error: "recording the provider install: " + err.Error()})
			return false
		}
		if recorded {
			d.log.Info("provider installed", "name", p.Name, "version", p.Version, "path", p.Path)
		}
		// Answer with the install as the store holds it, which is older than
		// this request when it was already recorded.
		installs, err := d.store.Providers()
		if err != nil {
			reply(conn, Response{Error: "reading the provider installs: " + err.Error()})
			return false
		}
		installs = slices.DeleteFunc(installs, func(i store.ProviderInstall) bool {
			return i.Name != p.Name || i.Version != p.Version
		})
		reply(conn, Response{Providers: installs})
	case CommandRepoAdd, CommandRepoList, CommandRepoShow, CommandRepoRemove:
		d.repoCommand(conn, req)
	case CommandJobOpen, CommandJobs, CommandJob, CommandBrief:
		d.jobCommand(conn, req)
	case CommandInbox, CommandHandoffSend, CommandHandoffAccept, CommandHandoffClarify, CommandHandoffDecline:
		d.handoffCommand(conn, req)
	case CommandAssign, CommandEffect, CommandResult, CommandBlocked, CommandAccept, CommandReject, CommandCancel:
		d.assignmentCommand(conn, req)
	case CommandTrailers:
		trailers, err := d.trailers(req)
		if err != nil {
			reply(conn, Response{Error: err.Error()})
			return false
		}
		reply(conn, Response{Trailers: trailers})
	case CommandStop:
		select {
		case d.stop <- conn:
			d.log.Info("stopping", "by", "asmai stop")
			return true
		default:
			reply(conn, Response{Error: "the daemon is already stopping"})
		}
	default:
		reply(conn, Response{Error: fmt.Sprintf("the daemon does not serve %q", req.Command)})
	}
	return false
}

func reply(conn *net.UnixConn, resp Response) {
	line, err := json.Marshal(resp)
	if err != nil {
		line, _ = json.Marshal(Response{Error: err.Error()})
	}
	conn.Write(append(line, '\n'))
}
