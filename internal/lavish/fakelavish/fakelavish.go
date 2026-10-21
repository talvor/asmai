// SPDX-License-Identifier: Apache-2.0

// Package fakelavish is a stand-in for the Lavish server, for the tests of
// the daemon's supervision of it. A test binary becomes the stand-in when it
// is run as `<binary> server --port N` with Variable set: its TestMain calls
// RunIfFake first. Like the real server it reads its state directory, bind
// address and port from the LAVISH_AXI_ variables, answers /health, shuts
// down on POST /shutdown, and keeps its review pages in state.json in the
// state directory, which is why they survive its restart. It also serves the
// parts of Lavish's API the daemon uses to render decision requests: opening
// a page, listening to it, sending what the user would from the page, and
// ending it.
package fakelavish

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
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
	mux.HandleFunc("GET /api/poll", s.poll)
	mux.HandleFunc("POST /api/{key}/prompts", s.prompts)
	mux.HandleFunc("POST /api/end", s.end)
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

// session is a review page as the stand-in keeps it in state.json: the
// fields of Lavish's own that the daemon reads or the stand-in needs.
type session struct {
	Key            string            `json:"key"`
	File           string            `json:"file"`
	URL            string            `json:"url"`
	Status         string            `json:"status"`
	EndedBy        string            `json:"ended_by,omitempty"`
	PendingPrompts int               `json:"pending_prompts"`
	Prompts        []json.RawMessage `json:"prompts"`
}

type state struct {
	Sessions map[string]*session `json:"sessions"`
}

// read and write keep the pages in state.json, which is why they survive a
// restart of the server. The caller holds s.mu.
func (s *server) read() state {
	st := state{Sessions: map[string]*session{}}
	if data, err := os.ReadFile(filepath.Join(s.dir, "state.json")); err == nil {
		json.Unmarshal(data, &st)
	}
	if st.Sessions == nil {
		st.Sessions = map[string]*session{}
	}
	return st
}

func (s *server) write(st state) error {
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.dir, "state.json"), data, 0o600)
}

// keyOf is a page's key, as Lavish makes it: from the file's real path.
func keyOf(file string) string {
	if real, err := filepath.EvalSymlinks(file); err == nil {
		file = real
	}
	sum := sha256.Sum256([]byte(file))
	return hex.EncodeToString(sum[:])[:16]
}

// open adds a review page for the file in the request, as opening one in
// Lavish does, and keeps it in state.json. A page its user ended stays ended
// unless the request reopens it.
func (s *server) open(w http.ResponseWriter, r *http.Request) {
	var req struct {
		File   string `json:"file"`
		Reopen bool   `json:"reopen"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.File == "" {
		http.Error(w, "a file is needed", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.read()
	key := keyOf(req.File)
	url := "http://" + r.Host + "/session/" + key
	if existing := st.Sessions[key]; existing != nil && existing.Status == "ended" && existing.EndedBy == "user" && !req.Reopen {
		json.NewEncoder(w).Encode(map[string]string{"key": key, "url": url, "status": "user-ended"})
		return
	}
	page := st.Sessions[key]
	if page == nil {
		page = &session{Key: key, File: req.File, URL: url, Prompts: []json.RawMessage{}}
		st.Sessions[key] = page
	}
	page.Status, page.EndedBy = "open", ""
	if page.PendingPrompts > 0 {
		page.Status = "feedback"
	}
	if err := s.write(st); err != nil {
		http.Error(w, strings.TrimSpace(err.Error()), http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"key": key, "url": url, "status": "opened"})
}

// prompts queues what the user sends from the page, as the Lavish chrome's
// Send to Agent does; with endSession it ends the page's session too.
func (s *server) prompts(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Prompts    []json.RawMessage `json:"prompts"`
		EndSession bool              `json:"endSession"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "the prompts are not JSON", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.read()
	page := st.Sessions[r.PathValue("key")]
	switch {
	case page == nil:
		http.Error(w, `{"error":"session not found"}`, http.StatusNotFound)
		return
	case page.Status == "ended":
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]any{"status": "ended", "error": "session already ended", "ended_by": page.EndedBy})
		return
	}
	page.Prompts = append(page.Prompts, req.Prompts...)
	page.PendingPrompts = len(page.Prompts)
	page.Status = "open"
	if page.PendingPrompts > 0 {
		page.Status = "feedback"
	}
	if req.EndSession {
		page.Status, page.EndedBy = "ended", "user"
	}
	if err := s.write(st); err != nil {
		http.Error(w, strings.TrimSpace(err.Error()), http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"status": "queued", "pending_prompts": page.PendingPrompts})
}

// end ends a page's session as an agent does.
func (s *server) end(w http.ResponseWriter, r *http.Request) {
	var req struct {
		File string `json:"file"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.File == "" {
		http.Error(w, "a file is needed", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.read()
	if page := st.Sessions[keyOf(req.File)]; page != nil {
		page.Status = "ended"
		if page.EndedBy == "" {
			page.EndedBy = "agent"
		}
		if err := s.write(st); err != nil {
			http.Error(w, strings.TrimSpace(err.Error()), http.StatusInternalServerError)
			return
		}
	}
	fmt.Fprint(w, `{"status":"ended"}`)
}

// take hands over what the user sent, as Lavish's takeFeedback does.
func (s *server) take(key string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.read()
	page := st.Sessions[key]
	switch {
	case page == nil:
		return map[string]any{"status": "missing"}
	case len(page.Prompts) == 0 && page.Status == "ended":
		return map[string]any{"status": "ended", "ended_by": page.EndedBy}
	case len(page.Prompts) == 0:
		return map[string]any{"status": "waiting"}
	}
	result := map[string]any{"status": "feedback", "prompts": page.Prompts}
	if page.Status == "ended" {
		result["session_ended"], result["ended_by"] = true, page.EndedBy
	} else {
		page.Status = "open"
	}
	page.Prompts, page.PendingPrompts = []json.RawMessage{}, 0
	s.write(st)
	return result
}

// poll listens to a page as Lavish does: it answers when the user has sent
// something, or ends, and without a timeout it keeps the connection alive
// with blanks until then.
func (s *server) poll(w http.ResponseWriter, r *http.Request) {
	file := r.URL.Query().Get("file")
	if file == "" {
		http.Error(w, "a file is needed", http.StatusBadRequest)
		return
	}
	key := keyOf(file)
	var timeout <-chan time.Time
	if ms := r.URL.Query().Get("timeoutMs"); ms != "" {
		n, _ := strconv.Atoi(ms)
		timeout = time.After(time.Duration(n) * time.Millisecond)
	} else {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, " ")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		result := s.take(key)
		if result["status"] != "waiting" {
			json.NewEncoder(w).Encode(result)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-timeout:
			json.NewEncoder(w).Encode(result)
			return
		case <-tick.C:
		}
	}
}
