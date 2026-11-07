// SPDX-License-Identifier: Apache-2.0

package recording

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"
)

// maxPause is the longest the replay waits between two events: a recording
// of a turn that waited minutes for the provider replays without them.
const maxPause = 2 * time.Second

// Play writes the output the recording at path holds to w, with the pauses
// it was recorded with, each at most maxPause, by calling wait. The
// terminal's resizes are not replayed: the replay is drawn at the size of
// the terminal it plays in. A recording whose last events were lost, as when
// the daemon died, plays as far as it goes.
func Play(w io.Writer, path string, wait func(time.Duration)) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening the recording: %w", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("reading the recording: %w", err)
	}
	r := bufio.NewReader(gz)
	if _, err := r.ReadBytes('\n'); err != nil {
		return fmt.Errorf("reading the recording's header: %w", err)
	}
	var last float64
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			var event []any
			if json.Unmarshal(line, &event) == nil && len(event) == 3 {
				at, _ := event[0].(float64)
				kind, _ := event[1].(string)
				data, _ := event[2].(string)
				if kind == "o" {
					wait(min(max(time.Duration((at-last)*float64(time.Second)), 0), maxPause))
					last = at
					if _, err := io.WriteString(w, data); err != nil {
						return err
					}
				}
			}
		}
		if err != nil {
			// A recording cut short ends without its closing bytes.
			return nil
		}
	}
}
