// SPDX-License-Identifier: Apache-2.0

// Package fakelavish is a stand-in for the Lavish server, for the tests of
// the daemon's supervision of it. A test binary becomes the stand-in when it
// is run as `<binary> server --port N` with Variable set: its TestMain calls
// RunIfFake first. Like the real server it reads its state directory, bind
// address and port from the LAVISH_AXI_ variables, answers /health, shuts
// down on POST /shutdown, and keeps its review pages in state.json in the
// state directory, which is why they survive its restart.
package fakelavish

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Variable marks a process as the stand-in; its value says how it misbehaves.
const Variable = "ASMAI_FAKE_LAVISH"

// How the stand-in behaves, as the value of Variable.
const (
	// Well is a server that does what Lavish does.
	Well = "well"
	// AlsoListens reports a listener on another address, as Lavish does on a
	// Tailscale address when it is not told its bind address.
	AlsoListens = "also-listens"
	// Hangs keeps running but never answers.
	Hangs = "hangs"
	// Exits exits at once, as a server that cannot start does.
	Exits = "exits"
)

// RunIfFake runs the stand-in and exits when this process was started as it;
// otherwise it returns.
func RunIfFake() {
	mode := os.Getenv(Variable)
	if mode == "" || len(os.Args) < 2 || os.Args[1] != "server" {
		return
	}
	os.Exit(run(mode))
}

func run(mode string) int {
	switch mode {
	case Exits:
		fmt.Fprintln(os.Stderr, "fake lavish: cannot start")
		return 1
	case Hangs:
		select {}
	}
	dir := os.Getenv("LAVISH_AXI_STATE_DIR")
	host := os.Getenv("LAVISH_AXI_HOST")
	port := os.Getenv("LAVISH_AXI_PORT")
	for i, a := range os.Args {
		if a == "--port" && i+1 < len(os.Args) {
			port = os.Args[i+1]
		}
	}
	if dir == "" || host == "" || port == "" {
		fmt.Fprintln(os.Stderr, "fake lavish: the state directory, host and port are needed")
		return 2
	}
	// Telemetry, a browser and the idle stop are as the daemon sets them.
	if os.Getenv("LAVISH_AXI_TELEMETRY") != "off" || os.Getenv("LAVISH_AXI_NO_OPEN") != "1" {
		fmt.Fprintln(os.Stderr, "fake lavish: telemetry must be off and no browser opened")
		return 2
	}
	l, err := net.Listen("tcp", net.JoinHostPort(host, port))
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake lavish:", err)
		return 1
	}
	s := &server{dir: dir, shutdown: make(chan struct{})}
	hosts := []string{host}
	if mode == AlsoListens {
		hosts = append(hosts, "100.64.0.7")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "app": "lavish-axi", "version": "0.0.0-fake", "state_dir": dir, "hosts": hosts})
	})
	mux.HandleFunc("POST /shutdown", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"status":"shutting-down"}`)
		go func() { s.shutdown <- struct{}{} }()
	})
	mux.HandleFunc("POST /api/sessions", s.open)
	srv := &http.Server{Handler: mux}
	go srv.Serve(l)
	fmt.Println("fake lavish listening on", l.Addr())
	<-s.shutdown
	srv.Close()
	return 0
}

type server struct {
	dir      string
	mu       sync.Mutex
	shutdown chan struct{}
}

// open adds a review page for the file in the request, as opening one in
// Lavish does, and keeps it in state.json.
func (s *server) open(w http.ResponseWriter, r *http.Request) {
	var req struct {
		File string `json:"file"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.File == "" {
		http.Error(w, "a file is needed", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path := filepath.Join(s.dir, "state.json")
	state := map[string]map[string]map[string]any{"sessions": {}}
	if data, err := os.ReadFile(path); err == nil {
		json.Unmarshal(data, &state)
	}
	key := fmt.Sprintf("%016x", len(state["sessions"])+1)
	url := "http://" + r.Host + "/session/" + key
	state["sessions"][key] = map[string]any{"key": key, "file": req.File, "url": url, "status": "open", "pending_prompts": 0}
	data, _ := json.Marshal(state)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		http.Error(w, strings.TrimSpace(err.Error()), http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"url": url})
}
