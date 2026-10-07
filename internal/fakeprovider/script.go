// SPDX-License-Identifier: Apache-2.0

// Package fakeprovider is the scripted fake provider CLI that AsmAI's
// development tests drive in place of a real provider: it plays a script of
// terminal screens, expected input and hook payloads. The fake never counts
// toward qualification, never certifies a combination and is never built into
// the asmai executable. README.md documents the script format.
package fakeprovider

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// A step is one entry of a script: exactly one of a screen to draw, input to
// wait for, or a hook payload to deliver.
type step struct {
	// screen is drawn on the terminal as it is, escape sequences included.
	screen string
	// expect is the input the fake waits for, byte for byte, before it goes on.
	expect string
	// hook is the name of the event whose payload is delivered to the hook
	// command configured for it.
	hook    string
	payload []byte
}

// scriptLine is a step as written in the script, with each field optional so
// that a missing field can be told apart from an empty one.
type scriptLine struct {
	Screen  *string         `json:"screen"`
	Expect  *string         `json:"expect"`
	Hook    *string         `json:"hook"`
	Payload json.RawMessage `json:"payload"`
}

var errPayloadOutsideHook = errors.New(`only a "hook" step has a "payload"`)

// readScript reads a script: one JSON object per line, each a step, played in
// order. Blank lines are skipped.
func readScript(r io.Reader) ([]step, error) {
	var steps []step
	lines := bufio.NewScanner(r)
	lines.Buffer(nil, 16<<20)
	for n := 1; lines.Scan(); n++ {
		line := bytes.TrimSpace(lines.Bytes())
		if len(line) == 0 {
			continue
		}
		s, err := parseStep(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
		steps = append(steps, s)
	}
	if err := lines.Err(); err != nil {
		return nil, err
	}
	if len(steps) == 0 {
		return nil, errors.New("the script has no steps")
	}
	return steps, nil
}

func parseStep(line []byte) (step, error) {
	var raw scriptLine
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return step{}, err
	}
	kinds := 0
	for _, f := range []*string{raw.Screen, raw.Expect, raw.Hook} {
		if f != nil {
			kinds++
		}
	}
	if kinds != 1 {
		return step{}, errors.New(`a step has exactly one of "screen", "expect" and "hook"`)
	}
	switch {
	case raw.Screen != nil:
		if raw.Payload != nil {
			return step{}, errPayloadOutsideHook
		}
		if *raw.Screen == "" {
			return step{}, errors.New(`a "screen" step draws something`)
		}
		return step{screen: *raw.Screen}, nil
	case raw.Expect != nil:
		if raw.Payload != nil {
			return step{}, errPayloadOutsideHook
		}
		if *raw.Expect == "" {
			return step{}, errors.New(`an "expect" step waits for some input`)
		}
		return step{expect: *raw.Expect}, nil
	default:
		if *raw.Hook == "" {
			return step{}, errors.New(`a "hook" step names its event`)
		}
		if raw.Payload == nil {
			return step{}, errors.New(`a "hook" step has a "payload"`)
		}
		return step{hook: *raw.Hook, payload: raw.Payload}, nil
	}
}
