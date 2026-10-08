// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/talvor/asmai/internal/logfile"
	"github.com/talvor/asmai/internal/statedir"
)

// ErrNotRunning is returned by Call when no daemon answers on the socket.
var ErrNotRunning = errors.New("the factory is not running; start it with `asmai start`")

// startTimeout is how long Start waits for a new daemon to answer.
const startTimeout = 30 * time.Second

// Call sends req to the daemon on socket and returns its answer. A daemon
// that answers with an error makes that the error.
func Call(socket string, req Request) (Response, error) {
	conn, _, resp, err := Open(socket, req)
	if conn != nil {
		conn.Close()
	}
	return resp, err
}

// Open sends req to the daemon on socket and returns its answer, with the
// connection and its reader for what the daemon sends after the answer, as
// it does to attach. The caller closes the connection, which is nil when
// the daemon could not be reached. A daemon that answers with an error makes
// that the error.
func Open(socket string, req Request) (net.Conn, *bufio.Reader, Response, error) {
	if req.Session == "" {
		req.Session = os.Getenv(SessionCredential)
	}
	conn, err := net.DialTimeout("unix", socket, 5*time.Second)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
			return nil, nil, Response{}, ErrNotRunning
		}
		return nil, nil, Response{}, fmt.Errorf("reaching the daemon: %w", err)
	}
	line, err := json.Marshal(req)
	if err != nil {
		return conn, nil, Response{}, err
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return conn, nil, Response{}, fmt.Errorf("reaching the daemon: %w", err)
	}
	r := bufio.NewReader(conn)
	answer, err := r.ReadBytes('\n')
	if err != nil {
		return conn, r, Response{}, fmt.Errorf("the daemon did not answer: %w", err)
	}
	var resp Response
	if err := json.Unmarshal(answer, &resp); err != nil {
		return conn, r, Response{}, fmt.Errorf("the daemon's answer is not JSON: %w", err)
	}
	if resp.Error != "" {
		return conn, r, resp, errors.New(resp.Error)
	}
	return conn, r, resp, nil
}

// Start starts the daemon in the background, as executable started with
// `start --foreground` in its own session, and returns once it answers on its
// socket. When a daemon is already running it starts none, and returns the
// running one's status with already set.
func Start(p statedir.Paths, executable string) (status Status, already bool, err error) {
	if resp, err := Call(p.Socket, Request{Command: CommandStatus}); err == nil {
		return *resp.Status, true, nil
	}
	if err := p.Prepare(); err != nil {
		return Status{}, false, err
	}
	// The daemon's own output, such as a crash report, goes to its log, which
	// is where a failed start is explained.
	logs, err := os.OpenFile(p.Log, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return Status{}, false, fmt.Errorf("opening the log: %w", err)
	}
	defer logs.Close()
	offset, err := logs.Seek(0, io.SeekEnd)
	if err != nil {
		return Status{}, false, fmt.Errorf("opening the log: %w", err)
	}
	cmd := exec.Command(executable, "start", "--foreground")
	cmd.Dir = "/"
	cmd.Stdout, cmd.Stderr = logs, logs
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return Status{}, false, fmt.Errorf("starting the daemon: %w", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	deadline := time.After(startTimeout)
	for {
		if resp, err := Call(p.Socket, Request{Command: CommandStatus}); err == nil {
			// A daemon that won a race with this one is just as good.
			return *resp.Status, resp.Status.PID != cmd.Process.Pid, nil
		}
		select {
		case err := <-exited:
			// The daemon exited without answering. Another start may have
			// won the race and still be starting; give it a moment.
			for range 20 {
				if resp, callErr := Call(p.Socket, Request{Command: CommandStatus}); callErr == nil {
					return *resp.Status, true, nil
				}
				time.Sleep(100 * time.Millisecond)
			}
			return Status{}, false, fmt.Errorf("the daemon exited during start (%v):\n%s", err, logSince(p.Log, offset))
		case <-deadline:
			return Status{}, false, fmt.Errorf("the daemon did not answer on %s within %s; see `asmai log`", p.Socket, startTimeout)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// logSince renders what the log at path holds after offset, which is what a
// daemon that failed to start wrote.
func logSince(path string, offset int64) string {
	f, err := os.Open(path)
	if err != nil {
		return "  (the log cannot be read: " + err.Error() + ")"
	}
	defer f.Close()
	if info, err := f.Stat(); err == nil && info.Size() >= offset {
		f.Seek(offset, io.SeekStart)
	}
	var b strings.Builder
	scanner := bufio.NewScanner(f)
	scanner.Buffer(nil, 1<<20)
	for scanner.Scan() {
		b.WriteString("  " + logfile.Format(scanner.Bytes()) + "\n")
	}
	if b.Len() == 0 {
		return "  (it wrote nothing to the log)"
	}
	return strings.TrimSuffix(b.String(), "\n")
}
