// SPDX-License-Identifier: Apache-2.0

package toonjson

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/toon-format/toon-go"
)

type row struct {
	ID      int64           `json:"id"`
	Name    string          `json:"name"`
	Note    string          `json:"note,omitempty"`
	At      time.Time       `json:"at"`
	Data    json.RawMessage `json:"data"`
	Skipped string          `json:"-"`
	Ptr     *int            `json:"ptr"`
}

// decoded returns the value a JSON document and a TOON document hold.
func decoded(t *testing.T, v any) (fromJSON, fromTOON any) {
	t.Helper()
	encoded, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &fromJSON); err != nil {
		t.Fatal(err)
	}
	document, err := Marshal(v)
	if err != nil {
		t.Fatalf("Marshal(%s): %v", encoded, err)
	}
	fromTOON, err = toon.Decode(document)
	if err != nil {
		t.Fatalf("decoding %s: %v\n%s", encoded, err, document)
	}
	return fromJSON, fromTOON
}

func TestMarshalHoldsTheDataJSONHolds(t *testing.T) {
	one := 1
	at := time.Date(2026, 10, 19, 12, 30, 0, 123, time.UTC)
	for name, v := range map[string]any{
		"an object":        map[string]any{"a": 1, "b": "two", "c": true, "d": nil},
		"nested objects":   map[string]any{"daemon": map[string]any{"running": true, "pid": 42, "inner": map[string]any{"x": []int{1, 2}}}},
		"empty containers": map[string]any{"list": []string{}, "object": map[string]any{}, "nil list": []string(nil), "nil map": map[string]any(nil)},
		"a uniform array": map[string]any{"agents": []row{
			{ID: 1, Name: "leader@coordination", At: at, Data: json.RawMessage(`{"k":"v"}`)},
			{ID: 2, Name: "worker", Note: "n", At: at, Data: json.RawMessage(`[1,2]`), Ptr: &one},
		}},
		"a mixed array": map[string]any{"mixed": []any{1, "a", nil, map[string]any{"a": 1}, []any{1, 2}, map[string]any{"b": 2}}},
		"a ragged array": map[string]any{"rows": []map[string]any{
			{"a": 1, "b": 2}, {"a": 1}, {"a": 1, "b": 2, "c": 3},
		}},
		"strings that need quoting": map[string]any{"strings": []string{
			"", " ", " padded ", "a,b", "a|b", "a\tb", "a: b", "a\"b", `a\b`, "true", "false", "null", "42", "-3.5", "1e5", "05",
			"- item", "[3]: x", "{a}", "#x", "line one\nline two", "\r\n", "tab\there", "ünïcödé ✓", "emoji 🎉", "<b>&amp;</b>", "key: value\nother: value",
		}},
		"awkward keys":  map[string]any{"a b": 1, "a:b": 2, "": 3, "true": 4, "1": 5, "a,b": 6, "-x": 7, "é": 8},
		"numbers":       map[string]any{"int": 7, "negative": -7, "float": 1.5, "tiny": 0.000001, "big": 1e21, "zero": 0, "safe": int64(1)<<53 - 1},
		"a root array":  []any{1, 2, 3},
		"a root string": "just a string",
		"a root number": 7,
		"a root null":   nil,
		"a root object": map[string]any{},
	} {
		t.Run(name, func(t *testing.T) {
			fromJSON, fromTOON := decoded(t, v)
			if !reflect.DeepEqual(fromJSON, fromTOON) {
				document, _ := Marshal(v)
				t.Errorf("TOON holds %#v, JSON holds %#v\n%s", fromTOON, fromJSON, document)
			}
		})
	}
}

func TestMarshalKeepsTheOrderJSONWritesKeysIn(t *testing.T) {
	document, err := Marshal(struct {
		Zebra  string `json:"zebra"`
		Apple  string `json:"apple"`
		Middle string `json:"middle"`
	}{"z", "a", "m"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(document), "zebra: z\napple: a\nmiddle: m"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestMarshalIsDeterministic(t *testing.T) {
	v := map[string]any{"b": []int{1, 2}, "a": map[string]any{"y": 1, "x": 2}, "c": "c"}
	first, err := Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	for range 50 {
		again, err := Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if string(again) != string(first) {
			t.Fatalf("got %q then %q", first, again)
		}
	}
}

func TestMarshalPrintsAUniformArrayOnce(t *testing.T) {
	document, err := Marshal(map[string]any{"agents": []map[string]any{
		{"agent": "a", "state": "running"},
		{"agent": "b", "state": "stopped"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(document), "agents[2]{agent,state}:\n  a,running\n  b,stopped"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// An integer a double cannot hold exactly is printed as a string, not rounded.
func TestMarshalDoesNotRoundAnIntegerADoubleCannotHold(t *testing.T) {
	for _, n := range []int64{1<<53 + 1, -(1<<53 + 1), 1<<63 - 1, -1 << 63} {
		document, err := Marshal(map[string]int64{"n": n})
		if err != nil {
			t.Fatal(err)
		}
		got, err := toon.Decode(document)
		if err != nil {
			t.Fatal(err)
		}
		want, _ := json.Marshal(n)
		if s, ok := got.(map[string]any)["n"].(string); !ok || s != string(want) {
			t.Errorf("%d decoded as %#v (%s)", n, got, document)
		}
	}
	document, err := Marshal(map[string]uint64{"n": 1<<64 - 1})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(document), `"18446744073709551615"`) {
		t.Errorf("got %s", document)
	}
}

func TestMarshalRefusesWhatJSONRefuses(t *testing.T) {
	if _, err := Marshal(map[string]any{"c": make(chan int)}); err == nil {
		t.Error("a channel was encoded")
	}
}

func TestFromJSONRefusesMoreThanOneValue(t *testing.T) {
	if _, err := FromJSON([]byte(`{"a":1} {"b":2}`)); err == nil {
		t.Error("two documents were encoded as one")
	}
	if _, err := FromJSON([]byte(`{"a":`)); err == nil {
		t.Error("a truncated document was encoded")
	}
}
