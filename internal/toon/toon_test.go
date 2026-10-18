// SPDX-License-Identifier: Apache-2.0

package toon

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Marshal writes TOON in the key order encoding/json gives: struct fields
// in declaration order, map keys sorted.
func TestMarshalWritesTheDocumentsOfTheSpec(t *testing.T) {
	type agent struct {
		Agent string `json:"agent"`
		State string `json:"state"`
		PID   int    `json:"pid,omitempty"`
	}
	tests := []struct {
		name string
		v    any
		want string
	}{
		{"primitive fields", map[string]any{"version": "v1.2.3", "running": true, "pid": 42, "gone": nil},
			"gone: null\npid: 42\nrunning: true\nversion: v1.2.3\n"},
		{"struct order", struct {
			Z string `json:"z"`
			A int    `json:"a"`
		}{"last", 1}, "z: last\na: 1\n"},
		{"nested object", map[string]any{"daemon": map[string]any{"running": false}, "state_dir": "/s"},
			"daemon:\n  running: false\nstate_dir: /s\n"},
		{"empty object field", map[string]any{"data": map[string]any{}}, "data:\n"},
		{"empty root", map[string]any{}, ""},
		{"primitive array", map[string]any{"lines": []string{"a", "b c", ""}}, "lines[3]: a,b c,\"\"\n"},
		{"empty array", map[string]any{"checks": []int{}}, "checks[0]:\n"},
		{"tabular array", map[string]any{"leaders": []agent{{"leader@coordination", "running", 7}, {"leader@engineering", "stopped", 9}}},
			"leaders[2]{agent,state,pid}:\n  leader@coordination,running,7\n  leader@engineering,stopped,9\n"},
		{"non-uniform objects become a list", map[string]any{"agents": []agent{{"a", "running", 7}, {"b", "stopped", 0}}},
			"agents[2]:\n  - agent: a\n    state: running\n    pid: 7\n  - agent: b\n    state: stopped\n"},
		{"list item with nested first field", map[string]any{"jobs": []any{map[string]any{"criteria": []string{"x", "y"}, "id": 1}, map[string]any{}}},
			"jobs[2]:\n  - criteria[2]: x,y\n    id: 1\n  -\n"},
		{"list item with tabular first field", map[string]any{"xs": []any{map[string]any{"rows": []map[string]int{{"a": 1}, {"a": 2}}, "z": 2}, "p"}},
			"xs[2]:\n  - rows[2]{a}:\n      1\n      2\n    z: 2\n  - p\n"},
		{"list of arrays", map[string]any{"m": []any{[]int{1, 2}, []int{}}}, "m[2]:\n  - [2]: 1,2\n  - [0]:\n"},
		{"root array", []int{1, 2}, "[2]: 1,2\n"},
		{"root primitive", "hello", "hello\n"},
		{"quoting", map[string]any{"s": []string{"true", "null", "12", "-1", "1e5", "05", " pad", "a:b", "a,b", `q"`, "back\\slash", "line\nnext\ttab\r", "-x", "[x]", "{x}"}},
			`s[15]: "true","null","12","-1","1e5","05"," pad","a:b","a,b","q\"","back\\slash","line\nnext\ttab\r","-x","[x]","{x}"` + "\n"},
		{"keys", map[string]any{"a b": 1, "1x": 2, "ok_key.x": 3, "": 4}, "\"\": 4\n\"1x\": 2\n\"a b\": 1\nok_key.x: 3\n"},
		{"numbers", map[string]any{"big": 1e21, "neg0": math.Copysign(0, -1), "f": 1.5, "small": 0.000001},
			"big: 1000000000000000000000\nf: 1.5\nneg0: 0\nsmall: 0.000001\n"},
		{"MarshalJSON is honoured", map[string]any{"at": time.Date(2026, 10, 18, 1, 2, 3, 0, time.UTC), "raw": json.RawMessage(`{"k":[1]}`)},
			"at: \"2026-10-18T01:02:03Z\"\nraw:\n  k[1]: 1\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Marshal(tt.v)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Errorf("Marshal printed\n%s\nwant\n%s", got, tt.want)
			}
		})
	}
}

// What Marshal writes, Unmarshal reads back as the value encoding/json
// reads from the JSON of the same Go value.
func TestUnmarshalReadsBackWhatMarshalWrites(t *testing.T) {
	values := []any{
		map[string]any{},
		[]any{},
		"plain",
		"",
		42,
		nil,
		true,
		[]any{1, "a", nil, map[string]any{"k": "v"}, []any{"x", []any{}}},
		map[string]any{
			"daemon":       map[string]any{"running": true, "pid": 12, "started_at": time.Unix(0, 0).UTC()},
			"leaders":      []map[string]any{{"agent": "leader@coordination", "state": "running"}, {"agent": "x", "state": "y"}},
			"mixed":        []map[string]any{{"a": 1}, {"b": 2}, {}},
			"nested":       []map[string]any{{"inner": map[string]any{"deep": []int{1}}, "z": "q:x"}},
			"tricky":       []string{"- dash", "a, b", `"quoted"`, "tab\there", "#", "|", "  ", "0", "1.0", "false"},
			"license":      "line one\nline two\n",
			"lists":        []any{[]any{map[string]any{"a": 1}, map[string]any{"a": 2}}, []map[string]any{{"x": []int{}}}},
			"empty":        map[string]any{"inner": map[string]any{}},
			"floats":       []float64{1.5, -2.25, 1e-7, 3e20},
			"weird key: x": map[string]any{"[k]": "v", "-k": []string{}},
		},
	}
	for _, v := range values {
		encoded, err := Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Unmarshal(encoded)
		if err != nil {
			t.Fatalf("Unmarshal of\n%s: %v", encoded, err)
		}
		js, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		var want any
		if err := json.Unmarshal(js, &want); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Unmarshal of\n%s\nreads %#v, want %#v", encoded, got, want)
		}
	}
}

func TestUnmarshalRefusesMalformedDocuments(t *testing.T) {
	for _, doc := range []string{
		"a[2]: 1\n",
		"a[2]{x}:\n  1\n",
		"a[1]{x,y}:\n  1\n",
		"a: \"open\n",
		"a:\n   b: 1\n",
		"a: 1\na: 2\n",
		"a[x]: 1\n",
	} {
		if v, err := Unmarshal([]byte(doc)); err == nil {
			t.Errorf("Unmarshal(%q) = %#v, want an error", doc, v)
		}
	}
}

// Marshal refuses what encoding/json refuses.
func TestMarshalRefusesWhatJSONRefuses(t *testing.T) {
	if _, err := Marshal(map[string]any{"c": make(chan int)}); err == nil || !strings.Contains(err.Error(), "chan") {
		t.Errorf("Marshal of a channel gave %v, want encoding/json's error", err)
	}
}
