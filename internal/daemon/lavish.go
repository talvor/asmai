// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/talvor/asmai/internal/lavish"
	"github.com/talvor/asmai/internal/providers"
)

// LavishConfig tunes how the daemon supervises the factory's Lavish server.
// Each zero field is the default.
type LavishConfig struct {
	// Port is the port the server listens on; with none, lavish.Port.
	Port int
	// CheckEvery, StartupGrace, Unhealthy, RestartDelay, StopGrace and
	// CheckTimeout are the supervisor's pace.
	CheckEvery   time.Duration
	StartupGrace time.Duration
	Unhealthy    int
	RestartDelay time.Duration
	StopGrace    time.Duration
	CheckTimeout time.Duration
	// Environ is the environment the server's is made from; with none, the
	// daemon's.
	Environ []string
	// PagesEvery is how often the daemon looks for decision pages to open,
	// to end and to listen to; with none, a second.
	PagesEvery time.Duration
}

// LavishStatus is the factory's Lavish server and the review pages waiting
// for the user, which `asmai lavish` shows (04 rule 21).
type LavishStatus struct {
	// Running says the server is up.
	Running bool `json:"running"`
	PID     int  `json:"pid,omitempty"`
	// URL is the server's address on this host, and Port its port.
	URL  string `json:"url"`
	Port int    `json:"port"`
	// StateDir is the server's own state directory.
	StateDir string `json:"state_dir"`
	// Build is the pinned Lavish build the server runs, such as
	// "0.1.89+deno2.9.6", when this asmai pins one.
	Build string `json:"build,omitempty"`
	// Problem says why the server is not running or not well.
	Problem  string `json:"problem,omitempty"`
	Restarts int    `json:"restarts"`
	// Pages are the review pages waiting for the user, and PagesError why
	// they could not be read.
	Pages      []lavish.Page `json:"pages"`
	PagesError string        `json:"pages_error,omitempty"`
}

// newLavish returns the supervisor of the factory's Lavish server.
func (d *daemon) newLavish() *lavish.Server {
	c := d.cfg.Lavish
	port := c.Port
	if port == 0 {
		port = lavish.Port
	}
	return &lavish.Server{
		Resolve:      d.resolveLavish(),
		StateDir:     d.cfg.Paths.Lavish,
		Port:         port,
		Log:          d.log.With("component", "lavish"),
		Environ:      c.Environ,
		CheckEvery:   c.CheckEvery,
		StartupGrace: c.StartupGrace,
		Unhealthy:    c.Unhealthy,
		RestartDelay: c.RestartDelay,
		StopGrace:    c.StopGrace,
		CheckTimeout: c.CheckTimeout,
	}
}

// pinnedLavishPlan plans the install of the pinned Lavish build on this
// platform.
func (d *daemon) pinnedLavishPlan() (providers.Plan, error) {
	goos, goarch := d.cfg.GOOS, d.cfg.GOARCH
	if goos == "" || goarch == "" {
		goos, goarch = runtime.GOOS, runtime.GOARCH
	}
	platform, ok := providers.Platform(goos, goarch)
	if !ok {
		return providers.Plan{}, fmt.Errorf("%w: Lavish is not built for %s/%s", providers.ErrNoLavishBuild, goos, goarch)
	}
	return d.cfg.Pins.LavishPlan(platform, d.cfg.Paths.Providers)
}

// resolveLavish returns what the supervisor asks before every start: the
// executable of the pinned Lavish build, or why there is none. It refuses
// any other Lavish, the user's own or a build that is not the pinned one:
// the file at the pinned path must be the download the pins file names. A
// file already checked is not read again until it changes.
func (d *daemon) resolveLavish() func() (string, error) {
	var mu sync.Mutex
	var checked struct {
		path string
		size int64
		mod  time.Time
	}
	return func() (string, error) {
		plan, err := d.pinnedLavishPlan()
		if err != nil {
			return "", fmt.Errorf("the Lavish server is not started: %w", err)
		}
		info, statErr := os.Stat(plan.Path)
		if errors.Is(statErr, os.ErrNotExist) {
			return "", fmt.Errorf("the pinned Lavish %s is not installed at %s; run `asmai providers install`", plan.Version, plan.Path)
		}
		mu.Lock()
		defer mu.Unlock()
		if statErr == nil && checked.path == plan.Path && checked.size == info.Size() && checked.mod.Equal(info.ModTime()) && providers.Executable(plan.Path) {
			return plan.Path, nil
		}
		present, err := providers.Present(plan)
		if err != nil {
			return "", fmt.Errorf("checking the pinned Lavish at %s: %w", plan.Path, err)
		}
		if !present {
			return "", fmt.Errorf("the Lavish at %s is not the pinned build %s (it does not match the pins file's SHA-256 %s, or is not executable), so it is not run; run `asmai providers install` to put the pinned build back", plan.Path, plan.Version, plan.Download.SHA256)
		}
		checked.path, checked.size, checked.mod = plan.Path, info.Size(), info.ModTime()
		return plan.Path, nil
	}
}

// lavishStatus describes the Lavish server and its pending pages.
func (d *daemon) lavishStatus() *LavishStatus {
	st := d.lavish.State()
	s := &LavishStatus{
		Running:  st.Running,
		PID:      st.PID,
		URL:      lavish.URL(d.lavish.Port),
		Port:     d.lavish.Port,
		StateDir: d.lavish.StateDir,
		Problem:  st.Problem,
		Restarts: st.Restarts,
	}
	if pin := d.cfg.Pins.Lavish; pin != nil {
		s.Build = pin.Build()
	}
	pages, err := lavish.Pages(d.lavish.StateDir)
	if err != nil {
		s.PagesError = err.Error()
		pages = []lavish.Page{}
	}
	// Each page the daemon opened for a decision request says which version it
	// renders, and how far it is answered.
	if open, err := d.store.LavishPagesOpen(); err == nil {
		for i := range pages {
			for _, r := range open {
				if r.PageFile == pages[i].File {
					pages[i].Request, pages[i].Version, pages[i].Job = r.Request, r.Version, r.Job
					pages[i].Answered, pages[i].Questions = len(r.Answered), len(r.Questions)
				}
			}
		}
	}
	s.Pages = pages
	return s
}
