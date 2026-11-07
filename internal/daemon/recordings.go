// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/talvor/asmai/internal/recording"
	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/session"
	"github.com/talvor/asmai/internal/store"
)

// recordingSweep is how often the daemon deletes the recordings whose
// retention has run out.
const recordingSweep = time.Hour

// dispatchRecording is the recording being made of the terminal of a
// leader's session, for the dispatch typed to it (03 rule 41).
type dispatchRecording struct {
	dispatch int64
	session  *session.Session
	writer   *recording.Writer
	path     string
}

// syncRecording makes l's recording that of the dispatch its session is
// working on: the one typed to it that it has not yet finished, or none. A
// recording failing never stops the agent; it is logged. d.mu is held.
func (d *daemon) syncRecording(l *leader) {
	want := int64(0)
	if l.session != nil && d.cfg.Paths.Recordings != "" {
		want = cmp.Or(l.currentDispatch, l.pendingDispatch)
	}
	if l.recording != nil && l.recording.dispatch == want && l.recording.session == l.session {
		return
	}
	d.stopRecording(l)
	if want == 0 {
		return
	}
	agent := l.address.String()
	r, err := d.store.RecordingStarted(want, recording.Path(d.cfg.Paths.Recordings, agent, want), time.Now())
	if err != nil {
		d.log.Error("journaling where a dispatch is recorded", "agent", agent, "dispatch", want, "error", err.Error())
		return
	}
	if !r.Deleted.IsZero() {
		// Its job ended and the retention ran out; it is not made again.
		return
	}
	screen := l.session.Screen()
	w, err := recording.Open(r.Path, r.Started, screen.Columns, screen.Rows)
	if err != nil {
		d.log.Error("recording a dispatch", "agent", agent, "dispatch", want, "error", err.Error())
		return
	}
	l.recording = &dispatchRecording{dispatch: want, session: l.session, writer: w, path: r.Path}
	l.session.Record(w)
}

// stopRecording ends l's recording, if it has one, and journals its size.
// d.mu is held.
func (d *daemon) stopRecording(l *leader) {
	r := l.recording
	if r == nil {
		return
	}
	l.recording = nil
	r.session.Record(nil)
	size, err := r.writer.Close()
	if err != nil {
		d.log.Error("finishing a dispatch's recording", "agent", l.address.String(), "dispatch", r.dispatch, "error", err.Error())
	}
	if err := d.store.RecordingFinished(r.dispatch, size, time.Now()); err != nil {
		d.log.Error("journaling the size of a dispatch's recording", "agent", l.address.String(), "dispatch", r.dispatch, "error", err.Error())
	}
}

// watchRecordings deletes the recordings whose job ended longer ago than the
// configured retention, now and then, until the daemon stops (03 rule 43).
func (d *daemon) watchRecordings() {
	defer d.wg.Done()
	tick := time.NewTicker(recordingSweep)
	defer tick.Stop()
	for {
		d.sweepRecordings(time.Now())
		select {
		case <-d.work.Done():
			return
		case <-tick.C:
		}
	}
}

// sweepRecordings deletes the files of the recordings whose job ended
// before now less the retention, and journals each deletion. A recording
// being made is never deleted.
func (d *daemon) sweepRecordings(now time.Time) {
	d.configMu.Lock()
	retention := d.applied.Logs.RecordingRetention
	d.configMu.Unlock()
	expired, err := d.store.ExpiredRecordings(now.Add(-retention))
	if err != nil {
		d.log.Error("finding the recordings to delete", "error", err.Error())
		return
	}
	for _, r := range expired {
		d.mu.Lock()
		live := false
		for _, l := range d.leaders {
			live = live || l.recording != nil && l.recording.dispatch == r.Dispatch
		}
		d.mu.Unlock()
		if live {
			continue
		}
		if err := os.Remove(r.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			d.log.Error("deleting a recording", "dispatch", r.Dispatch, "path", r.Path, "error", err.Error())
			continue
		}
		if err := d.store.RecordingDeleted(r.Dispatch, now); err != nil {
			d.log.Error("journaling a recording's deletion", "dispatch", r.Dispatch, "error", err.Error())
			continue
		}
		d.log.Info("a recording was deleted after its job ended", "dispatch", r.Dispatch, "job", r.Job, "retention", retention.String())
	}
}

// replay finds the recording req asks to replay: the one of the dispatch it
// names, which must have been typed to its agent, or the latest of its agent.
// It says plainly when there is none, and when it has been deleted.
func (d *daemon) replay(req Request) (Response, error) {
	address, err := roles.ParseAddress(req.Agent)
	if err != nil {
		return Response{}, err
	}
	agent := address.String()
	var r store.Recording
	var ok bool
	if req.Dispatch != 0 {
		r, ok, err = d.store.Recording(req.Dispatch)
		if err == nil && ok && r.Agent != agent {
			return Response{}, fmt.Errorf("dispatch %d was typed to %s, not %s", req.Dispatch, r.Agent, agent)
		}
		if err == nil && !ok {
			return Response{}, fmt.Errorf("dispatch %d has no recording for %s: it was never typed to an agent, or is not a dispatch of %s", req.Dispatch, agent, agent)
		}
	} else {
		r, ok, err = d.store.LatestRecording(agent)
		if err == nil && !ok {
			return Response{}, fmt.Errorf("%s has no recording: no dispatch has been typed to it", agent)
		}
	}
	if err != nil {
		d.log.Error("finding a recording to replay", "agent", agent, "error", err.Error())
		return Response{}, fmt.Errorf("finding the recording: %w", err)
	}
	if !r.Deleted.IsZero() {
		return Response{}, fmt.Errorf("the recording of dispatch %d of %s was deleted on %s, after its job ended and the retention ran out", r.Dispatch, agent, r.Deleted.Local().Format("2006-01-02 15:04"))
	}
	if _, err := os.Stat(r.Path); err != nil {
		return Response{}, fmt.Errorf("the recording of dispatch %d of %s is missing: its file %s cannot be read (%v)", r.Dispatch, agent, r.Path, err)
	}
	return Response{Recording: &r}, nil
}
