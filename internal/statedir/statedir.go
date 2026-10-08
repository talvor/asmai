// SPDX-License-Identifier: Apache-2.0

// Package statedir names the files the factory keeps in its state directory,
// ~/.local/state/asmai, and makes the directory readable only by the user.
package statedir

import (
	"fmt"
	"os"
	"path/filepath"
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
	// Agents holds each agent's working directory, by its address.
	Agents string
	// Repositories holds AsmAI's own clone of each registered repository,
	// in a directory named for it.
	Repositories string
}

// Default returns the paths in the user's state directory,
// ~/.local/state/asmai.
func Default() (Paths, error) {
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
		Agents:       filepath.Join(dir, "agents"),
		Repositories: filepath.Join(dir, "repositories"),
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
