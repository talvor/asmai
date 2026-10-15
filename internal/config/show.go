// SPDX-License-Identifier: Apache-2.0

package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/skills"
)

// Entry is one setting of the effective configuration.
type Entry struct {
	// Table is the table's name as its header writes it, such as
	// roles.engineering.
	Table string
	Name  string
	// Text is the value as the file writes it.
	Text string
	// List is the value of a list setting, and IsList says it is one.
	List   []string
	IsList bool
}

// Entries returns every setting of c in the order the file is documented,
// with the defaults of the ones the file did not set: limits, logs, the
// provider, each role's staffing and skills, and each repository's entry.
func Entries(c Config) []Entry {
	var out []Entry
	add := func(table, name, text string) { out = append(out, Entry{Table: table, Name: name, Text: text}) }
	addList := func(table, name string, items []string) {
		out = append(out, Entry{Table: table, Name: name, Text: list(items), List: slices.Clone(items), IsList: true})
	}
	number := func(n int) string { return strconv.Itoa(n) }
	l := c.Limits
	add("limits", "leader_idle_grace", quote(FormatDuration(l.LeaderIdleGrace)))
	add("limits", "leader_restarts", number(l.LeaderRestarts))
	add("limits", "leader_restart_window", quote(FormatDuration(l.LeaderRestartWindow)))
	add("limits", "silence_timeout", quote(FormatDuration(l.SilenceTimeout)))
	add("limits", "drain_timeout", quote(FormatDuration(l.DrainTimeout)))
	add("limits", "side_effect_free_retries", number(l.SideEffectFreeRetries))
	add("limits", "provider_error_resumes", number(l.ProviderErrorResumes))
	add("limits", "provider_error_wait", quote(FormatDuration(l.ProviderErrorWait)))
	add("limits", "provider_error_wait_max", quote(FormatDuration(l.ProviderErrorWaitMax)))
	add("limits", "recheck_interval", quote(FormatDuration(l.RecheckInterval)))
	add("limits", "dispatch_limit", number(l.DispatchLimit))
	add("limits", "failed_validations", number(l.FailedValidations))
	add("limits", "first_check_wait", quote(FormatDuration(l.FirstCheckWait)))
	add("limits", "free_space_floor", quote(FormatSize(l.FreeSpaceFloor)))

	add("logs", "daemon_files", number(c.Logs.DaemonFiles))
	add("logs", "daemon_file_size", quote(FormatSize(c.Logs.DaemonFileSize)))
	add("logs", "recording_retention", quote(FormatDuration(c.Logs.RecordingRetention)))

	provider := c.Providers[Claude]
	if provider.WorkerCap == 0 {
		provider.WorkerCap = 3
	}
	add("providers.claude", "worker_cap", number(provider.WorkerCap))
	if provider.DefaultModel != "" {
		add("providers.claude", "default_model", quote(provider.DefaultModel))
	}

	for _, role := range roles.All {
		staffing, staffed := c.Roles[role]
		if _, listed := c.Skills[role]; !staffed && !listed {
			continue
		}
		table := "roles." + role
		for _, name := range staffingFields {
			if text := *staffing.field(name); text != "" {
				add(table, name, quote(text))
			}
		}
		s := c.EffectiveSkills(role)
		addList(table, "leader_skills", s.LeaderSkills)
		addList(table, "worker_skills", s.WorkerSkills)
		addList(table, "leader_added_skills", s.LeaderAdded)
		addList(table, "worker_added_skills", s.WorkerAdded)
	}

	for _, name := range slices.Sorted(maps.Keys(c.Repositories)) {
		r := c.Repositories[name]
		o, ok := c.Options[name]
		if !ok {
			o = DefaultRepositoryOptions()
		}
		table := "repositories." + name
		add(table, "location", quote(r.Location))
		add(table, "origin", quote(r.Origin))
		add(table, "default_branch", quote(r.DefaultBranch))
		add(table, "notes", quote(o.Notes))
		add(table, "checks_one_at_a_time", strconv.FormatBool(o.ChecksOneAtATime))
		add(table, "no_ci", strconv.FormatBool(o.NoCI))
		add(table, "commit_trailers", strconv.FormatBool(o.CommitTrailers))
		addList(table, "repository_skills", o.RepositorySkills)
	}
	return out
}

// Render returns c as a configuration file with every setting written out,
// the defaults of the ones the file did not set included: what `asmai config
// show` prints. The file it is read from has the same meaning as this one,
// and passes Parse without a problem when c does.
func Render(c Config) []byte {
	var b strings.Builder
	table := ""
	for _, e := range Entries(c) {
		if e.Table != table {
			if table != "" {
				b.WriteString("\n")
			}
			fmt.Fprintf(&b, "[%s]\n", e.Table)
			table = e.Table
		}
		fmt.Fprintf(&b, "%s = %s\n", e.Name, e.Text)
	}
	return []byte(b.String())
}

// EffectiveSkills returns role's skill lists as its agents get them: the
// file's list, or the role's default when the file has none.
func (c Config) EffectiveSkills(role string) Skills {
	s := c.Skills[role]
	return Skills{
		LeaderSkills: orDefault(s.LeaderSkills, skills.LeaderList(role)),
		WorkerSkills: orDefault(s.WorkerSkills, skills.WorkerList(role)),
		LeaderAdded:  slices.Clone(s.LeaderAdded),
		WorkerAdded:  slices.Clone(s.WorkerAdded),
	}
}

func orDefault(set, fallback []string) []string {
	if set != nil {
		return set
	}
	return fallback
}

// quote writes text as a TOML basic string.
func quote(text string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.Encode(text)
	return strings.TrimSuffix(b.String(), "\n")
}

func list(items []string) string {
	quoted := make([]string, len(items))
	for i, item := range items {
		quoted[i] = quote(item)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// FormatDuration writes d as the file does, in the largest unit that holds
// it exactly: "14d", "10m" or "30s".
func FormatDuration(d time.Duration) string {
	switch {
	case d > 0 && d%(24*time.Hour) == 0:
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	case d > 0 && d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d > 0 && d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	}
	return fmt.Sprintf("%ds", d/time.Second)
}

// FormatSize writes n bytes as the file does, in the largest unit that holds
// it exactly: "5GB" or "20MB".
func FormatSize(n int64) string {
	for _, u := range []struct {
		suffix string
		size   int64
	}{{"TB", terabyte}, {"GB", gigabyte}, {"MB", megabyte}, {"KB", kilobyte}} {
		if n > 0 && n%u.size == 0 {
			return fmt.Sprintf("%d%s", n/u.size, u.suffix)
		}
	}
	return fmt.Sprintf("%dB", n)
}
