// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/skills"
)

// fakeUpstream makes upstream's latest skill release latest, or the lookup
// fail with err, for the test.
func fakeUpstream(t *testing.T, latest string, err error) {
	t.Helper()
	previous := latestSkillRelease
	latestSkillRelease = func(context.Context) (string, error) { return latest, err }
	t.Cleanup(func() { latestSkillRelease = previous })
}

// doctorIn runs `asmai doctor` in this process.
func doctorIn(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(append([]string{"doctor"}, args...), strings.NewReader(""), &out, &errOut)
	return out.String(), errOut.String(), code
}

// hostFiles reads the files doctor must leave as they are.
func hostFiles(t *testing.T, stateDir string) map[string]string {
	t.Helper()
	got := map[string]string{}
	for _, path := range []string{
		filepath.Join(os.Getenv("HOME"), ".config", "asmai", "config.toml"),
		filepath.Join(os.Getenv("HOME"), ".claude.json"),
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		got[path] = string(data)
	}
	return got
}

// A ready factory passes every check, and doctor reports the platform, the
// provider pin, the configuration, what skill hiding cannot cover and that
// upstream has a newer skill release, changing nothing.
func TestDoctorReportsAReadyFactoryAndUpstreamsNewerRelease(t *testing.T) {
	stateDir := readyFactory(t, idleLeader)
	fakeUpstream(t, "v99.0.0", nil)
	before := hostFiles(t, stateDir)

	stdout, stderr, code := doctorIn(t)
	if code != 0 {
		t.Fatalf("asmai doctor exited %d on a ready factory, printing %q", code, stdout+stderr)
	}
	configFile := filepath.Join(os.Getenv("HOME"), ".config", "asmai", "config.toml")
	for _, want := range []string{
		"Platform       ", "Provider pin   Claude Code 2.1.292", "Configuration  " + configFile,
		"ok      staffing", "ok      Claude Code 2.1.292", "ok      Claude Code version", "ok      Claude Code sign-in", "ok      permission rules", "ok      directory trust",
		"Claude Code's built-in slash commands, such as /security-review and /init",
		"Upstream has a newer skill release: v99.0.0, and the bundle is " + skills.Version(),
		"Every check passed.",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("asmai doctor printed\n%s\nwant it to say %q", stdout, want)
		}
	}
	if !strings.Contains(stdout, "Added and repository skills (unqualified") || !strings.Contains(stdout, "  none") {
		t.Errorf("asmai doctor printed\n%s\nwant it to list no added or repository skills", stdout)
	}
	after := hostFiles(t, stateDir)
	for path, text := range before {
		if after[path] != text {
			t.Errorf("asmai doctor changed %s", path)
		}
	}
}

// With the bundle upstream's latest, doctor says so; with the lookup
// failing, it says it could not look, and still passes.
func TestDoctorSaysWhenUpstreamIsNotNewerOrCannotBeReached(t *testing.T) {
	readyFactory(t, idleLeader)

	fakeUpstream(t, skills.Version(), nil)
	stdout, stderr, code := doctorIn(t)
	if want := "is upstream's latest skill release"; code != 0 || !strings.Contains(stdout, want) || strings.Contains(stdout, "newer skill release") {
		t.Errorf("with the bundle the latest, asmai doctor exited %d printing %q, want it to say %q", code, stdout+stderr, want)
	}

	fakeUpstream(t, "", errors.New("no route to host"))
	stdout, stderr, code = doctorIn(t)
	if want := "Could not look up upstream's latest release"; code != 0 || !strings.Contains(stdout, want) || !strings.Contains(stdout, "no route to host") {
		t.Errorf("with upstream out of reach, asmai doctor exited %d printing %q, want it to say %q", code, stdout+stderr, want)
	}

	fakeUpstream(t, "v0.0.1", nil)
	stdout, stderr, code = doctorIn(t)
	if code != 0 || !strings.Contains(stdout, "No newer upstream skill release was found") || strings.Contains(stdout, "is upstream's latest skill release") {
		t.Errorf("with an older upstream release, doctor exited %d printing %q", code, stdout+stderr)
	}
}

// Added skills and a repository's switched-on skills are listed as
// unqualified, with whose they are and for which roles.
func TestDoctorMarksAddedAndRepositorySkillsUnqualified(t *testing.T) {
	stateDir := readyFactory(t, idleLeader)
	fakeUpstream(t, skills.Version(), nil)
	added := addedSkill(t, t.TempDir(), "house-style", "Write code in the house style.")

	var out, errOut bytes.Buffer
	if code := run([]string{"repo", "add", fixtureOrigin(t), "--repository-skills", "engineering"}, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("asmai repo add exited %d: %s%s", code, out.String(), errOut.String())
	}
	clone := filepath.Join(stateDir, "repositories", "fixture")
	own := addedSkill(t, filepath.Join(clone, ".claude", "skills"), "house-rules", "Follow the fixture's rules.")
	configFile := filepath.Join(os.Getenv("HOME"), ".config", "asmai", "config.toml")
	text, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(text), "[roles.engineering]\n", "[roles.engineering]\nworker_added_skills = ["+quoteTOML(added)+"]\n", 1)
	if edited == string(text) {
		t.Fatalf("the configuration has no [roles.engineering] table:\n%s", text)
	}
	if err := os.WriteFile(configFile, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := doctorIn(t)
	if code != 0 {
		t.Fatalf("asmai doctor exited %d printing %q", code, stdout+stderr)
	}
	for _, want := range []string{
		"house-style  unqualified, the user's skill, added for engineering",
		added,
		"house-rules  unqualified, the repository's skill, repository fixture, switched on for engineering",
		own,
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("asmai doctor printed\n%s\nwant it to say %q", stdout, want)
		}
	}

	var report struct {
		Unqualified []struct {
			Name       string   `json:"name"`
			Source     string   `json:"source"`
			Roles      []string `json:"roles"`
			Repository string   `json:"repository"`
			Qualified  bool     `json:"qualified"`
		} `json:"unqualified_skills"`
		HidingGaps []struct {
			What string `json:"What"`
		} `json:"hiding_gaps"`
		Upstream struct {
			Newer bool `json:"newer"`
		} `json:"upstream"`
	}
	stdout, stderr, code = doctorIn(t, "--json")
	if err := json.Unmarshal([]byte(stdout), &report); code != 0 || err != nil {
		t.Fatalf("asmai doctor --json exited %d printing %q (%v)", code, stdout+stderr, err)
	}
	if len(report.Unqualified) != 2 || report.Unqualified[0].Name != "house-style" || report.Unqualified[0].Source != skills.User ||
		report.Unqualified[1].Name != "house-rules" || report.Unqualified[1].Repository != "fixture" || report.Unqualified[1].Source != skills.Repository {
		t.Errorf("asmai doctor --json lists %+v, want the added house-style and the repository's house-rules", report.Unqualified)
	}
	if len(report.HidingGaps) == 0 || report.Upstream.Newer {
		t.Errorf("asmai doctor --json reported hiding gaps %+v and upstream newer %v, want gaps and no newer release", report.HidingGaps, report.Upstream.Newer)
	}
}

func TestDoctorSuggestsEditingConfigurationForInvalidRepositorySkill(t *testing.T) {
	stateDir := readyFactory(t, idleLeader)
	fakeUpstream(t, skills.Version(), nil)
	stdout, stderr, code := runIn(t, "", "repo", "add", fixtureOrigin(t), "--repository-skills", "engineering")
	if code != 0 {
		t.Fatalf("repo add exited %d printing %q", code, stdout+stderr)
	}
	skill := addedSkill(t, filepath.Join(stateDir, "repositories", "fixture", ".claude", "skills"), "house-rules", "Follow the fixture's rules.")
	if err := os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte(skillFile("another-name", "Follow the fixture's rules.")), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code = doctorIn(t, "--json")
	if code != 1 {
		t.Fatalf("doctor with an invalid repository skill exited %d printing %q", code, stdout+stderr)
	}
	var report struct {
		Checks []daemon.Check `json:"checks"`
	}
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("doctor --json printed %q: %v", stdout, err)
	}
	for _, check := range report.Checks {
		if check.Name == "repository skills" {
			if check.OK || !strings.Contains(check.Fix, "`repository_skills`") || !strings.Contains(check.Fix, "`asmai config apply`") || strings.Contains(check.Fix, "repo add") {
				t.Errorf("doctor gave an unusable repository skill fix: %+v", check)
			}
			return
		}
	}
	t.Errorf("doctor omitted the invalid repository skill check: %+v", report.Checks)
}

func quoteTOML(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// With the pinned Claude Code missing and the roles unstaffed, doctor names
// each failed check with its fix and exits 1, as `asmai start` does.
func TestDoctorFailsNamingTheFixOfEachFailedCheck(t *testing.T) {
	factoryHome(t)
	standInChannelPinning(t, []byte("claude"), nil)
	fakeUpstream(t, skills.Version(), nil)

	stdout, stderr, code := doctorIn(t)
	if code != 1 {
		t.Errorf("asmai doctor exited %d on a host that was never initialised, want 1; it printed %q", code, stdout+stderr)
	}
	for _, want := range []string{
		"failed  staffing", "does not exist, so no role is staffed", "fix: run `asmai init` to write the configuration file",
		"failed  Claude Code 2.1.292", "fix: run `asmai providers install`",
		"failed  directory trust", "fix: run `asmai init`, which opens Claude Code there for you to trust it",
	} {
		if !strings.Contains(stdout+stderr, want) {
			t.Errorf("asmai doctor printed\n%s\nwant it to say %q", stdout+stderr, want)
		}
	}
}

// An agent cannot run doctor.
func TestAnAgentCannotRunDoctor(t *testing.T) {
	t.Setenv(daemon.SessionCredential, "a-session")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"doctor"}, strings.NewReader(""), &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "ask the user to run `asmai doctor`") {
		t.Errorf("an agent's asmai doctor exited %d printing %q, want it refused", code, stderr.String())
	}
}

// Doctor reports the prefix new branches go under and where it comes from,
// and makes no ID of its own.
func TestDoctorReportsTheBranchPrefixAndItsSource(t *testing.T) {
	stateDir := readyFactory(t, idleLeader)
	fakeUpstream(t, "v99.0.0", nil)
	idFile := filepath.Join(stateDir, "branch-id")
	os.Remove(idFile)

	stdout, _, _ := doctorIn(t)
	if !strings.Contains(stdout, "Branch prefix  none yet; ") {
		t.Errorf("with no ID made, asmai doctor printed\n%s\nwant it to say the prefix is not made yet", stdout)
	}
	if _, err := os.Stat(idFile); err == nil {
		t.Error("asmai doctor made a branch ID")
	}

	if err := os.WriteFile(idFile, []byte("k3x9qz\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, _, _ = doctorIn(t)
	if !strings.Contains(stdout, "Branch prefix  k3x9qz (from the ID kept in the state directory)") {
		t.Errorf("with an ID kept, asmai doctor printed\n%s\nwant its prefix and source", stdout)
	}

	configFile := filepath.Join(os.Getenv("HOME"), ".config", "asmai", "config.toml")
	text, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configFile, append([]byte("[branches]\nbranch_prefix = \"alice\"\n\n"), text...), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, _, _ = doctorIn(t)
	if !strings.Contains(stdout, "Branch prefix  alice (from branch_prefix in the configuration file)") {
		t.Errorf("with branch_prefix set, asmai doctor printed\n%s\nwant its prefix and source", stdout)
	}
}
