// SPDX-License-Identifier: Apache-2.0

// Package logfile is the daemon's log: JSON lines appended to a file that
// rotates through a fixed number of files of a fixed size, read back oldest
// first, and rendered as readable lines for the user.
package logfile

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// Files is how many files the log rotates through: the current file and
	// its older files, path.1 (the newest) to path.4 (the oldest).
	Files = 5
	// FileSize is the size, 20 MB, past which the current file is rotated.
	FileSize = 20 << 20
)

// Writer appends to a log that rotates through files of a fixed size. Each
// Write is one line, and is never split across files.
type Writer struct {
	mu    sync.Mutex
	path  string
	size  int64
	files int
	f     *os.File
	n     int64
}

// Open opens the log at path for appending, creating it readable only by the
// user, to rotate through Files files of FileSize.
func Open(path string) (*Writer, error) {
	return OpenSized(path, FileSize, Files)
}

// OpenSized opens the log at path for appending, to rotate through files
// files of size bytes.
func OpenSized(path string, size int64, files int) (*Writer, error) {
	w := &Writer{path: path, size: size, files: files}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *Writer) open() error {
	f, err := os.OpenFile(w.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("opening the log: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("opening the log: %w", err)
	}
	w.f, w.n = f, info.Size()
	return nil
}

// Write appends p, rotating first if it would take the current file past
// its size.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return 0, os.ErrClosed
	}
	if w.n > 0 && w.n+int64(len(p)) > w.size {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := w.f.Write(p)
	w.n += int64(n)
	return n, err
}

// rotate drops the oldest file, moves each other file one older, and starts
// a new current file.
func (w *Writer) rotate() error {
	if err := w.f.Close(); err != nil {
		return err
	}
	w.f = nil
	if err := os.Remove(older(w.path, w.files-1)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for i := w.files - 2; i >= 0; i-- {
		if err := os.Rename(older(w.path, i), older(w.path, i+1)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return w.open()
}

// Close closes the log.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

// older names the log's file i rotations older than the current one: path
// itself for 0.
func older(path string, i int) string {
	if i == 0 {
		return path
	}
	return path + "." + strconv.Itoa(i)
}

// Read calls line with each line of the log at path, oldest first, without
// its newline. A missing log has no lines.
func Read(path string, line func([]byte) error) error {
	for i := Files - 1; i >= 0; i-- {
		if err := readFile(older(path, i), line); err != nil {
			return err
		}
	}
	return nil
}

// readFile calls line with each line of the file at path, if it exists.
func readFile(path string, line func([]byte) error) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	partial, err := readLines(bufio.NewReader(f), nil, line)
	if err != nil || len(partial) == 0 {
		return err
	}
	return line(partial)
}

// readLines calls line with each whole line from r, appending the first to
// partial, until r's end. It returns the partial line left at the end.
func readLines(r *bufio.Reader, partial []byte, line func([]byte) error) ([]byte, error) {
	for {
		chunk, err := r.ReadBytes('\n')
		partial = append(partial, chunk...)
		if err == io.EOF {
			return partial, nil
		}
		if err != nil {
			return partial, err
		}
		if err := line(bytes.TrimSuffix(partial, []byte("\n"))); err != nil {
			return nil, err
		}
		partial = partial[:0]
	}
}

// Follow calls line with each line of the log at path, oldest first, then
// with each line written to it after, checking every poll, until ctx is done.
// It follows the log across rotations.
func Follow(ctx context.Context, path string, poll time.Duration, line func([]byte) error) error {
	// Every file but the current one is finished. The current one is read,
	// then followed.
	for i := Files - 1; i >= 1; i-- {
		if err := readFile(older(path, i), line); err != nil {
			return err
		}
	}
	var f *os.File
	defer func() {
		if f != nil {
			f.Close()
		}
	}()
	var r *bufio.Reader
	var partial []byte
	for {
		if f == nil {
			opened, err := os.Open(path)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if err == nil {
				f, r, partial = opened, bufio.NewReader(opened), nil
			}
		}
		if f != nil {
			var err error
			if partial, err = readLines(r, partial, line); err != nil {
				return err
			}
			rotated, err := rotatedAway(f, path)
			if err != nil {
				return err
			}
			if rotated {
				// The file read is no longer the current one. Finish it,
				// including what was written just before the rotation,
				// then read the new current file from its start.
				if partial, err = readLines(r, partial, line); err != nil {
					return err
				}
				if len(partial) > 0 {
					if err := line(partial); err != nil {
						return err
					}
				}
				f.Close()
				f = nil
				continue
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(poll):
		}
	}
}

// rotatedAway reports whether f is no longer the file at path.
func rotatedAway(f *os.File, path string) (bool, error) {
	current, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return !sameFile(f, current), nil
}

func sameFile(f *os.File, info os.FileInfo) bool {
	open, err := f.Stat()
	return err == nil && os.SameFile(open, info)
}

// Format renders a line of the log for the user: its time, level and
// message, then its other fields as key=value. A line that is not a log
// record, such as a crash report the daemon wrote, is returned as it is.
func Format(line []byte) string {
	fields, ok := record(line)
	if !ok {
		return string(line)
	}
	var at, level, msg string
	var rest []string
	for _, f := range fields {
		var s string
		if json.Unmarshal(f.value, &s) != nil {
			s = string(f.value)
		}
		switch f.key {
		case "time":
			at = s
			if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
				at = t.Format("2006-01-02T15:04:05.000Z07:00")
			}
		case "level":
			level = s
		case "msg":
			msg = s
		default:
			if strings.ContainsAny(s, " \t\"=") || s == "" {
				s = strconv.Quote(s)
			}
			rest = append(rest, f.key+"="+s)
		}
	}
	out := fmt.Sprintf("%s  %-5s  %s", at, level, msg)
	if len(rest) > 0 {
		out += "  " + strings.Join(rest, " ")
	}
	return out
}

type field struct {
	key   string
	value json.RawMessage
}

// record parses a log record's fields in the order they were written.
func record(line []byte) ([]field, bool) {
	dec := json.NewDecoder(bytes.NewReader(line))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, false
	}
	var fields []field
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, false
		}
		key, ok := t.(string)
		if !ok {
			return nil, false
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, false
		}
		fields = append(fields, field{key, value})
	}
	if t, err := dec.Token(); err != nil || t != json.Delim('}') {
		return nil, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, false
	}
	return fields, true
}
