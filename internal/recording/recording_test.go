// SPDX-License-Identifier: Apache-2.0

package recording

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func lines(t *testing.T, path string) []json.RawMessage {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	var out []json.RawMessage
	s := bufio.NewScanner(gz)
	for s.Scan() {
		out = append(out, json.RawMessage(append([]byte(nil), s.Bytes()...)))
	}
	if err := s.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestARecordingIsACompressedAsciicast(t *testing.T) {
	path := Path(t.TempDir(), "engineering/leader", 7)
	w, err := Open(path, time.Now(), 120, 40)
	if err != nil {
		t.Fatal(err)
	}
	w.Output([]byte("hello\r\n"))
	w.Resize(100, 30)
	size, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(path); info.Size() != size || size == 0 {
		t.Fatalf("Close returned size %d for a file of %v", size, info)
	}
	got := lines(t, path)
	if len(got) != 3 {
		t.Fatalf("got %d lines, want a header and two events: %s", len(got), got)
	}
	var header struct{ Version, Width, Height int }
	if err := json.Unmarshal(got[0], &header); err != nil || header != (struct{ Version, Width, Height int }{2, 120, 40}) {
		t.Fatalf("header = %s (%v)", got[0], err)
	}
	var out []any
	json.Unmarshal(got[1], &out)
	if len(out) != 3 || out[1] != "o" || out[2] != "hello\r\n" {
		t.Fatalf("output event = %s", got[1])
	}
	json.Unmarshal(got[2], &out)
	if out[1] != "r" || out[2] != "100x30" {
		t.Fatalf("resize event = %s", got[2])
	}
}

func TestReopeningARecordingAppendsToItWithoutANewHeader(t *testing.T) {
	path := Path(t.TempDir(), "a", 1)
	epoch := time.Now().Add(-time.Minute)
	for _, text := range []string{"one", "two"} {
		w, err := Open(path, epoch, 80, 24)
		if err != nil {
			t.Fatal(err)
		}
		w.Output([]byte(text))
		if _, err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}
	got := lines(t, path)
	// A header, "one", then the reopening's resize and "two".
	if len(got) != 4 {
		t.Fatalf("got %d lines: %s", len(got), got)
	}
	var out []any
	json.Unmarshal(got[3], &out)
	if at, _ := out[0].(float64); at < 60 {
		t.Fatalf("an appended event is timed %v s into the recording, want past the minute that had gone", out[0])
	}
}

func TestAClosedWriterRecordsNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r"+Extension)
	w, err := Open(path, time.Now(), 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
	w.Output([]byte("late"))
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if got := lines(t, path); len(got) != 1 {
		t.Fatalf("got %d lines, want only the header", len(got))
	}
}
