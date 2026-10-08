// SPDX-License-Identifier: Apache-2.0

// Package config reads the factory's configuration file,
// ~/.config/asmai/config.toml, which the user edits by hand, and edits the
// entries `asmai repo add` and `remove` own. This asmai reads only the fields
// M1 uses: each role's staffing, in [roles.<role>], and each registered
// repository's entry, in [repositories.<name>]. A file it cannot honour is
// refused with the line and the fix.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
	"github.com/pelletier/go-toml/v2/unstable"

	"github.com/talvor/asmai/internal/roles"
)

// Claude is the one provider this asmai runs; Codex arrives in M2.
const Claude = "claude"

// Config is the configuration file's content.
type Config struct {
	// Roles holds each role's staffing, by role name, for the roles the file
	// has a table for.
	Roles map[string]Staffing
	// Repositories holds each registered repository's entry, by name.
	Repositories map[string]Repository
}

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

	cfg := Config{Roles: map[string]Staffing{}, Repositories: map[string]Repository{}}
	var problems []Problem
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
				problems = append(problems, problem)
				skipping = true
				continue
			}
			cfg.declare(table)
		case unstable.ArrayTable:
			problems = append(problems, Problem{Line: line,
				Problem: fmt.Sprintf("[[%s]] is an array of tables, which this asmai does not read", strings.Join(keyPath(e.Key()), ".")),
				Fix:     "write each role or repository as its own table, such as [roles.coordination]"})
			skipping = true
		case unstable.KeyValue:
			if skipping {
				continue
			}
			problems = append(problems, keyValue(p, &cfg, append(append([]string(nil), table...), keyPath(e.Key())...), e.Value(), line)...)
		}
	}
	if err := p.Error(); err != nil {
		// The decoder above refuses whatever the parser does; this is only a
		// safeguard.
		problems = append(problems, Problem{Problem: "the file is not valid TOML: " + err.Error(), Fix: "correct the TOML"})
	}
	return cfg, problems
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
func (c *Config) declare(path []string) {
	if len(path) != 2 {
		return
	}
	switch path[0] {
	case "roles":
		if _, ok := c.Roles[path[1]]; !ok {
			c.Roles[path[1]] = Staffing{}
		}
	case "repositories":
		if _, ok := c.Repositories[path[1]]; !ok {
			c.Repositories[path[1]] = Repository{}
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

// checkTable checks a table header of path on line.
func checkTable(path []string, line int) (Problem, bool) {
	header := "[" + strings.Join(path, ".") + "]"
	notRead := Problem{Line: line,
		Problem: fmt.Sprintf("%s is not a table this asmai reads; it reads only [roles.<role>] and [repositories.<name>]", header),
		Fix:     fmt.Sprintf("remove %s and its fields", header)}
	switch {
	case path[0] != "roles" && path[0] != "repositories":
		return notRead, false
	case len(path) == 1:
		return Problem{}, true
	case len(path) > 2:
		return notRead, false
	case path[0] == "roles" && !roles.Known(path[1]):
		return Problem{Line: line,
			Problem: fmt.Sprintf("%s names no role; the roles are %s", header, strings.Join(roles.All, ", ")),
			Fix:     "name one of the roles, such as [roles.coordination]"}, false
	case path[0] == "repositories":
		if err := CheckRepositoryName(path[1]); err != nil {
			return Problem{Line: line,
				Problem: fmt.Sprintf("%s: %v", header, err),
				Fix:     "rename the repository, such as [repositories.otman]"}, false
		}
	}
	return Problem{}, true
}

// keyValue sets the field at path to value, found on line, or returns why it
// cannot.
func keyValue(p *unstable.Parser, cfg *Config, path []string, value *unstable.Node, line int) []Problem {
	if value.Kind == unstable.InlineTable {
		var problems []Problem
		if len(path) <= 2 {
			if problem, ok := checkTable(path, line); !ok {
				return []Problem{problem}
			}
		}
		cfg.declare(path)
		it := value.Children()
		for it.Next() {
			kv := it.Node()
			kvLine := keyLine(p, kv.Key())
			problems = append(problems, keyValue(p, cfg, append(append([]string(nil), path...), keyPath(kv.Key())...), kv.Value(), kvLine)...)
		}
		return problems
	}
	if len(path) == 3 {
		switch path[0] {
		case "roles":
			return staffingField(cfg, path, value, line)
		case "repositories":
			return repositoryField(cfg, path, value, line)
		}
	}
	key := strings.Join(path, ".")
	if len(path) >= 2 && (path[0] == "roles" || path[0] == "repositories") {
		if problem, ok := checkTable(path[:2], line); !ok {
			return []Problem{problem}
		}
	}
	return []Problem{{Line: line,
		Problem: fmt.Sprintf("%s is not a setting this asmai reads; it reads only %s under [roles.<role>] and %s under [repositories.<name>]", key, joinFields(), joinFieldNames(repositoryFields)),
		Fix:     fmt.Sprintf("remove %s", key)}}
}

// staffingField sets the staffing field at path, [roles.<role>] and its
// name, to value.
func staffingField(cfg *Config, path []string, value *unstable.Node, line int) []Problem {
	if problem, ok := checkTable(path[:2], line); !ok {
		return []Problem{problem}
	}
	role, name := path[1], path[2]
	staffing := cfg.Roles[role]
	field := staffing.field(name)
	if field == nil {
		return []Problem{{Line: line,
			Problem: fmt.Sprintf("%s in [roles.%s] is not a setting this asmai reads; the fields are %s", name, role, joinFields()),
			Fix:     fmt.Sprintf("remove %s, or correct its name", name)}}
	}
	if value.Kind != unstable.String {
		return []Problem{{Line: line,
			Problem: fmt.Sprintf("%s in [roles.%s] is %s, not a string", name, role, kindName(value.Kind)),
			Fix:     fmt.Sprintf("write %s as a string in double quotes", name) + example(name)}}
	}
	text := string(value.Data)
	switch {
	case strings.HasSuffix(name, "_provider") && text != Claude:
		why := fmt.Sprintf("%q is not a provider", text)
		if text == "codex" {
			why = "Codex arrives in a later asmai"
		}
		return []Problem{{Line: line,
			Problem: fmt.Sprintf("%s in [roles.%s] is %q, but this asmai runs only %q (%s)", name, role, text, Claude, why),
			Fix:     fmt.Sprintf("write %s = %q", name, Claude)}}
	case strings.HasSuffix(name, "_model") && strings.TrimSpace(text) == "":
		return []Problem{{Line: line,
			Problem: fmt.Sprintf("%s in [roles.%s] is empty", name, role),
			Fix:     fmt.Sprintf("write the model %s's %s uses, such as %s = \"opus\"", role, strings.TrimSuffix(name, "_model"), name)}}
	}
	*field = text
	cfg.Roles[role] = staffing
	return nil
}

// repositoryField sets the repository field at path, [repositories.<name>]
// and its field, to value.
func repositoryField(cfg *Config, path []string, value *unstable.Node, line int) []Problem {
	if problem, ok := checkTable(path[:2], line); !ok {
		return []Problem{problem}
	}
	repo, name := path[1], path[2]
	entry := cfg.Repositories[repo]
	field := entry.field(name)
	if field == nil {
		return []Problem{{Line: line,
			Problem: fmt.Sprintf("%s in [repositories.%s] is not a setting this asmai reads; the fields are %s", name, repo, joinFieldNames(repositoryFields)),
			Fix:     fmt.Sprintf("remove %s, or correct its name", name)}}
	}
	if value.Kind != unstable.String {
		return []Problem{{Line: line,
			Problem: fmt.Sprintf("%s in [repositories.%s] is %s, not a string", name, repo, kindName(value.Kind)),
			Fix:     fmt.Sprintf("write %s as a string in double quotes", name)}}
	}
	text := string(value.Data)
	// Only a repository registered by its URL has no location.
	if name != "location" && strings.TrimSpace(text) == "" {
		return []Problem{{Line: line,
			Problem: fmt.Sprintf("%s in [repositories.%s] is empty", name, repo),
			Fix:     fmt.Sprintf("write the repository's %s, or remove [repositories.%s] and register it again with `asmai repo add`", strings.ReplaceAll(name, "_", " "), repo)}}
	}
	*field = text
	cfg.Repositories[repo] = entry
	return nil
}

func joinFields() string {
	return joinFieldNames(staffingFields)
}

func joinFieldNames(names []string) string {
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

func example(name string) string {
	if strings.HasSuffix(name, "_provider") {
		return fmt.Sprintf(", such as %s = %q", name, Claude)
	}
	return fmt.Sprintf(", such as %s = \"opus\"", name)
}

func kindName(k unstable.Kind) string {
	switch k {
	case unstable.Integer:
		return "a number"
	case unstable.Float:
		return "a number"
	case unstable.Bool:
		return "a boolean"
	case unstable.Array:
		return "a list"
	case unstable.LocalDate, unstable.LocalTime, unstable.LocalDateTime, unstable.DateTime:
		return "a date or time"
	}
	return "not a string"
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
				Fix:     fmt.Sprintf("add [roles.%s] with %s", role, exampleStaffing())})
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

func exampleStaffing() string {
	return `leader_provider = "claude", leader_model, worker_provider = "claude" and worker_model`
}
