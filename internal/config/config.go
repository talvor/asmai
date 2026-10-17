// SPDX-License-Identifier: Apache-2.0

// Package config reads the factory's configuration file,
// ~/.config/asmai/config.toml, which the user edits by hand, and edits the
// entries `asmai repo add` and `remove` own. It reads every field of the
// file's schema except the ones that name Codex, which arrives later:
// [limits], [logs], [providers.claude], [roles.<role>] and
// [repositories.<name>]. A file it cannot honour is refused with the line and
// the fix.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	toml "github.com/pelletier/go-toml/v2"
	"github.com/pelletier/go-toml/v2/unstable"

	"github.com/talvor/asmai/internal/roles"
)

// Claude is the one provider this asmai runs; Codex arrives later.
const Claude = "claude"

// Config is the configuration file's content.
type Config struct {
	// Roles holds each role's staffing, by role name, for the roles the file
	// has a table for.
	Roles map[string]Staffing
	// Repositories holds each registered repository's entry, by name.
	Repositories map[string]Repository
	// Skills holds each role's skill lists, by role name, for the roles the
	// file has a table for.
	Skills map[string]Skills
	// Options holds each registered repository's settings other than the
	// ones `asmai repo add` records, by name.
	Options map[string]RepositoryOptions
	// Limits are the factory-wide operating limits, with the defaults of
	// the ones the file does not set.
	Limits Limits
	// Logs are the log settings, with their defaults.
	Logs Logs
	// Providers holds each provider's settings, by name; only Claude, which
	// is always there.
	Providers map[string]Provider
}

// Limits is [limits]: the factory-wide operating limits.
type Limits struct {
	LeaderIdleGrace       time.Duration
	LeaderRestarts        int
	LeaderRestartWindow   time.Duration
	SilenceTimeout        time.Duration
	DrainTimeout          time.Duration
	SideEffectFreeRetries int
	ProviderErrorResumes  int
	ProviderErrorWait     time.Duration
	ProviderErrorWaitMax  time.Duration
	RecheckInterval       time.Duration
	DispatchLimit         int
	FailedValidations     int
	FirstCheckWait        time.Duration
	// FreeSpaceFloor is in bytes.
	FreeSpaceFloor int64
}

// DefaultLimits are the limits of a file that sets none.
func DefaultLimits() Limits {
	return Limits{
		LeaderIdleGrace:       10 * time.Minute,
		LeaderRestarts:        3,
		LeaderRestartWindow:   15 * time.Minute,
		SilenceTimeout:        30 * time.Minute,
		DrainTimeout:          10 * time.Minute,
		SideEffectFreeRetries: 2,
		ProviderErrorResumes:  5,
		ProviderErrorWait:     30 * time.Second,
		ProviderErrorWaitMax:  8 * time.Minute,
		RecheckInterval:       5 * time.Minute,
		DispatchLimit:         5,
		FailedValidations:     3,
		FirstCheckWait:        15 * time.Minute,
		FreeSpaceFloor:        5 * gigabyte,
	}
}

// Logs is [logs].
type Logs struct {
	DaemonFiles        int
	DaemonFileSize     int64 // bytes
	RecordingRetention time.Duration
}

// DefaultLogs are the log settings of a file that sets none.
func DefaultLogs() Logs {
	return Logs{DaemonFiles: 5, DaemonFileSize: 20 * megabyte, RecordingRetention: 14 * 24 * time.Hour}
}

// Provider is [providers.<provider>].
type Provider struct {
	// WorkerCap is the most workers with a dispatch in progress on the
	// provider.
	WorkerCap int
	// DefaultModel is the model used when work falls back to the provider;
	// empty when the file sets none.
	DefaultModel string
}

// Skills is a role's skill lists, in [roles.<role>]. A nil list is not in the
// file, and means the role's default list.
type Skills struct {
	LeaderSkills []string
	WorkerSkills []string
	// LeaderAdded and WorkerAdded are directories of added skills.
	LeaderAdded []string
	WorkerAdded []string
}

// RepositoryOptions is a repository's settings in [repositories.<name>]
// other than its location, origin and default branch.
type RepositoryOptions struct {
	// Notes are the user's notes, which count as repository instructions.
	Notes string `json:"notes"`
	// ChecksOneAtATime says the repository's checks cannot run side by side.
	ChecksOneAtATime bool `json:"checks_one_at_a_time"`
	// NoCI says the repository has no CI.
	NoCI bool `json:"no_ci"`
	// CommitTrailers says commits carry the AsmAI trailers; true unless the
	// file says otherwise.
	CommitTrailers bool `json:"commit_trailers"`
	// RepositorySkills are the roles for which the repository's own skills
	// are switched on in its jobs.
	RepositorySkills []string `json:"repository_skills"`
}

// DefaultRepositoryOptions are the settings of a repository whose entry
// sets none.
func DefaultRepositoryOptions() RepositoryOptions {
	return RepositoryOptions{CommitTrailers: true}
}

// Sizes, in bytes, as the file writes them.
const (
	kilobyte int64 = 1000
	megabyte       = 1000 * kilobyte
	gigabyte       = 1000 * megabyte
	terabyte       = 1000 * gigabyte
)

// Repository is a registered repository's entry, which `asmai repo add`
// writes.
type Repository struct {
	// Location is where the user's own checkout is; empty for a repository
	// registered by its URL. It is never used for work.
	Location string `toml:"location"`
	// Origin is the origin remote.
	Origin string `toml:"origin"`
	// DefaultBranch is the branch jobs start from unless the user names
	// another.
	DefaultBranch string `toml:"default_branch"`
}

func (r *Repository) field(name string) *string {
	switch name {
	case "location":
		return &r.Location
	case "origin":
		return &r.Origin
	case "default_branch":
		return &r.DefaultBranch
	}
	return nil
}

// The repository fields, in the order the file is documented.
var repositoryFields = []string{"location", "origin", "default_branch"}

// Staffing is what a role's agents run on.
type Staffing struct {
	LeaderProvider string
	LeaderModel    string
	WorkerProvider string
	WorkerModel    string
}

// The staffing fields, in the order the file is documented.
var staffingFields = []string{"leader_provider", "leader_model", "worker_provider", "worker_model"}

func (s *Staffing) field(name string) *string {
	switch name {
	case "leader_provider":
		return &s.LeaderProvider
	case "leader_model":
		return &s.LeaderModel
	case "worker_provider":
		return &s.WorkerProvider
	case "worker_model":
		return &s.WorkerModel
	}
	return nil
}

// Problem is something in the configuration that keeps the factory from
// running, with how to fix it.
type Problem struct {
	// Line is the line of the file the problem is on, or 0 for a problem
	// with no line, such as a missing field.
	Line    int
	Problem string
	Fix     string
}

func (p Problem) Error() string {
	if p.Line > 0 {
		return fmt.Sprintf("line %d: %s", p.Line, p.Problem)
	}
	return p.Problem
}

// DefaultPath returns the configuration file's path in home:
// ~/.config/asmai/config.toml, on Linux and macOS alike.
func DefaultPath(home string) string {
	return filepath.Join(home, ".config", "asmai", "config.toml")
}

// ActivePath selects an explicit configuration file for a disposable
// qualification run, while ordinary use keeps the user's default path.
func ActivePath(home string) (string, error) {
	if path := os.Getenv("ASMAI_CONFIG_FILE"); path != "" {
		if !filepath.IsAbs(path) {
			return "", fmt.Errorf("ASMAI_CONFIG_FILE must be an absolute path")
		}
		return filepath.Clean(path), nil
	}
	return DefaultPath(home), nil
}

// ErrMissing is returned by Load for a configuration file that does not exist.
var ErrMissing = errors.New("the configuration file does not exist")

// Load reads and checks the configuration file at path. A file that does not
// exist is ErrMissing; a file this asmai cannot honour returns each Problem
// in it, by line.
func Load(path string) (Config, []Problem, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, nil, ErrMissing
	}
	if err != nil {
		return Config{}, nil, err
	}
	cfg, problems := Parse(data)
	return cfg, problems, nil
}

// Parse reads and checks a configuration file's content.
func Parse(data []byte) (Config, []Problem) {
	// The decoder checks the whole of TOML, such as a key defined twice, and
	// says where.
	var doc map[string]any
	if err := toml.Unmarshal(data, &doc); err != nil {
		var decodeErr *toml.DecodeError
		line := 0
		message := err.Error()
		if errors.As(err, &decodeErr) {
			line, _ = decodeErr.Position()
			message = strings.TrimPrefix(decodeErr.Error(), "toml: ")
		}
		return Config{}, []Problem{{Line: line, Problem: "the file is not valid TOML: " + message, Fix: "correct the TOML on that line"}}
	}

	st := &state{cfg: Config{
		Roles:        map[string]Staffing{},
		Repositories: map[string]Repository{},
		Skills:       map[string]Skills{},
		Options:      map[string]RepositoryOptions{},
		Limits:       DefaultLimits(),
		Logs:         DefaultLogs(),
		Providers:    map[string]Provider{Claude: {WorkerCap: 3}},
	}, lines: map[string]int{}}
	p := &unstable.Parser{}
	p.Reset(data)
	var table []string
	// skipping is set in a table already refused, whose fields are not
	// looked at.
	skipping := false
	for p.NextExpression() {
		e := p.Expression()
		// A key's position is the line: an expression's own range is not
		// always set, and may start before the line.
		var line int
		switch e.Kind {
		case unstable.KeyValue, unstable.Table, unstable.ArrayTable:
			line = keyLine(p, e.Key())
		}
		switch e.Kind {
		case unstable.Table:
			table, skipping = keyPath(e.Key()), false
			if problem, ok := checkTable(table, line); !ok {
				st.problems = append(st.problems, problem)
				skipping = true
				continue
			}
			st.declare(table)
		case unstable.ArrayTable:
			st.problems = append(st.problems, Problem{Line: line,
				Problem: fmt.Sprintf("[[%s]] is an array of tables, which this asmai does not read", strings.Join(keyPath(e.Key()), ".")),
				Fix:     "write each role or repository as its own table, such as [roles.coordination]"})
			skipping = true
		case unstable.KeyValue:
			if skipping {
				continue
			}
			st.keyValue(p, append(append([]string(nil), table...), keyPath(e.Key())...), e.Value(), line)
		}
	}
	if err := p.Error(); err != nil {
		// The decoder above refuses whatever the parser does; this is only a
		// safeguard.
		st.problems = append(st.problems, Problem{Problem: "the file is not valid TOML: " + err.Error(), Fix: "correct the TOML"})
	}
	st.crossCheck()
	return st.cfg, st.problems
}

// state is a file being read: what it has set so far, the problems found,
// and the line each setting was written on.
type state struct {
	cfg      Config
	problems []Problem
	// lines is the line of each setting written, by its dotted path.
	lines map[string]int
}

// crossCheck finds the problems between settings that are each fine alone.
func (st *state) crossCheck() {
	l := st.cfg.Limits
	if l.ProviderErrorWaitMax < l.ProviderErrorWait {
		line, ok := st.lines["limits.provider_error_wait_max"]
		if !ok {
			line = st.lines["limits.provider_error_wait"]
		}
		st.problems = append(st.problems, Problem{Line: line,
			Problem: fmt.Sprintf("provider_error_wait_max in [limits] (%s) is shorter than provider_error_wait (%s)", l.ProviderErrorWaitMax, l.ProviderErrorWait),
			Fix:     "make provider_error_wait_max at least provider_error_wait: the longest wait cannot be shorter than the first"})
	}
}

// keyLine returns the line the key it iterates over is on.
func keyLine(p *unstable.Parser, it unstable.Iterator) int {
	if !it.Next() {
		return 0
	}
	return p.Shape(it.Node().Raw).Start.Line
}

func keyPath(it unstable.Iterator) []string {
	var path []string
	for it.Next() {
		path = append(path, string(it.Node().Data))
	}
	return path
}

// declare notes that the table at path is in the file, so that a role or
// repository with a table and no fields is still there.
func (st *state) declare(path []string) {
	if len(path) != 2 {
		return
	}
	switch path[0] {
	case "roles":
		if _, ok := st.cfg.Roles[path[1]]; !ok {
			st.cfg.Roles[path[1]] = Staffing{}
		}
	case "repositories":
		if _, ok := st.cfg.Repositories[path[1]]; !ok {
			st.cfg.Repositories[path[1]] = Repository{}
			st.cfg.Options[path[1]] = RepositoryOptions{CommitTrailers: true}
		}
	}
}

// CheckRepositoryName returns why name cannot name a registered repository,
// or nil. A name is the key of its [repositories.<name>] table and the name
// of its clone's directory, so it is letters, digits, "-" and "_", and starts
// with a letter or digit.
func CheckRepositoryName(name string) error {
	if name == "" {
		return errors.New("a repository's name is empty")
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case (r == '-' || r == '_') && i > 0:
		default:
			return fmt.Errorf("%q is not a repository name: a name is letters, digits, \"-\" and \"_\", starting with a letter or digit", name)
		}
	}
	return nil
}

// tablesRead is what the file's tables are, as problems name them.
const tablesRead = "[limits], [logs], [providers.claude], [roles.<role>] and [repositories.<name>]"

// checkTable checks a table header of path on line.
func checkTable(path []string, line int) (Problem, bool) {
	header := "[" + strings.Join(path, ".") + "]"
	notRead := Problem{Line: line,
		Problem: fmt.Sprintf("%s is not a table this asmai reads; it reads only %s", header, tablesRead),
		Fix:     fmt.Sprintf("remove %s and its fields", header)}
	switch path[0] {
	case "limits", "logs":
		if len(path) > 1 {
			return notRead, false
		}
	case "providers", "roles", "repositories":
		if len(path) > 2 {
			return notRead, false
		}
	default:
		return notRead, false
	}
	if len(path) == 1 {
		return Problem{}, true
	}
	switch path[0] {
	case "providers":
		switch path[1] {
		case Claude:
		case "codex":
			return Problem{Line: line,
				Problem: fmt.Sprintf("%s names Codex, which arrives in a later asmai; this asmai runs only %q", header, Claude),
				Fix:     fmt.Sprintf("remove %s and its fields until Codex arrives", header)}, false
		default:
			return Problem{Line: line,
				Problem: fmt.Sprintf("%s names no provider; this asmai runs only %q", header, Claude),
				Fix:     "name the provider, such as [providers.claude]"}, false
		}
	case "roles":
		if !roles.Known(path[1]) {
			return Problem{Line: line,
				Problem: fmt.Sprintf("%s names no role; the roles are %s", header, strings.Join(roles.All, ", ")),
				Fix:     "name one of the roles, such as [roles.coordination]"}, false
		}
	case "repositories":
		if err := CheckRepositoryName(path[1]); err != nil {
			return Problem{Line: line,
				Problem: fmt.Sprintf("%s: %v", header, err),
				Fix:     "rename the repository, such as [repositories.otman]"}, false
		}
	}
	return Problem{}, true
}

// keyValue sets the field at path to value, found on line, or notes why it
// cannot.
func (st *state) keyValue(p *unstable.Parser, path []string, value *unstable.Node, line int) {
	if value.Kind == unstable.InlineTable {
		if len(path) <= 2 {
			if problem, ok := checkTable(path, line); !ok {
				st.problems = append(st.problems, problem)
				return
			}
		}
		st.declare(path)
		it := value.Children()
		for it.Next() {
			kv := it.Node()
			kvLine := keyLine(p, kv.Key())
			st.keyValue(p, append(append([]string(nil), path...), keyPath(kv.Key())...), kv.Value(), kvLine)
		}
		return
	}
	table, name := tableAndField(path)
	if table == nil {
		key := strings.Join(path, ".")
		if len(path) >= 2 {
			if problem, ok := checkTable(path[:2], line); !ok {
				st.problems = append(st.problems, problem)
				return
			}
		}
		st.problems = append(st.problems, Problem{Line: line,
			Problem: fmt.Sprintf("%s is not a setting this asmai reads; it reads only settings under %s", key, tablesRead),
			Fix:     fmt.Sprintf("remove %s", key)})
		return
	}
	if problem, ok := checkTable(table, line); !ok {
		st.problems = append(st.problems, problem)
		return
	}
	st.setField(table, name, value, line)
}

// tableAndField splits path into the table it is in and the field's name,
// or returns a nil table for a path that is no field of a table this asmai
// reads.
func tableAndField(path []string) (table []string, name string) {
	switch {
	case len(path) == 2 && (path[0] == "limits" || path[0] == "logs"):
		return path[:1], path[1]
	case len(path) == 3 && (path[0] == "providers" || path[0] == "roles" || path[0] == "repositories"):
		return path[:2], path[2]
	}
	return nil, ""
}

// setField sets the field name of table to value, found on line.
func (st *state) setField(table []string, name string, value *unstable.Node, line int) {
	header := "[" + strings.Join(table, ".") + "]"
	var settings []setting
	var commit func()
	switch table[0] {
	case "limits":
		settings = limitSettings(&st.cfg.Limits)
	case "logs":
		settings = logSettings(&st.cfg.Logs)
	case "providers":
		provider := st.cfg.Providers[table[1]]
		settings, commit = providerSettings(&provider), func() { st.cfg.Providers[table[1]] = provider }
	case "roles":
		staffing, skills := st.cfg.Roles[table[1]], st.cfg.Skills[table[1]]
		settings = append(staffingSettings(&staffing, table[1]), skillSettings(&skills)...)
		commit = func() { st.cfg.Roles[table[1]], st.cfg.Skills[table[1]] = staffing, skills }
	case "repositories":
		repo, options := st.cfg.Repositories[table[1]], st.cfg.Options[table[1]]
		if _, ok := st.cfg.Options[table[1]]; !ok {
			options.CommitTrailers = true
		}
		settings = append(repositorySettings(&repo, table[1]), optionSettings(&options)...)
		commit = func() { st.cfg.Repositories[table[1]], st.cfg.Options[table[1]] = repo, options }
	}
	i := slices.IndexFunc(settings, func(s setting) bool { return s.name == name })
	if i < 0 {
		names := make([]string, len(settings))
		for j, s := range settings {
			names[j] = s.name
		}
		st.problems = append(st.problems, Problem{Line: line,
			Problem: fmt.Sprintf("%s in %s is not a setting this asmai reads; the fields are %s", name, header, joinFieldNames(names)),
			Fix:     fmt.Sprintf("remove %s, or correct its name", name)})
		return
	}
	if problem := settings[i].apply(header, value, line); problem != nil {
		st.problems = append(st.problems, *problem)
		return
	}
	st.lines[strings.Join(table, ".")+"."+name] = line
	if commit != nil {
		commit()
	}
}

// A kind is the type of a field.
type kind int

const (
	kindString kind = iota
	kindInteger
	kindBoolean
	kindDuration
	kindSize
	kindStrings
)

// A setting is one field of a table: how it is written and where it goes.
type setting struct {
	name string
	kind kind
	// min is the least an integer may be.
	min int64
	// example is a value of the field as the file writes it, for a fix.
	example string
	// check returns why a string or a list's value is not honoured, with
	// the fix, or "" when it is.
	check func(text string, list []string) (problem, fix string)
	// set stores the value read, which is text for a string, number for an
	// integer, duration or size (a duration in nanoseconds, a size in
	// bytes), flag for a boolean and list for a list.
	set func(v value)
}

type value struct {
	text   string
	number int64
	flag   bool
	list   []string
}

func (k kind) String() string {
	switch k {
	case kindInteger:
		return "an integer"
	case kindBoolean:
		return "a boolean"
	case kindDuration:
		return `a duration string such as "10m"`
	case kindSize:
		return `a size string such as "5GB"`
	case kindStrings:
		return "a list of strings"
	}
	return "a string"
}

// how is how a fix says to write a field of the kind.
func (k kind) how() string {
	switch k {
	case kindInteger:
		return "a whole number"
	case kindBoolean:
		return "true or false"
	case kindDuration:
		return `a duration in double quotes, a whole number then s, m, h or d`
	case kindSize:
		return `a size in double quotes, a whole number then B, KB, MB, GB or TB`
	case kindStrings:
		return "a list of strings in double quotes"
	}
	return "a string in double quotes"
}

// apply checks value, found on line in the table header, and sets it, or
// returns the problem.
func (s setting) apply(header string, node *unstable.Node, line int) *Problem {
	wrongType := func(got string) *Problem {
		return &Problem{Line: line,
			Problem: fmt.Sprintf("%s in %s is %s, not %s", s.name, header, got, s.kind.wanted()),
			Fix:     fmt.Sprintf("write %s as %s, such as %s = %s", s.name, s.kind.how(), s.name, s.example)}
	}
	bad := func(why, fix string) *Problem {
		return &Problem{Line: line, Problem: fmt.Sprintf("%s in %s %s", s.name, header, why), Fix: fix}
	}
	var v value
	switch node.Kind {
	case unstable.String:
		v.text = string(node.Data)
		switch s.kind {
		case kindString:
		case kindDuration:
			d, err := parseDuration(v.text)
			if err != nil {
				return bad(fmt.Sprintf("is %q, which is not a duration: %v", v.text, err), fmt.Sprintf("write %s as a whole number then s, m, h or d, such as %s = %s", s.name, s.name, s.example))
			}
			v.number = int64(d)
		case kindSize:
			n, err := parseSize(v.text)
			if err != nil {
				return bad(fmt.Sprintf("is %q, which is not a size: %v", v.text, err), fmt.Sprintf("write %s as a whole number then B, KB, MB, GB or TB, such as %s = %s", s.name, s.name, s.example))
			}
			v.number = n
		default:
			return wrongType("a string")
		}
	case unstable.Integer:
		if s.kind != kindInteger {
			return wrongType("a number")
		}
		n, err := strconv.ParseInt(strings.ReplaceAll(string(node.Data), "_", ""), 0, 64)
		if err != nil {
			return bad("is not a whole number this asmai can read", fmt.Sprintf("write %s as a whole number, such as %s = %s", s.name, s.name, s.example))
		}
		if n < s.min {
			return bad(fmt.Sprintf("is %d, which is below the least, %d", n, s.min), fmt.Sprintf("write %s as %d or more, such as %s = %s", s.name, s.min, s.name, s.example))
		}
		v.number = n
	case unstable.Bool:
		if s.kind != kindBoolean {
			return wrongType("a boolean")
		}
		v.flag = string(node.Data) == "true"
	case unstable.Array:
		if s.kind != kindStrings {
			return wrongType("a list")
		}
		v.list = []string{}
		it := node.Children()
		for i := 1; it.Next(); i++ {
			item := it.Node()
			if item.Kind != unstable.String {
				return bad(fmt.Sprintf("has item %d that is %s, not a string", i, describe(item.Kind)), fmt.Sprintf("write each item of %s as a string in double quotes, such as %s = %s", s.name, s.name, s.example))
			}
			v.list = append(v.list, string(item.Data))
		}
	default:
		return wrongType(describe(node.Kind))
	}
	if s.check != nil {
		if why, fix := s.check(v.text, v.list); why != "" {
			return bad(why, fix)
		}
	}
	s.set(v)
	return nil
}

func (k kind) wanted() string {
	if k == kindInteger || k == kindBoolean || k == kindStrings {
		return k.String()
	}
	return "a string"
}

func describe(k unstable.Kind) string {
	switch k {
	case unstable.String:
		return "a string"
	case unstable.Integer, unstable.Float:
		return "a number"
	case unstable.Bool:
		return "a boolean"
	case unstable.Array:
		return "a list"
	case unstable.InlineTable:
		return "a table"
	case unstable.LocalDate, unstable.LocalTime, unstable.LocalDateTime, unstable.DateTime:
		return "a date or time"
	}
	return "not a string"
}

var (
	durationPattern = regexp.MustCompile(`^([0-9]+)(s|m|h|d)$`)
	sizePattern     = regexp.MustCompile(`^([0-9]+)(B|KB|MB|GB|TB)$`)
)

// parseDuration reads a duration as the file writes it: a whole number, then
// s, m, h or d, such as "30s", "10m" or "14d". It is more than zero.
func parseDuration(text string) (time.Duration, error) {
	m := durationPattern.FindStringSubmatch(text)
	if m == nil {
		return 0, errors.New("it is a whole number then s, m, h or d")
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	unit := map[string]time.Duration{"s": time.Second, "m": time.Minute, "h": time.Hour, "d": 24 * time.Hour}[m[2]]
	if err != nil || n > int64(math.MaxInt64/unit) {
		return 0, errors.New("it is too long")
	}
	if n == 0 {
		return 0, errors.New("it is more than zero")
	}
	return time.Duration(n) * unit, nil
}

// parseSize reads a size as the file writes it: a whole number, then B, KB,
// MB, GB or TB, in units of 1000. It is more than zero.
func parseSize(text string) (int64, error) {
	m := sizePattern.FindStringSubmatch(text)
	if m == nil {
		return 0, errors.New("it is a whole number then B, KB, MB, GB or TB")
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	unit := map[string]int64{"B": 1, "KB": kilobyte, "MB": megabyte, "GB": gigabyte, "TB": terabyte}[m[2]]
	if err != nil || n > math.MaxInt64/unit {
		return 0, errors.New("it is too large")
	}
	if n == 0 {
		return 0, errors.New("it is more than zero")
	}
	return n * unit, nil
}

func limitSettings(l *Limits) []setting {
	dur := func(name, example string, to *time.Duration) setting {
		return setting{name: name, kind: kindDuration, example: example, set: func(v value) { *to = time.Duration(v.number) }}
	}
	count := func(name string, min int64, example string, to *int) setting {
		return setting{name: name, kind: kindInteger, min: min, example: example, set: func(v value) { *to = int(v.number) }}
	}
	return []setting{
		dur("leader_idle_grace", `"10m"`, &l.LeaderIdleGrace),
		count("leader_restarts", 0, "3", &l.LeaderRestarts),
		dur("leader_restart_window", `"15m"`, &l.LeaderRestartWindow),
		dur("silence_timeout", `"30m"`, &l.SilenceTimeout),
		dur("drain_timeout", `"10m"`, &l.DrainTimeout),
		count("side_effect_free_retries", 0, "2", &l.SideEffectFreeRetries),
		count("provider_error_resumes", 0, "5", &l.ProviderErrorResumes),
		dur("provider_error_wait", `"30s"`, &l.ProviderErrorWait),
		dur("provider_error_wait_max", `"8m"`, &l.ProviderErrorWaitMax),
		dur("recheck_interval", `"5m"`, &l.RecheckInterval),
		count("dispatch_limit", 1, "5", &l.DispatchLimit),
		count("failed_validations", 1, "3", &l.FailedValidations),
		dur("first_check_wait", `"15m"`, &l.FirstCheckWait),
		{name: "free_space_floor", kind: kindSize, example: `"5GB"`, set: func(v value) { l.FreeSpaceFloor = v.number }},
	}
}

func logSettings(l *Logs) []setting {
	return []setting{
		{name: "daemon_files", kind: kindInteger, min: 1, example: "5", set: func(v value) { l.DaemonFiles = int(v.number) }},
		{name: "daemon_file_size", kind: kindSize, example: `"20MB"`, set: func(v value) { l.DaemonFileSize = v.number }},
		{name: "recording_retention", kind: kindDuration, example: `"14d"`, set: func(v value) { l.RecordingRetention = time.Duration(v.number) }},
	}
}

func providerSettings(p *Provider) []setting {
	return []setting{
		{name: "worker_cap", kind: kindInteger, min: 1, example: "3", set: func(v value) { p.WorkerCap = int(v.number) }},
		{name: "default_model", kind: kindString, example: `"opus"`,
			check: func(text string, _ []string) (string, string) {
				if strings.TrimSpace(text) == "" {
					return "is empty", `write the model to use when work falls back to this provider, such as default_model = "opus", or remove default_model`
				}
				return "", ""
			},
			set: func(v value) { p.DefaultModel = v.text }},
	}
}

func staffingSettings(s *Staffing, role string) []setting {
	var settings []setting
	for _, name := range staffingFields {
		field := s.field(name)
		set := setting{name: name, kind: kindString, example: `"opus"`, set: func(v value) { *field = v.text }}
		switch {
		case strings.HasSuffix(name, "_provider"):
			set.example = fmt.Sprintf("%q", Claude)
			set.check = func(text string, _ []string) (string, string) {
				if text == Claude {
					return "", ""
				}
				why := fmt.Sprintf("%q is not a provider", text)
				if text == "codex" {
					why = "Codex arrives in a later asmai"
				}
				return fmt.Sprintf("is %q, but this asmai runs only %q (%s)", text, Claude, why), fmt.Sprintf("write %s = %q", name, Claude)
			}
		default:
			set.check = func(text string, _ []string) (string, string) {
				if strings.TrimSpace(text) == "" {
					return "is empty", fmt.Sprintf("write the model %s's %s uses, such as %s = \"opus\"", role, strings.TrimSuffix(name, "_model"), name)
				}
				return "", ""
			}
		}
		settings = append(settings, set)
	}
	return settings
}

// skillNamePattern is what a skill's name is: letters, digits, "-", "_" and
// ".", starting with a letter or digit.
var skillNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func skillSettings(s *Skills) []setting {
	names := func(list []string) (string, string) {
		for i, name := range list {
			switch {
			case !skillNamePattern.MatchString(name):
				return fmt.Sprintf("names %q, which is not a skill name: a name is letters, digits, \".\", \"-\" and \"_\", starting with a letter or digit", name), "name the skill as the bundle does, such as \"tdd\""
			case slices.Contains(list[:i], name):
				return fmt.Sprintf("names %q twice", name), fmt.Sprintf("remove the second %q", name)
			}
		}
		return "", ""
	}
	dirs := func(list []string) (string, string) {
		for i, dir := range list {
			switch {
			case strings.TrimSpace(dir) == "":
				return "has an empty directory", "write each directory as an absolute path, such as \"/home/me/skills/mine\""
			case !filepath.IsAbs(dir) && dir != "~" && !strings.HasPrefix(dir, "~/"):
				return fmt.Sprintf("has %q, which is not an absolute path", dir), "write each directory as an absolute path, or one starting with ~/, such as \"~/skills/mine\""
			case slices.Contains(list[:i], dir):
				return fmt.Sprintf("names %q twice", dir), fmt.Sprintf("remove the second %q", dir)
			}
		}
		return "", ""
	}
	list := func(name, example string, check func([]string) (string, string), to *[]string) setting {
		return setting{name: name, kind: kindStrings, example: example,
			check: func(_ string, items []string) (string, string) { return check(items) },
			set:   func(v value) { *to = v.list }}
	}
	return []setting{
		list("leader_skills", `["pr"]`, names, &s.LeaderSkills),
		list("worker_skills", `["tdd", "pr"]`, names, &s.WorkerSkills),
		list("leader_added_skills", `["~/skills/mine"]`, dirs, &s.LeaderAdded),
		list("worker_added_skills", `["~/skills/mine"]`, dirs, &s.WorkerAdded),
	}
}

func repositorySettings(r *Repository, repo string) []setting {
	var settings []setting
	for _, name := range repositoryFields {
		field := r.field(name)
		set := setting{name: name, kind: kindString, example: `"main"`, set: func(v value) { *field = v.text }}
		// Only a repository registered by its URL has no location.
		if name != "location" {
			set.check = func(text string, _ []string) (string, string) {
				if strings.TrimSpace(text) == "" {
					return "is empty", fmt.Sprintf("write the repository's %s, or remove [repositories.%s] and register it again with `asmai repo add`", strings.ReplaceAll(name, "_", " "), repo)
				}
				return "", ""
			}
		}
		settings = append(settings, set)
	}
	return settings
}

func optionSettings(o *RepositoryOptions) []setting {
	flag := func(name string, to *bool) setting {
		return setting{name: name, kind: kindBoolean, example: "true", set: func(v value) { *to = v.flag }}
	}
	return []setting{
		{name: "notes", kind: kindString, example: `"Run make test before every commit."`, set: func(v value) { o.Notes = v.text }},
		flag("checks_one_at_a_time", &o.ChecksOneAtATime),
		flag("no_ci", &o.NoCI),
		flag("commit_trailers", &o.CommitTrailers),
		{name: "repository_skills", kind: kindStrings, example: `["engineering"]`,
			check: func(_ string, items []string) (string, string) {
				for i, role := range items {
					switch {
					case !roles.Known(role):
						return fmt.Sprintf("names %q, which is no role; the roles are %s", role, strings.Join(roles.All, ", ")), `write role names, such as ["engineering"]`
					case slices.Contains(items[:i], role):
						return fmt.Sprintf("names %q twice", role), fmt.Sprintf("remove the second %q", role)
					}
				}
				return "", ""
			},
			set: func(v value) { o.RepositorySkills = v.list }},
	}
}

func joinFieldNames(names []string) string {
	if len(names) == 1 {
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

func example(name string) string {
	if strings.HasSuffix(name, "_provider") {
		return fmt.Sprintf(", such as %s = %q", name, Claude)
	}
	return fmt.Sprintf(", such as %s = \"opus\"", name)
}

// MissingStaffing returns a problem for each staffing field the roles in
// need lack: every role there must have all four fields.
func (c Config) MissingStaffing(need []string) []Problem {
	var problems []Problem
	for _, role := range need {
		staffing, ok := c.Roles[role]
		if !ok {
			problems = append(problems, Problem{
				Problem: fmt.Sprintf("there is no [roles.%s], so %s is not staffed", role, roles.Title(role)),
				Fix:     fmt.Sprintf("add [roles.%s] with %s", role, exampleStaffing().inline())})
			continue
		}
		for _, name := range staffingFields {
			if *staffing.field(name) == "" {
				problems = append(problems, Problem{
					Problem: fmt.Sprintf("[roles.%s] has no %s", role, name),
					Fix:     fmt.Sprintf("add %s under [roles.%s]%s", name, role, example(name))})
			}
		}
	}
	return problems
}

// exampleStaffing is the staffing a missing role's fix suggests, and the
// staffing of each role in the file `asmai init` writes.
func exampleStaffing() Staffing {
	return Staffing{LeaderProvider: Claude, LeaderModel: "opus", WorkerProvider: Claude, WorkerModel: "opus"}
}

// assignments returns each of s's fields as a TOML assignment, in the order
// the file is documented.
func (s Staffing) assignments() []string {
	var lines []string
	for _, name := range staffingFields {
		lines = append(lines, fmt.Sprintf("%s = %q", name, *s.field(name)))
	}
	return lines
}

func (s Staffing) inline() string {
	return joinFieldNames(s.assignments())
}

// Example returns role's table staffed as a missing role's fix suggests.
func Example(role string) string {
	return "[roles." + role + "]\n" + strings.Join(exampleStaffing().assignments(), "\n")
}

// defaultHeader opens the file `asmai init` writes.
const defaultHeader = `# AsmAI's configuration file, which you edit by hand. ` + "`asmai init`" + ` wrote it
# once, with each role staffed, and never changes it. What each table and
# field means is in the README's section on the configuration file:
# https://github.com/talvor/asmai#the-configuration-file
`

// Default returns the configuration file `asmai init` writes when there is
// none: a header pointing to the README's section on the file, and a table
// staffing each role M1 runs as exampleStaffing does, with Claude's default
// fallback model. It registers no repository.
func Default(claudeModel string) []byte {
	var b strings.Builder
	b.WriteString(defaultHeader)
	model, _ := json.Marshal(claudeModel)
	b.WriteString("\n[providers.claude]\ndefault_model = " + string(model) + "\n")
	for _, role := range roles.Staffed {
		b.WriteString("\n" + Example(role) + "\n")
	}
	return []byte(b.String())
}

// exampleConfiguration is the example of rule 34 of the specification, with
// the defaults written out, for one role and one repository, as this asmai
// reads it: Codex arrives later, so the example is staffed on Claude alone.
const exampleConfiguration = `# An example configuration file, with every default written out, for one
# role and one repository. Copy what you need into ~/.config/asmai/config.toml
# and check it with ` + "`asmai config check`" + `.

[limits]
leader_idle_grace = "10m"
leader_restarts = 3
leader_restart_window = "15m"
silence_timeout = "30m"
drain_timeout = "10m"
side_effect_free_retries = 2
provider_error_resumes = 5
provider_error_wait = "30s"
provider_error_wait_max = "8m"
recheck_interval = "5m"
dispatch_limit = 5
failed_validations = 3
first_check_wait = "15m"
free_space_floor = "5GB"

[logs]
daemon_files = 5
daemon_file_size = "20MB"
recording_retention = "14d"

[providers.claude]
worker_cap = 3
default_model = "opus"

[roles.coordination]
leader_provider = "claude"
leader_model = "opus"
worker_provider = "claude"
worker_model = "opus"

[roles.quality]
leader_provider = "claude"
leader_model = "opus"
worker_provider = "claude"
worker_model = "opus"

[roles.engineering]
leader_provider = "claude"
leader_model = "opus"
worker_provider = "claude"
worker_model = "opus"
leader_skills = ["pr"]
worker_skills = ["implement", "tdd", "diagnosing-bugs", "prototype", "codebase-design", "improve-codebase-architecture", "pr", "wizard", "writing-for-agents"]
leader_added_skills = []
worker_added_skills = []

[repositories.otman]
location = "/home/me/src/otman"
origin = "git@github.com:me/otman.git"
default_branch = "main"
notes = ""
checks_one_at_a_time = false
no_ci = false
commit_trailers = true
repository_skills = []
`

// ExampleConfiguration returns the example configuration file, which passes
// Parse without a problem.
func ExampleConfiguration() []byte {
	return []byte(exampleConfiguration)
}

// CreateDefault writes Default to path, making its directory, unless a file
// is already there, which it leaves exactly as it is. It reports whether it
// wrote the file.
func CreateDefault(path, claudeModel string) (created bool, err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, fmt.Errorf("creating the configuration file's directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("creating the configuration file: %w", err)
	}
	_, err = f.Write(Default(claudeModel))
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(path)
		return false, fmt.Errorf("writing the configuration file: %w", err)
	}
	return true, nil
}
