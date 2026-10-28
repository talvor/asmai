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

	"github.com/talvor/asmai/internal/config"
	"github.com/talvor/asmai/internal/githooks"
	"github.com/talvor/asmai/internal/lavish"
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
	// CommandLavish answers with the factory's Lavish server and the review
	// pages waiting for the user.
	CommandLavish = "lavish"
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
	CommandRepoAdd    = "repo.add"
	CommandRepoList   = "repo.list"
	CommandRepoShow   = "repo.show"
	CommandRepoRemove = "repo.remove"
	// CommandConfigApply validates the configuration file the request
	// carries and shows what it changes and which running agents pick that
	// up; with Confirm it also applies it, which journals it.
	CommandConfigApply = "config.apply"
	CommandJobOpen     = "job.open"
	// CommandJobClean removes the scratch workspaces of a job with no
	// repository that has ended, at the user's command.
	CommandJobClean = "job.clean"
	// CommandJobLink links a follow-up job to the job it follows (02 rule
	// 20).
	CommandJobLink = "job.link"
	// CommandJobCancel is Coordination's leader cancelling a job at the
	// user's word: the daemon cancels its assignments, closes its pull
	// request, withdraws its decision requests and ends it as cancelled.
	CommandJobCancel      = "job.cancel"
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
	// CommandBlockedReport is a leader other than Coordination's that cannot
	// go on, for a reason that is not the user's choice, filing a blocked
	// report to Coordination's leader. CommandBlockedResolve is
	// Coordination's leader resolving it with a note of what changed, which
	// goes back to the leader that filed it.
	CommandBlockedReport  = "blocked.report"
	CommandBlockedResolve = "blocked.resolve"
	// CommandAccept, CommandReject and CommandCancel are the owning
	// leader's decisions on an assignment: accepting a result fast-forwards
	// the job branch to it, or for a validation records its findings and
	// delivers the report to Engineering's leader; rejecting returns the
	// assignment to its worker, and cancelling ends it once the worker has
	// pushed its branch, if it has one, and stopped.
	CommandAccept = "accept"
	CommandReject = "reject"
	CommandCancel = "cancel"
	// CommandRedispatch is the owning leader sending an active assignment, or
	// one that needs reconciliation, to its worker again in a new dispatch.
	CommandRedispatch = "redispatch"
	// CommandAnswerRedeliver and CommandAnswerRetire are the owning leader
	// recovering a decision answer whose worker's session exited before
	// acknowledging it: delivering it again in a new dispatch, or retiring it.
	CommandAnswerRedeliver = "answer.redeliver"
	CommandAnswerRetire    = "answer.retire"
	// CommandPush is the delivery owner asking the daemon to push a job's
	// branch to origin, which it refuses over commits AsmAI did not make.
	CommandPush = "push"
	// CommandDecisionRecord is a leader recording a decision. Of
	// Engineering's leader, the choice rerun-failed is its judgment that a
	// head's red checks failed for a reason unrelated to the change, on which
	// the daemon re-runs their failed jobs once.
	CommandDecisionRecord = "decision.record"
	// CommandTrailers answers a worker's git hook with the trailers its
	// commit carries.
	CommandTrailers = "trailers"
	// CommandDecisionRequest is an agent requesting a decision, or issuing a
	// new version of one: a worker's goes to its owning leader and a
	// leader's to the user through Coordination. CommandDecisionAnswer is
	// the owning leader answering one version of a worker's request, and
	// CommandDecisions the user listing the pending decisions, which
	// answers none.
	CommandDecisionRequest = "decision.request"
	CommandDecisionAnswer  = "decision.answer"
	CommandDecisions       = "decisions"
	// CommandDecisionPresent is Coordination recording that it showed the
	// user a version of a decision request in the conversation, once. The
	// user's answer is a witnessed message given after it, which Coordination
	// records with CommandDecisionRecord naming the request and version.
	CommandDecisionPresent = "decision.present"
	// CommandDecisionRecord with a Grant records one: the user's authorization
	// of an exact decision or action, given in words, at its scope. An agent
	// checks a consequential action against the grants with CommandAuthorize
	// before it takes it, and a leader reports that a condition of a grant no
	// longer holds with CommandGrantInvalidate. The user lists grants with
	// CommandGrants and revokes one with CommandGrantRevoke.
	CommandAuthorize       = "authorize"
	CommandGrants          = "grants"
	CommandGrantRevoke     = "grant.revoke"
	CommandGrantInvalidate = "grant.invalidate"
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
	Source     string `json:"source,omitempty"`
	Repository string `json:"repository,omitempty"`
	// DefaultBranch and Options are the user's choices for repo.add beyond
	// what the clone says: the branch jobs start from, and the repository's
	// other settings.
	DefaultBranch string                    `json:"default_branch,omitempty"`
	Options       *config.RepositoryOptions `json:"options,omitempty"`
	// Configuration is the configuration file's content that config.apply
	// shows the change of, and Confirm has it applied.
	Configuration string       `json:"configuration,omitempty"`
	Confirm       bool         `json:"confirm,omitempty"`
	Preview       *ConfigApply `json:"preview,omitempty"`

	Job           int64    `json:"job,omitempty"`
	Follows       int64    `json:"follows,omitempty"`
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
	// BlockedReport is the leader's blocked report a blocked.resolve
	// resolves, and Note what changed.
	BlockedReport int64  `json:"blocked_report,omitempty"`
	Note          string `json:"note,omitempty"`
	// Assignment and Reasons say which assignment a decision is about and
	// why the owning leader made it.
	Assignment int64    `json:"assignment,omitempty"`
	Reasons    []string `json:"reasons,omitempty"`
	// ReadOnly and Commit make an assign a read-only validation fixed at the
	// commit, which is the job branch's tip.
	ReadOnly bool   `json:"read_only,omitempty"`
	Commit   string `json:"commit,omitempty"`
	// Skill and Skills make an assign name the skill the worker is to run,
	// and narrow its skill list to Skills.
	Skill  string   `json:"skill,omitempty"`
	Skills []string `json:"skills,omitempty"`
	// EndsJob is the owning leader of a job with no repository accepting,
	// with an accept, the job's outcome against its mandate, which ends the job.
	EndsJob bool `json:"ends_job,omitempty"`
	// Prototype is the owning leader of a writing assignment in a repository
	// accepting, with an accept, its result as a prototype that stays on its
	// own assignment branch, so that the job branch does not move.
	Prototype bool `json:"prototype,omitempty"`
	// Approved is the decision record of the user's own decision that
	// starts the skill an assign names, a skill that starts only on request
	// which the job's mandate does not call for.
	Approved int64 `json:"approved,omitempty"`
	// Choice and Rationale are what a recorded decision chose and why, with
	// Evidence what it rests on.
	Choice    string `json:"choice,omitempty"`
	Rationale string `json:"rationale,omitempty"`
	// Request and Version name the decision request, and the version of it,
	// that a decision request or an answer is about; Content, Questions and
	// Blocks are what a request holds besides its Kind: the agent's own
	// content above the questions, the questions, and the assignments whose
	// work waits for the choice. ReliedOn names the record an answer rests
	// on, or the mandate or grant a recorded decision relied on.
	Request   int64            `json:"request,omitempty"`
	Version   int              `json:"version,omitempty"`
	Content   string           `json:"content,omitempty"`
	Questions []store.Question `json:"questions,omitempty"`
	Blocks    []int64          `json:"blocks,omitempty"`
	ReliedOn  string           `json:"relied_on,omitempty"`
	// Step, with Skill, is the human step of a skill that a worker's step
	// request asks for. An answer to it rests on Records, the user's decision
	// records of the job, and Grants, the grants whose scope covers it.
	Step   string  `json:"step,omitempty"`
	Grants []int64 `json:"grants,omitempty"`
	// Carries is the worker's decision request that a leader's request
	// carries to the user, and UserAnswer the leader's request, answered by
	// the user, whose answer the leader returns to the worker.
	Carries    int64 `json:"carries,omitempty"`
	UserAnswer int64 `json:"user_answer,omitempty"`
	// Findings are the findings of the job that a leader's request of kind
	// finding or dispute is about, which the daemon quotes to the user.
	Findings []int64 `json:"findings,omitempty"`
	// DecisionSource and Cites say whether a recorded decision is delegated or
	// human, and for a human one the witnessed message it cites. Assumptions and
	// ReopenWhen are its material assumptions and the conditions that would
	// reopen it.
	DecisionSource string   `json:"decision_source,omitempty"`
	Cites          int64    `json:"cites,omitempty"`
	Assumptions    []string `json:"assumptions,omitempty"`
	ReopenWhen     []string `json:"reopen_when,omitempty"`
	// Records are the decision records of the job that an assign or a
	// handoff send passes on.
	Records []int64 `json:"records,omitempty"`
	// Grant is the grant a decision.record records, Action the consequential
	// action an authorize checks, and GrantID and Why the grant a revocation
	// or an invalidation ends, and for an invalidation the condition that no
	// longer holds.
	Grant   *store.GrantInput    `json:"grant,omitempty"`
	Action  *store.ActionRequest `json:"action,omitempty"`
	GrantID int64                `json:"grant_id,omitempty"`
	Why     string               `json:"why,omitempty"`
	// Log is a Dashboard log read, and Entry the journal ID of the entry a
	// Dashboard entry read is for.
	Log   *LogQuery `json:"log,omitempty"`
	Entry int64     `json:"entry,omitempty"`
}

// Response is the daemon's answer to a Request: one JSON line.
type Response struct {
	Error     string                  `json:"error,omitempty"`
	Status    *Status                 `json:"status,omitempty"`
	Journal   []store.Entry           `json:"journal,omitempty"`
	Providers []store.ProviderInstall `json:"providers,omitempty"`
	// Lavish is the factory's Lavish server and its pending pages.
	Lavish *LavishStatus `json:"lavish,omitempty"`
	// Checks are the checks the last start ran.
	Checks  []Check  `json:"checks,omitempty"`
	Leaders []Leader `json:"leaders,omitempty"`
	// Prompts are the native prompts agents wait at that nobody can
	// answer.
	Prompts []WaitingPrompt `json:"prompts,omitempty"`
	Agents  []store.Agent   `json:"agents,omitempty"`
	// Change is one tick of the Dashboard's change subscription.
	Change *DashboardChange `json:"change,omitempty"`
	// Dashboard is the Dashboard's snapshot of the factory.
	Dashboard *DashboardSnapshot `json:"dashboard,omitempty"`
	// Log is a page of journal summaries, and LogEntry one entry with its
	// message body, for the Dashboard.
	Log      *DashboardLog   `json:"log,omitempty"`
	LogEntry *DashboardEntry `json:"log_entry,omitempty"`
	// JobDetail is one job's detail, for the Dashboard.
	JobDetail *DashboardJobDetail `json:"job_detail,omitempty"`
	// Attached is the agent an attach follows, and Screen its screen when
	// a snapshot was asked for.
	Attached *Leader   `json:"attached,omitempty"`
	Screen   *vt.State `json:"screen,omitempty"`
	// Repositories are the registered repositories, and Repository the one
	// a repo command added, showed or removed.
	Repositories []store.Repository `json:"repositories,omitempty"`
	Repository   *store.Repository  `json:"repository,omitempty"`
	// Writable are the folders outside their workspaces that the shown
	// repository's workers may write.
	Writable []config.Folder `json:"writable,omitempty"`
	// Removed are the paths a job clean removed.
	Removed []string `json:"removed,omitempty"`
	// Cancellation is what a job cancel did.
	Cancellation *store.JobCancellation `json:"cancellation,omitempty"`
	// Returned says an accept found the job branch moved after the result
	// was submitted, so it returned the result to its worker instead of
	// accepting it: not a rejection, and Decision is nil.
	Returned bool `json:"returned,omitempty"`
	// Continues says a job link had the follow-up continue the job branch
	// and the pull request of the job it follows, and Starts why it starts
	// afresh otherwise.
	Continues *store.PullRequest `json:"continues,omitempty"`
	Starts    string             `json:"starts,omitempty"`
	Jobs      []store.Job        `json:"jobs,omitempty"`
	Job       *store.Job         `json:"job,omitempty"`
	Brief     string             `json:"brief,omitempty"`
	Handoff   *store.Handoff     `json:"handoff,omitempty"`
	Dispatch  *store.Dispatch    `json:"dispatch,omitempty"`
	Messages  []store.Message    `json:"messages,omitempty"`
	// DecisionRequest is the version of a decision request that a request or
	// an answer made, and DecisionRequests the pending ones `asmai
	// decisions` lists.
	DecisionRequest  *store.DecisionRequest  `json:"decision_request,omitempty"`
	DecisionRequests []store.DecisionRequest `json:"decision_requests,omitempty"`
	Assignment       *store.Assignment       `json:"assignment,omitempty"`
	Effect           *store.Effect           `json:"effect,omitempty"`
	Report           *store.Report           `json:"report,omitempty"`
	Decision         *store.Decision         `json:"decision,omitempty"`
	JobBranch        *store.JobBranch        `json:"job_branch,omitempty"`
	Prototype        *store.Prototype        `json:"prototype,omitempty"`
	Push             *store.Push             `json:"push,omitempty"`
	// BlockedReport is the leader's blocked report a blocked.report filed or
	// a blocked.resolve resolved.
	BlockedReport *store.BlockedReport `json:"blocked_report,omitempty"`
	// PullRequest is the pull request the delivery owner's effect named,
	// as the daemon recorded it.
	PullRequest *store.PullRequest `json:"pull_request,omitempty"`
	// DecisionRecord is the decision a leader recorded, and Rerun the
	// daemon's re-run of a head's failed jobs on it.
	DecisionRecord *store.DecisionRecord `json:"decision_record,omitempty"`
	Rerun          *store.Rerun          `json:"rerun,omitempty"`
	// Grant is the grant a leader recorded, Grants the grants `asmai grants`
	// lists, Authorization the grant's cover of an action that was checked,
	// and GrantEnding what ending a grant leaves.
	Grant         *store.Grant         `json:"grant,omitempty"`
	Grants        []store.Grant        `json:"grants,omitempty"`
	Authorization *store.Authorization `json:"authorization,omitempty"`
	GrantEnding   *store.GrantEnding   `json:"grant_ending,omitempty"`
	// ConfigApply is what config.apply shows, or applied.
	ConfigApply *ConfigApply `json:"config_apply,omitempty"`
	// Trailers are the trailers a worker's commit carries, each as
	// "Key: value".
	Trailers []string `json:"trailers,omitempty"`
	// NoTrailers says the worker's repository has the trailers switched off
	// in the configuration file, so its commits carry none.
	NoTrailers bool `json:"no_trailers,omitempty"`
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
	// ClaudeRecord is the file Claude Code records the user's directory
	// trust in, ~/.claude.json, which the daemon reads and never writes; with
	// none, no agent starts.
	ClaudeRecord string
	// ClaudeVersion reads the version the Claude Code at path reports, which
	// the checks require to be the pinned one; with none, it is
	// providers.ClaudeCodeVersion, which runs it with --version.
	ClaudeVersion func(ctx context.Context, path string) (string, error)
	// GOOS and GOARCH name the platform the checks certify; with none, the
	// platform this daemon runs on.
	GOOS, GOARCH string
	// Lavish tunes the supervision of the factory's Lavish server; its zero
	// value is how the daemon runs it.
	Lavish LavishConfig
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
		stop:    make(chan *net.UnixConn, 1),
		lookNow: make(chan struct{}, 1),
	}
	d.lavish = d.newLavish()
	d.pages = d.newPages()
	d.applied = readApplied(cfg.ConfigFile)
	registered, err := st.Repositories()
	if err != nil {
		return fmt.Errorf("reading the registered repositories: %w", err)
	}
	d.applied = withRegisteredRepositories(d.applied, registered)
	if err := d.sweepTmp(); err != nil {
		return fmt.Errorf("sweeping the workers' temporary directories: %w", err)
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
	// The daemon watches CI itself, with no agent, and removes what a job
	// that ended left local.
	d.removeEndedJobsLocal()
	d.wg.Add(1)
	go d.watchCI()
	// The daemon runs the factory's Lavish server itself, so that review
	// pages outlive any agent; it stops with the work.
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		d.lavish.Run(d.work)
	}()
	// The daemon renders the decision requests answered in Lavish, collects
	// what the user sends from their pages and routes it.
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		d.pages.run(d.work)
	}()

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
	// lavish supervises the factory's Lavish server.
	lavish *lavish.Server
	// pages keeps the Lavish pages of decision requests answered there.
	pages *pages
	// lookNow has the CI watch look at every job's checks at once, as after
	// a push or a re-run.
	lookNow chan struct{}

	// assignMu makes giving assignments one at a time: each makes a job
	// branch and a workspace of AsmAI's clone, and takes the next worker
	// number and slot.
	assignMu sync.Mutex

	// configMu guards applied.
	configMu sync.Mutex
	// applied is the configuration the running agents were started on, as
	// of the daemon's start or the user's last apply: what `asmai config
	// apply` compares the file with.
	applied config.Config

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
	// versionSeen holds, by path, the executable the version check last read
	// the version of, so that a start looks again only at a file that
	// changed, as one that updated itself does.
	versionSeen map[string]versionRead
	// rulesMu guards rulesOK.
	rulesMu sync.Mutex
	// rulesOK is the install path whose permission rules check passed, so
	// the probe runs once per daemon run rather than at every agent start.
	rulesOK struct {
		path  string
		check Check
		// managedOnly is set when that check found the managed policy.
		managedOnly bool
		// autoMode is whether Claude Code reported auto mode available to a
		// session, and why not when it did not.
		autoMode    bool
		autoModeWhy string
	}
	stopping  bool
	stoppedBy string
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
		reply(conn, Response{Status: &d.status, Checks: checks, Leaders: d.leaderStates(), Prompts: d.waitingPrompts()})
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
	case CommandDashboardSnapshot:
		snapshot, err := d.dashboardSnapshot()
		if err != nil {
			d.log.Error("reading the dashboard snapshot", "error", err.Error())
			reply(conn, Response{Error: err.Error()})
			return false
		}
		reply(conn, Response{Dashboard: &snapshot})
	case CommandDashboardSubscribe:
		d.subscribe(conn, r)
	case CommandDashboardLog:
		var q LogQuery
		if req.Log != nil {
			q = *req.Log
		}
		page, err := d.dashboardLog(q)
		if err != nil {
			d.log.Error("reading the dashboard log", "error", err.Error())
			reply(conn, Response{Error: err.Error()})
			return false
		}
		reply(conn, Response{Log: &page})
	case CommandDashboardEntry:
		entry, err := d.dashboardEntry(req.Entry)
		if err != nil {
			reply(conn, Response{Error: err.Error()})
			return false
		}
		reply(conn, Response{LogEntry: &entry})
	case CommandDashboardJob:
		detail, err := d.dashboardJob(req.Job)
		if err != nil {
			reply(conn, Response{Error: err.Error()})
			return false
		}
		reply(conn, Response{JobDetail: &detail})
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
	case CommandLavish:
		reply(conn, Response{Lavish: d.lavishStatus()})
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
	case CommandConfigApply:
		d.configApplyCommand(conn, req)
	case CommandJobOpen, CommandJobLink, CommandJobCancel, CommandJobClean, CommandJobs, CommandJob, CommandBrief:
		d.jobCommand(conn, req)
	case CommandInbox, CommandHandoffSend, CommandHandoffAccept, CommandHandoffClarify, CommandHandoffDecline:
		d.handoffCommand(conn, req)
	case CommandAssign, CommandEffect, CommandResult, CommandBlocked, CommandAccept, CommandReject, CommandCancel, CommandRedispatch, CommandAnswerRedeliver, CommandAnswerRetire:
		d.assignmentCommand(conn, req)
	case CommandBlockedReport, CommandBlockedResolve:
		d.blockedCommand(conn, req)
	case CommandPush:
		d.pushCommand(conn, req)
	case CommandAuthorize, CommandGrants, CommandGrantRevoke, CommandGrantInvalidate:
		d.grantCommand(conn, req)
	case CommandDecisionRecord:
		if req.Grant != nil {
			d.grantCommand(conn, req)
		} else if req.Request != 0 {
			// The user's answer to a decision request, in the conversation.
			d.decisionRequestCommand(conn, req)
		} else {
			d.decisionRecordCommand(conn, req)
		}
	case CommandDecisionRequest, CommandDecisionAnswer, CommandDecisions, CommandDecisionPresent:
		d.decisionRequestCommand(conn, req)
	case CommandTrailers:
		trailers, err := d.trailers(req)
		if err != nil {
			reply(conn, Response{Error: err.Error()})
			return false
		}
		reply(conn, Response{Trailers: trailers, NoTrailers: len(trailers) == 0})
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
