// SPDX-License-Identifier: Apache-2.0

// Package toonjson encodes a Go value as TOON (Token-Oriented Object
// Notation) with exactly the data encoding/json would marshal it to, so that
// `--toon` and `--json` print the same data and only the encoding differs.
//
// The value is marshaled by encoding/json first, so struct tags, omitempty,
// json.RawMessage and MarshalJSON methods decide what is printed in both
// formats; the JSON is then read back in document order and written as TOON.
// Object keys keep the order encoding/json wrote them in.
package toonjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"

	"github.com/toon-format/toon-go"
)

// maxSafeInteger is the largest integer TOON's number model holds exactly:
// 2^53 - 1, as in JavaScript.
const maxSafeInteger = 1<<53 - 1

// Marshal returns v encoded as a TOON document, without a trailing newline.
//
// TOON numbers are IEEE 754 doubles, so an integer beyond ±(2^53 - 1), which
// JSON would print as a number, is printed as a quoted decimal string
// instead, losing nothing; an unrepresentable number would not.
func Marshal(v any) ([]byte, error) {
	encoded, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return FromJSON(encoded)
}

// FromJSON returns the JSON document as a TOON document, without a trailing
// newline.
func FromJSON(document []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(document))
	dec.UseNumber()
	value, err := read(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("toonjson: more than one JSON value")
	}
	return toon.Marshal(value)
}

// read reads one JSON value in document order.
func read(dec *json.Decoder) (any, error) {
	token, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch token := token.(type) {
	case json.Delim:
		switch token {
		case '{':
			object := toon.NewObject()
			for dec.More() {
				key, err := dec.Token()
				if err != nil {
					return nil, err
				}
				value, err := read(dec)
				if err != nil {
					return nil, err
				}
				object.Fields = append(object.Fields, toon.Field{Key: key.(string), Value: value})
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return object, nil
		case '[':
			array := []any{}
			for dec.More() {
				value, err := read(dec)
				if err != nil {
					return nil, err
				}
				array = append(array, value)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return array, nil
		}
		return nil, fmt.Errorf("toonjson: unexpected %q", token)
	case json.Number:
		return number(token), nil
	default:
		// A string, a bool or null.
		return token, nil
	}
}

// number is n as TOON holds it: a number, unless it is an integer a double
// cannot hold exactly.
func number(n json.Number) any {
	if i, err := strconv.ParseInt(string(n), 10, 64); err == nil && (i > maxSafeInteger || i < -maxSafeInteger) {
		return string(n)
	}
	if u, err := strconv.ParseUint(string(n), 10, 64); err == nil && u > maxSafeInteger {
		return string(n)
	}
	return n
}
