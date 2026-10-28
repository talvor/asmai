// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// In the conversation the user's terminal is asked to tell Shift-Enter from
// Enter: by the kitty keyboard protocol's first enhancement, which
// disambiguates escape codes, and by xterm's modifyOtherKeys. A terminal
// that has neither ignores both, and sends Enter for Shift-Enter; Alt-Enter
// and backslash-Enter still start a new line there. The kitty keyboard
// protocol keeps a stack for each screen, so it is enabled on the alternate
// screen and restored before leaving it.
const (
	enhanceKeys = "\x1b[>1u\x1b[>4;1m"
	restoreKeys = "\x1b[<u\x1b[>4m"
)

// newline is what the agent's terminal is sent to start a new line in its
// prompt without submitting it: Alt-Enter, which Claude Code takes as a
// newline.
const newline = "\x1b\r"

// The brackets a terminal puts around pasted text.
const (
	pasteStart = "\x1b[200~"
	pasteEnd   = "\x1b[201~"
)

// keyReader turns the keys the user's terminal sends, which may be in the
// kitty keyboard protocol's or modifyOtherKeys' encoding, into the legacy
// bytes the agent's terminal sends its provider, as a terminal without
// either would. Shift-Enter, Alt-Enter and a backslash before Enter become
// newline. Pasted text passes as it is.
type keyReader struct {
	// carry holds the start of an escape sequence the next keys may finish.
	carry []byte
	// pasting is set inside a bracketed paste.
	pasting bool
	// backslash is set when the last key sent was a backslash typed outside
	// a paste.
	backslash bool
}

// maxSequence is the longest escape sequence keyReader waits to finish;
// anything longer passes as it is.
const maxSequence = 64

// escapeWait is how long the start of an escape sequence, a lone Esc
// included, is held for the keys that may finish it, before it is sent as
// it is.
const escapeWait = 100 * time.Millisecond

// holding reports whether the start of an escape sequence is held for the
// next keys.
func (k *keyReader) holding() bool {
	return len(k.carry) > 0
}

// flush returns what is held, as it is, once no key followed it in time.
func (k *keyReader) flush() []byte {
	held := k.carry
	k.carry = nil
	if held != nil {
		k.backslash = false
	}
	return held
}

// read returns the legacy keys of keys, the next the user's terminal sent.
func (k *keyReader) read(keys []byte) []byte {
	b := append(k.carry, keys...)
	k.carry = nil
	var out []byte
	for i := 0; i < len(b); {
		if b[i] == 0x1b && i+1 == len(b) {
			// A lone Esc, which the next keys may make part of a sequence
			// or of Alt-Enter.
			k.carry = []byte{0x1b}
			return out
		}
		if b[i] != 0x1b || b[i+1] != '[' {
			c := b[i]
			i++
			if k.pasting {
				out = append(out, c)
				continue
			}
			if c == '\r' && k.backslash {
				// The backslash is erased, as Claude Code does.
				out = append(out, 0x7f)
				out = append(out, newline...)
			} else {
				out = append(out, c)
			}
			k.backslash = c == '\\'
			continue
		}
		// A control sequence: ESC [, parameters and intermediates, then the
		// final byte.
		end := i + 2
		for end < len(b) && b[end] >= 0x20 && b[end] <= 0x3f {
			end++
		}
		if end >= len(b) {
			if end-i < maxSequence {
				k.carry = append([]byte(nil), b[i:]...)
				return out
			}
			out = append(out, b[i:]...)
			return out
		}
		seq := b[i : end+1]
		i = end + 1
		switch {
		case string(seq) == pasteStart:
			k.pasting, k.backslash = true, false
			out = append(out, seq...)
		case string(seq) == pasteEnd:
			k.pasting = false
			out = append(out, seq...)
		case k.pasting:
			out = append(out, seq...)
		default:
			legacy, ok := legacyKey(seq)
			if !ok {
				out = append(out, seq...)
				k.backslash = false
				continue
			}
			out = append(out, legacy...)
			k.backslash = legacy == "\\"
		}
	}
	return out
}

// The kitty keyboard protocol's modifier bits, less one.
const (
	modShift = 1 << iota
	modAlt
	modCtrl
	modSuper
	modHyper
	modMeta
	modCapsLock
	modNumLock
)

// legacyKey returns the legacy bytes of the key the control sequence seq
// encodes, kitty's CSI code;mods u or modifyOtherKeys' CSI 27;mods;code ~,
// or false when seq is neither or encodes a key with no legacy bytes.
func legacyKey(seq []byte) (string, bool) {
	s := string(seq[2:])
	var code, mods int
	switch {
	case strings.HasSuffix(s, "u"):
		params := strings.Split(strings.TrimSuffix(s, "u"), ";")
		if len(params) > 3 {
			return "", false
		}
		var ok bool
		if code, ok = number(params[0], 0); !ok {
			return "", false
		}
		mods = 1
		if len(params) > 1 {
			if mods, ok = number(params[1], 1); !ok {
				return "", false
			}
		}
	case strings.HasPrefix(s, "27;") && strings.HasSuffix(s, "~"):
		params := strings.Split(strings.TrimSuffix(s, "~"), ";")
		if len(params) != 3 {
			return "", false
		}
		var ok bool
		if mods, ok = number(params[1], 1); !ok {
			return "", false
		}
		if code, ok = number(params[2], 0); !ok {
			return "", false
		}
	default:
		return "", false
	}
	if mods < 1 {
		return "", false
	}
	m := (mods - 1) &^ (modCapsLock | modNumLock)
	if key, ok := keypadKeys[code]; ok {
		return keypadKey(key, m)
	}
	if m&(modSuper|modHyper|modMeta) != 0 {
		return "", false
	}
	shift, alt, ctrl := m&modShift != 0, m&modAlt != 0, m&modCtrl != 0
	var key string
	switch {
	case code == '\r':
		if shift || alt {
			return newline, true
		}
		key = "\r"
	case code == 0x1b:
		key = "\x1b"
	case code == '\t':
		key = "\t"
		if shift {
			key = "\x1b[Z"
		}
	case code == 0x7f || code == 0x08:
		key = "\x7f"
		if ctrl {
			key = "\x08"
		}
	case ctrl && code == ' ':
		key = "\x00"
	case ctrl && code == '?':
		key = "\x7f"
	case ctrl && code >= 'a' && code <= 'z':
		key = string(rune(code - 'a' + 1))
	case ctrl && code >= '@' && code <= '_':
		key = string(rune(code - '@'))
	case ctrl:
		return "", false
	case code >= ' ' && code < 0xe000 && utf8.ValidRune(rune(code)):
		r := rune(code)
		if shift && r >= 'a' && r <= 'z' {
			r -= 'a' - 'A'
		}
		key = string(r)
	default:
		return "", false
	}
	if alt {
		key = "\x1b" + key
	}
	return key, true
}

// keypadKeys are the legacy keys of the keypad's keys, by their codes in the
// kitty keyboard protocol, which reports them all in CSI u: a key's text, or
// the final byte, and the number before it if any, of its control sequence.
var keypadKeys = map[int]string{
	57399: "0", 57400: "1", 57401: "2", 57402: "3", 57403: "4",
	57404: "5", 57405: "6", 57406: "7", 57407: "8", 57408: "9",
	57409: ".", 57410: "/", 57411: "*", 57412: "-", 57413: "+",
	57414: "\r", 57415: "=", 57416: ",",
	57417: "D", 57418: "C", 57419: "A", 57420: "B",
	57421: "5~", 57422: "6~", 57423: "H", 57424: "F",
	57425: "2~", 57426: "3~", 57427: "E",
}

// keypadKey returns the legacy bytes of the keypad's key, one of
// keypadKeys, with the modifiers m. Its text is sent as typed, a modifier
// aside, except that Shift- and Alt-Enter start a new line as Enter's do;
// its other keys are the control sequences of the keys they stand for, with
// the modifiers in them as xterm sends them.
func keypadKey(key string, m int) (string, bool) {
	switch {
	case key == "\r":
		if m&(modShift|modAlt) != 0 {
			return newline, true
		}
		return key, true
	case len(key) == 1 && (key[0] < 'A' || key[0] > 'Z'):
		return key, true
	}
	final := key[len(key)-1:]
	param := strings.TrimSuffix(key, final)
	if mods := m & (modShift | modAlt | modCtrl); mods != 0 {
		if param == "" {
			param = "1"
		}
		return "\x1b[" + param + ";" + strconv.Itoa(mods+1) + final, true
	}
	return "\x1b[" + param + final, true
}

// number reads a parameter's first field, which is empty for def.
func number(param string, def int) (int, bool) {
	field, _, _ := strings.Cut(param, ":")
	if field == "" {
		return def, def > 0
	}
	n, err := strconv.Atoi(field)
	return n, err == nil && n >= 0
}
