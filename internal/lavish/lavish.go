// SPDX-License-Identifier: Apache-2.0

// Package lavish runs the factory's own Lavish server and reads what it
// keeps. The daemon supervises one Lavish server, AsmAI's pinned Lavish
// build run in the foreground with AsmAI's own state directory and port,
// bound to localhost only and with telemetry off, so that it never shares
// either with the user's personal Lavish and never relies on Lavish
// starting a server itself (04 rules 20 and 21, ADR 0010). Agents never run
// Lavish.
package lavish

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Port is the port the factory's Lavish server listens on. It is AsmAI's
// own: Lavish's default, 4387, is the user's personal Lavish's.
const Port = 14387

// Host is the only address the server binds.
const Host = "127.0.0.1"

// prefix starts the names of the variables Lavish reads.
const prefix = "LAVISH_AXI_"

// Environment returns the environment the Lavish server runs in: base with
// every variable Lavish reads removed, so that the user's own Lavish settings
// (another link host, more allowed hosts, a telemetry endpoint) never reach
// the factory's server, then AsmAI's: its own state directory and port, the
// loopback address as the bind address, so Lavish never also listens on a
// Tailscale address, telemetry off, no browser launched, and no idle
// self-stop.
func Environment(base []string, stateDir string, port int) []string {
	env := slices.DeleteFunc(slices.Clone(base), func(kv string) bool {
		return strings.HasPrefix(kv, prefix)
	})
	return append(env,
		prefix+"STATE_DIR="+stateDir,
		prefix+"PORT="+strconv.Itoa(port),
		prefix+"HOST="+Host,
		prefix+"TELEMETRY=off",
		prefix+"NO_OPEN=1",
		// Lavish stops itself after 30 idle minutes, which the daemon would
		// only restart; the longest delay a timer takes keeps it running.
		prefix+"IDLE_TIMEOUT_MS=2147483647",
	)
}

// Args are the arguments that run Lavish's server in the foreground on port.
func Args(port int) []string {
	return []string{"server", "--port", strconv.Itoa(port)}
}

// URL is the address of the server on port.
func URL(port int) string {
	return "http://" + Host + ":" + strconv.Itoa(port)
}

// Page is a Lavish review page that is waiting for the user: a session the
// user has not ended.
type Page struct {
	// File is the page's HTML file.
	File string `json:"file"`
	// URL is the link the user opens.
	URL string `json:"url"`
	// Status is "open", or "feedback" when the user has sent something that
	// has not been collected yet.
	Status string `json:"status"`
	// PendingPrompts counts what the user sent that has not been collected.
	PendingPrompts int       `json:"pending_prompts"`
	UpdatedAt      time.Time `json:"updated_at,omitzero"`
	// Request, Version and Job name the decision request version the page
	// renders, and Answered how many of its Questions have an answer. The
	// daemon sets them, from its store, on the pages it opened; Lavish does
	// not know them.
	Request   int64 `json:"request,omitempty"`
	Version   int   `json:"version,omitempty"`
	Job       int64 `json:"job,omitempty"`
	Answered  int   `json:"answered,omitempty"`
	Questions int   `json:"questions,omitempty"`
}

// StateFile is the file in the Lavish state directory that holds the review
// pages, which is why they survive a restart of the server.
const StateFile = "state.json"

// Pages returns the review pages waiting for the user in the Lavish state
// directory, by file. A state directory the server has not written yet has
// none.
func Pages(stateDir string) ([]Page, error) {
	data, err := os.ReadFile(filepath.Join(stateDir, StateFile))
	if errors.Is(err, os.ErrNotExist) {
		return []Page{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading Lavish's pages: %w", err)
	}
	var state struct {
		Sessions map[string]struct {
			File           string    `json:"file"`
			URL            string    `json:"url"`
			Status         string    `json:"status"`
			PendingPrompts int       `json:"pending_prompts"`
			UpdatedAt      time.Time `json:"updated_at"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("reading Lavish's pages: %s is not Lavish's state: %w", filepath.Join(stateDir, StateFile), err)
	}
	pages := []Page{}
	for _, s := range state.Sessions {
		if s.Status == "ended" {
			continue
		}
		pages = append(pages, Page{File: s.File, URL: s.URL, Status: s.Status, PendingPrompts: s.PendingPrompts, UpdatedAt: s.UpdatedAt})
	}
	slices.SortFunc(pages, func(a, b Page) int {
		if c := strings.Compare(a.File, b.File); c != 0 {
			return c
		}
		return strings.Compare(a.URL, b.URL)
	})
	return pages, nil
}

// ForwardCommand is the command that makes the server reachable from the
// user's own machine when the factory runs on another: it forwards the
// server's port over SSH to the host, which is named as the user reaches it
// (04 rule 21).
func ForwardCommand(destination string, port int) string {
	p := strconv.Itoa(port)
	return "ssh -N -L " + p + ":" + Host + ":" + p + " " + destination
}
