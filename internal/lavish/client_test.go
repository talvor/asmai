// SPDX-License-Identifier: Apache-2.0

package lavish_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/talvor/asmai/internal/lavish"
	"github.com/talvor/asmai/internal/lavish/fakelavish"
)

// stand-in runs the stand-in server, which serves the parts of Lavish's API
// the daemon uses, and returns a client of it and the state directory.
func standIn(t *testing.T) (lavish.Client, string) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	dir := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "server", "--port", strconv.Itoa(port))
	cmd.Env = lavish.Environment(append(os.Environ(), fakelavish.Variable+"="+fakelavish.Well), dir, port)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	client := lavish.Client{Port: port}
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := http.Get(lavish.URL(port) + "/health")
		if err == nil {
			resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the stand-in did not answer")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return client, dir
}

func send(t *testing.T, c lavish.Client, url string, end bool, prompts ...string) {
	t.Helper()
	raw := make([]json.RawMessage, len(prompts))
	for i, p := range prompts {
		raw[i] = json.RawMessage(p)
	}
	body, _ := json.Marshal(map[string]any{"prompts": raw, "endSession": end})
	resp, err := http.Post(lavish.URL(c.Port)+"/api/"+path.Base(url)+"/prompts", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestAClientOpensListensToAndEndsAPage(t *testing.T) {
	c, dir := standIn(t)
	file := filepath.Join(t.TempDir(), "page.html")
	if err := os.WriteFile(file, []byte("<p>hi</p>"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	opened, err := c.OpenPage(ctx, file)
	if err != nil || opened.Status != "opened" || !strings.Contains(opened.URL, "/session/") {
		t.Fatalf("opened = %+v (%v)", opened, err)
	}
	if pages, err := lavish.Pages(dir); err != nil || len(pages) != 1 || pages[0].URL != opened.URL || pages[0].File != file {
		t.Fatalf("pages = %+v (%v)", pages, err)
	}

	// Listening without a timeout waits for the user, and returns what they
	// send exactly as it was sent.
	got := make(chan lavish.Feedback, 1)
	go func() {
		f, err := c.Poll(ctx, file, 0)
		if err != nil {
			t.Error(err)
		}
		got <- f
	}()
	time.Sleep(100 * time.Millisecond)
	prompt := `{"uid":"u1","prompt":"Answer","tag":"asmai-answer","target":{"type":"asmai-answer","question":"q1"}}`
	send(t, c, opened.URL, false, prompt, `{"uid":"u2","tag":"annotation"}`)
	select {
	case f := <-got:
		if f.Status != "feedback" || len(f.Prompts) != 2 || string(f.Prompts[0]) != prompt || f.SessionEnded {
			t.Errorf("feedback = %+v", f)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the listener was not given what the user sent")
	}
	if f, err := c.Poll(ctx, file, 20); err != nil || f.Status != "waiting" || len(f.Prompts) != 0 {
		t.Errorf("a wait that ran out = %+v (%v)", f, err)
	}

	// A user who ends the session in sending leaves it ended until the
	// daemon opens it again.
	send(t, c, opened.URL, true, `{"uid":"u3","tag":"message","text":"done"}`)
	f, err := c.Poll(ctx, file, 20)
	if err != nil || len(f.Prompts) != 1 || !f.SessionEnded || f.EndedBy != "user" {
		t.Fatalf("feedback of an ended session = %+v (%v)", f, err)
	}
	if f, err := c.Poll(ctx, file, 20); err != nil || f.Status != "ended" {
		t.Errorf("an ended session = %+v (%v)", f, err)
	}
	if again, err := c.OpenPage(ctx, file); err != nil || again.Status != "opened" || again.URL != opened.URL {
		t.Errorf("reopened = %+v (%v)", again, err)
	}

	if err := c.EndPage(ctx, file); err != nil {
		t.Fatal(err)
	}
	if pages, err := lavish.Pages(dir); err != nil || len(pages) != 0 {
		t.Errorf("pages after ending = %+v (%v)", pages, err)
	}
	if err := c.EndPage(ctx, file); err != nil {
		t.Errorf("ending a page twice = %v", err)
	}
}

func TestAClientToAServerThatIsNotThereSaysSo(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	c := lavish.Client{Port: port}
	for name, err := range map[string]error{
		"open": func() error { _, err := c.OpenPage(context.Background(), "/x.html"); return err }(),
		"poll": func() error { _, err := c.Poll(context.Background(), "/x.html", 10); return err }(),
		"end":  c.EndPage(context.Background(), "/x.html"),
	} {
		if err == nil || !strings.Contains(err.Error(), "/x.html") {
			t.Errorf("%s = %v", name, err)
		}
	}
}
