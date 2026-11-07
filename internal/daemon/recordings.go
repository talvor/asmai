// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"cmp"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/talvor/asmai/internal/recording"
	"github.com/talvor/asmai/internal/session"
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
		os.Remove(filepath.Dir(r.Path)) // only when empty
		if err := d.store.RecordingDeleted(r.Dispatch, now); err != nil {
			d.log.Error("journaling a recording's deletion", "dispatch", r.Dispatch, "error", err.Error())
			continue
		}
		d.log.Info("a recording was deleted after its job ended", "dispatch", r.Dispatch, "job", r.Job, "retention", retention.String())
	}
}
