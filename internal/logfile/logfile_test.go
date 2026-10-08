// SPDX-License-Identifier: Apache-2.0

package logfile

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func readAll(t *testing.T, path string) []string {
	t.Helper()
	var got []string
	err := Read(path, func(line []byte) error {
		got = append(got, string(line))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func writeLines(t *testing.T, w *Writer, from, to int) {
	t.Helper()
	for i := from; i < to; i++ {
		if _, err := fmt.Fprintf(w, "line %02d\n", i); err != nil {
			t.Fatal(err)
		}
	}
}

func lineRange(from, to int) []string {
	var lines []string
	for i := from; i < to; i++ {
		lines = append(lines, fmt.Sprintf("line %02d", i))
	}
	return lines
}

func TestTheLogRotatesThroughFiveFilesOf20MB(t *testing.T) {
	if Files != 5 || FileSize != 20<<20 {
		t.Errorf("the log rotates through %d files of %d bytes, want 5 of 20 MB", Files, FileSize)
	}
}

func TestTheLogRotatesThroughItsFilesAndDropsTheOldest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	// Each line is 8 bytes, so each file holds 3 lines.
	w, err := OpenSized(path, 24, 5)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	writeLines(t, w, 0, 20)

	if got, want := readAll(t, path), lineRange(6, 20); !slices.Equal(got, want) {
		t.Errorf("the log reads\n%q\nwant\n%q", got, want)
	}
	for i, want := range map[int][]string{0: lineRange(18, 20), 1: lineRange(15, 18), 4: lineRange(6, 9)} {
		data, err := os.ReadFile(older(path, i))
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n"); !slices.Equal(got, want) {
			t.Errorf("%s holds %q, want %q", older(path, i), got, want)
		}
	}
	if _, err := os.Stat(older(path, 5)); !os.IsNotExist(err) {
		t.Errorf("a sixth file exists (%v), want five at most", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("the log's mode is %v, want -rw-------", mode)
	}
}

func TestReopeningTheLogAppendsToIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	w, err := OpenSized(path, 24, 5)
	if err != nil {
		t.Fatal(err)
	}
	writeLines(t, w, 0, 2)
	w.Close()
	if w, err = OpenSized(path, 24, 5); err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	writeLines(t, w, 2, 4)
	if got, want := readAll(t, path), lineRange(0, 4); !slices.Equal(got, want) {
		t.Errorf("the log reads %q, want %q", got, want)
	}
	if _, err := os.Stat(older(path, 1)); err != nil {
		t.Errorf("reopening did not count what the file already held: no rotation (%v)", err)
	}
}

func TestAMissingLogHasNoLines(t *testing.T) {
	if got := readAll(t, filepath.Join(t.TempDir(), "daemon.log")); len(got) != 0 {
		t.Errorf("a missing log reads %q, want nothing", got)
	}
}

func TestFollowingTheLogReadsWhatIsWrittenAcrossRotations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	w, err := OpenSized(path, 24, 5)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	writeLines(t, w, 0, 4)

	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	var got []string
	done := make(chan error, 1)
	go func() {
		done <- Follow(ctx, path, 5*time.Millisecond, func(line []byte) error {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, string(line))
			return nil
		})
	}()
	waitFor := func(n int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			mu.Lock()
			have := len(got)
			mu.Unlock()
			if have >= n {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("following read %d lines, want %d", have, n)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	waitFor(4)
	// Written slowly, each line is read before the next rotation.
	for i := 4; i < 12; i++ {
		writeLines(t, w, i, i+1)
		waitFor(i + 1)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if want := lineRange(0, 12); !slices.Equal(got, want) {
		t.Errorf("following read\n%q\nwant\n%q", got, want)
	}
}

func TestFormatRendersALogRecordForTheUser(t *testing.T) {
	for line, want := range map[string]string{
		`{"time":"2026-10-08T09:00:00.123456Z","level":"INFO","msg":"daemon started","version":"dev","pid":42}`: "2026-10-08T09:00:00.123Z  INFO   daemon started  version=dev pid=42",
		`{"time":"2026-10-08T09:00:00Z","level":"WARN","msg":"stopping","by":"asmai stop","empty":""}`:          `2026-10-08T09:00:00.000Z  WARN   stopping  by="asmai stop" empty=""`,
		`panic: something broke`: "panic: something broke",
		`{"msg": "two"} {}`:      `{"msg": "two"} {}`,
	} {
		if got := Format([]byte(line)); got != want {
			t.Errorf("Format(%s) = %q, want %q", line, got, want)
		}
	}
}
