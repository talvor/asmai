// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/talvor/asmai/internal/decisionpage"
	"github.com/talvor/asmai/internal/lavish"
	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/store"
)

// pages is the daemon's keeping of the Lavish pages of decision requests: it
// renders each version answered in Lavish as a page, opens it in the
// factory's Lavish server, listens for what the user sends from it, routes
// that to the owning leader, and ends the page once its version is settled
// (04 rules 23 to 29). No agent runs Lavish or renders a page; an agent only
// makes the request.
type pages struct {
	d *daemon
	// syncing serializes the passes that open, listen to and end pages.
	syncing sync.Mutex
	// mu guards listeners, which holds one listener for each open page.
	mu        sync.Mutex
	listeners map[int64]*pageListener
}

// pageListener listens to one page until its version is settled.
type pageListener struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func (d *daemon) newPages() *pages {
	return &pages{d: d, listeners: map[int64]*pageListener{}}
}

// client returns the connection to the factory's Lavish server.
func (p *pages) client() lavish.Client {
	return lavish.Client{Port: p.d.lavish.Port}
}

// file is the file the page of version r is rendered to.
func (p *pages) file(r store.DecisionRequest) string {
	return filepath.Join(p.d.cfg.Paths.DecisionPages, fmt.Sprintf("request-%d-version-%d.html", r.Request, r.Version))
}

// render writes the page of r with the answers the user gave so far. The
// write replaces the file whole, so that Lavish, which reloads the page when
// the file changes, never shows half of one.
func (p *pages) render(r store.DecisionRequest) (string, error) {
	given, err := p.d.store.LavishAnswered(r.ID)
	if err != nil {
		return "", err
	}
	html, err := decisionpage.Render(r, given)
	if err != nil {
		return "", err
	}
	file := p.file(r)
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return "", fmt.Errorf("making the folder of decision pages: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(file), ".page-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(html); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return "", err
	}
	return file, os.Rename(tmp.Name(), file)
}

// run keeps the pages until ctx is done: it opens the pages that wait to be
// opened, listens to those that are open, and ends those whose version is
// settled, whenever the Lavish server is up.
func (p *pages) run(ctx context.Context) {
	every := p.d.cfg.Lavish.PagesEvery
	if every == 0 {
		every = time.Second
	}
	for {
		p.sync(ctx)
		select {
		case <-ctx.Done():
			p.stopListening()
			return
		case <-time.After(every):
		}
	}
}

// sync brings the pages to what the store says they are: one pass of run, and
// what a new request or version has done at once.
func (p *pages) sync(ctx context.Context) {
	if ctx.Err() != nil || !p.d.lavish.State().Running {
		return
	}
	p.syncing.Lock()
	defer p.syncing.Unlock()
	d := p.d
	toOpen, err := d.store.LavishPagesToOpen()
	if err != nil {
		d.log.Error("reading the decision pages to open", "error", err.Error())
		return
	}
	for _, r := range toOpen {
		if err := p.open(ctx, r); err != nil {
			d.log.Warn("opening a decision page failed", "request", r.Request, "version", r.Version, "error", err.Error())
		}
	}
	// A version replaced, answered or withdrawn leaves its page to end.
	toClose, err := d.store.LavishPagesToClose()
	if err != nil {
		d.log.Error("reading the decision pages to end", "error", err.Error())
		return
	}
	for _, r := range toClose {
		if err := p.close(ctx, r); err != nil {
			d.log.Warn("ending a decision page failed", "request", r.Request, "version", r.Version, "error", err.Error())
		}
	}
	open, err := d.store.LavishPagesOpen()
	if err != nil {
		d.log.Error("reading the open decision pages", "error", err.Error())
		return
	}
	for _, r := range open {
		p.listen(ctx, r)
	}
}

// open renders the page of r and opens it in Lavish. That is showing the
// version to the user: the store journals it, and an answer given after it
// answers this version.
func (p *pages) open(ctx context.Context, r store.DecisionRequest) error {
	file, err := p.render(r)
	if err != nil {
		return err
	}
	opened, err := p.client().OpenPage(ctx, file)
	if err != nil {
		return err
	}
	r, err = p.d.store.LavishPageOpened(r.ID, file, opened.URL, time.Now())
	if err != nil {
		return err
	}
	p.d.log.Info("decision page opened", "request", r.Request, "version", r.Version, "job", r.Job, "url", r.PageURL, "shown", r.Shown)
	return nil
}

// close ends the page of r, whose version is settled. What the user sent from
// it before is collected first, and refused when the version no longer takes
// an answer, so that nothing is lost unseen.
func (p *pages) close(ctx context.Context, r store.DecisionRequest) error {
	p.stop(r.ID)
	c := p.client()
	// What the user sent and Lavish still holds is collected before the page
	// ends; a page Lavish has lost holds nothing, and any other failure is
	// tried again at the next pass, so that nothing is lost unseen.
	fb, err := c.Poll(ctx, r.PageFile, 50)
	switch {
	case err == nil:
		if len(fb.Prompts) > 0 {
			p.collect(ctx, r, fb.Prompts)
		}
	case !errors.Is(err, lavish.ErrNoPage):
		return err
	}
	if err := c.EndPage(ctx, r.PageFile); err != nil {
		return err
	}
	if err := p.d.store.LavishPageClosed(r.ID, time.Now()); err != nil {
		return err
	}
	p.d.log.Info("decision page ended", "request", r.Request, "version", r.Version, "state", r.State)
	return nil
}

// listen starts listening to the page of r, if it is not listened to yet. The
// listener sends what the user sends to the owning leader as soon as they
// send it, and shows the user that someone is listening.
func (p *pages) listen(ctx context.Context, r store.DecisionRequest) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if l := p.listeners[r.ID]; l != nil {
		select {
		case <-l.done:
		default:
			return
		}
	}
	lctx, cancel := context.WithCancel(ctx)
	l := &pageListener{cancel: cancel, done: make(chan struct{})}
	p.listeners[r.ID] = l
	p.d.wg.Add(1)
	go func() {
		defer p.d.wg.Done()
		defer close(l.done)
		p.listenTo(lctx, r)
	}()
}

// stop ends the listening to the page of the version whose record is id and
// waits for it to end.
func (p *pages) stop(id int64) {
	p.mu.Lock()
	l := p.listeners[id]
	delete(p.listeners, id)
	p.mu.Unlock()
	if l != nil {
		l.cancel()
		<-l.done
	}
}

func (p *pages) stopListening() {
	p.mu.Lock()
	listeners := p.listeners
	p.listeners = map[int64]*pageListener{}
	p.mu.Unlock()
	for _, l := range listeners {
		l.cancel()
		<-l.done
	}
}

// retryAfter is how long a listener waits before it listens again after the
// server did not answer.
const retryAfter = 200 * time.Millisecond

// listenTo listens to the page of r until ctx is done or its version is
// settled. When the server does not answer, because it is restarting or has
// lost the page, it opens the page again, which the store already has, and
// listens again.
func (p *pages) listenTo(ctx context.Context, r store.DecisionRequest) {
	c := p.client()
	for ctx.Err() == nil {
		fb, err := c.Poll(ctx, r.PageFile, 0)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			p.d.log.Debug("listening to a decision page failed", "request", r.Request, "version", r.Version, "error", err.Error())
			if _, err := c.OpenPage(ctx, r.PageFile); err != nil {
				p.d.log.Debug("reopening a decision page failed", "request", r.Request, "version", r.Version, "error", err.Error())
			}
			if !sleep(ctx, retryAfter) {
				return
			}
			continue
		}
		if len(fb.Prompts) > 0 {
			p.collect(ctx, r, fb.Prompts)
		}
		current, err := p.d.store.DecisionRequest(r.Request)
		if err != nil || current.ID != r.ID || current.State != store.RequestPending {
			// Settled: the pass that ends pages ends this one.
			return
		}
		if len(fb.Prompts) == 0 && !fb.SessionEnded && fb.Status != "ended" {
			// Lavish answered with nothing to collect, as when the browser
			// disconnected: listen again, but not in a tight loop.
			if !sleep(ctx, retryAfter) {
				return
			}
			continue
		}
		if fb.SessionEnded || fb.Status == "ended" {
			// The user ended the page's session, as Send & End does. The
			// version is pending still, so the page stands for the rest.
			if _, err := c.OpenPage(ctx, r.PageFile); err != nil {
				p.d.log.Warn("reopening a decision page the user ended failed", "request", r.Request, "version", r.Version, "error", err.Error())
				if !sleep(ctx, retryAfter) {
					return
				}
			}
		}
	}
}

// sleep waits for d and reports false if ctx ended first.
func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// collect journals what the user sent from the page of r, applies it, routes
// it to the owning leader and has Coordination confirm it in one line, and
// shows the page with its answers.
func (p *pages) collect(ctx context.Context, r store.DecisionRequest, prompts []json.RawMessage) {
	d := p.d
	// Lavish hands what the user sent over once, so a failure to journal it is
	// tried again before it is given up.
	var got store.LavishCollected
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		if got, err = d.store.LavishCollected(r.ID, prompts, time.Now()); err == nil {
			break
		}
		if !sleep(context.WithoutCancel(ctx), 100*time.Millisecond) {
			break
		}
	}
	if err != nil {
		d.log.Error("collecting what the user sent from a decision page failed", "request", r.Request, "version", r.Version, "error", err.Error())
		return
	}
	if got.Refused != "" {
		d.log.Info("what a decision page sent was refused", "request", r.Request, "version", r.Version, "reason", got.Refused)
		return
	}
	d.log.Info("the user sent a decision page", "request", r.Request, "version", r.Version, "job", r.Job, "answer", got.Answer.ID, "answers", len(got.Answer.Answers), "comments", len(got.Answer.Comments), "refused", len(got.Answer.Refused), "unanswered", len(got.Unanswered))
	if _, err := p.render(got.Request); err != nil {
		d.log.Error("rendering a decision page with its answers failed", "request", r.Request, "version", r.Version, "error", err.Error())
	}
	var agents []string
	d.mu.Lock()
	for _, dispatch := range []*store.Dispatch{got.Owner, got.Confirm} {
		if dispatch == nil || slices.Contains(agents, dispatch.Agent) {
			continue
		}
		agents = append(agents, dispatch.Agent)
		if to, err := roles.ParseAddress(dispatch.Agent); err == nil {
			if err := d.ensureAgent(to); err != nil {
				d.log.Error("starting the agent that receives what the user sent", "agent", dispatch.Agent, "dispatch", dispatch.ID, "error", err.Error())
			}
		}
	}
	d.mu.Unlock()
	for _, agent := range agents {
		d.nudge(agent)
	}
}
