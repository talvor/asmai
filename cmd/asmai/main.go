// SPDX-License-Identifier: Apache-2.0

// Command asmai is AsmAI's single executable: the per-user daemon, and the
// commands that reach it over its Unix socket.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/talvor/asmai"
	"github.com/talvor/asmai/internal/config"
	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/logfile"
	"github.com/talvor/asmai/internal/providers"
	"github.com/talvor/asmai/internal/statedir"
	"github.com/talvor/asmai/internal/store"
)

// version is set at build time with -ldflags "-X main.version=v1.2.3".
// Development builds keep "dev".
var version = "dev"

const usage = `usage: asmai [<command>] [--json]

commands:
  asmai, asmai chat           talk with Coordination: attach this terminal to Coordination's leader,
                              starting the factory first if it is stopped. What you type reaches
                              Coordination, and each message you submit is journaled word for word.
                              Ctrl-] leaves the conversation, which pauses nothing
  asmai start [--foreground]  start the factory: run its checks, its daemon and Coordination's leader;
                              the daemon runs in the background unless --foreground
  asmai stop                  persist the factory's state and stop its daemon and agents; queued
                              messages and open handoffs survive it, and the next start continues
                              the work: it restores Coordination and each leader with open work, and
                              runs each worker that had stopped at a boundary again, resuming its
                              native session where it can, in a new dispatch
  asmai status                show the daemon, its version and each leader
  asmai agents                list the factory's agents
	asmai jobs                  list numbered jobs
	asmai job <number>          show a job and its witnessed request
	asmai job open --message latest|<id> --repository <name> --reading <text> --mandate tested-pr --criterion <text>
	                            Coordination's leader opens a job; repeat --criterion for each acceptance criterion
	asmai brief <number>        read a job's canonical brief
	asmai inbox [--dispatch <id>]  fetch messages; a fetch records delivery once per message
	asmai handoff send --job <number> --to <leader> --outcome <text> --decisions <text> --evidence <text> --constraints <text> --permissions <text> --criterion <text>
	asmai handoff accept|clarify|decline --handoff <id> [--answer <text>]
	asmai assign --job <number> --outcome <text> --criterion <text>
	                            Engineering's leader gives a writing assignment to a worker, in a workspace of
	                            AsmAI's clone on its own branch; repeat --criterion for each acceptance criterion
	asmai assign --read-only --commit <sha> --job <number> --outcome <text> --criterion <text>
	                            Quality's leader gives a validation to a worker, in a clean workspace fixed at the
	                            job branch's exact head, with the job's mandate and criteria
	asmai effect <kind> <ref>   a worker or the delivery owner records an effect it made, tagged with its dispatch
	asmai result --evidence <text> --test <text>|none --check '<command> -> <outcome>'|none --gap <text>|none --pr-section <text> [--artifact <text>]
	                            a worker submits its assignment's result; repeat a flag for each value
	asmai result --evidence <text> --check '<command> -> <outcome>'|none --gap <text>|none --finding '<blocking|advisory|needs-you>: <text>'|none [--resolved <id>] [--unresolved <id>]
	                            a Quality worker submits its validation report
	asmai blocked --reason <text> [--needs <text>]
	                            a worker reports that it cannot go on
	asmai accept --assignment <id> --reason <text>
	                            Engineering's leader accepts the result of an assignment it owns, and the
	                            daemon fast-forwards the job branch to it; Quality's leader accepts a validation
	                            report, and the daemon delivers it to Engineering's leader. Repeat --reason
	                            for each reason
	asmai reject --assignment <id> --reason <text>
	                            a leader returns the result to the same worker, in a new dispatch
	asmai cancel --assignment <id> --reason <text>
	                            a leader cancels an assignment it owns; its worker pushes its assignment
	                            branch, if it has one, and stops
  asmai attach <agent>        show an agent's terminal and observe it; Ctrl-] detaches.
                              Address an agent as name@role, or by its role for its leader
  asmai log [--follow]        print the daemon's log; --follow waits for more
  asmai export                write the journal out for inspection
  asmai providers install     fetch the pinned provider CLIs, after you confirm
  asmai providers list        show the provider CLIs AsmAI has installed
  asmai repo add <path-or-url> [--name <name>]
                              register a repository: record its location, origin and default
                              branch in the store and the configuration file, and make AsmAI's
                              own clone of it, fetched from origin. Your own checkout is never
                              used for work
  asmai repo list             list the registered repositories
  asmai repo show <name>      show what is recorded for a registered repository
  asmai repo remove <name>    remove a repository's record, its configuration entry and its
                              clone; refused while it has an open job
  asmai hook                  report a hook event to the daemon; agent sessions' hooks run it
  asmai version               print the version of this executable
  asmai notices               print AsmAI's license and the third-party notices

Every command accepts --json to print JSON instead of tables; asmai attach --json prints the
agent's screen as it is now, and asmai chat --json Coordination's.
`

// groups are the commands that are grouped by noun, with their subcommands.
var groups = map[string][]string{
	"providers": {"install", "list"},
	"repo":      {"add", "list", "show", "remove"},
	"handoff":   {"send", "accept", "clarify", "decline"},
}

// positional are the commands that take arguments, with what to say when
// each is missing.
var positional = map[string][]string{
	"attach":      {"name the agent, such as leader@coordination or coordination"},
	"repo add":    {"name a local checkout or a URL"},
	"repo show":   {"name the repository, as `asmai repo list` shows it"},
	"repo remove": {"name the repository, as `asmai repo list` shows it"},
	"job show":    {"name the job number, as `asmai jobs` shows it"},
	"brief":       {"name the job number, as `asmai jobs` shows it"},
	"effect":      {"name the kind of effect, one word such as commit or push", "name what the effect is of, such as the commit or the branch pushed"},
}

// logPoll is how often `asmai log --follow` looks for new lines.
const logPoll = 200 * time.Millisecond

// pins is the pins file asmai installs providers from: the one embedded in
// it, except in tests.
var pins = asmai.Pins

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	original := slices.Clone(args)
	if len(args) > 0 && (args[0] == "help" || args[0] == "-h" || args[0] == "--help") {
		if os.Getenv(daemon.SessionCredential) != "" {
			fmt.Fprintln(stderr, "asmai: an agent may run only its coordination commands")
			return 1
		}
		fmt.Fprint(stdout, usage)
		return 0
	}
	// A bare asmai, flags and all, is the conversation.
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		args = append([]string{"chat"}, args...)
	}
	name, args := args[0], args[1:]
	if name == "job" {
		if len(args) > 0 && args[0] == "open" {
			name, args = "job open", args[1:]
		} else {
			name = "job show"
		}
	}
	if subcommands, ok := groups[name]; ok {
		if len(args) == 0 || !slices.Contains(subcommands, args[0]) {
			fmt.Fprintf(stderr, "asmai %s: want %s\n\n%s", name, strings.Join(subcommands[:len(subcommands)-1], ", ")+" or "+subcommands[len(subcommands)-1], usage)
			return 2
		}
		name, args = name+" "+args[0], args[1:]
	}
	flags := flag.NewFlagSet("asmai "+name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	jsonOutput := flags.Bool("json", false, "print JSON instead of tables")
	foreground, follow, repoName := new(bool), new(bool), new(string)
	var message string
	var reading, mandate, repository string
	var handoffJob, handoffID, dispatchID int64
	var recipient, outcome, decisions, evidence, constraints, permissions, answer string
	var criteria criteriaFlags
	var assignJob int64
	var assignReadOnly bool
	var assignCommit string
	var assignOutcome, blockedReason, blockedNeeds string
	var decidedAssignment int64
	var reasons listFlags
	var resultArgs resultFlags
	switch name {
	case "start":
		foreground = flags.Bool("foreground", false, "keep the daemon attached to this terminal")
	case "log":
		follow = flags.Bool("follow", false, "after the log, print each new line as it is written")
	case "repo add":
		repoName = flags.String("name", "", "register the repository under this name, not one made from its origin")
	case "job open":
		flags.StringVar(&message, "message", "", "latest or the witnessed message's journal ID")
		flags.StringVar(&repository, "repository", "", "registered repository name")
		flags.StringVar(&reading, "reading", "", "Coordination's reading of the user's words")
		flags.StringVar(&mandate, "mandate", store.MandateTestedPR, "job mandate")
		flags.Var(&criteria, "criterion", "one acceptance criterion; may be repeated")
	case "handoff send":
		flags.Int64Var(&handoffJob, "job", 0, "job number")
		flags.StringVar(&recipient, "to", "", "receiving leader")
		flags.StringVar(&outcome, "outcome", "", "requested outcome")
		flags.StringVar(&decisions, "decisions", "", "relevant decisions")
		flags.StringVar(&evidence, "evidence", "", "relevant evidence")
		flags.StringVar(&constraints, "constraints", "", "constraints")
		flags.StringVar(&permissions, "permissions", "", "permissions granted")
		flags.Var(&criteria, "criterion", "one acceptance criterion; may be repeated")
	case "handoff accept", "handoff clarify", "handoff decline":
		flags.Int64Var(&handoffID, "handoff", 0, "handoff ID")
		flags.StringVar(&answer, "answer", "", "answer, question or reason")
	case "inbox":
		flags.Int64Var(&dispatchID, "dispatch", 0, "fetch one dispatch")
	case "assign":
		flags.Int64Var(&assignJob, "job", 0, "job number")
		flags.StringVar(&assignOutcome, "outcome", "", "the outcome the worker is to deliver")
		flags.Var(&criteria, "criterion", "one acceptance criterion; may be repeated")
		flags.BoolVar(&assignReadOnly, "read-only", false, "give a validation, which changes nothing, instead of a writing assignment")
		flags.StringVar(&assignCommit, "commit", "", "with --read-only, the job branch's exact head commit the validation is fixed at")
	case "effect":
	case "result":
		flags.Var(&resultArgs.evidence, "evidence", "what shows the work does; may be repeated")
		flags.Var(&resultArgs.artifacts, "artifact", "a link to something the work produced; may be repeated")
		flags.Var(&resultArgs.tests, "test", "a test added, or none; may be repeated")
		flags.Var(&resultArgs.checks, "check", "a check run and its outcome at the commit, '<command> -> <outcome>', or none; may be repeated")
		flags.Var(&resultArgs.gaps, "gap", "an unresolved gap, or none; may be repeated")
		flags.StringVar(&resultArgs.prSection, "pr-section", "", "the worker's part of the pull request: what changed, with before-and-after evidence")
		flags.Var(&resultArgs.findings, "finding", "a validation's finding, '<blocking|advisory|needs-you>: <text>', or none; may be repeated")
		flags.Var(&resultArgs.resolved, "resolved", "an earlier blocking finding's ID that is resolved at this commit; may be repeated")
		flags.Var(&resultArgs.unresolved, "unresolved", "an earlier blocking finding's ID that is still open at this commit; may be repeated")
	case "blocked":
		flags.StringVar(&blockedReason, "reason", "", "why the worker cannot go on")
		flags.StringVar(&blockedNeeds, "needs", "", "what would unblock the worker")
	case "accept", "reject", "cancel":
		flags.Int64Var(&decidedAssignment, "assignment", 0, "assignment ID")
		flags.Var(&reasons, "reason", "a reason for the decision; may be repeated")
	case "chat", "stop", "status", "agents", "jobs", "job show", "brief", "attach", "hook", "export", "version", "notices", "providers install", "providers list", "repo list", "repo show", "repo remove":
	default:
		fmt.Fprintf(stderr, "asmai: unknown command %q\n\n%s", name, usage)
		return 2
	}
	if err := flags.Parse(args); err != nil {
		return 2
	}
	var operands []string
	for _, want := range positional[name] {
		if flags.NArg() == 0 {
			fmt.Fprintf(stderr, "asmai %s: %s\n\n%s", name, want, usage)
			return 2
		}
		operands = append(operands, flags.Arg(0))
		// Flags may come after an argument too.
		if err := flags.Parse(flags.Args()[1:]); err != nil {
			return 2
		}
	}
	var arg string
	if len(operands) > 0 {
		arg = operands[0]
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "asmai %s: unexpected argument %q\n\n%s", name, flags.Arg(0), usage)
		return 2
	}
	o := &output{stdout: stdout, stderr: stderr, json: *jsonOutput}
	if os.Getenv(daemon.SessionCredential) != "" && !slices.Contains([]string{"status", "agents", "jobs", "brief", "job open", "hook", "inbox", "handoff send", "handoff accept", "handoff clarify", "handoff decline", "assign", "effect", "result", "blocked", "accept", "reject", "cancel"}, name) {
		if slices.Contains([]string{"start", "stop", "providers install", "providers list", "repo add", "repo list", "repo show", "repo remove"}, name) {
			return o.fail(fmt.Errorf("an agent cannot run this command; ask the user to run `asmai %s`", strings.Join(original, " ")))
		}
		return o.fail(fmt.Errorf("an agent cannot run asmai %s; use an agent coordination command", name))
	}

	switch name {
	case "version":
		if o.json {
			return o.printJSON(map[string]string{"version": version})
		}
		fmt.Fprintf(stdout, "asmai %s\n", version)
		return 0
	case "notices":
		if o.json {
			return o.printJSON(map[string]string{"license": string(asmai.License), "third_party_notices": string(asmai.ThirdPartyNotices)})
		}
		stdout.Write(asmai.License)
		fmt.Fprintln(stdout)
		stdout.Write(asmai.ThirdPartyNotices)
		return 0
	}

	paths, err := statedir.Default()
	if err != nil {
		return o.fail(err)
	}
	switch name {
	case "start":
		if *foreground {
			return startForeground(o, paths)
		}
		return start(o, paths)
	case "stop":
		return stop(o, paths)
	case "status":
		return status(o, paths)
	case "agents":
		return agents(o, paths)
	case "jobs":
		return jobs(o, paths)
	case "job show":
		return jobShow(o, paths, arg)
	case "job open":
		return jobOpen(o, paths, message, repository, reading, mandate, criteria)
	case "brief":
		return jobBrief(o, paths, arg)
	case "inbox":
		return inbox(o, paths, dispatchID)
	case "handoff send":
		return handoffSend(o, paths, handoffJob, recipient, outcome, decisions, evidence, constraints, permissions, criteria)
	case "handoff accept", "handoff clarify", "handoff decline":
		return handoffAnswer(o, paths, name, handoffID, answer)
	case "assign":
		return assign(o, paths, assignJob, assignOutcome, criteria, assignReadOnly, assignCommit)
	case "effect":
		return effect(o, paths, operands[0], operands[1])
	case "result":
		return result(o, paths, resultArgs)
	case "blocked":
		return blocked(o, paths, blockedReason, blockedNeeds)
	case "accept", "reject", "cancel":
		return decide(o, paths, name, decidedAssignment, reasons)
	case "chat":
		return chat(o, paths, stdin)
	case "attach":
		return attach(o, paths, arg, stdin)
	case "hook":
		return hook(o, paths, stdin)
	case "log":
		return printLog(o, paths, *follow)
	case "providers install":
		return providersInstall(o, paths, stdin)
	case "providers list":
		return providersList(o, paths)
	case "repo add":
		return repoAdd(o, paths, arg, *repoName)
	case "repo list":
		return repoList(o, paths)
	case "repo show":
		return repoShow(o, paths, arg)
	case "repo remove":
		return repoRemove(o, paths, arg)
	default: // export
		return export(o, paths)
	}
}

// start starts the daemon in the background and returns once it answers.
func start(o *output, paths statedir.Paths) int {
	executable, err := os.Executable()
	if err != nil {
		return o.fail(fmt.Errorf("finding this executable: %w", err))
	}
	st, already, err := daemon.Start(paths, executable)
	if err != nil {
		return o.fail(err)
	}
	return o.startFactory(st, already, paths)
}

// startFactory has the running daemon run the checks and start Coordination's
// leader, and reports what happened.
func (o *output) startFactory(st daemon.Status, already bool, paths statedir.Paths) int {
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandStart})
	if err != nil {
		return o.fail(err)
	}
	return o.started(st, already, paths, resp)
}

// startForeground runs the daemon in this process, showing its log on
// stderr, until `asmai stop`, an interrupt or a termination signal.
func startForeground(o *output, paths statedir.Paths) int {
	if resp, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandStatus}); err == nil {
		return o.startFactory(*resp.Status, true, paths)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	executable, err := os.Executable()
	if err != nil {
		return o.fail(fmt.Errorf("finding this executable: %w", err))
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return o.fail(fmt.Errorf("finding the configuration file: %w", err))
	}
	configFile, err := config.ActivePath(home)
	if err != nil {
		return o.fail(err)
	}
	p, err := providers.ParsePins(asmai.Pins)
	if err != nil {
		return o.fail(err)
	}
	cfg := daemon.Config{Paths: paths, Version: version, ConfigFile: configFile, Executable: executable, Pins: p}
	// Started in the background, the daemon's stderr is its log, which needs
	// no copy.
	if !isFile(o.stderr, paths.Log) {
		cfg.Mirror = o.stderr
		if !o.json {
			cfg.Mirror = &rendered{w: o.stderr}
		}
	}
	err = daemon.Run(ctx, cfg)
	if errors.Is(err, daemon.ErrAlreadyRunning) {
		if resp, callErr := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandStatus}); callErr == nil {
			return o.startFactory(*resp.Status, true, paths)
		}
	}
	if err != nil {
		return o.fail(err)
	}
	return 0
}

func (o *output) started(st daemon.Status, already bool, paths statedir.Paths, resp daemon.Response) int {
	var failures []daemon.Check
	for _, c := range resp.Checks {
		if !c.OK {
			failures = append(failures, c)
		}
	}
	if len(failures) > 0 {
		daemonState := "Started the factory's daemon"
		if already {
			daemonState = "The factory's daemon is running"
		}
		message := daemonState + ", but not Coordination's leader: " + plural(len(failures), "a check failed", "checks failed")
		for _, l := range resp.Leaders {
			if l.Agent == "leader@coordination" && l.State == store.AgentRunning {
				message = daemonState + " and so is Coordination's leader, but " + plural(len(failures), "a check failed", "checks failed")
			}
		}
		if o.json {
			o.printJSON(map[string]any{"error": message, "started": !already, "daemon": runningJSON(st), "checks": resp.Checks, "leaders": resp.Leaders, "state_dir": paths.Dir})
			return 1
		}
		fmt.Fprintf(o.stderr, "asmai: %s.\n", message)
		for _, c := range resp.Checks {
			if c.OK {
				fmt.Fprintf(o.stderr, "  ok      %s\n", c.Name)
				continue
			}
			fmt.Fprintf(o.stderr, "  failed  %s: %s\n", c.Name, c.Problem)
			fmt.Fprintf(o.stderr, "          fix: %s\n", strings.ReplaceAll(c.Fix, "\n", "\n            "))
		}
		fmt.Fprintln(o.stderr, "Fix what failed, then run `asmai start` again.")
		return 1
	}
	if o.json {
		return o.printJSON(map[string]any{"started": !already, "daemon": runningJSON(st), "checks": resp.Checks, "leaders": resp.Leaders, "state_dir": paths.Dir})
	}
	if already {
		fmt.Fprintln(o.stdout, "The factory is already running; no second daemon was started.")
	} else {
		fmt.Fprintln(o.stdout, "Started the factory.")
	}
	o.table(append(runningRows(st, paths), leaderRows(resp.Leaders)...))
	return 0
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func leaderRows(leaders []daemon.Leader) [][]string {
	var rows [][]string
	for _, l := range leaders {
		row := []string{l.Agent, l.State}
		if l.Input != "" {
			row = append(row, "input: "+l.Input)
		}
		if l.Conversation != "" {
			row = append(row, "conversation open in "+l.Conversation)
		}
		rows = append(rows, row)
	}
	return rows
}

func stop(o *output, paths statedir.Paths) int {
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandStop})
	if errors.Is(err, daemon.ErrNotRunning) {
		if o.json {
			return o.printJSON(map[string]any{"stopped": false, "daemon": map[string]any{"running": false}})
		}
		fmt.Fprintln(o.stdout, "The factory is not running.")
		return 0
	}
	if err != nil {
		return o.fail(err)
	}
	if o.json {
		return o.printJSON(map[string]any{"stopped": true, "daemon": map[string]any{"running": false, "version": resp.Status.Version, "pid": resp.Status.PID}})
	}
	fmt.Fprintln(o.stdout, "Stopped the factory; its state is saved in the store.")
	o.table([][]string{
		{"daemon", "stopped"},
		{"version", resp.Status.Version},
		{"pid", strconv.Itoa(resp.Status.PID)},
		{"store", paths.Store},
	})
	return 0
}

func status(o *output, paths statedir.Paths) int {
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandStatus})
	if errors.Is(err, daemon.ErrNotRunning) {
		if o.json {
			return o.printJSON(map[string]any{"daemon": map[string]any{"running": false}, "state_dir": paths.Dir})
		}
		o.table([][]string{{"daemon", "not running"}, {"state directory", paths.Dir}})
		return 0
	}
	if err != nil {
		return o.fail(err)
	}
	checks := resp.Checks
	if checks == nil {
		checks = []daemon.Check{}
	}
	if o.json {
		return o.printJSON(map[string]any{"daemon": runningJSON(*resp.Status), "leaders": resp.Leaders, "checks": checks, "state_dir": paths.Dir})
	}
	rows := append(runningRows(*resp.Status, paths), leaderRows(resp.Leaders)...)
	failed := false
	for _, c := range checks {
		if !c.OK {
			rows = append(rows, []string{"check failed", c.Name + ": " + c.Problem})
			failed = true
		}
	}
	o.table(rows)
	if failed {
		fmt.Fprintln(o.stdout, "`asmai start` shows how to fix what failed.")
	}
	return 0
}

// agents lists the agents the store records.
func agents(o *output, paths statedir.Paths) int {
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandAgents})
	if err != nil {
		return o.fail(err)
	}
	list := resp.Agents
	if list == nil {
		list = []store.Agent{}
	}
	if o.json {
		return o.printJSON(map[string]any{"agents": list})
	}
	if len(list) == 0 {
		fmt.Fprintln(o.stdout, "No agent has run yet; `asmai start` starts Coordination's leader.")
		return 0
	}
	rows := [][]string{{"AGENT", "STATE", "GENERATION", "PROVIDER", "VERSION", "MODEL", "PID", "STARTED"}}
	for _, a := range list {
		pid := "-"
		if a.State == store.AgentRunning {
			pid = strconv.Itoa(a.PID)
		}
		rows = append(rows, []string{a.Agent, a.State, strconv.Itoa(a.Generation), a.Provider, a.Version, a.Model, pid, a.StartedAt.Local().Format(time.RFC3339)})
	}
	o.table(rows)
	return 0
}

// hook reports what an agent session's hook was given on stdin to the
// daemon, which journals it as an observation of the session named by the
// credential in the environment. It prints nothing on stdout, because Claude
// Code adds what some hooks print to the session, and it never exits 2,
// which would make Claude Code block what the hook reports.
func hook(o *output, paths statedir.Paths, stdin io.Reader) int {
	credential := os.Getenv(daemon.SessionCredential)
	if credential == "" {
		return o.fail(fmt.Errorf("hook: there is no %s: only an agent session's hooks run asmai hook", daemon.SessionCredential))
	}
	payload, err := io.ReadAll(stdin)
	if err != nil {
		return o.fail(fmt.Errorf("hook: reading the payload: %w", err))
	}
	if !json.Valid(payload) {
		return o.fail(errors.New("hook: the payload on stdin is not JSON"))
	}
	if _, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandHook, Session: credential, Payload: payload}); err != nil {
		return o.fail(fmt.Errorf("hook: %w", err))
	}
	if o.json {
		return o.printJSON(map[string]bool{"journaled": true})
	}
	return 0
}

func runningJSON(st daemon.Status) map[string]any {
	return map[string]any{"running": true, "version": st.Version, "pid": st.PID, "started_at": st.StartedAt}
}

func runningRows(st daemon.Status, paths statedir.Paths) [][]string {
	return [][]string{
		{"daemon", "running"},
		{"version", st.Version},
		{"pid", strconv.Itoa(st.PID)},
		{"started", st.StartedAt.Format(time.RFC3339)},
		{"state directory", paths.Dir},
	}
}

// printLog prints the daemon's log from its files, so that it can be read
// when the daemon is not running, such as after a failed start. With --json
// it prints the log's own JSON lines.
func printLog(o *output, paths statedir.Paths, follow bool) int {
	line := func(l []byte) error {
		if !o.json {
			_, err := fmt.Fprintln(o.stdout, logfile.Format(l))
			return err
		}
		if json.Valid(l) && bytes.HasPrefix(bytes.TrimSpace(l), []byte("{")) {
			_, err := fmt.Fprintf(o.stdout, "%s\n", l)
			return err
		}
		// Output the daemon wrote that is not a log record, such as a crash
		// report.
		encoded, _ := json.Marshal(map[string]string{"text": string(l)})
		_, err := fmt.Fprintf(o.stdout, "%s\n", encoded)
		return err
	}
	var err error
	if follow {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		err = logfile.Follow(ctx, paths.Log, logPoll, line)
	} else {
		err = logfile.Read(paths.Log, line)
	}
	if err != nil {
		return o.fail(fmt.Errorf("reading the log: %w", err))
	}
	return 0
}

func export(o *output, paths statedir.Paths) int {
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandExport})
	if err != nil {
		return o.fail(err)
	}
	journal := resp.Journal
	if journal == nil {
		journal = []store.Entry{}
	}
	if o.json {
		return o.printJSON(map[string]any{"journal": journal})
	}
	rows := [][]string{{"ID", "TIME", "KIND", "DETAILS"}}
	for _, e := range journal {
		rows = append(rows, []string{strconv.FormatInt(e.ID, 10), e.At.Local().Format(time.RFC3339), e.Kind, details(e.Data)})
	}
	o.table(rows)
	return 0
}

// providersInstall installs the pinned Claude Code: it shows what it will
// fetch and from where, and once the user confirms, fetches it from Claude
// Code's official channel, checks it against the pins file, keeps it in the
// state directory and has the daemon record it. A pin already installed is
// left as it is. It never runs or changes the user's own Claude Code, and
// never touches ~/.claude.
func providersInstall(o *output, paths statedir.Paths, stdin io.Reader) int {
	p, err := providers.ParsePins(pins)
	if err != nil {
		return o.fail(err)
	}
	platform, ok := providers.Platform(runtime.GOOS, runtime.GOARCH)
	if !ok {
		return o.fail(fmt.Errorf("Claude Code is not published for %s/%s", runtime.GOOS, runtime.GOARCH))
	}
	plan, err := p.ClaudeCodePlan(platform, paths.Providers)
	if err != nil {
		return o.fail(err)
	}
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandProviders})
	if err != nil {
		return o.fail(err)
	}
	for _, installed := range resp.Providers {
		if installed.Name == plan.Name && installed.Version == plan.Version && installed.Path == plan.Path && exists(plan.Path) {
			if o.json {
				return o.printJSON(map[string]any{"installed": false, "provider": installed})
			}
			fmt.Fprintf(o.stdout, "%s %s is already installed; nothing to do.\n", plan.Title, plan.Version)
			o.table(installRows(installed))
			return 0
		}
	}

	// With --json, stdout carries only the result.
	w := o.stdout
	if o.json {
		w = o.stderr
	}
	fmt.Fprintf(w, "asmai will install %s %s, as pinned in this asmai:\n", plan.Title, plan.Version)
	writeTable(w, [][]string{
		{"  fetch", plan.URL},
		{"  from", fmt.Sprintf("%s's official channel (%d MB)", plan.Title, (plan.Download.Size+1<<20-1)>>20)},
		{"  check", "SHA-256 " + plan.Download.SHA256},
		{"  keep at", plan.Path},
	})
	fmt.Fprintf(w, "It is AsmAI's own copy: your own %s and ~/.claude are not touched.\n", plan.Title)
	fmt.Fprint(w, "Install it? [y/N] ")
	answer, _ := bufio.NewReader(stdin).ReadString('\n')
	if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
		if !strings.HasSuffix(answer, "\n") {
			fmt.Fprintln(w)
		}
		return o.fail(errors.New("not confirmed; nothing was fetched or installed"))
	}

	present, err := providers.Present(plan)
	if err != nil {
		return o.fail(err)
	}
	if !present {
		fmt.Fprintf(w, "Fetching %s...\n", plan.URL)
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		if err := providers.Fetch(ctx, http.DefaultClient, plan); err != nil {
			return o.fail(fmt.Errorf("%w; nothing was installed", err))
		}
	}
	resp, err = daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandProviderInstalled, Provider: &store.ProviderInstall{
		Name:    plan.Name,
		Version: plan.Version,
		Path:    plan.Path,
		SHA256:  plan.Download.SHA256,
	}})
	if err != nil {
		return o.fail(err)
	}
	if len(resp.Providers) != 1 {
		return o.fail(errors.New("the daemon did not answer with the install it recorded"))
	}
	installed := resp.Providers[0]
	if o.json {
		return o.printJSON(map[string]any{"installed": true, "provider": installed})
	}
	fmt.Fprintf(o.stdout, "Installed %s %s.\n", plan.Title, plan.Version)
	o.table(installRows(installed))
	return 0
}

func installRows(p store.ProviderInstall) [][]string {
	return [][]string{
		{"provider", p.Name},
		{"version", p.Version},
		{"path", p.Path},
		{"installed", p.InstalledAt.Local().Format(time.RFC3339)},
	}
}

// providersList shows the provider installs the store records.
func providersList(o *output, paths statedir.Paths) int {
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandProviders})
	if err != nil {
		return o.fail(err)
	}
	installs := resp.Providers
	if installs == nil {
		installs = []store.ProviderInstall{}
	}
	if o.json {
		return o.printJSON(map[string]any{"providers": installs})
	}
	if len(installs) == 0 {
		fmt.Fprintln(o.stdout, "No providers are installed; install the pinned ones with `asmai providers install`.")
		return 0
	}
	rows := [][]string{{"NAME", "VERSION", "PATH", "INSTALLED"}}
	for _, p := range installs {
		rows = append(rows, []string{p.Name, p.Version, p.Path, p.InstalledAt.Local().Format(time.RFC3339)})
	}
	o.table(rows)
	return 0
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// details renders a journal entry's data as key=value pairs.
func details(data json.RawMessage) string {
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		return string(data)
	}
	var b bytes.Buffer
	for _, k := range slices.Sorted(maps.Keys(fields)) {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		v := fields[k]
		if s, ok := v.(string); ok {
			fmt.Fprintf(&b, "%s=%s", k, s)
		} else {
			encoded, _ := json.Marshal(v)
			fmt.Fprintf(&b, "%s=%s", k, encoded)
		}
	}
	return b.String()
}

// output prints a command's result: readable tables for the user, or JSON
// with --json.
type output struct {
	stdout, stderr io.Writer
	json           bool
}

func (o *output) printJSON(v any) int {
	enc := json.NewEncoder(o.stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintf(o.stderr, "asmai: %v\n", err)
		return 1
	}
	return 0
}

func (o *output) table(rows [][]string) {
	writeTable(o.stdout, rows)
}

func writeTable(out io.Writer, rows [][]string) {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, row := range rows {
		for i, cell := range row {
			if i > 0 {
				fmt.Fprint(w, "\t")
			}
			fmt.Fprint(w, cell)
		}
		fmt.Fprintln(w)
	}
	w.Flush()
}

// fail reports err and returns the exit code for it: with --json as a JSON
// object on stdout, otherwise on stderr.
func (o *output) fail(err error) int {
	if o.json {
		o.printJSON(map[string]string{"error": err.Error()})
	} else {
		fmt.Fprintf(o.stderr, "asmai: %v\n", err)
	}
	return 1
}

// isFile reports whether w is the file at path.
func isFile(w io.Writer, path string) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	open, err := f.Stat()
	if err != nil {
		return false
	}
	named, err := os.Stat(path)
	return err == nil && os.SameFile(open, named)
}

// rendered writes the log lines it is given rendered for the user.
type rendered struct {
	w io.Writer
}

func (r *rendered) Write(p []byte) (int, error) {
	for line := range bytes.Lines(p) {
		fmt.Fprintln(r.w, logfile.Format(bytes.TrimSuffix(line, []byte("\n"))))
	}
	return len(p), nil
}
