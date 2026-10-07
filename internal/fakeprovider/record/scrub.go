// SPDX-License-Identifier: Apache-2.0

package record

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Placeholders stand in for the personal data a recording would otherwise
// hold.
const (
	placeholderHome    = "/home/user"
	placeholderDir     = "/home/user/project"
	placeholderTilde   = "~/project"
	placeholderUser    = "user"
	placeholderHost    = "host"
	placeholderEmail   = "user@example.com"
	placeholderRedact  = "redacted"
	placeholderUUIDFmt = "00000000-0000-4000-8000-%012d"
)

// placeholderWords are the words the placeholders are made of.
var placeholderWords = map[string]bool{"home": true, "user": true, "project": true, "host": true, "example": true, "com": true, "redacted": true}

var (
	emailPattern           = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
	uuidPattern            = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	placeholderUUIDPattern = regexp.MustCompile(`^00000000-0000-4000-8000-[0-9]{12}$`)
	// ansiPattern matches the escape sequences a terminal draws with, so a
	// check can read the text they surround.
	ansiPattern = regexp.MustCompile(`\x1b(\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(\x07|\x1b\\)|P[^\x1b]*\x1b\\|[@-Z\\-_])`)
	// homePattern matches a home directory, as a path and as Claude Code
	// names a project directory after it, with the name of its owner.
	homePattern = regexp.MustCompile(`/(?:home|Users)/(\w+)|-(?:home|Users)-(\w+)`)
	// cursorPattern matches what moves the cursor or erases. A renderer
	// that redraws only the cells that changed writes it between the pieces
	// of a line it draws.
	cursorPattern = regexp.MustCompile("\x1b\\[[0-9;?]*[A-HJKSTXdf@P`]|[\r\n\b]")
	// secretName is an environment variable whose value is likely a
	// credential.
	secretName = regexp.MustCompile(`(?i)TOKEN|KEY|SECRET|PASSWORD|PASSWD|CREDENTIAL|AUTH|COOKIE`)
)

// credentials are the shapes of credential a recording must never hold. A
// recording holding one is refused rather than scrubbed, because a credential
// that was drawn or sent means the session itself went wrong.
var credentials = []struct {
	what    string
	pattern *regexp.Regexp
}{
	{"an Anthropic API key or OAuth token", regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{8,}`)},
	{"an API key", regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}`)},
	{"a GitHub token", regexp.MustCompile(`\b(gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})`)},
	{"a JSON Web Token", regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.`)},
	{"a bearer token", regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/-]{16,}`)},
	{"a private key", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{"an AWS access key", regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
	{"a Slack token", regexp.MustCompile(`xox[abprs]-[A-Za-z0-9-]{10,}`)},
}

// credentialFragments are the fragments of the start every Anthropic
// credential shares, which a redraw of only the cells that changed can draw
// apart from the rest of it.
var credentialFragments = newFragments(nil, []string{"sk-ant-"})

const credentialFragment = "a fragment of a credential around a cursor movement"

// A scrubber replaces the personal data of the host and session it records
// with placeholders, and finds what must not be written at all.
type scrubber struct {
	// replacements are applied in order, longest value first.
	replacements []replacement
	// secrets are the values of the environment's credential-like
	// variables: a recording holding one is refused.
	secrets []string
	uuids   map[string]string
	// fragments are the pieces of the personal data a redraw can draw.
	fragments fragments
}

type replacement struct {
	value       string
	pattern     *regexp.Regexp
	placeholder string
}

// newScrubber collects the personal data of this host and session: the
// user's name, home directory and host name, the working directory dir, and
// each of redact, the values the person recording names.
func newScrubber(dir string, redact []string) *scrubber {
	s := &scrubber{uuids: map[string]string{}}
	pairs := map[string]string{}
	add := func(value, placeholder string) {
		// A value that is a word of a placeholder, such as a full name
		// with "User" in it, would be found again in every placeholder.
		if len(value) >= 3 && !placeholderWords[strings.ToLower(value)] {
			pairs[value] = placeholder
		}
	}
	addPath := func(path, placeholder string) {
		add(path, placeholder)
		// Claude Code names a project's directory in ~/.claude/projects
		// after its path, with each "/" and "." made "-".
		add(dashed(path), dashed(placeholder))
	}

	home, _ := os.UserHomeDir()
	if home != "" && home != "/" {
		addPath(home, placeholderHome)
	}
	if dir != "" && dir != "/" {
		addPath(dir, placeholderDir)
		if home != "" {
			if rel, err := filepath.Rel(home, dir); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
				add("~/"+rel, placeholderTilde)
			}
		}
	}
	if u, err := user.Current(); err == nil {
		add(u.Username, placeholderUser)
		for _, name := range strings.Fields(u.Name) {
			add(name, placeholderRedact)
		}
	}
	for _, name := range []string{"USER", "LOGNAME"} {
		add(os.Getenv(name), placeholderUser)
	}
	if host, err := os.Hostname(); err == nil {
		add(host, placeholderHost)
		short, _, _ := strings.Cut(host, ".")
		add(short, placeholderHost)
	}
	for _, v := range redact {
		add(v, placeholderRedact)
	}

	values := make([]string, 0, len(pairs))
	for v := range pairs {
		values = append(values, v)
	}
	slices.SortFunc(values, func(a, b string) int {
		if len(a) != len(b) {
			return len(b) - len(a)
		}
		return strings.Compare(a, b)
	})
	for _, v := range values {
		s.replacements = append(s.replacements, replacement{value: v, pattern: valuePattern(v), placeholder: pairs[v]})
	}
	s.fragments = newFragments(values, nil)

	for _, kv := range os.Environ() {
		name, value, _ := strings.Cut(kv, "=")
		if secretName.MatchString(name) && len(value) >= 8 {
			s.secrets = append(s.secrets, value)
		}
	}
	return s
}

// valuePattern matches v case-insensitively, but not inside a longer word,
// so that a user named "al" does not scrub "also".
func valuePattern(v string) *regexp.Regexp {
	p := "(?i)" + regexp.QuoteMeta(v)
	if isWord(v[0]) {
		p = `(^|[^A-Za-z0-9_])` + p
	} else {
		p = "()" + p
	}
	if isWord(v[len(v)-1]) {
		p += `($|[^A-Za-z0-9_])`
	} else {
		p += "()"
	}
	return regexp.MustCompile(p)
}

func isWord(b byte) bool {
	return b == '_' || '0' <= b && b <= '9' || 'a' <= b && b <= 'z' || 'A' <= b && b <= 'Z'
}

func dashed(path string) string {
	return strings.NewReplacer("/", "-", ".", "-").Replace(path)
}

// scrub replaces the personal data in text with placeholders. Each UUID is
// replaced by a placeholder of its own, the same one everywhere it appears,
// so a session's identifier still ties its hook payloads together.
func (s *scrubber) scrub(text string) string {
	for _, r := range s.replacements {
		text = r.pattern.ReplaceAllString(text, "${1}"+strings.ReplaceAll(r.placeholder, "$", "$$")+"${2}")
	}
	text = emailPattern.ReplaceAllString(text, placeholderEmail)
	return uuidPattern.ReplaceAllStringFunc(text, func(id string) string {
		id = strings.ToLower(id)
		if p, ok := s.uuids[id]; ok {
			return p
		}
		p := fmt.Sprintf(placeholderUUIDFmt, len(s.uuids)+1)
		s.uuids[id] = p
		return p
	})
}

// scrubPayload scrubs a hook payload and writes it on one line.
func (s *scrubber) scrubPayload(payload []byte) json.RawMessage {
	payload = bytes.TrimSpace(payload)
	if !json.Valid(payload) {
		// Claude Code sends JSON; anything else is kept as a string.
		quoted, _ := json.Marshal(string(payload))
		payload = quoted
	}
	var compact bytes.Buffer
	json.Compact(&compact, payload)
	scrubbed := json.RawMessage(s.scrub(compact.String()))
	if !json.Valid(scrubbed) {
		// A placeholder never breaks JSON, but refuse to write it if it did.
		quoted, _ := json.Marshal(string(scrubbed))
		return quoted
	}
	return scrubbed
}

// problem names what text holds that a recording must not: a credential, or
// personal data the scrubber knows but could not replace. It looks at the
// text as written and as a terminal shows it, without escape sequences, so a
// value split by styling is still found. It returns "" when text is clean.
// It never returns the value it found.
func (s *scrubber) problem(text string) string {
	shown := ansiPattern.ReplaceAllString(text, "")
	for _, t := range []string{text, shown} {
		if what := genericProblem(t); what != "" {
			return what
		}
		for _, v := range s.secrets {
			if strings.Contains(t, v) {
				return "the value of a credential in the environment"
			}
		}
		for _, r := range s.replacements {
			if r.pattern.MatchString(t) {
				return "personal data from this host"
			}
		}
	}
	return ""
}

// fragment names what text, a screen as Claude Code drew it before
// scrubbing, holds in pieces around a cursor movement: a fragment of personal
// data the scrubber knows, or of a credential. Claude Code redraws only the
// cells that changed on its own screen, which shows the values themselves, so
// such a fragment is neither scrubbed nor found whole. It returns "" when
// text holds none, and never returns the fragment.
func (s *scrubber) fragment(text string) string {
	if s.fragments.in(text) {
		return "a fragment of personal data from this host around a cursor movement"
	}
	if credentialFragments.in(text) {
		return credentialFragment
	}
	return ""
}

// minFragment is the fewest characters a fragment has, so that ordinary
// words do not look like one.
const minFragment = 4

// fragments are the prefixes and suffixes of values that, drawn just before
// or just after a cursor movement, may be the rest of a value redrawn in part.
type fragments struct {
	prefixes, suffixes []string
	// whole are the values drawn whole, which are scrubbed rather than
	// refused.
	whole []string
}

// newFragments collects the fragments of each of personal, which are drawn
// whole and scrubbed, and of each of starts, which are refused even whole.
// Fragments of the placeholders, or of macOS's home directories, are common
// to everyone and left out.
func newFragments(personal, starts []string) fragments {
	common := strings.ToLower(strings.Join([]string{placeholderDir, dashed(placeholderDir), placeholderTilde, placeholderEmail, "/Users/", "-Users-"}, "\n"))
	var f fragments
	keep := func(list *[]string, fragment string) {
		if !strings.Contains(common, fragment) {
			*list = append(*list, fragment)
		}
	}
	add := func(v string, whole bool) {
		v = strings.ToLower(v)
		longest := len(v) - 1
		if whole {
			longest = len(v)
		}
		for n := minFragment; n <= longest; n++ {
			keep(&f.prefixes, v[:n])
			keep(&f.suffixes, v[len(v)-n:])
		}
	}
	for _, v := range personal {
		add(v, false)
		f.whole = append(f.whole, strings.ToLower(v))
	}
	for _, v := range starts {
		add(v, true)
	}
	return f
}

// in reports whether text holds one of the fragments just before or just
// after a cursor movement, other than as part of a value drawn whole.
func (f fragments) in(text string) bool {
	text = ansiPattern.ReplaceAllStringFunc(text, func(seq string) string {
		if cursorPattern.FindString(seq) == seq {
			return seq
		}
		return ""
	})
	pieces := cursorPattern.Split(text, -1)
	for i := 0; i+1 < len(pieces); i++ {
		before, after := strings.ToLower(pieces[i]), strings.ToLower(pieces[i+1])
		for _, p := range f.prefixes {
			start := len(before) - len(p)
			if strings.HasSuffix(before, p) && !(isWord(p[0]) && start > 0 && isWord(before[start-1])) && !f.drawnWhole(before, len(p), strings.HasSuffix) {
				return true
			}
		}
		for _, s := range f.suffixes {
			if strings.HasPrefix(after, s) && !(isWord(s[len(s)-1]) && len(after) > len(s) && isWord(after[len(s)])) && !f.drawnWhole(after, len(s), strings.HasPrefix) {
				return true
			}
		}
	}
	return false
}

// drawnWhole reports whether piece starts or ends, as at says, with a value
// drawn whole that is at least n characters long.
func (f fragments) drawnWhole(piece string, n int, at func(string, string) bool) bool {
	for _, w := range f.whole {
		if len(w) >= n && at(piece, w) {
			return true
		}
	}
	return false
}

// genericProblem finds what no recording may hold, whoever made it: a
// credential, an email address that is not the placeholder's, a UUID that is
// not a placeholder, or a home directory that is not the placeholder's.
func genericProblem(text string) string {
	for _, c := range credentials {
		if c.pattern.MatchString(text) {
			return c.what
		}
	}
	for _, email := range emailPattern.FindAllString(text, -1) {
		if email != placeholderEmail {
			return "an email address"
		}
	}
	for _, id := range uuidPattern.FindAllString(text, -1) {
		if !placeholderUUIDPattern.MatchString(id) {
			return "an identifier that is not a placeholder"
		}
	}
	for _, m := range homePattern.FindAllStringSubmatch(text, -1) {
		if m[1]+m[2] != placeholderUser {
			return "a home directory that is not the placeholder's"
		}
	}
	return ""
}
