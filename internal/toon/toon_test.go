// SPDX-License-Identifier: Apache-2.0

package toon

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Marshal follows the TOON specification's encoding of each JSON shape.
func TestMarshalEncodesEachShapeAsTheSpecificationDoes(t *testing.T) {
	type user struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
		Role string `json:"role,omitempty"`
	}
	for _, c := range []struct {
		name string
		v    any
		want string
	}{
		{"primitive fields", map[string]any{"id": 1, "name": "Ada", "active": true, "none": nil},
			"active: true\nid: 1\nname: Ada\nnone: null\n"},
		{"struct fields keep their order", user{ID: 1, Name: "Ada"}, "id: 1\nname: Ada\n"},
		{"nested object", map[string]any{"daemon": map[string]any{"running": false}},
			"daemon:\n  running: false\n"},
		{"empty object value", map[string]any{"meta": map[string]any{}}, "meta:\n"},
		{"primitive array", map[string]any{"tags": []string{"a", "b"}}, "tags[2]: a,b\n"},
		{"empty array", map[string]any{"tags": []string{}}, "tags[0]:\n"},
		{"tabular array", map[string]any{"users": []user{{1, "Alice", "admin"}, {2, "Bob", "user"}}},
			"users[2]{id,name,role}:\n  1,Alice,admin\n  2,Bob,user\n"},
		{"objects of different fields are a list", map[string]any{"users": []user{{1, "Alice", "admin"}, {2, "Bob", ""}}},
			"users[2]:\n  - id: 1\n    name: Alice\n    role: admin\n  - id: 2\n    name: Bob\n"},
		{"mixed list", map[string]any{"items": []any{1, map[string]any{"a": 1}, "text"}},
			"items[3]:\n  - 1\n  - a: 1\n  - text\n"},
		{"list item with a nested object first", map[string]any{"items": []any{map[string]any{"a": map[string]any{"id": 1}, "b": true}}},
			"items[1]:\n  - a:\n      id: 1\n    b: true\n"},
		{"list item with a table first", map[string]any{"items": []any{map[string]any{"a": []user{{ID: 1, Name: "x"}}, "b": []any{}}}},
			"items[1]:\n  - a[1]{id,name}:\n      1,x\n    b[0]:\n"},
		{"empty object in a list", []any{map[string]any{}, 1}, "[2]:\n  -\n  - 1\n"},
		{"arrays of arrays", map[string]any{"pairs": [][]int{{1, 2}, {3}}},
			"pairs[2]:\n  - [2]: 1,2\n  - [1]: 3\n"},
		{"root array", []int{1, 2}, "[2]: 1,2\n"},
		{"root primitive", "hello", "hello\n"},
		{"empty root object", map[string]any{}, ""},
		{"time as JSON marshals it", map[string]any{"at": time.Date(2026, 10, 18, 9, 30, 0, 0, time.UTC)},
			"at: \"2026-10-18T09:30:00Z\"\n"},
		{"raw JSON is data", map[string]any{"data": json.RawMessage(`{"pid":7}`)}, "data:\n  pid: 7\n"},
		{"numbers are canonical", map[string]any{"a": 1.5, "b": 1e21, "c": -0.0, "d": 100}, "a: 1.5\nb: 1000000000000000000000\nc: 0\nd: 100\n"},
	} {
		got, err := Marshal(c.v)
		if err != nil {
			t.Errorf("%s: Marshal: %v", c.name, err)
			continue
		}
		if string(got) != c.want {
			t.Errorf("%s: Marshal printed\n%s\nwant\n%s", c.name, got, c.want)
		}
	}
}

// A string that would read as something else is quoted, and a key that is
// not a plain identifier.
func TestMarshalQuotesWhatWouldReadAsSomethingElse(t *testing.T) {
	for s, want := range map[string]string{
		"plain words":  "plain words",
		"":             `""`,
		" padded":      `" padded"`,
		"true":         `"true"`,
		"null":         `"null"`,
		"42":           `"42"`,
		"-3.5e2":       `"-3.5e2"`,
		"05":           `"05"`,
		"a:b":          `"a:b"`,
		"a,b":          `"a,b"`,
		"[x]":          `"[x]"`,
		"{x}":          `"{x}"`,
		"- item":       `"- item"`,
		`say "hi"`:     `"say \"hi\""`,
		"two\nlines\t": `"two\nlines\t"`,
		`back\slash`:   `"back\\slash"`,
		"café":         "café",
	} {
		got, err := Marshal(map[string]string{"k": s})
		if err != nil {
			t.Fatal(err)
		}
		if line := "k: " + want + "\n"; string(got) != line {
			t.Errorf("Marshal of %q printed %q, want %q", s, got, line)
		}
	}
	got, _ := Marshal(map[string]int{"state dir": 1, "a.b_c": 2, "9lives": 3})
	if want := "\"9lives\": 3\na.b_c: 2\n\"state dir\": 1\n"; string(got) != want {
		t.Errorf("Marshal printed %q, want %q", got, want)
	}
}

// Decoding what Marshal prints gives what decoding the JSON of the same value
// does, for every shape.
func TestDecodeReadsBackWhatMarshalPrints(t *testing.T) {
	values := []any{
		map[string]any{"version": "dev"},
		map[string]any{"daemon": map[string]any{"running": true, "pid": 12, "started_at": time.Now()}, "leaders": []map[string]any{{"agent": "leader@coordination", "state": "running"}}, "prompts": []any{}, "state_dir": "/tmp/a b"},
		map[string]any{"items": []any{1, "x", nil, true, map[string]any{"a": []any{map[string]any{"b": 1}}, "c": map[string]any{"d": "e:f"}}, []any{[]any{1}, map[string]any{}}}},
		map[string]any{"text": "line one\nline \"two\"\t\\", "odd": []string{"", " ", "-", "a,b", "1.0", "true"}},
		map[string]any{"rows": []map[string]any{{"x": "a,b", "y": nil}, {"y": 2.5, "x": "-1"}}},
		map[string]any{"table first": []any{map[string]any{"t": []map[string]int{{"a": 1}, {"a": 2}}, "u": 1}}},
		[]any{},
		[]any{[]any{}, []any{map[string]any{"k": 1}}},
		"just a string",
		3.25,
		map[string]any{},
		map[string]any{"screen": strings.Repeat("row \x1b[0m café\n", 3)},
	}
	for _, v := range values {
		encoded, err := Marshal(v)
		if err != nil {
			t.Fatalf("Marshal(%v): %v", v, err)
		}
		got, err := Decode(encoded)
		if err != nil {
			t.Errorf("Decode of\n%s\nfailed: %v", encoded, err)
			continue
		}
		data, _ := json.Marshal(v)
		var want any
		json.Unmarshal(data, &want)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Decode of\n%s\ngave %#v, want %#v", encoded, got, want)
		}
	}
}

// Marshal prints the same bytes for the same value every time.
func TestMarshalIsDeterministic(t *testing.T) {
	v := map[string]any{"b": 1, "a": map[string]any{"z": 1, "y": 2, "x": []any{map[string]any{"q": 1, "p": 2}}}}
	first, _ := Marshal(v)
	for range 50 {
		if again, _ := Marshal(v); string(again) != string(first) {
			t.Fatalf("Marshal printed %q, then %q", first, again)
		}
	}
}

func TestMarshalFailsWhereJSONDoes(t *testing.T) {
	if _, err := Marshal(map[string]any{"f": func() {}}); err == nil {
		t.Error("Marshal of a func succeeded, want the error json.Marshal gives")
	}
}

func TestDecodeRefusesWhatIsNotTOON(t *testing.T) {
	for _, s := range []string{"a: 1\n  b: 2\n", "x[2]: 1\n", "x[2]{a}:\n  1\n", "k: \"open\n"} {
		if v, err := Decode([]byte(s)); err == nil {
			t.Errorf("Decode(%q) gave %#v, want an error", s, v)
		}
	}
}
