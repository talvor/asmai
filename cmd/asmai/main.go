// SPDX-License-Identifier: Apache-2.0

// Command asmai is AsmAI's single executable: the per-user daemon, and the
// commands that reach it over its Unix socket.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/talvor/asmai"
	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/logfile"
	"github.com/talvor/asmai/internal/statedir"
	"github.com/talvor/asmai/internal/store"
)

// version is set at build time with -ldflags "-X main.version=v1.2.3".
// Development builds keep "dev".
var version = "dev"

const usage = `usage: asmai <command> [--json]

commands:
  asmai start [--foreground]  start the factory's daemon, in the background unless --foreground
  asmai stop                  persist the factory's state and stop its daemon
  asmai status                show whether the daemon is running, and its version
  asmai log [--follow]        print the daemon's log; --follow waits for more
  asmai export                write the journal out for inspection
  asmai version               print the version of this executable
  asmai notices               print AsmAI's license and the third-party notices

Every command accepts --json to print JSON instead of tables.
`

// logPoll is how often `asmai log --follow` looks for new lines.
const logPoll = 200 * time.Millisecond

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	name, args := args[0], args[1:]
	flags := flag.NewFlagSet("asmai "+name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	jsonOutput := flags.Bool("json", false, "print JSON instead of tables")
	foreground, follow := new(bool), new(bool)
	switch name {
	case "start":
		foreground = flags.Bool("foreground", false, "keep the daemon attached to this terminal")
	case "log":
		follow = flags.Bool("follow", false, "after the log, print each new line as it is written")
	case "stop", "status", "export", "version", "notices":
	default:
		fmt.Fprintf(stderr, "asmai: unknown command %q\n\n%s", name, usage)
		return 2
	}
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "asmai %s: unexpected argument %q\n\n%s", name, flags.Arg(0), usage)
		return 2
	}
	o := &output{stdout: stdout, stderr: stderr, json: *jsonOutput}

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
	case "log":
		return printLog(o, paths, *follow)
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
	return o.started(st, already, paths)
}

// startForeground runs the daemon in this process, showing its log on
// stderr, until `asmai stop`, an interrupt or a termination signal.
func startForeground(o *output, paths statedir.Paths) int {
	if resp, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandStatus}); err == nil {
		return o.started(*resp.Status, true, paths)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	cfg := daemon.Config{Paths: paths, Version: version}
	// Started in the background, the daemon's stderr is its log, which needs
	// no copy.
	if !isFile(o.stderr, paths.Log) {
		cfg.Mirror = o.stderr
		if !o.json {
			cfg.Mirror = &rendered{w: o.stderr}
		}
	}
	err := daemon.Run(ctx, cfg)
	if errors.Is(err, daemon.ErrAlreadyRunning) {
		if resp, callErr := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandStatus}); callErr == nil {
			return o.started(*resp.Status, true, paths)
		}
	}
	if err != nil {
		return o.fail(err)
	}
	return 0
}

func (o *output) started(st daemon.Status, already bool, paths statedir.Paths) int {
	if o.json {
		return o.printJSON(map[string]any{"started": !already, "daemon": runningJSON(st), "state_dir": paths.Dir})
	}
	if already {
		fmt.Fprintln(o.stdout, "The factory is already running; no second daemon was started.")
	} else {
		fmt.Fprintln(o.stdout, "Started the factory.")
	}
	o.table(runningRows(st, paths))
	return 0
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
	if o.json {
		return o.printJSON(map[string]any{"daemon": runningJSON(*resp.Status), "state_dir": paths.Dir})
	}
	o.table(runningRows(*resp.Status, paths))
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
	w := tabwriter.NewWriter(o.stdout, 0, 0, 2, ' ', 0)
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
