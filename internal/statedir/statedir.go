// SPDX-License-Identifier: Apache-2.0

// Package statedir names the files the factory keeps in its state directory,
// ~/.local/state/asmai, and makes the directory readable only by the user.
package statedir

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// maxSocketPath is the longest path a Unix socket can be bound to on every
// target platform: macOS allows 103 bytes, Linux 107.
const maxSocketPath = 103

// Paths are the files in one state directory.
type Paths struct {
	// Dir is the state directory itself.
	Dir string
	// Socket is the Unix socket the daemon listens on, the only way commands
	// reach it.
	Socket string
	// Lock is held by the running daemon, so that each user runs at most one.
	Lock string
	// Store is the SQLite store.
	Store string
	// Log is the daemon's current log file; its older files are Log.1 to Log.4.
	Log string
	// Providers holds AsmAI's own copies of the provider CLIs, one directory
	// per provider and version, apart from the user's own installations.
	Providers string
	// Bin holds Executable, and is first on every agent session's PATH.
	Bin string
	// Executable is the daemon's copy of the asmai executable, at one fixed
	// path: every agent session runs it, and its hooks name it.
	Executable string
	// GitHooks holds the git hooks every worker's git runs, which add the
	// commit trailers and then run the repository's own hooks.
	GitHooks string
	// Agents holds each agent's working directory, by its address.
	Agents string
	// Plugins holds each agent's per-session plugin, the skills its session
	// is given, in a directory named for its address.
	Plugins string
	// Repositories holds AsmAI's own clone of each registered repository,
	// in a directory named for it.
	Repositories string
	// Workspaces holds each assignment's workspace and its temporary
	// directory, in a directory named for its job and assignment.
	Workspaces string
	// Views holds the leaders' read-only view of each repository job, in a
	// directory named for the job.
	Views string
}

// Default returns the paths in the user's state directory,
// ~/.local/state/asmai.
func Default() (Paths, error) {
	if dir := os.Getenv("ASMAI_STATE_DIR"); dir != "" {
		if !filepath.IsAbs(dir) {
			return Paths{}, fmt.Errorf("ASMAI_STATE_DIR must be an absolute path")
		}
		p := At(filepath.Clean(dir))
		if len(p.Socket) > maxSocketPath {
			return Paths{}, fmt.Errorf("the state directory %s is too deep: its socket path is %d bytes, and a Unix socket allows %d", p.Dir, len(p.Socket), maxSocketPath)
		}
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, fmt.Errorf("finding the state directory: %w", err)
	}
	p := At(filepath.Join(home, ".local", "state", "asmai"))
	if len(p.Socket) > maxSocketPath {
		return Paths{}, fmt.Errorf("the state directory %s is too deep: its socket path is %d bytes, and a Unix socket allows %d", p.Dir, len(p.Socket), maxSocketPath)
	}
	return p, nil
}

// At returns the paths in the state directory dir.
func At(dir string) Paths {
	return Paths{
		Dir:          dir,
		Socket:       filepath.Join(dir, "daemon.sock"),
		Lock:         filepath.Join(dir, "daemon.lock"),
		Store:        filepath.Join(dir, "store.db"),
		Log:          filepath.Join(dir, "daemon.log"),
		Providers:    filepath.Join(dir, "providers"),
		Bin:          filepath.Join(dir, "bin"),
		Executable:   filepath.Join(dir, "bin", "asmai"),
		GitHooks:     filepath.Join(dir, "githooks"),
		Agents:       filepath.Join(dir, "agents"),
		Plugins:      filepath.Join(dir, "plugins"),
		Repositories: filepath.Join(dir, "repositories"),
		Workspaces:   filepath.Join(dir, "workspaces"),
		Views:        filepath.Join(dir, "views"),
	}
}

// Prepare creates the state directory if it is missing and makes it readable
// only by the user, so that its socket, store and log are too.
func (p Paths) Prepare() error {
	if err := os.MkdirAll(p.Dir, 0o700); err != nil {
		return fmt.Errorf("creating the state directory: %w", err)
	}
	if err := os.Chmod(p.Dir, 0o700); err != nil {
		return fmt.Errorf("making the state directory private: %w", err)
	}
	return nil
}

// Folders are the folders the factory keeps in the state directory, besides
// the directory itself.
func (p Paths) Folders() []string {
	return []string{p.Providers, p.Bin, p.GitHooks, p.Agents, p.Repositories, p.Workspaces, p.Views}
}

// MakeFolders prepares the state directory and makes each of its Folders
// that is missing, readable only by the user. It reports whether it made
// any.
func (p Paths) MakeFolders() (made bool, err error) {
	for _, dir := range append([]string{p.Dir}, p.Folders()...) {
		if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
			made = true
		}
	}
	if err := p.Prepare(); err != nil {
		return false, err
	}
	for _, dir := range p.Folders() {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return false, fmt.Errorf("creating %s: %w", dir, err)
		}
	}
	return made, nil
}

// Clones returns AsmAI's clone of each registered repository, in
// Repositories, by name: every directory there but a registration's
// unfinished clone, whose name starts with a dot. It returns none when
// Repositories is missing.
func (p Paths) Clones() ([]string, error) {
	entries, err := os.ReadDir(p.Repositories)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the registered repositories' clones: %w", err)
	}
	var clones []string
	for _, entry := range entries {
		if entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") {
			clones = append(clones, filepath.Join(p.Repositories, entry.Name()))
		}
	}
	return clones, nil
}
