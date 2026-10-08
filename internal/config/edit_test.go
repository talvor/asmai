// SPDX-License-Identifier: Apache-2.0

package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, text string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), mode); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

var otman = Repository{Location: "/home/me/src/otman", Origin: "git@github.com:me/otman.git", DefaultBranch: "main"}

func TestAddingARepositoryCreatesTheFileAndItReadsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "asmai", "config.toml")
	if err := AddRepository(path, "otman", otman); err != nil {
		t.Fatal(err)
	}
	cfg, problems, err := Load(path)
	if err != nil || len(problems) != 0 || cfg.Repositories["otman"] != otman {
		t.Errorf("the new file reads as %+v, %v, %v; want the repository back", cfg, problems, err)
	}
	if got := readFile(t, path); !strings.HasPrefix(got, "[repositories.otman]\n") || strings.Count(got, "\n") != 4 {
		t.Errorf("the new file is %q, want the one table", got)
	}
}

func TestAddingARepositoryKeepsTheRestOfTheFileAsItIs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, staffed[:len(staffed)-1], 0o600)

	// A path and a branch that TOML has to escape.
	odd := Repository{Location: `/home/me/my "src"/o\tman`, Origin: "https://example.test/o.git", DefaultBranch: "feature/x"}
	if err := AddRepository(path, "otman", odd); err != nil {
		t.Fatal(err)
	}
	if err := AddRepository(path, "fixture", Repository{Origin: "o", DefaultBranch: "trunk"}); err != nil {
		t.Fatal(err)
	}

	got := readFile(t, path)
	if !strings.HasPrefix(got, staffed+"\n[repositories.otman]\n") {
		t.Errorf("the file is\n%s\nwant the staffing, comments and all, untouched, then the entry", got)
	}
	cfg, problems := Parse([]byte(got))
	if len(problems) != 0 || cfg.Repositories["otman"] != odd || cfg.Repositories["fixture"].DefaultBranch != "trunk" || cfg.Roles["quality"].WorkerModel != "haiku" {
		t.Errorf("the file reads as %+v, %v", cfg, problems)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("the file's mode is %v (%v), want it kept at -rw-------", info.Mode().Perm(), err)
	}
}

func TestAddingARepositoryRefusesWhatItWouldDisturb(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, "[repositories.otman]\norigin = \"o\"\ndefault_branch = \"main\"\n", 0o644)
	if err := AddRepository(path, "otman", otman); !errors.Is(err, ErrRepositoryEntryExists) {
		t.Errorf("adding a name the file has returned %v, want ErrRepositoryEntryExists", err)
	}

	broken := "[roles.coordination]\nleader_provider = \"codex\"\n"
	writeFile(t, path, broken, 0o644)
	var problems *ProblemsError
	if err := AddRepository(path, "fixture", otman); !errors.As(err, &problems) || problems.Problems[0].Line != 2 {
		t.Errorf("adding to a file with a problem returned %v, want its ProblemsError", err)
	}
	if got := readFile(t, path); got != broken {
		t.Errorf("the refused add changed the file to %q", got)
	}
	if err := AddRepository(path, "a.b", otman); err == nil {
		t.Error("adding a name that is not a table key succeeded")
	}
}

func TestAddingThroughASymbolicLinkReplacesItsTargetAndKeepsTheLink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "dotfiles", "asmai.toml")
	writeFile(t, target, staffed, 0o644)
	link := filepath.Join(dir, "config.toml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := AddRepository(link, "otman", otman); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the configuration file is no longer a symbolic link (%v)", err)
	}
	if got := readFile(t, target); !strings.Contains(got, "[repositories.otman]") {
		t.Errorf("the link's target is\n%s\nwant the entry in it", got)
	}
}

func TestRemovingARepositoryRemovesOnlyItsTable(t *testing.T) {
	const file = `# Staffing.
[roles.coordination]
leader_model = "opus"

# otman, which I work on.
[repositories.otman]
location = "/home/me/src/otman"
origin = "o"
default_branch = "main"

# The fixture.
[repositories.fixture]
origin = "f"
default_branch = "trunk"

[repositories.last]
origin = "l"
default_branch = "main"
`
	path := filepath.Join(t.TempDir(), "config.toml")
	for name, want := range map[string]string{
		"otman":   strings.Replace(file, "# otman, which I work on.\n[repositories.otman]\nlocation = \"/home/me/src/otman\"\norigin = \"o\"\ndefault_branch = \"main\"\n\n", "", 1),
		"fixture": strings.Replace(file, "# The fixture.\n[repositories.fixture]\norigin = \"f\"\ndefault_branch = \"trunk\"\n\n", "", 1),
		"last":    strings.Replace(file, "\n[repositories.last]\norigin = \"l\"\ndefault_branch = \"main\"\n", "", 1),
	} {
		writeFile(t, path, file, 0o644)
		removed, err := RemoveRepository(path, name)
		if err != nil || !removed {
			t.Fatalf("removing %s returned %v, %v", name, removed, err)
		}
		if got := readFile(t, path); got != want {
			t.Errorf("after removing %s the file is\n%s\nwant\n%s", name, got, want)
		}
		if cfg, problems := Parse([]byte(readFile(t, path))); len(problems) != 0 {
			t.Errorf("after removing %s the file has problems %v", name, problems)
		} else if _, ok := cfg.Repositories[name]; ok {
			t.Errorf("after removing %s the file still has it", name)
		}
	}
}

func TestRemovingTheOnlyContentLeavesAnEmptyFileAndAMissingEntryChangesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if removed, err := RemoveRepository(path, "otman"); err != nil || removed {
		t.Errorf("removing from a missing file returned %v, %v", removed, err)
	}
	if err := AddRepository(path, "otman", otman); err != nil {
		t.Fatal(err)
	}
	if removed, err := RemoveRepository(path, "fixture"); err != nil || removed {
		t.Errorf("removing an entry the file lacks returned %v, %v", removed, err)
	}
	if removed, err := RemoveRepository(path, "otman"); err != nil || !removed {
		t.Fatalf("removing the entry returned %v, %v", removed, err)
	}
	if got := readFile(t, path); got != "" {
		t.Errorf("the file is %q, want it empty", got)
	}
}

func TestAnEntryNotWrittenAsATableIsLeftForTheUserToRemove(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	const file = "[repositories]\notman = { origin = \"o\", default_branch = \"main\" }\n"
	writeFile(t, path, file, 0o644)
	if _, err := RemoveRepository(path, "otman"); !errors.Is(err, ErrRepositoryEntryNotATable) {
		t.Errorf("removing an inline entry returned %v, want ErrRepositoryEntryNotATable", err)
	}
	if got := readFile(t, path); got != file {
		t.Errorf("the refused removal changed the file to %q", got)
	}
}
