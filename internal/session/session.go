// SPDX-License-Identifier: Apache-2.0

// Package session runs an agent's provider CLI in a pseudo-terminal the
// daemon owns. The session emulates the terminal with AsmAI's own emulation,
// answering the provider's queries about it, and lets attach clients follow
// its screen. Nothing is ever typed into it here: attaching observes.
package session

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"

	"github.com/talvor/asmai/internal/vt"
)

// Spec is the provider CLI a session runs, and its terminal's size.
type Spec struct {
	Path string
	Args []string
	// Env is the session's whole environment.
	Env []string
	Dir string
	// Columns and Rows are the terminal's size.
	Columns, Rows int
}

// Event is what a follower of a session receives: a State to start again
// from, or output to feed to the terminal it made from the last State.
type Event struct {
	State  *vt.State
	Output []byte
}

// Session is a provider CLI running in a pseudo-terminal.
type Session struct {
	cmd  *exec.Cmd
	ptmx *os.File

	mu        sync.Mutex
	term      *vt.Terminal
	followers map[chan Event]struct{}

	// ended is set, under mu, once the session has no more followers to
	// serve.
	ended bool
	read  chan struct{}
	done  chan struct{}
	// exit is how the process ended, set before done is closed.
	exit error
}

// followerBuffer is how many events a follower may fall behind before it is
// dropped; a dropped follower follows again from a new State.
const followerBuffer = 256

// Start starts spec's CLI in a new pseudo-terminal of its own, as the leader
// of a new session and process group.
func Start(spec Spec) (*Session, error) {
	cmd := exec.Command(spec.Path, spec.Args...)
	cmd.Env = spec.Env
	cmd.Dir = spec.Dir
	columns, rows := max(spec.Columns, 1), max(spec.Rows, 1)
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(columns), Rows: uint16(rows)})
	if err != nil {
		return nil, fmt.Errorf("starting %s: %w", spec.Path, err)
	}
	s := &Session{
		cmd:       cmd,
		ptmx:      ptmx,
		term:      vt.New(columns, rows),
		followers: map[chan Event]struct{}{},
		read:      make(chan struct{}),
		done:      make(chan struct{}),
	}
	go s.readOutput()
	go s.wait()
	return s, nil
}

// PID is the process ID of the provider CLI.
func (s *Session) PID() int { return s.cmd.Process.Pid }

// Done is closed once the provider CLI has exited and its output is read.
func (s *Session) Done() <-chan struct{} { return s.done }

// Exit returns how the provider CLI ended, once Done is closed: nil for an
// exit status of 0.
func (s *Session) Exit() error {
	<-s.done
	return s.exit
}

// readOutput feeds the provider's output to the terminal and its followers,
// and sends the terminal's answers to the provider's queries back as input.
func (s *Session) readOutput() {
	defer close(s.read)
	buf := make([]byte, 32<<10)
	for {
		n, err := s.ptmx.Read(buf)
		if n > 0 {
			out := append([]byte(nil), buf[:n]...)
			s.mu.Lock()
			s.term.Write(out)
			answers := s.term.TakeResponses()
			for f := range s.followers {
				select {
				case f <- Event{Output: out}:
				default:
					delete(s.followers, f)
					close(f)
				}
			}
			s.mu.Unlock()
			if len(answers) > 0 {
				s.ptmx.Write(answers)
			}
		}
		if err != nil {
			return
		}
	}
}

// wait waits for the provider CLI to exit, then for the rest of its output,
// and ends the session.
func (s *Session) wait() {
	err := s.cmd.Wait()
	select {
	case <-s.read:
	case <-time.After(time.Second):
		// Something the provider started still holds the terminal open.
	}
	s.ptmx.Close()
	select {
	case <-s.read:
	case <-time.After(time.Second):
		// Where closing the terminal does not interrupt a read, the read is
		// left to end on its own.
	}
	s.mu.Lock()
	s.ended = true
	for f := range s.followers {
		delete(s.followers, f)
		close(f)
	}
	s.exit = err
	s.mu.Unlock()
	close(s.done)
}

// Follow returns the session's events, starting with its terminal's State,
// and a function that stops following. The channel is closed when the
// session ends, when the follower stops, or when it fell too far behind;
// Done tells which of the first and the last it was.
func (s *Session) Follow() (<-chan Event, func()) {
	f := make(chan Event, followerBuffer)
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.term.State()
	f <- Event{State: &state}
	if s.ended {
		close(f)
	} else {
		s.followers[f] = struct{}{}
	}
	return f, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, ok := s.followers[f]; ok {
			delete(s.followers, f)
			close(f)
		}
	}
}

// Screen returns the session's terminal as it is now.
func (s *Session) Screen() vt.State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.term.State()
}

// Resize makes the session's terminal columns by rows, and sends every
// follower the resized terminal's State.
func (s *Session) Resize(columns, rows int) error {
	columns, rows = max(columns, 1), max(rows, 1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, r := s.term.Size(); c == columns && r == rows {
		return nil
	}
	if err := pty.Setsize(s.ptmx, &pty.Winsize{Cols: uint16(columns), Rows: uint16(rows)}); err != nil {
		return err
	}
	s.term.Resize(columns, rows)
	state := s.term.State()
	for f := range s.followers {
		select {
		case f <- Event{State: &state}:
		default:
			delete(s.followers, f)
			close(f)
		}
	}
	return nil
}

// Stop ends the session: it asks the provider CLI's process group to
// terminate, kills it if it is still running after grace, and returns once
// the session has ended.
func (s *Session) Stop(grace time.Duration) {
	pgid := s.cmd.Process.Pid
	if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		s.cmd.Process.Signal(syscall.SIGTERM)
	}
	select {
	case <-s.done:
		return
	case <-time.After(grace):
	}
	syscall.Kill(-pgid, syscall.SIGKILL)
	s.cmd.Process.Kill()
	<-s.done
}
