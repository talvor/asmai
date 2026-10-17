// SPDX-License-Identifier: Apache-2.0

package skills

import (
	"io/fs"
	"slices"
	"strings"
	"testing"

	"github.com/google/licensecheck"
)

func TestTheBundleIsUpstreamsPromotedSetAtItsPinnedRelease(t *testing.T) {
	if Version() != "v1.3.1" {
		t.Errorf("the bundle is release %q, want upstream v1.3.1", Version())
	}
	if Source() != "https://github.com/mattpocock/skills" || len(Commit()) != 40 {
		t.Errorf("the bundle names source %q at commit %q", Source(), Commit())
	}
	if got := len(Names()); got != 27 {
		t.Errorf("the bundle has %d skills, want the 27 of upstream's engineering and productivity folders", got)
	}
	for _, name := range Names() {
		s, _ := Get(name)
		if s.Group != "engineering" && s.Group != "productivity" {
			t.Errorf("%s is from upstream's %q folder, want engineering or productivity", name, s.Group)
		}
		if s.Description == "" {
			t.Errorf("%s has no description", name)
		}
	}
}

func TestSkillsNotShippedAreAbsent(t *testing.T) {
	for _, name := range []string{
		"git-guardrails-claude-code", "migrate-to-shoehorn", "scaffold-exercises", "setup-pre-commit",
		"claude-handoff", "loop-me", "setup-ts-deep-modules", "writing-beats", "writing-fragments", "writing-shape",
		"lavish", "find-skills", "no-mistakes",
	} {
		if _, ok := Get(name); ok {
			t.Errorf("%s is shipped", name)
		}
		if _, err := Files(name); err == nil {
			t.Errorf("%s has files in the bundle", name)
		}
		for _, role := range Roles() {
			if slices.Contains(LeaderList(role), name) || slices.Contains(WorkerList(role), name) {
				t.Errorf("%s is on %s's list", name, role)
			}
		}
	}
}

func TestEveryShippedSkillIsUpstreamsTextUnlessAPatchIsListedWithItsReason(t *testing.T) {
	patched := map[string]bool{}
	for _, p := range Patches() {
		if p.Skill == "" || p.File == "" || strings.TrimSpace(p.Reason) == "" {
			t.Errorf("patch %+v is listed without its skill, file or reason", p)
		}
		patched[p.Skill] = true
	}
	for name, want := range Upstream() {
		got, ok := Fingerprint(name)
		if !ok {
			t.Errorf("%s is pinned but not shipped", name)
			continue
		}
		if got != want && !patched[name] {
			t.Errorf("%s differs from upstream's release and no patch is listed for it", name)
		}
		if got == want && patched[name] {
			t.Errorf("%s has a patch listed but is upstream's text", name)
		}
	}
	if len(Fingerprints()) != len(Upstream()) {
		t.Errorf("%d skills are shipped but %d are pinned", len(Fingerprints()), len(Upstream()))
	}
}

func TestFingerprintsChangeWithAnyFileOfASkill(t *testing.T) {
	files, err := Files("tdd")
	if err != nil {
		t.Fatal(err)
	}
	want, err := FingerprintFS(files)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := Fingerprint("tdd"); got != want {
		t.Errorf("Fingerprint(tdd) = %s, want %s", got, want)
	}
	other, _ := Fingerprint("grilling")
	if other == want {
		t.Error("two skills have the same fingerprint")
	}
	changed := fsWith{files, "tests.md", "changed"}
	got, err := FingerprintFS(changed)
	if err != nil || got == want {
		t.Errorf("a changed file keeps fingerprint %s (%v)", got, err)
	}
}

// fsWith is fsys with one file's content replaced.
type fsWith struct {
	fs.FS
	name, content string
}

func (f fsWith) Open(name string) (fs.File, error) {
	if name == f.name {
		return nil, fs.ErrNotExist
	}
	return f.FS.Open(name)
}

func (f fsWith) ReadFile(name string) ([]byte, error) {
	if name == f.name {
		return []byte(f.content), nil
	}
	return fs.ReadFile(f.FS, name)
}

func TestTheBundleCarriesUpstreamsMITNotice(t *testing.T) {
	cov := licensecheck.Scan(License())
	if cov.Percent < 90 || len(cov.Match) != 1 || cov.Match[0].ID != "MIT" || LicenseID() != "MIT" {
		t.Errorf("the bundle's license is %v at %.0f%%, want MIT", cov.Match, cov.Percent)
	}
	if !strings.Contains(string(License()), "Matt Pocock") {
		t.Error("the notice does not name its copyright holder")
	}
	if !strings.Contains(LicenseName(), "v1.3.1") {
		t.Errorf("LicenseName() = %q", LicenseName())
	}
}

func TestDefaultListsNameShippedSkillsAndOptionalSkillsAreOnNoList(t *testing.T) {
	onList := map[string]bool{}
	for _, role := range Roles() {
		for _, name := range slices.Concat(LeaderList(role), WorkerList(role)) {
			if _, ok := Get(name); !ok {
				t.Errorf("%s's list names %s, which is not shipped", role, name)
			}
			onList[name] = true
		}
	}
	for _, name := range Optional() {
		if _, ok := Get(name); !ok {
			t.Errorf("optional skill %s is not shipped", name)
		}
		if onList[name] {
			t.Errorf("optional skill %s is on a list", name)
		}
	}
	for _, name := range Names() {
		if !onList[name] && !slices.Contains(Optional(), name) {
			t.Errorf("%s is shipped but on no list and not optional", name)
		}
	}
	for role, want := range map[string][]string{
		"coordination": {"setup-matt-pocock-skills"},
		"research":     {"research", "to-questionnaire"},
		"quality":      {"code-review", "retro", "tdd"},
	} {
		if got := WorkerList(role); !slices.Equal(got, want) {
			t.Errorf("%s's workers have %v, want %v", role, got, want)
		}
	}
	if got := LeaderList("coordination"); !slices.Equal(got, []string{"ask-matt", "wait-what"}) {
		t.Errorf("Coordination's leader has %v", got)
	}
	if got := LeaderList("engineering"); !slices.Equal(got, []string{"pr"}) {
		t.Errorf("Engineering's leader has %v", got)
	}
	for _, name := range []string{"implement", "tdd", "pr"} {
		if !slices.Contains(WorkerList("engineering"), name) {
			t.Errorf("Engineering's workers lack %s", name)
		}
	}
}

func TestALeadersIndexIsGeneratedFromItsWorkersListsAndGivesEachSkillItsFourFacts(t *testing.T) {
	for _, role := range Roles() {
		entries := IndexOf(role)
		list := WorkerList(role)
		if len(entries) != len(list) {
			t.Errorf("%s's index has %d skills, its workers have %d", role, len(entries), len(list))
		}
		text := Index(role)
		for i, e := range entries {
			if e.Name != list[i] {
				t.Errorf("%s's index entry %d is %s, want %s", role, i, e.Name, list[i])
			}
			s, _ := Get(e.Name)
			if e.Purpose == "" || e.Applies == "" {
				t.Errorf("%s's index gives %s no purpose or no time it applies: %+v", role, e.Name, e)
			}
			if e.UserInvoked != s.UserInvoked {
				t.Errorf("%s's index has %s user-invoked = %v, upstream says %v", role, e.Name, e.UserInvoked, s.UserInvoked)
			}
			if !strings.Contains(text, "## "+e.Name+"\n") || !strings.Contains(text, e.Purpose) || !strings.Contains(text, e.Applies) {
				t.Errorf("%s's index text lacks facts of %s", role, e.Name)
			}
		}
	}
	text := Index("planning")
	if !strings.Contains(text, "starts only on request") || !strings.Contains(text, "model-invoked") {
		t.Errorf("Planning's index does not say which skills start only on request:\n%s", text)
	}
	if Index("nowhere") != "" {
		t.Error("a role with no workers has an index")
	}
	// wayfinder is user-invoked upstream and grilling is not.
	wayfinder, grilling := false, false
	for _, e := range IndexOf("planning") {
		wayfinder = wayfinder || e.Name == "wayfinder" && e.UserInvoked
		grilling = grilling || e.Name == "grilling" && !e.UserInvoked
	}
	if !wayfinder || !grilling {
		t.Error("Planning's index does not carry upstream's user-invoked marks")
	}
}

func TestTheOperatingGuideHasTheRequiredContent(t *testing.T) {
	guide := strings.ToLower(Guide())
	for what, needles := range map[string][]string{
		// Rule 7's mappings.
		"ask the user": {"decision request"},
		"subagents":    {"subagents", "assignments", "handoffs"},
		"worktrees":    {"worktrees", "workspaces", "assignment branches"},
		"integration":  {"integration branch", "job branch"},
		"setup":        {"setup-matt-pocock-skills", "repository setup"},
		"code-review":  {"/code-review", "quality"},
		"the pr skill": {"pr skill", "pr section", "instead of opening a pull request"},
		"commit":       {"commit your work to the current branch", "assignment branch"},
		// Rule 8.
		"only the list": {"only the skills on your list"},
		// Rule 9.
		"structured": {"with:\n\n- an id;", "- a title;", "- a body that", "- the options;", "- your recommendation", "grilling", "wayfinder", "lavish"},
		// Rule 37, every agent.
		"coordinate":       {"asmai inbox", "dispatch"},
		"brief":            {"asmai brief"},
		"toon":             {"pass `--toon` to every `asmai` command", "asmai brief <job> --toon"},
		"never ask":        {"never ask the user directly", "never supply the user's side"},
		"choice":           {"distinct viable approaches", "routine choices"},
		"repository":       {"never widen", "escalate a conflict"},
		"effects":          {"asmai effect"},
		"factory-changing": {"never run factory-changing commands", "tell the user the command"},
		// Rule 38.
		"pr section's form": {"evidence", "before and after", "merge danger", "conflicts"},
	} {
		for _, needle := range needles {
			if !strings.Contains(guide, needle) {
				t.Errorf("the operating guide lacks %q (%s)", needle, what)
			}
		}
	}
}
