// SPDX-License-Identifier: Apache-2.0

// Package toon writes and reads TOON, Token-Oriented Object Notation
// (https://github.com/toon-format/spec): the JSON data model in fewer
// tokens, with indentation for nesting, a declared length on every array
// and a header row for an array of objects that share their primitive
// fields.
//
// Marshal encodes a Go value by way of encoding/json, so that TOON shows the
// very data, in the very key order, that JSON shows of the same value: json
// tags, omitempty and MarshalJSON all apply. It writes the comma delimiter
// and two-space indentation, without key folding. Unmarshal reads what
// Marshal writes, into the values encoding/json's Unmarshal gives an any.
package toon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// indent is one level of nesting.
const indent = "  "

// member is a field of an object, in the order JSON has it.
type member struct {
	key   string
	value any
}

// object is a JSON object with its fields in order. The other values a
// document holds are []any, string, json.Number, bool and nil.
type object []member

// Marshal returns the TOON encoding of v: the document for v's JSON
// encoding, ending in a newline unless it is empty, as the document for an
// empty object is.
func Marshal(v any) ([]byte, error) {
	js, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(js))
	dec.UseNumber()
	value, err := readValue(dec)
	if err != nil {
		return nil, err
	}
	var e encoder
	switch value := value.(type) {
	case object:
		e.fields(value, 0)
	case []any:
		e.entry(0, "", "", value, 1)
	default:
		e.line(0, primitive(value))
	}
	return e.buf.Bytes(), nil
}

// readValue reads the next JSON value from dec, keeping objects' key order.
func readValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch tok {
	case json.Delim('{'):
		o := object{}
		for dec.More() {
			key, err := dec.Token()
			if err != nil {
				return nil, err
			}
			value, err := readValue(dec)
			if err != nil {
				return nil, err
			}
			o = append(o, member{key.(string), value})
		}
		_, err := dec.Token()
		return o, err
	case json.Delim('['):
		a := []any{}
		for dec.More() {
			value, err := readValue(dec)
			if err != nil {
				return nil, err
			}
			a = append(a, value)
		}
		_, err := dec.Token()
		return a, err
	}
	return tok, nil
}

type encoder struct {
	buf bytes.Buffer
}

func (e *encoder) line(depth int, s string) {
	e.buf.WriteString(strings.Repeat(indent, depth))
	e.buf.WriteString(s)
	e.buf.WriteByte('\n')
}

// fields writes o's fields, each on a line at depth.
func (e *encoder) fields(o object, depth int) {
	for _, m := range o {
		e.entry(depth, "", encodeKey(m.key), m.value, depth+1)
	}
}

// entry writes value on a line at depth that starts with prefix, "" or a
// list item's "- ", and then key, which is "" for a list item or the root
// array. What the value nests goes at childDepth.
func (e *encoder) entry(depth int, prefix, key string, value any, childDepth int) {
	switch value := value.(type) {
	case object:
		if key == "" { // a list item
			if len(value) == 0 {
				e.line(depth, "-")
				return
			}
			// The first field shares the hyphen's line; its children go a
			// level deeper than the fields after it.
			e.entry(depth, prefix, encodeKey(value[0].key), value[0].value, depth+2)
			e.fields(value[1:], depth+1)
			return
		}
		e.line(depth, prefix+key+":")
		e.fields(value, childDepth)
	case []any:
		header := fmt.Sprintf("%s%s[%d]", prefix, key, len(value))
		if len(value) == 0 {
			e.line(depth, header+":")
			return
		}
		if allPrimitive(value) {
			cells := make([]string, len(value))
			for i, v := range value {
				cells[i] = primitive(v)
			}
			e.line(depth, header+": "+strings.Join(cells, ","))
			return
		}
		if fields, ok := tabular(value); ok {
			keys := make([]string, len(fields))
			for i, f := range fields {
				keys[i] = encodeKey(f)
			}
			e.line(depth, header+"{"+strings.Join(keys, ",")+"}:")
			for _, v := range value {
				row := make([]string, len(fields))
				for i, f := range fields {
					row[i] = primitive(lookup(v.(object), f))
				}
				e.line(childDepth, strings.Join(row, ","))
			}
			return
		}
		e.line(depth, header+":")
		for _, v := range value {
			e.entry(childDepth, "- ", "", v, childDepth+1)
		}
	default:
		if key == "" {
			e.line(depth, strings.TrimSuffix(prefix, " ")+" "+primitive(value))
			return
		}
		e.line(depth, prefix+key+": "+primitive(value))
	}
}

func isPrimitive(v any) bool {
	switch v.(type) {
	case object, []any:
		return false
	}
	return true
}

func allPrimitive(a []any) bool {
	for _, v := range a {
		if !isPrimitive(v) {
			return false
		}
	}
	return true
}

// tabular returns the fields of a, an array of objects that have the same
// fields, all primitive, in the first object's order; ok is false for any
// other array.
func tabular(a []any) (fields []string, ok bool) {
	first, ok := a[0].(object)
	if !ok || len(first) == 0 {
		return nil, false
	}
	for _, m := range first {
		fields = append(fields, m.key)
	}
	for _, v := range a {
		o, ok := v.(object)
		if !ok || len(o) != len(fields) {
			return nil, false
		}
		for _, m := range o {
			if !isPrimitive(m.value) || lookup(first, m.key) == missing {
				return nil, false
			}
		}
	}
	return fields, true
}

// missing is what lookup returns for a key an object lacks.
var missing = new(int)

func lookup(o object, key string) any {
	for _, m := range o {
		if m.key == key {
			return m.value
		}
	}
	return missing
}

// primitive encodes a string, number, boolean or null.
func primitive(v any) string {
	switch v := v.(type) {
	case nil:
		return "null"
	case bool:
		return strconv.FormatBool(v)
	case json.Number:
		return canonicalNumber(v)
	case string:
		if needsQuotes(v) {
			return quote(v)
		}
		return v
	}
	panic(fmt.Sprintf("toon: not a primitive: %T", v))
}

// canonicalNumber writes n in decimal without an exponent, and -0 as 0.
func canonicalNumber(n json.Number) string {
	if i, err := strconv.ParseInt(string(n), 10, 64); err == nil {
		return strconv.FormatInt(i, 10)
	}
	f, err := strconv.ParseFloat(string(n), 64)
	if err != nil || math.IsInf(f, 0) {
		return string(n)
	}
	if f == 0 {
		return "0"
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// needsQuotes reports whether s would read back as something other than
// itself unquoted: as a number, a boolean or null, as structure, or with
// its edges trimmed.
func needsQuotes(s string) bool {
	if s == "" || s != strings.TrimSpace(s) || s == "true" || s == "false" || s == "null" || strings.HasPrefix(s, "-") {
		return true
	}
	if _, err := strconv.ParseFloat(s, 64); err == nil {
		return true
	}
	return strings.ContainsAny(s, ":\"\\[]{},\n\r\t") || strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// encodeKey writes a key bare when it is an identifier, and quoted
// otherwise.
func encodeKey(k string) string {
	if k == "" {
		return quote(k)
	}
	for i, r := range k {
		letter := r == '_' || ('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z')
		if !letter && (i == 0 || !(r == '.' || ('0' <= r && r <= '9'))) {
			return quote(k)
		}
	}
	return k
}

// errSyntax wraps every error Unmarshal returns.
var errSyntax = errors.New("toon")
