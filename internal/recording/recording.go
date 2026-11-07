// SPDX-License-Identifier: Apache-2.0

// Package recording writes the compressed recording of an agent's terminal
// that each dispatch has (03 rule 41). A recording is an asciicast v2 file,
// one JSON header line and then one JSON line per event, compressed with
// gzip: the output the provider wrote to the terminal and the terminal's
// resizes, each with its time since the recording began.
package recording

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Extension ends the name of every recording file.
const Extension = ".cast.gz"

// flushEvery is how long output may wait in the compressor before it is
// flushed to the file, so that a daemon that dies loses at most this much.
const flushEvery = time.Second

// Path returns the file that holds the recording of dispatch for agent
// under dir.
func Path(dir, agent string, dispatch int64) string {
	return filepath.Join(dir, agent, fmt.Sprintf("dispatch-%d%s", dispatch, Extension))
}

// Writer appends to one recording. It is safe for concurrent use.
type Writer struct {
	mu      sync.Mutex
	f       *os.File
	gz      *gzip.Writer
	epoch   time.Time
	flushed time.Time
	err     error
}

// Open opens the recording at path, making it, and its folder, if missing.
// The terminal is columns by rows. An existing recording is appended to,
// as a dispatch the user interrupted is typed again into the same terminal:
// its events are timed from epoch, the time its recording began, and it
// keeps its first header.
func Open(path string, epoch time.Time, columns, rows int) (*Writer, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("making the recordings folder: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening a recording: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("opening a recording: %w", err)
	}
	w := &Writer{f: f, gz: gzip.NewWriter(f), epoch: epoch, flushed: time.Now()}
	if info.Size() == 0 {
		w.line(map[string]any{"version": 2, "width": columns, "height": rows, "timestamp": epoch.Unix()})
	} else {
		w.Resize(columns, rows)
	}
	if w.err != nil {
		f.Close()
		return nil, fmt.Errorf("writing a recording: %w", w.err)
	}
	return w, nil
}

// Output records p, written to the terminal.
func (w *Writer) Output(p []byte) { w.event("o", string(p)) }

// Resize records the terminal becoming columns by rows.
func (w *Writer) Resize(columns, rows int) { w.event("r", fmt.Sprintf("%dx%d", columns, rows)) }

func (w *Writer) event(kind, data string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.gz == nil {
		return
	}
	w.line([]any{time.Since(w.epoch).Seconds(), kind, data})
	if time.Since(w.flushed) >= flushEvery && w.err == nil {
		w.err = w.gz.Flush()
		w.flushed = time.Now()
	}
}

// line writes v as a line, once w.mu is held or w is not yet shared.
func (w *Writer) line(v any) {
	if w.err != nil {
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		w.err = err
		return
	}
	_, w.err = w.gz.Write(append(b, '\n'))
}

// Close ends the recording and returns the size of its file. A closed
// writer records nothing.
func (w *Writer) Close() (int64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.gz == nil {
		return 0, w.err
	}
	err := w.err
	if cerr := w.gz.Close(); err == nil {
		err = cerr
	}
	w.gz = nil
	info, serr := w.f.Stat()
	if cerr := w.f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = serr
	}
	var size int64
	if serr == nil {
		size = info.Size()
	}
	if err != nil {
		return size, fmt.Errorf("writing a recording: %w", err)
	}
	return size, nil
}
