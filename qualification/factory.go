// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/statedir"
	"github.com/talvor/asmai/internal/store"
	"github.com/talvor/asmai/internal/vt"
)

// Factory is the harness user's own factory, run by the asmai built from the
// commit under test: the harness drives it only through asmai's commands and
// the daemon's socket, as a user's terminal does.
type Factory struct {
	asmai string
	paths statedir.Paths
	// env is the environment every command runs in, and so the daemon's.
	env []string
}

// NewFactory returns the factory asmai runs in the state directory paths,
// whose daemon starts with environment env.
func NewFactory(asmai string, paths statedir.Paths, env []string) *Factory {
	return &Factory{asmai: asmai, paths: paths, env: env}
}

// Run runs asmai with args, giving it stdin, and returns what it printed and
// its exit status.
func (f *Factory) Run(ctx context.Context, stdin string, args ...string) (stdout, stderr string, code int, err error) {
	cmd := exec.CommandContext(ctx, f.asmai, args...)
	cmd.Env = f.env
	cmd.Dir = "/"
	cmd.Stdin = strings.NewReader(stdin)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err = cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return out.String(), errOut.String(), exit.ExitCode(), nil
	}
	return out.String(), errOut.String(), 0, err
}

// StartResult is what `asmai start --json` printed.
type StartResult struct {
	Started bool `json:"started"`
	Daemon  struct {
		PID int `json:"pid"`
	} `json:"daemon"`
	Checks  []daemon.Check  `json:"checks"`
	Leaders []daemon.Leader `json:"leaders"`
	Error   string          `json:"error"`
}

// Start runs `asmai start`, which starts the daemon and, if the checks pass,
// Coordination's leader. It returns what start printed and its exit status:
// 1 when a check failed, which leaves the daemon running.
func (f *Factory) Start(ctx context.Context) (StartResult, int, error) {
	stdout, stderr, code, err := f.Run(ctx, "", "start", "--json")
	if err != nil {
		return StartResult{}, 0, fmt.Errorf("running asmai start: %w", err)
	}
	var res StartResult
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		return StartResult{}, code, fmt.Errorf("asmai start exited %d printing %q%s, which is not JSON", code, stdout, stderrNote(stderr))
	}
	return res, code, nil
}

func stderrNote(stderr string) string {
	if stderr = strings.TrimSpace(stderr); stderr != "" {
		return " and " + stderr + " on stderr"
	}
	return ""
}

// failedChecks describes the checks of a start that failed, with their fixes.
func failedChecks(checks []daemon.Check) string {
	var parts []string
	for _, c := range checks {
		if !c.OK {
			parts = append(parts, fmt.Sprintf("%s: %s (fix: %s)", c.Name, c.Problem, c.Fix))
		}
	}
	return strings.Join(parts, "; ")
}

// Stop stops the factory's daemon and agents. A factory that is not running
// is already stopped.
func (f *Factory) Stop(ctx context.Context) error {
	_, stderr, code, err := f.Run(ctx, "", "stop")
	if err != nil {
		return fmt.Errorf("running asmai stop: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("asmai stop exited %d: %s", code, strings.TrimSpace(stderr))
	}
	return nil
}

// Running reports whether a daemon answers on the factory's socket, and its
// process ID if so.
func (f *Factory) Running() (pid int, running bool) {
	resp, err := daemon.Call(f.paths.Socket, daemon.Request{Command: daemon.CommandStatus})
	if err != nil || resp.Status == nil {
		return 0, false
	}
	return resp.Status.PID, true
}

// Leader returns Coordination's leader as the store records it.
func (f *Factory) Leader() (store.Agent, bool, error) {
	resp, err := daemon.Call(f.paths.Socket, daemon.Request{Command: daemon.CommandAgents})
	if err != nil {
		return store.Agent{}, false, err
	}
	address := roles.LeaderOf(roles.Coordination).String()
	for _, a := range resp.Agents {
		if a.Agent == address {
			return a, true, nil
		}
	}
	return store.Agent{}, false, nil
}

// Screen returns the rows of text Coordination's leader shows.
func (f *Factory) Screen() ([]string, error) {
	resp, err := daemon.Call(f.paths.Socket, daemon.Request{Command: daemon.CommandAttach, Agent: roles.LeaderOf(roles.Coordination).String(), Snapshot: true})
	if err != nil {
		return nil, err
	}
	if resp.Screen == nil {
		return nil, errors.New("the daemon did not answer with the leader's screen")
	}
	t, err := vt.FromState(*resp.Screen)
	if err != nil {
		return nil, err
	}
	return t.Lines(), nil
}

// Observed returns the lifecycle events the hooks of generation of agent's
// session reported, in order. It reads only the events' names, never their
// payloads.
func (f *Factory) Observed(agent string, generation int) ([]string, error) {
	resp, err := daemon.Call(f.paths.Socket, daemon.Request{Command: daemon.CommandExport})
	if err != nil {
		return nil, err
	}
	var events []string
	for _, e := range resp.Journal {
		if e.Kind != store.KindObservation {
			continue
		}
		var data struct {
			Agent      string `json:"agent"`
			Generation int    `json:"generation"`
			Event      string `json:"event"`
		}
		if json.Unmarshal(e.Data, &data) == nil && data.Agent == agent && data.Generation == generation {
			events = append(events, data.Event)
		}
	}
	return events, nil
}

// Press types keys into Coordination's leader as a user does in the
// conversation: it opens the conversation, types them and leaves. The harness
// does this only to answer a native prompt it has recognised.
func (f *Factory) Press(keys string) error {
	conn, r, _, err := daemon.Open(f.paths.Socket, daemon.Request{Command: daemon.CommandConversation, Terminal: "the qualification harness"})
	if conn != nil {
		defer conn.Close()
	}
	if err != nil {
		return err
	}
	closed := make(chan struct{})
	go func() {
		io.Copy(io.Discard, r)
		close(closed)
	}()
	enc := json.NewEncoder(conn)
	if err := enc.Encode(daemon.ClientMessage{Keys: []byte(keys)}); err != nil {
		return fmt.Errorf("typing into the conversation: %w", err)
	}
	if err := enc.Encode(daemon.ClientMessage{Leave: true}); err != nil {
		return fmt.Errorf("leaving the conversation: %w", err)
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
	}
	return nil
}
