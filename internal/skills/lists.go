// SPDX-License-Identifier: Apache-2.0

package skills

import (
	"fmt"
	"slices"
	"strings"
)

// The roles, as the names the factory gives them.
const (
	coordination = "coordination"
	planning     = "planning"
	research     = "research"
	engineering  = "engineering"
	quality      = "quality"
)

// leaders are the skills on each role's leader's list: the index each leader
// is given is not a skill. Coordination's leader has ask-matt, the map of
// the flows it routes by, and wait-what, for re-explaining in its own
// conversation. Engineering's has pr, for composing the pull request.
var leaders = map[string][]string{
	coordination: {"ask-matt", "wait-what"},
	planning:     nil,
	research:     nil,
	engineering:  {"pr"},
	quality:      nil,
}

// workers are the skills on each role's workers' list.
var workers = map[string][]string{
	coordination: {"setup-matt-pocock-skills"},
	planning:     {"wayfinder", "grilling", "grill-with-docs", "grill-me", "domain-modeling", "codebase-design", "to-spec", "to-tickets", "triage", "to-questionnaire", "writing-for-agents"},
	research:     {"research", "to-questionnaire"},
	engineering:  {"implement", "tdd", "diagnosing-bugs", "prototype", "codebase-design", "improve-codebase-architecture", "pr", "wizard", "writing-for-agents"},
	quality:      {"code-review", "retro", "tdd"},
}

// optional ship in the bundle but are on no list until the user adds them.
var optional = []string{"handoff", "teach", "implement-spec"}

// applies is when each skill that is on a worker's list applies in the
// factory, as the leaders' indexes give it.
var applies = map[string]string{
	"setup-matt-pocock-skills":      "Setting a repository up for the skills: its issue tracker, triage labels and domain docs. Run once, when a repository is added.",
	"wayfinder":                     "A foggy effort too big for one session, such as a greenfield project: the way to the destination is not visible yet.",
	"grilling":                      "A plan, decision or idea needs stress-testing by interview, with nothing to write down along the way.",
	"grill-with-docs":               "A plan or design needs sharpening by interview in a repository, leaving its glossary and ADRs updated.",
	"grill-me":                      "A plan or design needs sharpening by interview where there is no repository to leave a paper trail in.",
	"domain-modeling":               "The domain language is fuzzy or overloaded, or a hard-to-reverse decision needs an ADR.",
	"codebase-design":               "A module's interface needs designing or improving, a seam needs placing, or code needs to be more testable.",
	"to-spec":                       "A settled discussion or map of decisions needs turning into a spec on the issue tracker.",
	"to-tickets":                    "A spec or plan needs breaking into tracer-bullet tickets with their blocking edges.",
	"triage":                        "Issues or external pull requests that arrived raw need moving through triage and writing up as agent-ready briefs.",
	"to-questionnaire":              "A decision cannot be fully answered here and needs a questionnaire for someone else to fill in.",
	"writing-for-agents":            "Creating or editing a skill, or an AGENTS.md or CLAUDE.md.",
	"research":                      "A question needs investigating against primary sources, with the findings captured as a Markdown file.",
	"implement":                     "A ticket or spec is ready to build: implements it with tdd, checks as it goes and commits to the assignment branch.",
	"tdd":                           "Building a feature or fixing a bug test-first, one red-green slice at a time, at agreed seams.",
	"diagnosing-bugs":               "A bug resists a first glance: an intermittent failure, a regression, a performance problem.",
	"prototype":                     "A design question needs a throwaway prototype of its logic or its UI to be answered.",
	"improve-codebase-architecture": "Scanning a codebase for deepening opportunities that make it easier to work in.",
	"pr":                            "Writing a pull request body or the PR section of a result: the smallest view, before-and-after evidence, merge danger.",
	"wizard":                        "A human has to perform steps only they can, such as provisioning, credentials or a cutover.",
	"code-review":                   "Reviewing changes since a fixed point against the repository's standards and against the spec.",
	"retro":                         "Looking back on a session that went sideways to improve the agent's environment.",
	"ask-matt":                      "Deciding which skill or flow fits a situation: the map of flows Coordination routes work by.",
	"wait-what":                     "A message did not land with the user and needs re-pitching in plain terms.",
}

// Roles are the roles that have skill lists.
func Roles() []string {
	return []string{coordination, planning, research, engineering, quality}
}

// LeaderList returns the default skills of role's leader, besides its
// index, or nil for a role with none.
func LeaderList(role string) []string {
	return slices.Clone(leaders[role])
}

// WorkerList returns the default skills of role's workers.
func WorkerList(role string) []string {
	return slices.Clone(workers[role])
}

// Optional returns the skills that ship but are on no list.
func Optional() []string {
	return slices.Clone(optional)
}

// IndexEntry is one skill in a leader's index. Source is where the skill
// comes from, Shipped, User or Repository.
type IndexEntry struct {
	Name        string
	Purpose     string
	Applies     string
	UserInvoked bool
	Source      string
}

// IndexOf returns role's leader's index of its workers' default list: see
// IndexFor.
func IndexOf(role string) []IndexEntry {
	var list []string
	for _, name := range workers[role] {
		if _, ok := byName[name]; ok {
			list = append(list, name)
		}
	}
	return IndexFor(Bundled(list))
}

// IndexFor returns a leader's index of its workers' list: for each skill,
// its name, its purpose, when it applies, and whether it starts only on
// request. For a skill that is not shipped, or an optional one the factory's
// own index does not cover, when it applies comes from its own description,
// which says it after "Use when".
func IndexFor(list []Given) []IndexEntry {
	var entries []IndexEntry
	for _, g := range list {
		description, userInvoked := g.describe()
		purpose := Skill{Description: description}.Purpose()
		var when string
		if g.Qualified() {
			when = applies[g.Name]
		}
		if when == "" {
			when = description
			if _, rest, ok := strings.Cut(description, "Use when"); ok {
				when = "When" + strings.TrimRight(rest, " ")
			}
		}
		if when == "" {
			when = "See the skill's own SKILL.md."
		}
		if purpose == "" {
			purpose = when
		}
		entries = append(entries, IndexEntry{Name: g.Name, Purpose: purpose, Applies: when, UserInvoked: userInvoked, Source: g.Source})
	}
	return entries
}

// Index returns the text of role's leader's index of its workers' default
// list, or "" when its workers have no skills.
func Index(role string) string {
	return IndexText(role, IndexOf(role))
}

// IndexText returns the text of role's leader's index entries, or "" when
// there are none. A skill that is not shipped is marked as the user's or
// the repository's, and unqualified.
func IndexText(role string, entries []IndexEntry) string {
	if len(entries) == 0 {
		return ""
	}
	var b strings.Builder
	title := strings.ToUpper(role[:1]) + role[1:]
	fmt.Fprintf(&b, "# Your workers' skills\n\n%s's workers have these skills. Name the skill in the assignment, and narrow the list where that helps. A skill that starts only on request starts only when the user's request or the job's mandate calls for that kind of outcome.\n", title)
	for _, e := range entries {
		kind := "model-invoked"
		if e.UserInvoked {
			kind = "starts only on request"
		}
		fmt.Fprintf(&b, "\n## %s\n\n- Purpose: %s\n- Applies: %s\n- Invocation: %s\n", e.Name, e.Purpose, e.Applies, kind)
		if g := (Given{Source: e.Source}); e.Source != "" && !g.Qualified() {
			fmt.Fprintf(&b, "- Source: %s, unqualified\n", g.Owner())
		}
	}
	return b.String()
}
