// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ClaudeCodeRecord returns the file Claude Code keeps its record of the
// user's directory trust in: .claude.json in CLAUDE_CONFIG_DIR when getenv
// sets it, and ~/.claude.json, in home, otherwise. AsmAI only ever reads it.
func ClaudeCodeRecord(home string, getenv func(string) string) string {
	if dir := getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, ".claude.json")
	}
	return filepath.Join(home, ".claude.json")
}

// TrustFolder returns the folder whose trust Claude Code looks for when it
// starts in dir. In a git repository, or in a worktree of one, that is the
// main repository's root, the folder whose .git directory holds the
// repository, and repository is true: on the pinned Claude Code, trust given
// to a folder above a repository does not cover it, and trust given to the
// main repository covers each of its worktrees. Elsewhere it is dir, which
// trust given to a folder above it covers. TrustFolder reads git's own files
// and runs no git.
func TrustFolder(dir string) (folder string, repository bool) {
	dir = filepath.Clean(dir)
	for d := dir; ; {
		dotGit := filepath.Join(d, ".git")
		if info, err := os.Stat(dotGit); err == nil {
			if info.IsDir() {
				return d, true
			}
			if root, ok := mainRepository(dotGit); ok {
				return root, true
			}
			return d, true
		}
		parent := filepath.Dir(d)
		if parent == d {
			return dir, false
		}
		d = parent
	}
}

// mainRepository reads a worktree's .git file, which names the worktree's git
// directory inside the main repository's, and returns the main repository's
// root: the folder holding the .git directory the worktree shares. It
// reports false for a .git file it cannot follow there, such as a
// submodule's.
func mainRepository(file string) (string, bool) {
	data, err := os.ReadFile(file)
	if err != nil {
		return "", false
	}
	gitDir, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir:")
	if !ok {
		return "", false
	}
	gitDir = strings.TrimSpace(gitDir)
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(filepath.Dir(file), gitDir)
	}
	common, err := os.ReadFile(filepath.Join(gitDir, "commondir"))
	if err != nil {
		return "", false
	}
	shared := strings.TrimSpace(string(common))
	if !filepath.IsAbs(shared) {
		shared = filepath.Join(gitDir, shared)
	}
	shared = filepath.Clean(shared)
	if filepath.Base(shared) != ".git" {
		return "", false
	}
	return filepath.Dir(shared), true
}

// Trusted reports whether the Claude Code record at path holds the user's
// trust of dir: their "Yes, I trust this folder", given for the folder
// TrustFolder returns for dir. For a repository or a worktree, that is given
// in its main repository's root; elsewhere, in dir or in a directory above
// it, which on the pinned Claude Code covers every directory below it. A
// folder counts as given and with its symbolic links resolved, as Claude Code
// records the directory it runs in. A record that does not exist holds no
// trust. Trusted never changes the record.
func Trusted(path, dir string) (bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var record struct {
		Projects map[string]struct {
			HasTrustDialogAccepted bool `json:"hasTrustDialogAccepted"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(data, &record); err != nil {
		return false, fmt.Errorf("%s is not Claude Code's JSON record: %w", path, err)
	}
	folder, repository := TrustFolder(dir)
	resolved, err := filepath.EvalSymlinks(folder)
	if err != nil {
		resolved = folder
	}
	if repository {
		// Only the repository's own entry trusts it, recorded as git or as
		// Claude Code names its path.
		for project, entry := range record.Projects {
			if !entry.HasTrustDialogAccepted {
				continue
			}
			if project == folder || project == resolved {
				return true, nil
			}
			if p, err := filepath.EvalSymlinks(project); err == nil && p == resolved {
				return true, nil
			}
		}
		return false, nil
	}
	for _, d := range []string{folder, resolved} {
		for {
			if record.Projects[d].HasTrustDialogAccepted {
				return true, nil
			}
			parent := filepath.Dir(d)
			if parent == d {
				break
			}
			d = parent
		}
	}
	return false, nil
}
