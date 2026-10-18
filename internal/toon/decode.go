// SPDX-License-Identifier: Apache-2.0

package toon

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Unmarshal reads a TOON document, as Marshal writes it, into the values
// encoding/json's Unmarshal gives an any: map[string]any, []any, string,
// float64, bool and nil. It is strict: an array whose length is not the
// one declared, a duplicate key or indentation that is not a whole number
// of levels is an error.
func Unmarshal(data []byte) (any, error) {
	text := strings.TrimSuffix(string(data), "\n")
	p := &parser{}
	if text != "" {
		for i, l := range strings.Split(text, "\n") {
			trimmed := strings.TrimLeft(l, " ")
			spaces := len(l) - len(trimmed)
			if spaces%len(indent) != 0 || trimmed == "" || strings.HasPrefix(trimmed, "\t") {
				return nil, p.errorf(i, "indentation is not a whole number of levels, or the line is blank")
			}
			p.lines = append(p.lines, line{depth: spaces / len(indent), text: trimmed, number: i + 1})
		}
	}
	v, err := p.document()
	if err != nil {
		return nil, err
	}
	if p.pos < len(p.lines) {
		return nil, p.errorf(p.pos, "unexpected line")
	}
	return v, nil
}

type line struct {
	depth  int
	text   string
	number int
}

type parser struct {
	lines []line
	pos   int
}

func (p *parser) errorf(i int, format string, args ...any) error {
	n := i + 1
	if i < len(p.lines) {
		n = p.lines[i].number
	}
	return fmt.Errorf("%w: line %d: %s", errSyntax, n, fmt.Sprintf(format, args...))
}

func (p *parser) document() (any, error) {
	if len(p.lines) == 0 {
		return map[string]any{}, nil
	}
	first := p.lines[0]
	if first.depth != 0 {
		return nil, p.errorf(0, "the document is indented")
	}
	if strings.HasPrefix(first.text, "[") {
		p.pos++
		return p.array(first.text, 1)
	}
	if _, _, ok := splitKey(first.text); !ok && len(p.lines) == 1 {
		p.pos++
		return parsePrimitive(first.text)
	}
	return p.object(0)
}

// object reads the fields at depth, up to the first line that is less deep.
func (p *parser) object(depth int) (map[string]any, error) {
	o := map[string]any{}
	for p.pos < len(p.lines) {
		l := p.lines[p.pos]
		if l.depth < depth {
			break
		}
		if l.depth > depth {
			return nil, p.errorf(p.pos, "unexpected indentation")
		}
		if err := p.field(o, l.text, depth+1); err != nil {
			return nil, err
		}
	}
	return o, nil
}

// field reads the field whose line text is the current line's, or the rest
// of a list item's, into o; what it nests is at childDepth.
func (p *parser) field(o map[string]any, text string, childDepth int) error {
	at := p.pos
	key, rest, ok := splitKey(text)
	if !ok {
		return p.errorf(at, "want a key")
	}
	if _, dup := o[key]; dup {
		return p.errorf(at, "duplicate key %q", key)
	}
	p.pos++
	var v any
	var err error
	if strings.HasPrefix(rest, "[") {
		v, err = p.array(rest, childDepth)
	} else {
		v, err = p.value(rest, childDepth)
	}
	if err != nil {
		return err
	}
	o[key] = v
	return nil
}

// value reads what follows a key: ": <primitive>", or ":" and a nested
// object at childDepth, which may be empty.
func (p *parser) value(rest string, childDepth int) (any, error) {
	if rest == ":" {
		if p.pos < len(p.lines) && p.lines[p.pos].depth == childDepth {
			return p.object(childDepth)
		}
		return map[string]any{}, nil
	}
	if !strings.HasPrefix(rest, ": ") {
		return nil, p.errorf(p.pos-1, "want \": \" after the key")
	}
	return parsePrimitive(rest[2:])
}

// array reads an array from its header, "[N]: v,...", "[N]{f,...}:" with N
// rows at childDepth, or "[N]:" with N list items at childDepth.
func (p *parser) array(header string, childDepth int) ([]any, error) {
	at := p.pos - 1
	end := strings.IndexByte(header, ']')
	if end < 0 {
		return nil, p.errorf(at, "an array header without ]")
	}
	n, err := strconv.Atoi(header[1:end])
	if err != nil || n < 0 {
		return nil, p.errorf(at, "an array length that is not a number")
	}
	rest := header[end+1:]
	var fields []string
	if strings.HasPrefix(rest, "{") {
		close := strings.LastIndex(rest, "}")
		if close < 0 {
			return nil, p.errorf(at, "a field list without }")
		}
		for _, cell := range splitCells(rest[1:close]) {
			key, tail, ok := splitKey(cell + ":")
			if !ok || tail != ":" {
				return nil, p.errorf(at, "a field name that is not a key")
			}
			fields = append(fields, key)
		}
		rest = rest[close+1:]
	}
	if !strings.HasPrefix(rest, ":") {
		return nil, p.errorf(at, "want : after the array header")
	}
	inline := rest[1:]
	a := []any{}
	switch {
	case fields != nil:
		if inline != "" {
			return nil, p.errorf(at, "a tabular array with values on its header")
		}
		for range n {
			if p.pos >= len(p.lines) || p.lines[p.pos].depth != childDepth {
				return nil, p.errorf(at, "fewer rows than the %d declared", n)
			}
			cells := splitCells(p.lines[p.pos].text)
			if len(cells) != len(fields) {
				return nil, p.errorf(p.pos, "%d values for %d fields", len(cells), len(fields))
			}
			row := map[string]any{}
			for i, c := range cells {
				v, err := parsePrimitive(c)
				if err != nil {
					return nil, p.errorf(p.pos, "%v", err)
				}
				row[fields[i]] = v
			}
			a = append(a, row)
			p.pos++
		}
	case inline != "":
		if !strings.HasPrefix(inline, " ") {
			return nil, p.errorf(at, "want a space after the array header")
		}
		for _, c := range splitCells(inline[1:]) {
			v, err := parsePrimitive(c)
			if err != nil {
				return nil, p.errorf(at, "%v", err)
			}
			a = append(a, v)
		}
		if len(a) != n {
			return nil, p.errorf(at, "%d values, not the %d declared", len(a), n)
		}
	default:
		for range n {
			if p.pos >= len(p.lines) || p.lines[p.pos].depth != childDepth {
				return nil, p.errorf(at, "fewer items than the %d declared", n)
			}
			v, err := p.item(childDepth)
			if err != nil {
				return nil, err
			}
			a = append(a, v)
		}
	}
	if p.pos < len(p.lines) && p.lines[p.pos].depth >= childDepth {
		return nil, p.errorf(p.pos, "more than the %d declared", n)
	}
	return a, nil
}

// item reads the list item on the current line, at depth.
func (p *parser) item(depth int) (any, error) {
	text := p.lines[p.pos].text
	if text == "-" {
		p.pos++
		return map[string]any{}, nil
	}
	if !strings.HasPrefix(text, "- ") {
		return nil, p.errorf(p.pos, "want a list item")
	}
	content := text[2:]
	if strings.HasPrefix(content, "[") {
		p.pos++
		return p.array(content, depth+1)
	}
	if _, _, ok := splitKey(content); !ok {
		p.pos++
		return parsePrimitive(content)
	}
	// An object: its first field is on the hyphen's line, with its children
	// a level deeper than the fields after it.
	o := map[string]any{}
	if err := p.field(o, content, depth+2); err != nil {
		return nil, err
	}
	rest, err := p.object(depth + 1)
	if err != nil {
		return nil, err
	}
	for k, v := range rest {
		if _, dup := o[k]; dup {
			return nil, p.errorf(p.pos-1, "duplicate key %q", k)
		}
		o[k] = v
	}
	return o, nil
}

// splitKey splits a field's text into its key and the rest, which starts
// with ":" or "["; ok is false when the text is not a field.
func splitKey(text string) (key, rest string, ok bool) {
	if strings.HasPrefix(text, `"`) {
		s, n, err := unquote(text)
		if err != nil || n == len(text) || (text[n] != ':' && text[n] != '[') {
			return "", "", false
		}
		return s, text[n:], true
	}
	i := strings.IndexAny(text, ":[")
	if i <= 0 || encodeKey(text[:i]) != text[:i] {
		return "", "", false
	}
	return text[:i], text[i:], true
}

// splitCells splits s at the commas outside quotes.
func splitCells(s string) []string {
	var cells []string
	start, quoted := 0, false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			if quoted {
				i++
			}
		case '"':
			quoted = !quoted
		case ',':
			if !quoted {
				cells = append(cells, s[start:i])
				start = i + 1
			}
		}
	}
	return append(cells, s[start:])
}

var number = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$`)

func parsePrimitive(s string) (any, error) {
	if strings.HasPrefix(s, `"`) {
		v, n, err := unquote(s)
		if err != nil {
			return nil, err
		}
		if n != len(s) {
			return nil, fmt.Errorf("text after a quoted string: %q", s)
		}
		return v, nil
	}
	switch s {
	case "true":
		return true, nil
	case "false":
		return false, nil
	case "null":
		return nil, nil
	}
	if number.MatchString(s) {
		return strconv.ParseFloat(s, 64)
	}
	return s, nil
}

// unquote reads the quoted string s starts with, and returns it and how
// many bytes of s it took.
func unquote(s string) (string, int, error) {
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		switch c := s[i]; c {
		case '"':
			return b.String(), i + 1, nil
		case '\\':
			if i+1 == len(s) {
				return "", 0, fmt.Errorf("%w: an unterminated string", errSyntax)
			}
			i++
			switch s[i] {
			case '"', '\\':
				b.WriteByte(s[i])
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			default:
				return "", 0, fmt.Errorf("%w: an unknown escape \\%c", errSyntax, s[i])
			}
		default:
			b.WriteByte(c)
		}
	}
	return "", 0, fmt.Errorf("%w: an unterminated string", errSyntax)
}
