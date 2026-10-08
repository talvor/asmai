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

	"github.com/talvor/asmai/internal/logfile"
	"github.com/talvor/asmai/internal/statedir"
	"github.com/talvor/asmai/internal/store"
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
)

// Request is a command sent to the daemon: one JSON line per connection.
type Request struct {
	Command  string                 `json:"command"`
	Provider *store.ProviderInstall `json:"provider,omitempty"`
}

// Response is the daemon's answer to a Request: one JSON line.
type Response struct {
	Error     string                  `json:"error,omitempty"`
	Status    *Status                 `json:"status,omitempty"`
	Journal   []store.Entry           `json:"journal,omitempty"`
	Providers []store.ProviderInstall `json:"providers,omitempty"`
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

	d := &daemon{
		store: st,
		log:   log,
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

	served := make(chan struct{})
	go func() {
		d.serve(listener)
		close(served)
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

	// Stop serving, wait for the commands being served, then persist.
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
	store  *store.Store
	log    *slog.Logger
	status Status
	// stop receives the connection that asked the daemon to stop, which is
	// answered once it has.
	stop chan *net.UnixConn
	wg   sync.WaitGroup
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
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err == nil {
		err = json.Unmarshal(line, &req)
	}
	if err != nil {
		reply(conn, Response{Error: "the request is not a JSON line"})
		return false
	}
	switch req.Command {
	case CommandStatus:
		reply(conn, Response{Status: &d.status})
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
