// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/creack/pty"
	"github.com/talvor/asmai/internal/vt"
)

// TestSpikeHandover drives the throwaway Bubble Tea program through its real
// child command, a running asmai attach connected to the isolated test daemon.
func TestSpikeHandoverRestoresTerminalAfterRealAttach(t *testing.T) {
	readyFactory(t, []string{`{"screen":"\u001b[2J\u001b[HCoordination is ready\r\n"}`, `{"expect":"input the observer never sends"}`})
	var started struct {
		Leaders []leaderJSON `json:"leaders"`
	}
	asmaiJSON(t, &started, "start")
	if len(started.Leaders) == 0 || started.Leaders[0].State != "running" {
		t.Fatalf("coordination did not start: %+v", started.Leaders)
	}

	spike := filepath.Join(t.TempDir(), "spike-handover")
	build := exec.Command("go", "build", "-o", spike, "../../internal/cmd/spike-handover")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build spike: %v\n%s", err, output)
	}
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	const columns, rows = 80, 25
	if err := pty.Setsize(ptmx, &pty.Winsize{Cols: columns, Rows: rows}); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(spike, "--asmai", filepath.Join(asmaiBin, "asmai"), "coordination")
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	tty.Close()
	var mu sync.Mutex
	term := vt.New(columns, rows)
	var raw strings.Builder
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			mu.Lock()
			term.Write(buf[:n])
			raw.Write(buf[:n])
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		cmd.Process.Kill()
		ptmx.Close()
		<-readDone
	})
	lines := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return term.Lines()
	}
	shows := func(needle string) bool { return strings.Contains(strings.Join(lines(), "\n"), needle) }
	frame := func() string { return strings.Join(lines(), "\n") }

	waitFor(t, "the initial dashboard", func() bool { return shows("spike-handover") && shows("size 80x25") })
	initial := frame()
	if _, err := ptmx.WriteString("a"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "real asmai attach to draw the leader", func() bool {
		return shows("Coordination is ready") && shows("observing")
	})
	attached := frame()
	if _, err := ptmx.WriteString("\x1d"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the dashboard to return from attach", func() bool {
		return shows("attaches returned: 1") && shows("size 80x25")
	})
	restored := frame()
	if !strings.Contains(restored, "last error: <nil>") || !strings.Contains(restored, "tty state identical before/after: true") {
		t.Fatalf("dashboard did not restore tty state after attach:\n%s", restored)
	}

	// Resize the actual terminal before reattaching; the dashboard must learn
	// the new size, and keep it across the next handover.
	if err := pty.Setsize(ptmx, &pty.Winsize{Cols: 100, Rows: 30}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	term.Resize(100, 30)
	mu.Unlock()
	if err := cmd.Process.Signal(syscall.SIGWINCH); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the resized dashboard", func() bool { return shows("size 100x30") })

	// A second handover tests that the restored keyboard and screen are usable.
	if _, err := ptmx.WriteString("a"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the second attach", func() bool { return shows("Coordination is ready") && shows("observing") })
	if _, err := ptmx.WriteString("\x1d"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the second return", func() bool { return shows("attaches returned: 2") && shows("size 100x30") })
	if !shows("tty state identical before/after: true") {
		t.Fatalf("second handover did not restore tty state:\n%s", frame())
	}
	resized := frame()

	// When the daemon is stopped, real attach reports a failure. Its child
	// process still returns control to Bubble Tea and the terminal remains usable.
	var stopped struct{}
	asmaiJSON(t, &stopped, "stop")
	if _, err := ptmx.WriteString("a"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "return from attach to a stopped daemon", func() bool {
		return shows("attaches returned: 3") && shows("size 100x30")
	})
	if !shows("tty state identical before/after: true") {
		t.Fatalf("failed attach did not restore tty state:\n%s", frame())
	}
	stoppedScreen := frame()
	if _, err := ptmx.WriteString("q"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("spike exited: %v", err)
	}
	ptmx.Close()
	<-readDone
	mu.Lock()
	output := raw.String()
	mu.Unlock()
	if strings.Count(output, "\x1b[?1049h") < 3 || strings.Count(output, "\x1b[?1049l") < 3 {
		t.Errorf("terminal stream did not show alternate-screen release and restore on all visits")
	}
	t.Logf("actual terminal frames:\ninitial:\n%s\nattached:\n%s\nrestored:\n%s\nresized:\n%s\nafter stopped daemon:\n%s", initial, attached, restored, resized, stoppedScreen)
	if path := os.Getenv("ASMAI_SPIKE_EVIDENCE"); path != "" {
		page := "<!doctype html><meta charset=utf-8><title>AsmAI handover frames</title><style>body{background:#111;color:#eee;font:15px monospace;padding:2rem}pre{background:#222;padding:1rem;white-space:pre;overflow:auto}</style>"
		for _, item := range []struct{ name, screen string }{{"Initial dashboard", initial}, {"Real asmai attach", attached}, {"Restored dashboard", restored}, {"Resized dashboard after second attach", resized}, {"Dashboard after attach to stopped daemon", stoppedScreen}} {
			page += fmt.Sprintf("<h2>%s</h2><pre>%s</pre>", html.EscapeString(item.name), html.EscapeString(item.screen))
		}
		if err := os.WriteFile(path, []byte(page), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
