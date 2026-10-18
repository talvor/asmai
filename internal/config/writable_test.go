// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const goEntry = `[repositories.otman]
origin = "o"
default_branch = "main"

[[repositories.otman.writable]]
path = "~/.cache/go-build"
env = "GOCACHE"

[[repositories.otman.writable]]
path = "/var/cache/tools"
`

func TestAWritableFolderIsReadFromItsTableWithItsOptionalVariable(t *testing.T) {
	cfg, problems := Parse([]byte(goEntry))
	if len(problems) != 0 {
		t.Fatalf("Parse found %v", problems)
	}
	want := []Writable{{Path: "~/.cache/go-build", Env: "GOCACHE"}, {Path: "/var/cache/tools"}}
	if got := cfg.Options["otman"].Writable; !slices.Equal(got, want) {
		t.Errorf("[repositories.otman] declares %+v, want %+v", got, want)
	}
	if _, ok := cfg.Repositories["otman"]; !ok {
		t.Error("the repository is not in the file")
	}
	cfg, problems = Parse([]byte("[[repositories.bare.writable]]\npath = \"/a\"\n"))
	if _, ok := cfg.Repositories["bare"]; ok || len(problems) != 1 || !strings.Contains(problems[0].Problem, "has no [repositories.bare] entry") {
		t.Errorf("a writable folder without a repository is %v, %v", cfg.Repositories, problems)
	}
	cfg, problems = Parse([]byte("[[repositories.bare.writable]]\npath = \"/a\"\n[repositories.bare]\norigin = \"o\"\n"))
	if len(problems) != 0 || !slices.Equal(cfg.Options["bare"].Writable, []Writable{{Path: "/a"}}) {
		t.Errorf("a repository declared after its folder is %v, %v", cfg.Options["bare"].Writable, problems)
	}
	// None declared is none.
	cfg, _ = Parse([]byte("[repositories.otman]\norigin = \"o\"\n"))
	if got := cfg.Options["otman"].Writable; len(got) != 0 {
		t.Errorf("a repository declaring none has %+v", got)
	}
}

func TestAWritableFolderThisAsmaiCannotHonourNamesTheLineAndTheFix(t *testing.T) {
	const entry = "[repositories.otman]\norigin = \"o\"\n"
	for name, tc := range map[string]struct {
		file    string
		line    int
		problem string
		fix     string
	}{
		"no path":                      {entry + "[[repositories.otman.writable]]\nenv = \"X\"\n", 3, "[[repositories.otman.writable]] has no path", "add path"},
		"a relative path":              {entry + "[[repositories.otman.writable]]\npath = \"cache\"\n", 4, `writable folder "cache" is not an absolute path`, "absolute path"},
		"a user's home":                {entry + "[[repositories.otman.writable]]\npath = \"~me/cache\"\n", 4, "is not an absolute path", "~/"},
		"an empty path":                {entry + "[[repositories.otman.writable]]\npath = \"\"\n", 4, `"" is not a path`, "path"},
		"an explicitly empty variable": {entry + "[[repositories.otman.writable]]\npath = \"/a\"\nenv = \"\"\n", 5, `exports "", which is not an environment variable name`, "GOCACHE"},
		"a variable that is no name":   {entry + "[[repositories.otman.writable]]\npath = \"/a\"\nenv = \"1X\"\n", 5, `exports "1X", which is not an environment variable name`, "GOCACHE"},
		"HOME":                         {entry + "[[repositories.otman.writable]]\npath = \"/a\"\nenv = \"HOME\"\n", 5, `exports "HOME"`, "remove env"},
		"PATH":                         {entry + "[[repositories.otman.writable]]\npath = \"/a\"\nenv = \"PATH\"\n", 5, `exports "PATH"`, "remove env"},
		"TMPDIR":                       {entry + "[[repositories.otman.writable]]\npath = \"/a\"\nenv = \"TMPDIR\"\n", 5, `exports "TMPDIR"`, "remove env"},
		"a git variable":               {entry + "[[repositories.otman.writable]]\npath = \"/a\"\nenv = \"GIT_DIR\"\n", 5, `exports "GIT_DIR"`, "remove env"},
		"the slot variable":            {entry + "[[repositories.otman.writable]]\npath = \"/a\"\nenv = \"ASMAI_SLOT\"\n", 5, `exports "ASMAI_SLOT"`, "remove env"},
		"one variable twice":           {entry + "[[repositories.otman.writable]]\npath = \"/a\"\nenv = \"X\"\n[[repositories.otman.writable]]\npath = \"/b\"\nenv = \"X\"\n", 8, `"/b" exports "X", which "/a" already exports`, "own variable"},
		"a misspelt field":             {entry + "[[repositories.otman.writable]]\npath = \"/a\"\nvar = \"X\"\n", 5, "var in [[repositories.otman.writable]] is not a setting this asmai reads; the fields are path and env", "remove var"},
		"a path that is no string":     {entry + "[[repositories.otman.writable]]\npath = 4\n", 4, "path in [[repositories.otman.writable]] is a number, not a string", "double quotes"},
		"a list in the entry":          {entry + "writable = [\"/a\"]\n", 3, "writable in [repositories.otman] is not written as a list", "[[repositories.otman.writable]]"},
		"another array of tables":      {"[[roles.coordination]]\nleader_model = \"x\"\n", 1, "[[roles.coordination]] is an array of tables", "its own table"},
		"a repository with a bad name": {"[[repositories.\"a/b\".writable]]\npath = \"/a\"\n", 1, `"a/b" is not a repository name`, "rename"},
	} {
		t.Run(name, func(t *testing.T) {
			_, problems := Parse([]byte(tc.file))
			if len(problems) != 1 {
				t.Fatalf("Parse found %v, want one problem", problems)
			}
			p := problems[0]
			if p.Line != tc.line || !strings.Contains(p.Problem, tc.problem) || !strings.Contains(p.Fix, tc.fix) {
				t.Errorf("the problem is line %d: %q, fix %q; want line %d: %q, fix %q", p.Line, p.Problem, p.Fix, tc.line, tc.problem, tc.fix)
			}
		})
	}
}

func TestWritableTableReportsItsFirstReason(t *testing.T) {
	file := "[repositories.otman]\norigin = \"o\"\n[[repositories.otman.writable]]\npath = \"relative\"\nenv = \"\"\n"
	_, problems := Parse([]byte(file))
	if len(problems) != 1 {
		t.Fatalf("Parse found %+v, want one path problem", problems)
	}
	if problems[0].Line != 4 || !strings.Contains(problems[0].Problem, `"relative" is not an absolute path`) {
		t.Errorf("path problem: %+v", problems[0])
	}
}

func TestWritableListReportsOneReasonPerEntry(t *testing.T) {
	bounds := NewBounds("/home/me", "/home/me/.local/state/asmai", "/home/me/.local/state/asmai/repositories", []string{"otman"})
	err := CheckWritable("otman", []Writable{{Path: "~/.ssh", Env: "PATH"}}, bounds)
	problems, ok := err.(*EntryProblemsError)
	if !ok || len(problems.Problems) != 1 {
		t.Fatalf("CheckWritable returned %v, want one reason", err)
	}
	if !strings.Contains(problems.Problems[0].Problem, `exports "PATH"`) {
		t.Errorf("CheckWritable returned %+v", problems.Problems)
	}
	err = CheckWritable("otman", []Writable{{Path: "~/.ssh"}}, bounds)
	if err == nil || !strings.Contains(err.Error(), `is ~/.ssh`) {
		t.Errorf("CheckWritable after fixing env returned %v", err)
	}
}

func TestTheFoldersARepositoryOpensAreResolvedAndListed(t *testing.T) {
	cfg, _ := Parse([]byte(goEntry + "\n[repositories.plain]\norigin = \"o\"\n"))
	open := cfg.OpenFolders("/home/me")
	if len(open) != 1 || open[0].Repository != "otman" {
		t.Fatalf("the folders opened are %+v, want otman's alone", open)
	}
	want := []Folder{{Path: "~/.cache/go-build", Resolved: "/home/me/.cache/go-build", Env: "GOCACHE"}, {Path: "/var/cache/tools", Resolved: "/var/cache/tools"}}
	if !slices.Equal(open[0].Folders, want) {
		t.Errorf("the folders are %+v, want %+v", open[0].Folders, want)
	}
	for path, want := range map[string]string{"~": "/home/me", "~/a/../b": "/home/me/b", "/x/./y/": "/x/y"} {
		if got, err := Resolve("/home/me", path); err != nil || got != want {
			t.Errorf("Resolve(%q) = %q, %v; want %q", path, got, err, want)
		}
	}
	if _, err := Resolve("/home/me", "rel"); err == nil {
		t.Error("a relative path resolved")
	}
}

func TestWritableFoldersGivenOnTheCommandLine(t *testing.T) {
	for text, want := range map[string]Writable{
		"~/.cache/go-build=GOCACHE": {Path: "~/.cache/go-build", Env: "GOCACHE"},
		"/var/cache/tools":          {Path: "/var/cache/tools"},
		"/a=b=C":                    {Path: "/a=b", Env: "C"},
	} {
		if got, err := ParseWritable(text); err != nil || got != want {
			t.Errorf("ParseWritable(%q) = %+v, %v; want %+v", text, got, err, want)
		}
	}
	for _, text := range []string{"", "  ", "/a="} {
		if _, err := ParseWritable(text); err == nil {
			t.Errorf("ParseWritable(%q) was accepted", text)
		}
	}
}

func TestTheLanguageOfAClonePicksItsSuggestedFolders(t *testing.T) {
	dir := t.TempDir()
	if got := DetectWritable(dir); len(got) != 0 {
		t.Errorf("an empty clone suggests %+v", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module m\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, want := DetectWritable(dir), []Writable{{Path: "~/.cache/go-build", Env: "GOCACHE"}}; !slices.Equal(got, want) {
		t.Errorf("a Go clone suggests %+v, want %+v", got, want)
	}
	// A folder named like a marker is not one.
	other := t.TempDir()
	if err := os.Mkdir(filepath.Join(other, "go.mod"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := DetectWritable(other); len(got) != 0 {
		t.Errorf("a directory named go.mod suggests %+v", got)
	}
}

func TestTheBoundsRefuseTheFoldersThatWouldOpenTooMuch(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home", "me")
	state := filepath.Join(home, ".local", "state", "asmai")
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	b := NewBounds(home, state, filepath.Join(state, "repositories"), []string{"otman"})
	refused := map[string]string{
		home:                                "your home directory or one of its ancestors",
		filepath.Dir(home):                  "your home directory or one of its ancestors",
		root:                                "your home directory or one of its ancestors",
		"/":                                 "your home directory or one of its ancestors",
		"~":                                 "your home directory or one of its ancestors",
		"~/.ssh":                            "is ~/.ssh, where your SSH keys are",
		"~/.ssh/keys":                       "is inside ~/.ssh",
		"~/.claude":                         "is ~/.claude",
		"~/.claude/projects":                "is inside ~/.claude",
		"~/.claude.json":                    "is ~/.claude.json",
		"~/../me/.ssh":                      "is ~/.ssh",
		"~/.local/state/asmai":              "is AsmAI's state directory",
		"~/.local/state/asmai/store":        "is inside AsmAI's state directory",
		"~/.local/state/asmai/repositories": "is inside AsmAI's state directory",
		"~/.local/state/asmai/repositories/otman":     "is AsmAI's clone of otman",
		"~/.local/state/asmai/repositories/otman/src": "is inside AsmAI's clone of otman",
	}
	for path, want := range refused {
		why, fix := b.check(Writable{Path: path})
		if !strings.Contains(why, want) || fix == "" {
			t.Errorf("%q: the bounds said %q (fix %q), want %q", path, why, fix, want)
		}
	}
	for _, path := range []string{"~/.cache/go-build", "~/work", "/var/cache/tools", "~/.sshd", "~/.claudes", "~/.local/state/other"} {
		if why, _ := b.check(Writable{Path: path}); why != "" {
			t.Errorf("%q was refused: %s", path, why)
		}
	}
}

func TestASymbolicLinkIsNoWayIntoAClosedPlace(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, ".ssh"), filepath.Join(home, "cache")); err != nil {
		t.Fatal(err)
	}
	b := NewBounds(home, filepath.Join(root, "state"), filepath.Join(root, "state", "repositories"), nil)
	for _, path := range []string{"~/cache", "~/cache/more"} {
		if why, _ := b.check(Writable{Path: path}); !strings.Contains(why, "~/.ssh") {
			t.Errorf("%q was said to be %q, want it refused as ~/.ssh", path, why)
		}
	}
}

func TestEveryRepositoryOfTheFileIsCheckedAgainstTheBounds(t *testing.T) {
	cfg, problems := Parse([]byte(goEntry + "\n[repositories.bad]\norigin = \"b\"\n[[repositories.bad.writable]]\npath = \"~/.ssh\"\n"))
	if len(problems) != 0 {
		t.Fatal(problems)
	}
	found := cfg.WritableProblems(NewBounds("/home/me", "/home/me/.local/state/asmai", "/home/me/.local/state/asmai/repositories", []string{"otman", "bad"}))
	if len(found) != 1 || !strings.Contains(found[0].Problem, `[repositories.bad] writable folder "~/.ssh" is ~/.ssh`) {
		t.Errorf("the problems are %+v, want only bad's ~/.ssh", found)
	}
}

func TestWritableFoldersAreWrittenIntoANewEntryAndReadBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("# mine\n[roles.engineering]\nleader_model = \"opus\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	options := DefaultRepositoryOptions()
	options.Writable = []Writable{{Path: "~/.cache/go-build", Env: "GOCACHE"}, {Path: "/var/cache/tools"}}
	if err := AddRepositoryWithOptions(path, "otman", Repository{Origin: "o", DefaultBranch: "main"}, options); err != nil {
		t.Fatal(err)
	}
	cfg, problems, err := Load(path)
	if err != nil || len(problems) != 0 {
		t.Fatalf("the file reads as %v, %v", problems, err)
	}
	if got := cfg.Options["otman"].Writable; !slices.Equal(got, options.Writable) {
		t.Errorf("the entry declares %+v, want %+v", got, options.Writable)
	}
	data, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(data), "# mine\n") || !strings.Contains(string(data), "\n[[repositories.otman.writable]]\npath = \"~/.cache/go-build\"\nenv = \"GOCACHE\"\n\n[[repositories.otman.writable]]\npath = \"/var/cache/tools\"\n") {
		t.Errorf("the file is\n%s", data)
	}
	// Removing the entry removes its folders with it.
	if removed, err := RemoveRepository(path, "otman"); err != nil || !removed {
		t.Fatalf("removing the entry returned %v, %v", removed, err)
	}
	if data, _ := os.ReadFile(path); strings.Contains(string(data), "writable") {
		t.Errorf("the entry's folders stayed behind:\n%s", data)
	}
}

func TestAnEntryWithAFolderTheFileWouldNotHonourIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	options := DefaultRepositoryOptions()
	options.Writable = []Writable{{Path: "~/.cache/x", Env: "HOME"}}
	if err := CheckRepositoryOptions(options); err == nil || !strings.Contains(err.Error(), `exports "HOME"`) {
		t.Errorf("CheckRepositoryOptions returned %v, want HOME refused", err)
	}
	err := AddRepositoryWithOptions(path, "otman", Repository{Origin: "o", DefaultBranch: "main"}, options)
	if err == nil || !strings.Contains(err.Error(), `exports "HOME"`) {
		t.Errorf("AddRepositoryWithOptions returned %v, want HOME refused", err)
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Error("a file was written for a refused entry")
	}
}

func TestWritableFoldersAreRenderedAndDiffed(t *testing.T) {
	cfg, problems := Parse([]byte(goEntry))
	if len(problems) != 0 {
		t.Fatal(problems)
	}
	rendered := string(Render(cfg))
	if !strings.Contains(rendered, "[[repositories.otman.writable]]\npath = \"~/.cache/go-build\"\nenv = \"GOCACHE\"\n") {
		t.Errorf("the rendered file lacks the folders:\n%s", rendered)
	}
	again, problems := Parse([]byte(rendered))
	if len(problems) != 0 || !slices.Equal(again.Options["otman"].Writable, cfg.Options["otman"].Writable) {
		t.Errorf("the rendered file reads as %+v, %v", again.Options["otman"].Writable, problems)
	}

	none, _ := Parse([]byte("[repositories.otman]\norigin = \"o\"\ndefault_branch = \"main\"\n"))
	changes := Diff(none, cfg)
	if len(changes) != 1 || changes[0].Setting != "repositories.otman.writable" || !changes[0].Folders || changes[0].Repository != "otman" ||
		!slices.Equal(changes[0].Gained, []string{`{ path = "~/.cache/go-build", env = "GOCACHE" }`, `{ path = "/var/cache/tools" }`}) {
		t.Errorf("opening folders is %+v", changes)
	}
	changes = Diff(cfg, none)
	if len(changes) != 1 || !changes[0].Folders || !slices.Equal(changes[0].Lost, []string{`{ path = "~/.cache/go-build", env = "GOCACHE" }`, `{ path = "/var/cache/tools" }`}) {
		t.Errorf("closing folders is %+v", changes)
	}
	// A repository that is new with folders is a change of its folders too.
	changes = Diff(Config{Options: map[string]RepositoryOptions{}, Repositories: map[string]Repository{}}, cfg)
	if !slices.ContainsFunc(changes, func(c Change) bool { return c.Folders && len(c.Gained) == 2 }) {
		t.Errorf("a new repository with folders is %+v", changes)
	}
}

func TestWritableDiffDistinguishesEqualsInPathFromEnv(t *testing.T) {
	const entry = "[repositories.otman]\norigin = \"o\"\n"
	pathOnly, pathProblems := Parse([]byte(entry + "[[repositories.otman.writable]]\npath = \"/a=B\"\n"))
	withEnv, envProblems := Parse([]byte(entry + "[[repositories.otman.writable]]\npath = \"/a\"\nenv = \"B\"\n"))
	if len(pathProblems) != 0 || len(envProblems) != 0 {
		t.Fatalf("Parse found %v and %v", pathProblems, envProblems)
	}
	for _, tc := range []struct {
		before, after Config
		gained, lost  string
	}{
		{pathOnly, withEnv, `{ path = "/a", env = "B" }`, `{ path = "/a=B" }`},
		{withEnv, pathOnly, `{ path = "/a=B" }`, `{ path = "/a", env = "B" }`},
	} {
		changes := Diff(tc.before, tc.after)
		if len(changes) != 1 || !changes[0].Folders || changes[0].Before == changes[0].After ||
			!slices.Equal(changes[0].Gained, []string{tc.gained}) || !slices.Equal(changes[0].Lost, []string{tc.lost}) {
			t.Errorf("writable change is %+v, want gained %q and lost %q", changes, tc.gained, tc.lost)
		}
	}
}

func TestRemovingAnEntryRemovesEveryTableOfItsFolders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	file := `[repositories.keep]
origin = "k"
default_branch = "main"

[[repositories.keep.writable]]
path = "/keep"

# otman's folder, written first
[[repositories.otman.writable]]
path = "/a"

[repositories.otman]
origin = "o"
default_branch = "main"

[[repositories.otman.writable]]
path = "/b"
env = "B"

[roles.engineering]
leader_model = "opus"
`
	if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	if removed, err := RemoveRepository(path, "otman"); err != nil || !removed {
		t.Fatalf("removing the entry returned %v, %v", removed, err)
	}
	want := `[repositories.keep]
origin = "k"
default_branch = "main"

[[repositories.keep.writable]]
path = "/keep"

[roles.engineering]
leader_model = "opus"
`
	if got, _ := os.ReadFile(path); string(got) != want {
		t.Errorf("the file is\n%s\nwant\n%s", got, want)
	}
}
