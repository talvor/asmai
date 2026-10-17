// SPDX-License-Identifier: Apache-2.0

// Package toon encodes values as TOON, Token-Oriented Object Notation
// (https://github.com/toon-format/spec): the JSON data model in fewer tokens,
// with uniform arrays of objects as tables. Marshal encodes a value from the
// JSON encoding/json gives it, so that its fields, names and values are
// exactly those --json prints; only the notation differs. It writes
// two-space indentation, comma delimiters and no key folding, and keeps the
// order of the fields as encoding/json writes them, so the same value always
// prints the same bytes. Decode reads that notation back, for tests.
package toon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// object is a JSON object with its fields in the order they were written.
type object struct {
	keys   []string
	values map[string]any
}

// Marshal returns the TOON encoding of v, which is the TOON form of what
// json.Marshal(v) returns. It fails where json.Marshal does.
func Marshal(v any) ([]byte, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	tree, err := ordered(dec)
	if err != nil {
		return nil, err
	}
	var e encoder
	switch t := tree.(type) {
	case *object:
		e.fields(t, 0)
	case []any:
		e.array("", 0, t)
	default:
		e.line(0, primitive(t))
	}
	return e.b.Bytes(), nil
}

// ordered reads the next JSON value from dec, keeping each object's fields in
// order.
func ordered(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch tok {
	case json.Delim('{'):
		o := &object{values: map[string]any{}}
		for dec.More() {
			key, err := dec.Token()
			if err != nil {
				return nil, err
			}
			v, err := ordered(dec)
			if err != nil {
				return nil, err
			}
			k := key.(string)
			if _, seen := o.values[k]; !seen {
				o.keys = append(o.keys, k)
			}
			o.values[k] = v
		}
		_, err = dec.Token()
		return o, err
	case json.Delim('['):
		list := []any{}
		for dec.More() {
			v, err := ordered(dec)
			if err != nil {
				return nil, err
			}
			list = append(list, v)
		}
		_, err = dec.Token()
		return list, err
	}
	return tok, nil
}

type encoder struct {
	b bytes.Buffer
}

func (e *encoder) line(depth int, s string) {
	e.b.WriteString(strings.Repeat("  ", depth))
	e.b.WriteString(s)
	e.b.WriteByte('\n')
}

// fields writes o's fields at depth.
func (e *encoder) fields(o *object, depth int) {
	for _, k := range o.keys {
		e.field(strings.Repeat("  ", depth), depth, k, o.values[k])
	}
}

// field writes key and its value v, its first line starting with lead and
// what is nested under it at depth+1.
func (e *encoder) field(lead string, depth int, key string, v any) {
	head := lead + encodeKey(key)
	switch t := v.(type) {
	case *object:
		e.line(0, head+":")
		e.fields(t, depth+1)
	case []any:
		e.array(head, depth, t)
	default:
		e.line(0, head+": "+primitive(t))
	}
}

// array writes list after head, which is its key or a list item's hyphen:
// inline when every item is a primitive, as a table when every item is an
// object with the same primitive fields, and otherwise as a list at depth+1.
func (e *encoder) array(head string, depth int, list []any) {
	n := fmt.Sprintf("[%d]", len(list))
	if len(list) == 0 {
		e.line(0, head+n+":")
		return
	}
	if cells, ok := primitives(list); ok {
		e.line(0, head+n+": "+strings.Join(cells, ","))
		return
	}
	if fields, ok := tabular(list); ok {
		names := make([]string, len(fields))
		for i, f := range fields {
			names[i] = encodeKey(f)
		}
		e.line(0, head+n+"{"+strings.Join(names, ",")+"}:")
		for _, item := range list {
			o := item.(*object)
			cells := make([]string, len(fields))
			for i, f := range fields {
				cells[i] = primitive(o.values[f])
			}
			e.line(depth+1, strings.Join(cells, ","))
		}
		return
	}
	e.line(0, head+n+":")
	for _, item := range list {
		e.item(depth+1, item)
	}
}

// item writes one item of a list at depth. An object's first field goes on
// the hyphen's line, and its other fields under it.
func (e *encoder) item(depth int, v any) {
	hyphen := strings.Repeat("  ", depth) + "- "
	switch t := v.(type) {
	case *object:
		if len(t.keys) == 0 {
			e.line(depth, "-")
			return
		}
		e.field(hyphen, depth+1, t.keys[0], t.values[t.keys[0]])
		for _, k := range t.keys[1:] {
			e.field(strings.Repeat("  ", depth+1), depth+1, k, t.values[k])
		}
	case []any:
		e.array(hyphen, depth, t)
	default:
		e.line(0, hyphen+primitive(t))
	}
}

// primitives returns list's items encoded, if every one is a primitive.
func primitives(list []any) ([]string, bool) {
	cells := make([]string, len(list))
	for i, v := range list {
		switch v.(type) {
		case *object, []any:
			return nil, false
		}
		cells[i] = primitive(v)
	}
	return cells, true
}

// tabular returns the fields of list's objects, in the first one's order, if
// every item is an object with the same fields and only primitive values.
func tabular(list []any) ([]string, bool) {
	first, ok := list[0].(*object)
	if !ok || len(first.keys) == 0 {
		return nil, false
	}
	for _, item := range list {
		o, ok := item.(*object)
		if !ok || len(o.keys) != len(first.keys) {
			return nil, false
		}
		for _, k := range first.keys {
			v, ok := o.values[k]
			if !ok {
				return nil, false
			}
			switch v.(type) {
			case *object, []any:
				return nil, false
			}
		}
	}
	return first.keys, true
}

var (
	plainKey    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.]*$`)
	numberLike  = regexp.MustCompile(`(?i)^-?\d+(?:\.\d+)?(?:e[+-]?\d+)?$`)
	leadingZero = regexp.MustCompile(`^-?0\d`)
)

func encodeKey(k string) string {
	if plainKey.MatchString(k) {
		return k
	}
	return quote(k)
}

// primitive encodes a JSON primitive: a string, a json.Number, a bool or nil.
func primitive(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case bool:
		return strconv.FormatBool(t)
	case json.Number:
		return canonical(t)
	case string:
		if needsQuotes(t) {
			return quote(t)
		}
		return t
	}
	panic(fmt.Sprintf("toon: %T is not a JSON primitive", v))
}

// canonical writes a number in decimal, without an exponent or trailing
// zeros, and -0 as 0.
func canonical(n json.Number) string {
	s := n.String()
	if !strings.ContainsAny(s, ".eE") {
		if s == "-0" {
			return "0"
		}
		return s
	}
	f, err := n.Float64()
	if err != nil {
		return s
	}
	if f == 0 {
		return "0"
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// needsQuotes reports whether s, unquoted, would read as something other
// than the string s.
func needsQuotes(s string) bool {
	if s == "" || s != strings.TrimSpace(s) || s == "true" || s == "false" || s == "null" {
		return true
	}
	if numberLike.MatchString(s) || leadingZero.MatchString(s) || strings.HasPrefix(s, "-") {
		return true
	}
	if strings.ContainsAny(s, ":\"\\[]{},") {
		return true
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func quote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`, "\t", `\t`)
	return `"` + r.Replace(s) + `"`
}

// Decode reads a TOON document as Marshal writes it into the values
// json.Unmarshal gives for the same data: map[string]any, []any, string,
// float64, bool and nil.
func Decode(data []byte) (any, error) {
	text := strings.TrimSuffix(string(data), "\n")
	p := &parser{}
	if text != "" {
		for _, l := range strings.Split(text, "\n") {
			content := strings.TrimLeft(l, " ")
			indent := len(l) - len(content)
			if indent%2 != 0 {
				return nil, fmt.Errorf("toon: line %d is indented by %d spaces, not a multiple of 2", len(p.lines)+1, indent)
			}
			p.lines = append(p.lines, line{depth: indent / 2, text: content})
		}
	}
	if len(p.lines) == 0 {
		return map[string]any{}, nil
	}
	first := p.lines[0].text
	var v any
	var err error
	switch {
	case strings.HasPrefix(first, "["):
		p.next++
		v, err = p.arrayAfter(first, 0)
	case len(p.lines) == 1 && !isField(first):
		p.next++
		v, err = parsePrimitive(first)
	default:
		v, err = p.object(0)
	}
	if err != nil {
		return nil, err
	}
	if p.next != len(p.lines) {
		return nil, fmt.Errorf("toon: line %d is not part of the document", p.next+1)
	}
	return v, nil
}

type line struct {
	depth int
	text  string
}

type parser struct {
	lines []line
	next  int
}

// at reports whether the next line is at depth.
func (p *parser) at(depth int) bool {
	return p.next < len(p.lines) && p.lines[p.next].depth == depth
}

// object reads the fields at depth.
func (p *parser) object(depth int) (map[string]any, error) {
	o := map[string]any{}
	for p.at(depth) && !strings.HasPrefix(p.lines[p.next].text, "-") {
		text := p.lines[p.next].text
		p.next++
		if err := p.field(o, text, depth); err != nil {
			return nil, err
		}
	}
	if p.next < len(p.lines) && p.lines[p.next].depth > depth {
		return nil, fmt.Errorf("toon: line %d is indented more than its field", p.next+1)
	}
	return o, nil
}

// field reads the field written in text, whose nested lines are at depth+1,
// into o.
func (p *parser) field(o map[string]any, text string, depth int) error {
	key, rest, err := splitKey(text)
	if err != nil {
		return fmt.Errorf("toon: line %d: %w", p.next, err)
	}
	var v any
	switch {
	case strings.HasPrefix(rest, "["):
		v, err = p.arrayAfter(rest, depth)
	case rest == ":":
		if p.at(depth + 1) {
			v, err = p.object(depth + 1)
		} else {
			v = map[string]any{}
		}
	case strings.HasPrefix(rest, ": "):
		v, err = parsePrimitive(rest[2:])
	default:
		err = fmt.Errorf("toon: line %d: want a colon after the key %q", p.next, key)
	}
	if err != nil {
		return err
	}
	o[key] = v
	return nil
}

// arrayAfter reads the array whose header begins header, with its rows or
// items at depth+1.
func (p *parser) arrayAfter(header string, depth int) ([]any, error) {
	end := strings.Index(header, "]")
	if end < 0 {
		return nil, fmt.Errorf("toon: line %d: an array header without ]", p.next)
	}
	n, err := strconv.Atoi(header[1:end])
	if err != nil || n < 0 {
		return nil, fmt.Errorf("toon: line %d: %q is not an array length", p.next, header[1:end])
	}
	rest := header[end+1:]
	var fields []string
	if strings.HasPrefix(rest, "{") {
		close := strings.Index(rest, "}")
		if close < 0 {
			return nil, fmt.Errorf("toon: line %d: a table header without }", p.next)
		}
		for _, cell := range splitCells(rest[1:close]) {
			name, err := parseKeyText(cell)
			if err != nil {
				return nil, err
			}
			fields = append(fields, name)
		}
		rest = rest[close+1:]
	}
	list := []any{}
	switch {
	case fields != nil && rest == ":":
		for range n {
			if !p.at(depth + 1) {
				return nil, fmt.Errorf("toon: a table of %d rows has fewer", n)
			}
			cells := splitCells(p.lines[p.next].text)
			p.next++
			if len(cells) != len(fields) {
				return nil, fmt.Errorf("toon: line %d has %d cells for %d fields", p.next, len(cells), len(fields))
			}
			row := map[string]any{}
			for i, c := range cells {
				if row[fields[i]], err = parsePrimitive(c); err != nil {
					return nil, err
				}
			}
			list = append(list, row)
		}
	case rest == ":" && n == 0:
	case rest == ":":
		for range n {
			if !p.at(depth+1) || !strings.HasPrefix(p.lines[p.next].text, "-") {
				return nil, fmt.Errorf("toon: a list of %d items has fewer", n)
			}
			item, err := p.item(depth + 1)
			if err != nil {
				return nil, err
			}
			list = append(list, item)
		}
	case strings.HasPrefix(rest, ": "):
		for _, c := range splitCells(rest[2:]) {
			v, err := parsePrimitive(c)
			if err != nil {
				return nil, err
			}
			list = append(list, v)
		}
		if len(list) != n {
			return nil, fmt.Errorf("toon: line %d has %d values for an array of %d", p.next, len(list), n)
		}
	default:
		return nil, fmt.Errorf("toon: line %d: want a colon after the array header", p.next)
	}
	return list, nil
}

// item reads the list item on the next line, at depth.
func (p *parser) item(depth int) (any, error) {
	text := p.lines[p.next].text
	p.next++
	if text == "-" {
		return map[string]any{}, nil
	}
	if !strings.HasPrefix(text, "- ") {
		return nil, fmt.Errorf("toon: line %d: want a space after the hyphen", p.next)
	}
	text = text[2:]
	if strings.HasPrefix(text, "[") {
		return p.arrayAfter(text, depth)
	}
	if !isField(text) {
		return parsePrimitive(text)
	}
	o := map[string]any{}
	// The first field's nested lines are at depth+2, its other fields at
	// depth+1.
	if err := p.field(o, text, depth+1); err != nil {
		return nil, err
	}
	rest, err := p.object(depth + 1)
	if err != nil {
		return nil, err
	}
	for k, v := range rest {
		o[k] = v
	}
	return o, nil
}

// isField reports whether text is a key and what follows it, rather than a
// primitive.
func isField(text string) bool {
	_, rest, err := splitKey(text)
	return err == nil && (strings.HasPrefix(rest, ":") || strings.HasPrefix(rest, "["))
}

// splitKey splits text into its key and what follows it.
func splitKey(text string) (key, rest string, err error) {
	if strings.HasPrefix(text, `"`) {
		end := closingQuote(text)
		if end < 0 {
			return "", "", errors.New("an unterminated quoted key")
		}
		key, err = unquote(text[:end+1])
		return key, text[end+1:], err
	}
	i := strings.IndexAny(text, ":[")
	if i <= 0 {
		return "", "", fmt.Errorf("%q has no key", text)
	}
	return text[:i], text[i:], nil
}

func parseKeyText(s string) (string, error) {
	if strings.HasPrefix(s, `"`) {
		return unquote(s)
	}
	return s, nil
}

// closingQuote returns the index of the quote that closes the string s
// begins, or -1.
func closingQuote(s string) int {
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			return i
		}
	}
	return -1
}

// splitCells splits s at the commas outside quotes.
func splitCells(s string) []string {
	var cells []string
	start, quoted := 0, false
	for i := 0; i < len(s); i++ {
		switch {
		case quoted && s[i] == '\\':
			i++
		case s[i] == '"':
			quoted = !quoted
		case !quoted && s[i] == ',':
			cells = append(cells, s[start:i])
			start = i + 1
		}
	}
	return append(cells, s[start:])
}

var jsonNumber = regexp.MustCompile(`^-?(?:0|[1-9]\d*)(?:\.\d+)?(?:[eE][+-]?\d+)?$`)

func parsePrimitive(s string) (any, error) {
	switch {
	case strings.HasPrefix(s, `"`):
		if closingQuote(s) != len(s)-1 {
			return nil, fmt.Errorf("toon: %q is not one quoted string", s)
		}
		return unquote(s)
	case s == "null":
		return nil, nil
	case s == "true":
		return true, nil
	case s == "false":
		return false, nil
	case jsonNumber.MatchString(s):
		return strconv.ParseFloat(s, 64)
	}
	return s, nil
}

func unquote(s string) (string, error) {
	var b strings.Builder
	for i := 1; i < len(s)-1; i++ {
		if s[i] != '\\' {
			b.WriteByte(s[i])
			continue
		}
		i++
		if i >= len(s)-1 {
			return "", fmt.Errorf("toon: %q ends in a backslash", s)
		}
		switch s[i] {
		case '\\', '"':
			b.WriteByte(s[i])
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		default:
			return "", fmt.Errorf("toon: %q has the escape \\%c, which TOON does not have", s, s[i])
		}
	}
	return b.String(), nil
}
