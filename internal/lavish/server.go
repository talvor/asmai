// SPDX-License-Identifier: Apache-2.0

package lavish

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The supervisor's pace, which a Server's fields override.
const (
	// CheckEvery is how often a running server is asked whether it is well,
	// and how often a server that is not running is tried again.
	CheckEvery = 30 * time.Second
	// StartupGrace is how long a starting server has to answer.
	StartupGrace = 30 * time.Second
	// Unhealthy is how many checks in a row a server may fail before it is
	// restarted.
	Unhealthy = 3
	// RestartDelay is the wait before the server is started again after it
	// exits; it doubles while the server keeps exiting soon after starting,
	// up to MaxRestartDelay.
	RestartDelay    = time.Second
	MaxRestartDelay = 30 * time.Second
	// stableRun is how long a server runs for its next restart to start from
	// RestartDelay again.
	stableRun = 30 * time.Second
	// StopGrace is how long a server has to exit when asked to, before it is
	// killed.
	StopGrace = 5 * time.Second
	// CheckTimeout is how long a server has to answer a check.
	CheckTimeout = 3 * time.Second
)

// Server supervises the factory's Lavish server: it runs the pinned Lavish
// build in the foreground, restarts it whenever it exits, and also checks it
// periodically and restarts it when it does not answer, binds anything but
// the loopback address or serves another state directory (04 rules 20 and
// 21). It never relies on Lavish starting a server itself.
type Server struct {
	// Resolve returns the executable to run: the pinned Lavish build, or why
	// there is none to run, as when it is not installed or is not the pinned
	// one. It is asked before every start.
	Resolve func() (string, error)
	// StateDir is the Lavish state directory, which holds the review pages
	// and so keeps them across restarts.
	StateDir string
	// Port is the port the server listens on.
	Port int
	// Log receives the supervisor's events and each line the server prints.
	Log *slog.Logger
	// Environ is the environment the daemon runs in, which the server's is
	// made from; with none, the process's.
	Environ []string

	// The supervisor's pace; each defaults to the constant of its name.
	CheckEvery   time.Duration
	StartupGrace time.Duration
	Unhealthy    int
	RestartDelay time.Duration
	StopGrace    time.Duration
	CheckTimeout time.Duration

	mu    sync.Mutex
	state State
}

// State is what the supervisor knows of the server.
type State struct {
	// Running says the server is up and has answered its health check.
	Running bool `json:"running"`
	PID     int  `json:"pid,omitempty"`
	// Started is when the running server started.
	Started time.Time `json:"started,omitzero"`
	// Restarts counts the times the supervisor started the server after the
	// first.
	Restarts int `json:"restarts"`
	// Problem says why the server is not running, or is not well.
	Problem string `json:"problem,omitempty"`
}

// State returns what the supervisor knows of the server now.
func (s *Server) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

func (s *Server) update(f func(*State)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(&s.state)
}

func pick[T comparable](v, fallback T) T {
	var zero T
	if v == zero {
		return fallback
	}
	return v
}

// Run supervises the server until ctx is done, and then stops it: the
// server's process has exited when Run returns.
func (s *Server) Run(ctx context.Context) {
	checkEvery := pick(s.CheckEvery, CheckEvery)
	baseDelay := pick(s.RestartDelay, RestartDelay)
	delay := baseDelay
	started := false
	lastProblem := ""
	for ctx.Err() == nil {
		path, err := s.Resolve()
		if err == nil {
			err = s.reclaim(ctx)
		}
		if err != nil {
			s.update(func(st *State) { *st = State{Restarts: st.Restarts, Problem: err.Error()} })
			if err.Error() != lastProblem {
				s.Log.Warn("the Lavish server is not started", "problem", err.Error())
				lastProblem = err.Error()
			}
			if !sleep(ctx, checkEvery) {
				return
			}
			continue
		}
		lastProblem = ""
		if started {
			s.update(func(st *State) { st.Restarts++ })
		}
		started = true
		ran := s.runOnce(ctx, path)
		if ctx.Err() != nil {
			return
		}
		if ran >= stableRun {
			delay = baseDelay
		}
		if !sleep(ctx, delay) {
			return
		}
		delay = min(delay*2, MaxRestartDelay)
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// runOnce runs the server at path until it exits or is stopped, checking it
// as it runs, and returns how long it ran.
func (s *Server) runOnce(ctx context.Context, path string) time.Duration {
	if err := os.MkdirAll(s.StateDir, 0o700); err != nil {
		s.update(func(st *State) { st.Problem = fmt.Sprintf("creating Lavish's state directory: %v", err) })
		s.Log.Error("the Lavish server is not started", "problem", err.Error())
		return 0
	}
	cmd := exec.Command(path, Args(s.Port)...)
	cmd.Dir = s.StateDir
	environ := s.Environ
	if environ == nil {
		environ = os.Environ()
	}
	cmd.Env = Environment(environ, s.StateDir, s.Port)
	out := &lineLogger{log: s.Log}
	cmd.Stdout, cmd.Stderr = out, out
	// Its own process group, so that a signal to the daemon's terminal
	// reaches the server only through the daemon.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	begun := time.Now()
	if err := cmd.Start(); err != nil {
		s.update(func(st *State) { st.Problem = fmt.Sprintf("starting %s: %v", path, err) })
		s.Log.Error("the Lavish server did not start", "path", path, "error", err.Error())
		return 0
	}
	s.Log.Info("the Lavish server started", "pid", cmd.Process.Pid, "path", path, "state_dir", s.StateDir, "url", URL(s.Port))
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	stop := func(why string) {
		s.Log.Warn("stopping the Lavish server", "pid", cmd.Process.Pid, "why", why)
		terminate(cmd, exited, pick(s.StopGrace, StopGrace))
	}
	defer func() {
		out.flush()
		s.update(func(st *State) { st.Running, st.PID = false, 0 })
	}()

	checkEvery := pick(s.CheckEvery, CheckEvery)
	deadline := time.Now().Add(pick(s.StartupGrace, StartupGrace))
	limit := pick(s.Unhealthy, Unhealthy)
	failures := 0
	healthy := false
	tick := time.NewTicker(min(checkEvery, 250*time.Millisecond))
	defer tick.Stop()
	nextCheck := time.Now()
	for {
		select {
		case <-ctx.Done():
			stop("the daemon is stopping")
			return time.Since(begun)
		case err := <-exited:
			s.update(func(st *State) {
				st.Problem = "the server exited" + exitDetail(err)
			})
			s.Log.Warn("the Lavish server exited", "pid", cmd.Process.Pid, "error", errString(err))
			return time.Since(begun)
		case <-tick.C:
		}
		if time.Now().Before(nextCheck) {
			continue
		}
		problem := s.check(ctx)
		if problem == "" {
			failures = 0
			nextCheck = time.Now().Add(checkEvery)
			if !healthy {
				healthy = true
				pid := cmd.Process.Pid
				s.update(func(st *State) { *st = State{Running: true, PID: pid, Started: begun, Restarts: st.Restarts} })
				s.Log.Info("the Lavish server is up", "pid", pid, "url", URL(s.Port))
			}
			continue
		}
		if !healthy {
			// Starting: it has until the grace is over to answer, unless what
			// answers is the wrong server.
			if isWrongServer(problem) || time.Now().After(deadline) {
				s.update(func(st *State) { st.Problem = problem })
				stop("it did not answer within its start-up grace: " + problem)
				return time.Since(begun)
			}
			nextCheck = time.Now().Add(250 * time.Millisecond)
			continue
		}
		failures++
		s.update(func(st *State) { st.Problem = problem })
		s.Log.Warn("the Lavish server failed its check", "problem", problem, "failures", failures)
		nextCheck = time.Now().Add(checkEvery)
		if failures >= limit || isWrongServer(problem) {
			stop(problem)
			return time.Since(begun)
		}
	}
}

func exitDetail(err error) string {
	if err == nil {
		return ""
	}
	return ": " + err.Error()
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// terminate asks the server to exit, and kills it if it has not within grace;
// exited is its process's wait.
func terminate(cmd *exec.Cmd, exited <-chan error, grace time.Duration) {
	cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-exited:
	case <-time.After(grace):
		cmd.Process.Kill()
		<-exited
	}
}

// Health is what the server's /health answers.
type Health struct {
	OK       bool     `json:"ok"`
	App      string   `json:"app"`
	Version  string   `json:"version"`
	StateDir string   `json:"state_dir"`
	Hosts    []string `json:"hosts"`
}

// wrongServer prefixes the problems that no retry mends: the server answering
// is not the factory's, or is not bound to localhost only.
const wrongServer = "wrong server: "

func isWrongServer(problem string) bool {
	return strings.HasPrefix(problem, wrongServer)
}

// health asks the server on the port for its health.
func (s *Server) health(ctx context.Context) (Health, error) {
	ctx, cancel := context.WithTimeout(ctx, pick(s.CheckTimeout, CheckTimeout))
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, URL(s.Port)+"/health", nil)
	if err != nil {
		return Health{}, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Health{}, err
	}
	defer resp.Body.Close()
	var h Health
	if resp.StatusCode != http.StatusOK {
		return Health{}, fmt.Errorf("answered %s", resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&h); err != nil {
		return Health{}, fmt.Errorf("answered something that is not Lavish's health: %w", err)
	}
	return h, nil
}

// check asks the running server whether it is well, and returns what is
// wrong, or "" when nothing is: it answers, it is Lavish, it serves the
// factory's state directory and it listens on loopback addresses only.
func (s *Server) check(ctx context.Context) string {
	h, err := s.health(ctx)
	if err != nil {
		return fmt.Sprintf("the server does not answer on %s: %v", URL(s.Port), err)
	}
	return s.verify(h)
}

// verify returns what is wrong with a server that answered with h.
func (s *Server) verify(h Health) string {
	switch {
	case !h.OK || h.App != "lavish-axi":
		return wrongServer + "the server is not ready or is not Lavish"
	case !sameDir(h.StateDir, s.StateDir):
		return fmt.Sprintf("%sthe server serves the state directory %s, not %s", wrongServer, h.StateDir, s.StateDir)
	}
	for _, host := range h.Hosts {
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			return fmt.Sprintf("%sthe server also listens on %s, but it must listen on localhost only", wrongServer, host)
		}
	}
	return ""
}

// sameDir reports whether a and b name the same directory.
func sameDir(a, b string) bool {
	resolve := func(p string) string {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return r
		}
		return filepath.Clean(p)
	}
	return resolve(a) == resolve(b)
}

// reclaim makes the port free of a server that a previous daemon left
// running: one serving the factory's own state directory is shut down, so
// the supervisor can run its own. Anything else listening is an error, which
// the supervisor reports and tries again.
func (s *Server) reclaim(ctx context.Context) error {
	h, err := s.health(ctx)
	if err != nil {
		var refused *net.OpError
		if errors.As(err, &refused) || errors.Is(err, syscall.ECONNREFUSED) {
			return nil
		}
		return fmt.Errorf("port %d is in use by a program that is not Lavish: %v", s.Port, err)
	}
	if h.App != "lavish-axi" {
		return fmt.Errorf("port %d is in use by a program that is not Lavish", s.Port)
	}
	if !sameDir(h.StateDir, s.StateDir) {
		return fmt.Errorf("port %d is in use by another Lavish, which serves %s; the factory's Lavish needs the port for %s", s.Port, h.StateDir, s.StateDir)
	}
	s.Log.Warn("shutting down the factory's Lavish server that a previous daemon left running", "port", s.Port)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, URL(s.Port)+"/shutdown", nil)
	if err != nil {
		return err
	}
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
	}
	for range 50 {
		if _, err := s.health(ctx); err != nil {
			return nil
		}
		if !sleep(ctx, 100*time.Millisecond) {
			return ctx.Err()
		}
	}
	return fmt.Errorf("the factory's Lavish server that a previous daemon left running did not shut down on port %d", s.Port)
}

// lineLogger logs each line written to it as one event.
type lineLogger struct {
	log *slog.Logger
	mu  sync.Mutex
	buf []byte
}

func (l *lineLogger) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf = append(l.buf, p...)
	for {
		i := bytes.IndexByte(l.buf, '\n')
		if i < 0 {
			break
		}
		line := string(l.buf[:i])
		l.buf = l.buf[i+1:]
		if line != "" {
			l.log.Info("lavish", "line", line)
		}
	}
	return len(p), nil
}

func (l *lineLogger) flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.buf) > 0 {
		l.log.Info("lavish", "line", string(l.buf))
		l.buf = nil
	}
}
