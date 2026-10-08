// SPDX-License-Identifier: Apache-2.0

package session

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/vt"
)

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !cond(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func screenText(s *Session) string {
	t, _ := vt.FromState(s.Screen())
	return t.String()
}

// The session's terminal answers the program's queries, as a terminal does,
// and followers see its screen and what it draws next.
func TestASessionEmulatesItsTerminalForItsFollowers(t *testing.T) {
	script := `stty raw -echo; printf 'hi\033[6n'; reply=$(dd bs=1 count=6 2>/dev/null); ` +
		`printf '\r\nreply:%s\r\n' "$(printf %s "$reply" | od -An -c | tr -d ' \n')"; ` +
		`read -r line; printf 'size:%s\r\n' "$(stty size)"; read -r line`
	s, err := Start(Spec{Path: "/bin/sh", Args: []string{"-c", script}, Env: os.Environ(), Dir: t.TempDir(), Columns: 30, Rows: 5})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Stop(time.Second) })
	waitFor(t, "the reply to the cursor position query", func() bool { return strings.Contains(screenText(s), `reply:033[1;3R`) })

	events, stop := s.Follow()
	defer stop()
	first := <-events
	if first.State == nil || first.State.Columns != 30 || first.State.Rows != 5 {
		t.Fatalf("a follower's first event is %+v, want the 30 by 5 terminal's state", first)
	}
	if err := s.Resize(40, 8); err != nil {
		t.Fatal(err)
	}
	if resized := <-events; resized.State == nil || resized.State.Columns != 40 || resized.State.Rows != 8 {
		t.Fatalf("after a resize, a follower received %+v, want the 40 by 8 terminal's state", resized)
	}
	s.ptmx.Write([]byte("\n"))
	term, _ := vt.FromState(s.Screen())
	var output strings.Builder
	waitFor(t, "the program's next output", func() bool {
		select {
		case e := <-events:
			term.Write(e.Output)
			output.Write(e.Output)
		default:
		}
		return strings.Contains(output.String(), "size:8 40")
	})
	if !strings.Contains(term.String(), "size:8 40") {
		t.Errorf("the follower's terminal shows %q", term.String())
	}

	s.Stop(time.Second)
	select {
	case <-s.Done():
	default:
		t.Fatal("the session has not ended after Stop")
	}
	if err := s.Exit(); err == nil || !strings.Contains(err.Error(), "signal") {
		t.Errorf("the stopped session exited %v, want a signal", err)
	}
	if _, ok := <-events; ok {
		t.Error("a follower's events go on after the session ended")
	}
}

func TestASessionEndsWhenItsProgramExits(t *testing.T) {
	s, err := Start(Spec{Path: "/bin/sh", Args: []string{"-c", "printf done; exit 3"}, Env: os.Environ(), Columns: 20, Rows: 2})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the session did not end")
	}
	if err := s.Exit(); err == nil || err.Error() != "exit status 3" {
		t.Errorf("the session exited %v, want exit status 3", err)
	}
	if text := screenText(s); !strings.HasPrefix(text, "done") {
		t.Errorf("the screen shows %q, want what the program drew", text)
	}
	events, _ := s.Follow()
	if e := <-events; e.State == nil {
		t.Error("following an ended session gives no state")
	}
	if _, ok := <-events; ok {
		t.Error("following an ended session goes on")
	}
}
